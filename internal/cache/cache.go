// Package cache manages the host-wide OCI pull-through cache: a single zot
// container, shared by every rooket cluster on the host, that proxies upstream
// registries so an image is fetched from the internet once rather than once per
// node per cluster.
package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/jhoblitt/rooket/internal/zot"
)

const (
	// ContainerName deliberately carries no cluster name. rooket's teardown
	// paths delete "<cluster>-registry" by exact, anchored name match, so a
	// container named this way is invisible to them — which is what lets a
	// pull survive 'rooket down' and seed the next cluster.
	ContainerName = "rooket-cache"

	// VolumeName must be a *named* volume. The container is recreated whenever
	// the zot image or the generated config changes, and 'podman rm -v' reaps
	// only anonymous volumes — an anonymous one would silently discard the
	// whole cache on every recreation.
	VolumeName = "rooket-cache-data"

	// HostPort binds the cache for debugging and for host-side pulls. Per-cluster
	// registries are allocated from 5001 upward, so 5000 stays free for this.
	HostPort = 5000
)

// Upstreams are the registries the cache proxies, covering everything a rook
// deployment pulls plus common headroom.
//
// zot resolves an upstream from the repository prefix in the request path, not
// from the ?ns= parameter containerd sends, so each upstream must be listed
// here and given its own hosts.toml on the nodes. The failure mode of an
// incomplete list is benign: a registry that is absent is simply not proxied,
// and its images pull directly from the internet exactly as they did before the
// cache existed.
var Upstreams = []string{
	"quay.io",
	"registry.k8s.io",
	"docker.io",
	"ghcr.io",
	"gcr.io",
}

// Config holds the parameters needed to run the cache container.
type Config struct {
	// Engine is the container engine (podman or docker) that runs the cache.
	Engine engine.Engine
	// Network is the container network to attach to. kind's podman provider
	// hardcodes "kind" (const fixedNetworkName) and never removes it, so one
	// cache on that network is reachable by name from every cluster's nodes.
	Network string
	// HostConfigPath is the generated zot config on the host, bind-mounted read-only.
	HostConfigPath string
	// Upstreams are the registries to proxy; defaults to Upstreams when empty.
	Upstreams []string
}

// InClusterAddr returns the address cluster nodes use to reach the cache.
func InClusterAddr() string {
	return fmt.Sprintf("%s:%d", ContainerName, zot.InternalPort)
}

// upstreamURL maps a registry namespace to the URL zot pulls from. docker.io is
// the one namespace whose registry API lives on a different host than its name.
func upstreamURL(ns string) string {
	if ns == "docker.io" {
		return "https://index.docker.io"
	}
	return "https://" + ns
}

