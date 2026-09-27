// Package registry manages a cluster's local OCI registry container — the
// push target for locally built images — via the configured container engine
// (podman or docker). The registry runs zot, the same pinned image as the
// shared pull-through cache, so bring-up needs no second registry image and
// both roles carry one set of quirks.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/jhoblitt/rooket/internal/zot"
)

const (
	// ConfigLabel records on the container which generated config it was
	// created from, so the next run can tell a current registry from one that
	// must be recreated by inspecting the container itself.
	ConfigLabel = "dev.rooket.registry-config"

	// ReadyTimeout bounds the wait for zot to answer after the engine
	// launches it. A local zot serves in well under a second; this is sized
	// for a loaded host, not for a registry that is coming up slowly.
	ReadyTimeout = 30 * time.Second
)

// Config holds registry configuration.
type Config struct {
	// Engine is the container engine (podman or docker) that runs the registry.
	Engine engine.Engine
	// Name is the registry container name.
	Name string
	// HostPort is the port bound on the host (e.g. 5001).
	HostPort int
	// Network is the container network to attach the registry to (e.g. "kind")
	// so cluster nodes can reach the registry by name.
	Network string
	// HostConfigPath is the generated zot config on the host, bind-mounted
	// read-only.
	HostConfigPath string
}

// ContainerName returns the registry container name for a given cluster name.
func ContainerName(clusterName string) string {
	return clusterName + "-registry"
}

