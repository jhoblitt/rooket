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

	// ZotImage is pinned: an unpinned cache would resync on every silent
	// upstream rebuild.
	ZotImage = "ghcr.io/project-zot/zot-linux-amd64:v2.1.5"

	// InternalPort is the port zot listens on inside the container.
	InternalPort = 5000

	// HostPort binds the cache for debugging and for host-side pulls. Per-cluster
	// registries are allocated from 5001 upward, so 5000 stays free for this.
	HostPort = 5000

	// ConfigPath is where the generated zot config is bind-mounted in the
	// container.
	ConfigPath = "/etc/zot/config.json"

	// StoragePath is zot's root directory inside the container, backed by
	// VolumeName.
	StoragePath = "/var/lib/zot"
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
	return fmt.Sprintf("%s:%d", ContainerName, InternalPort)
}

// upstreamURL maps a registry namespace to the URL zot pulls from. docker.io is
// the one namespace whose registry API lives on a different host than its name.
func upstreamURL(ns string) string {
	if ns == "docker.io" {
		return "https://index.docker.io"
	}
	return "https://" + ns
}

type zotStorage struct {
	RootDirectory string `json:"rootDirectory"`
	GC            bool   `json:"gc"`
}

type zotHTTP struct {
	Address string   `json:"address"`
	Port    string   `json:"port"`
	Compat  []string `json:"compat,omitempty"`
}

type zotLog struct {
	Level string `json:"level"`
}

type zotContent struct {
	Prefix      string `json:"prefix"`
	Destination string `json:"destination"`
}

type zotRegistry struct {
	URLs      []string     `json:"urls"`
	Content   []zotContent `json:"content"`
	OnDemand  bool         `json:"onDemand"`
	TLSVerify bool         `json:"tlsVerify"`
}

type zotSync struct {
	Enable     bool          `json:"enable"`
	Registries []zotRegistry `json:"registries"`
}

type zotExtensions struct {
	Sync zotSync `json:"sync"`
}

type zotConfig struct {
	DistSpecVersion string        `json:"distSpecVersion"`
	Storage         zotStorage    `json:"storage"`
	HTTP            zotHTTP       `json:"http"`
	Log             zotLog        `json:"log"`
	Extensions      zotExtensions `json:"extensions"`
}

// GenerateConfig renders the zot configuration proxying each upstream under a
// repository prefix equal to its namespace, so that upstream "cephcsi/cephcsi"
// on quay.io is served locally as "quay.io/cephcsi/cephcsi" — the path the
// nodes' hosts.toml asks for.
func GenerateConfig(upstreams []string) ([]byte, error) {
	if len(upstreams) == 0 {
		upstreams = Upstreams
	}
	regs := make([]zotRegistry, 0, len(upstreams))
	for _, ns := range upstreams {
		regs = append(regs, zotRegistry{
			URLs:      []string{upstreamURL(ns)},
			Content:   []zotContent{{Prefix: "**", Destination: "/" + ns}},
			OnDemand:  true,
			TLSVerify: true,
		})
	}
	cfg := zotConfig{
		DistSpecVersion: "1.1.1",
		Storage:         zotStorage{RootDirectory: StoragePath, GC: true},
		// zot stores OCI-native; docker2s2 lets it also serve the Docker
		// schema-2 manifests much of the ecosystem still publishes.
		HTTP: zotHTTP{Address: "0.0.0.0", Port: fmt.Sprint(InternalPort), Compat: []string{"docker2s2"}},
		Log:  zotLog{Level: "info"},
		Extensions: zotExtensions{
			Sync: zotSync{Enable: true, Registries: regs},
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

// Create starts the cache container if it does not already exist. Like the
// per-cluster registry it must be created after a kind cluster exists, so that
// cfg.Network is present to attach to.
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
		run.Fprintf(out, "cache container %q already exists, skipping creation\n", ContainerName)
		return nil
	}
	args := []string{
		"run", "-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", HostPort, InternalPort),
		"-v", VolumeName + ":" + StoragePath,
		"-v", cfg.HostConfigPath + ":" + ConfigPath + ":ro",
		"--name", ContainerName,
	}
	if cfg.Network != "" {
		args = append(args, "--network="+cfg.Network)
	}
	args = append(args, ZotImage, "serve", ConfigPath)
	err := run.CmdTo(out, cfg.Engine.String(), args...)
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