// GenerateConfig renders the zot configuration proxying each upstream under a
// repository prefix equal to its namespace, so that upstream "cephcsi/cephcsi"
// on quay.io is served locally as "quay.io/cephcsi/cephcsi" — the path the
// nodes' hosts.toml asks for.
func GenerateConfig(upstreams []string) ([]byte, error) {
	if len(upstreams) == 0 {
		upstreams = Upstreams
	}
	regs := make([]zot.SyncRegistry, 0, len(upstreams))
	for _, ns := range upstreams {
		regs = append(regs, zot.SyncRegistry{
			URLs:      []string{upstreamURL(ns)},
			Content:   []zot.Content{{Prefix: "**", Destination: "/" + ns}},
			OnDemand:  true,
			TLSVerify: true,
		})
	}
	cfg := zot.Config{
		DistSpecVersion: zot.DistSpecVersion,
		Storage:         zot.Storage{RootDirectory: zot.StoragePath, GC: true},
		// zot stores OCI-native; docker2s2 lets it also serve the Docker
		// schema-2 manifests much of the ecosystem still publishes.
		HTTP: zot.HTTP{Address: "0.0.0.0", Port: fmt.Sprint(zot.InternalPort), Compat: []string{"docker2s2"}},
		Log:  zot.Log{Level: "info"},
		Extensions: &zot.Extensions{
			Sync: zot.Sync{Enable: true, Registries: regs},
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Exists returns true if the cache container already exists (running or stopped).
func Exists(out io.Writer, eng engine.Engine) bool {
	res, err := run.OutputTo(out, eng.String(), "ps", "-a",
		"--filter", "name=^"+ContainerName+"$", "--format", "{{.Names}}")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(res, "\n") {
		if strings.TrimSpace(line) == ContainerName {
			return true
		}
	}
	return false
}

// runArgs renders the engine arguments that create the cache container.
//
// The config mount carries ",z": the file is generated under the user's home,
// whose SELinux type a confined container cannot read, and without a relabel
// zot exits at startup on an enforcing host — leaving every node to pull from
// upstream. The option is inert where SELinux is disabled and is understood by
// both engines.
func runArgs(cfg Config) []string {
	args := []string{
		"run", "-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", HostPort, zot.InternalPort),
		"-v", VolumeName + ":" + zot.StoragePath,
		"-v", cfg.HostConfigPath + ":" + zot.ConfigPath + ":ro,z",
		"--name", ContainerName,
	}
	if cfg.Network != "" {
		args = append(args, "--network="+cfg.Network)
	}
	return append(args, zot.Image, "serve", zot.ConfigPath)
}

// containerImage returns the image reference the existing cache container was
// created from.
func containerImage(out io.Writer, eng engine.Engine) (string, error) {
	res, err := run.OutputTo(out, eng.String(), "inspect", "-f", "{{.Config.Image}}", ContainerName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res), nil
}

// Create starts the cache container if it does not already exist, replacing
// one built from a different zot image. Like the per-cluster registry it must
// be created after a kind cluster exists, so that cfg.Network is present to
// attach to.
//
// The image check makes a moved pin reach a host that already has a cache;
// nothing else here would ever replace that container. Its cost is that two
// rooket versions used side by side flap it between their pins, which is the
// same trade setupCache already documents for a changed config: a flap costs a
// pull falling back upstream, never a failed bring-up.
//
// Unlike the per-cluster registry the cache is a host-wide singleton, so the
// Exists check races: rooket supports one cluster per rook clone and they are
// brought up concurrently, and two 'rooket up' runs can both find no cache and
// both try to create it. The engine settles that — the second gets "name
// already in use" — so a failed create re-checks and reports success if the
// winner's container is there. Losing the race is the expected path, not an
// error; without this the loser would silently skip cache wiring and pull
// everything from upstream.
func Create(out io.Writer, cfg Config) error {
	if Exists(out, cfg.Engine) {
		img, imgErr := containerImage(out, cfg.Engine)
		if imgErr == nil && img != zot.Image {
			// A container from an older pin keeps running its own image
			// forever otherwise: nothing else here would ever replace it, so
			// a pin moved for a fix — or for an architecture this host can
			// actually run — would never reach an existing cache.
			run.Fprintf(out, "cache container %q runs %s; recreating it with %s (cached images are preserved in volume %s)\n",
				ContainerName, img, zot.Image, VolumeName)
			// A concurrent run may have removed it already, which is the
			// outcome this wanted anyway.
			if err := RemoveContainer(out, cfg.Engine); err != nil && Exists(out, cfg.Engine) {
				return fmt.Errorf("recreate cache container: %w", err)
			}
		} else {
			// Stopped is the common state after a host reboot (--restart=always
			// covers only the engine restarting), and a skipped-over dead cache
			// silently sends every node back to pulling from upstream. Starting an
			// already-running container is a no-op.
			run.Fprintf(out, "cache container %q already exists; ensuring it is running\n", ContainerName)
			err := run.CmdTo(out, cfg.Engine.String(), "start", ContainerName)
			if err == nil {
				return nil
			}
			// The same race the create below absorbs, seen from the other side: a
			// concurrent run recreating the cache for a changed config removes the
			// container between the check above and this start. Fall through and
			// create it rather than reporting the loss as a failure, which would
			// strand this cluster with no cache upstreams for its whole life.
			if Exists(out, cfg.Engine) {
				return err
			}
			run.Fprintf(out, "cache container %q went away while starting it; creating it\n", ContainerName)
		}
	}
	err := run.CmdTo(out, cfg.Engine.String(), runArgs(cfg)...)
	if err != nil && Exists(out, cfg.Engine) {
		run.Fprintf(out, "cache container %q was created concurrently; using it\n", ContainerName)
		return nil
	}
	return err
}

// RemoveContainer removes the cache container but keeps VolumeName, so a
// config or image change can recreate the container without refetching
// everything it has already cached.
func RemoveContainer(out io.Writer, eng engine.Engine) error {
	if !Exists(out, eng) {
		return nil
	}
	return run.CmdTo(out, eng.String(), "rm", "-f", ContainerName)
}

// Delete removes the cache container and its named volume. The volume needs its
// own removal: 'rm -v' reaps only anonymous volumes, so stopping at the
// container would report success while leaving the entire cache on disk.
func Delete(out io.Writer, eng engine.Engine) error {
	if err := RemoveContainer(out, eng); err != nil {
		return err
	}
	if err := run.CmdTo(out, eng.String(), "volume", "rm", "--force", VolumeName); err != nil {
		return fmt.Errorf("remove cache volume %s: %w", VolumeName, err)
	}
	return nil
}