// GenerateConfig renders the registry's zot configuration: a plain OCI
// registry — no sync section, which would turn it into a second mirror
// shadowing the shared cache — that also accepts the Docker schema-2
// manifests docker engines push, and garbage-collects the blobs each
// same-tag re-push orphans.
func GenerateConfig() ([]byte, error) {
	cfg := zot.Config{
		DistSpecVersion: zot.DistSpecVersion,
		Storage:         zot.Storage{RootDirectory: zot.StoragePath, GC: true},
		// The listener binds all interfaces because cluster nodes connect
		// over the kind network; loopback-only exposure on the host side is
		// the port mapping's job.
		HTTP: zot.HTTP{Address: "0.0.0.0", Port: fmt.Sprint(zot.InternalPort), Compat: []string{"docker2s2"}},
		Log:  zot.Log{Level: "info"},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Exists returns true if the registry container already exists (running or
// stopped). An engine that cannot be queried reads as no container; see Lookup.
func Exists(out io.Writer, eng engine.Engine, name string) bool {
	found, _ := Lookup(out, eng, name)
	return found
}

// Lookup is Exists for a caller that must tell a container that is not there
// from an engine that could not say.
func Lookup(out io.Writer, eng engine.Engine, name string) (bool, error) {
	res, err := run.OutputTo(out, eng.String(), "ps", "-a",
		"--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(res, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// runArgs renders the engine arguments that create the registry container.
//
// The config mount carries ",z": the file is generated under the user's home,
// whose SELinux type a confined container cannot read, and without a relabel
// zot exits at startup on an enforcing host. The option is inert where SELinux
// is disabled and is understood by both engines.
func runArgs(cfg Config, configSum string) []string {
	args := []string{
		"run", "-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HostPort, zot.InternalPort),
		"-v", cfg.HostConfigPath + ":" + zot.ConfigPath + ":ro,z",
		"--label", ConfigLabel + "=" + configSum,
		"--name", cfg.Name,
	}
	if cfg.Network != "" {
		args = append(args, "--network="+cfg.Network)
	}
	return append(args, zot.Image, "serve", zot.ConfigPath)
}

// configSum identifies a generated config by content. It is what the running
// container is labelled with, so "is this registry current" is answered by
// what the container carries rather than by what some earlier step in this
// process decided — a distinction that matters because an interrupt between
// installing a config and recreating the container would otherwise strand the
// registry on the old one for good.
func configSum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// inspectField returns one Go-template field of an existing container.
func inspectField(out io.Writer, eng engine.Engine, name, format string) (string, error) {
	res, err := run.OutputTo(out, eng.String(), "inspect", "-f", format, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res), nil
}

// current reports whether an existing container is already running the image
// and config this run wants.
//
// Anything unreadable answers "current": recreating a registry destroys the
// images pushed to it, so that needs positive evidence of a mismatch, not the
// absence of evidence.
func current(out io.Writer, eng engine.Engine, name, wantSum string) bool {
	img, err := inspectField(out, eng, name, "{{.Config.Image}}")
	if err != nil {
		return true
	}
	sum, err := inspectField(out, eng, name, "{{index .Config.Labels \""+ConfigLabel+"\"}}")
	if err != nil {
		return true
	}
	return img == zot.Image && sum == wantSum
}

// ensureImage makes sure the engine can start the registry without reaching
// the network, pulling only when the image is absent.
//
// It runs before an outdated container is removed. Recreating destroys the
// images pushed to that registry, so discovering only afterwards that the new
// image cannot be fetched would leave the cluster with no registry at all;
// this way a failed pull leaves the old one serving.
func ensureImage(out io.Writer, eng engine.Engine) error {
	if id, err := run.OutputTo(out, eng.String(), "images", "-q", zot.Image); err == nil && strings.TrimSpace(id) != "" {
		return nil
	}
	run.Fprintf(out, "pulling %s\n", zot.Image)
	return run.CmdTo(out, eng.String(), "pull", zot.Image)
}

// WaitReady blocks until the registry answers the distribution API's base
// endpoint, or timeout elapses.
//
// The engine's 'run -d' returns once it has accepted the container, which
// says nothing about zot having started: an unparseable config or an image
// that cannot run on this architecture leaves a container that exits
// immediately. Without this the bring-up reports a registry that is not there
// and the failure resurfaces later as a confusing push error.
func WaitReady(out io.Writer, hostPort int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", hostPort)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			// 401 is a registry that is up and wants credentials. rooket's
			// is open, but readiness is the question here, not authorization.
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry on port %d did not answer %s within %s (last error: %v); "+
				"inspect the registry container's logs", hostPort, url, timeout, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Create brings this cluster's registry up and returns once it is serving.
// The registry must be created after the kind cluster so that cfg.Network
// ("kind") already exists; attaching at creation time makes it reachable by
// name from the cluster nodes.
//
// An existing container that does not match the wanted image and config — a
// registry:2 survivor from before the switch to zot, an older zot pin, or a
// config a rooket upgrade changed — is recreated rather than started. Its
// pushed images are lost, but the build stamp detects the empty registry and
// the next 'rooket build push' repopulates it; images put there by 'rooket
// load' have to be loaded again.
func Create(out io.Writer, cfg Config) error {
	sum, err := configSum(cfg.HostConfigPath)
	if err != nil {
		return fmt.Errorf("read registry config: %w", err)
	}
	if Exists(out, cfg.Engine, cfg.Name) {
		if current(out, cfg.Engine, cfg.Name, sum) {
			// A container from an earlier session can be stopped: a host reboot
			// leaves it Exited, since --restart=always covers only the engine
			// restarting, not the machine booting. Start it — a no-op when it is
			// already running — so a re-run resumes a working registry rather than
			// skipping past a dead one and failing later at push time.
			run.Fprintf(out, "registry container %q already exists; ensuring it is running\n", cfg.Name)
			if err := run.CmdTo(out, cfg.Engine.String(), "start", cfg.Name); err != nil {
				return err
			}
			return WaitReady(out, cfg.HostPort, ReadyTimeout)
		}
		run.Fprintf(out, "registry %q does not match the wanted image or config; recreating it "+
			"(re-push with 'rooket build push'; re-load any 'rooket load'ed images)\n", cfg.Name)
		if err := ensureImage(out, cfg.Engine); err != nil {
			return fmt.Errorf("make %s available before replacing registry %q: %w", zot.Image, cfg.Name, err)
		}
		if err := Delete(out, cfg.Engine, cfg.Name); err != nil {
			return fmt.Errorf("remove outdated registry %q: %w", cfg.Name, err)
		}
	}
	if err := run.CmdTo(out, cfg.Engine.String(), runArgs(cfg, sum)...); err != nil {
		return err
	}
	return WaitReady(out, cfg.HostPort, ReadyTimeout)
}

// Delete stops and removes the registry container. The -v is for registries
// that predate the switch to zot: registry:2 declared VOLUME
// /var/lib/registry, and removing such a container without -v leaks a
// ~600MB anonymous volume. zot declares no volume, so for current
// containers it is a no-op.
func Delete(out io.Writer, eng engine.Engine, name string) error {
	if !Exists(out, eng, name) {
		return nil
	}
	return run.CmdTo(out, eng.String(), "rm", "-f", "-v", name)
}
