// Package engine selects the container engine — podman or docker — that rooket
// drives for image builds, the local OCI registry, and the kind provider.
package engine

import (
	"fmt"
	"os/exec"
	"strings"
)

// Engine is a supported container engine. Its string value doubles as the
// binary name rooket execs and the value kind expects in
// KIND_EXPERIMENTAL_PROVIDER and rook's make expects in DOCKERCMD.
type Engine string

const (
	Podman Engine = "podman"
	Docker Engine = "docker"
)

// Default is the engine used when neither --engine nor ROOKET_ENGINE is set.
const Default = Podman

// EnvVar is the environment variable that selects the engine when --engine is
// not passed.
const EnvVar = "ROOKET_ENGINE"

// String returns the engine's binary name (also its kind provider name).
func (e Engine) String() string { return string(e) }

// PushArgs returns the engine arguments to push ref to the local registry over
// plain HTTP. podman needs --tls-verify=false to allow it; docker rejects that
// flag but already treats localhost (loopback) registries as insecure, so the
// flag is simply omitted.
func (e Engine) PushArgs(ref string) []string {
	if e == Podman {
		return []string{"push", "--tls-verify=false", ref}
	}
	return []string{"push", ref}
}

// Parse validates a user-supplied engine name and returns the typed Engine.
func Parse(name string) (Engine, error) {
	switch Engine(name) {
	case Podman, Docker:
		return Engine(name), nil
	default:
		return "", fmt.Errorf("unsupported container engine %q (must be %q or %q)", name, Podman, Docker)
	}
}

// Prober runs an engine "info" query and returns its trimmed stdout, mirroring
// run.Output. It is the seam that lets Resolve be unit-tested without a real
// container engine on the host.
type Prober func(name string, args ...string) (string, error)

// DefaultProber is the production Prober. It execs the engine directly (rather
// than via the run package) so probe commands stay quiet and add no import
// dependency.
func DefaultProber(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// usable reports whether the engine responds to `info`, i.e. its binary is on
// PATH and its daemon/backend is reachable.
func usable(e Engine, probe Prober) bool {
	_, err := probe(e.String(), "info")
	return err == nil
}

// podmanRootless reports whether the local podman runs rootless. The error is
// non-nil when podman is not usable at all (not installed, backend unreachable),
// which Resolve treats the same as "not suitable" and falls back from.
func podmanRootless(probe Prober) (bool, error) {
	out, err := probe(Podman.String(), "info", "--format", "{{.Host.Security.Rootless}}")
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// Resolve selects the container engine rooket will drive, applying runtime
// detection to the engine the user requested (via --engine or $ROOKET_ENGINE).
//
// rooket requires rootful podman: it shares the host's image store and runs
// privileged helper containers, neither of which works under rootless podman.
// So when podman is auto-selected but is only available rootless (or is not
// usable at all), Resolve warns and falls back to docker. When the user
// explicitly asked for podman, that fallback is suppressed and the rootless/
// unusable condition is a hard error — an explicit choice is honored or fails,
// never silently switched. When docker is requested it is used as long as it is
// reachable. If no suitable engine is found, Resolve returns an error.
//
// explicit reports whether requested came from a deliberate --engine/
// $ROOKET_ENGINE selection rather than the built-in Default.
//
// warn receives human-readable, non-fatal messages to surface to the user.
func Resolve(requested Engine, explicit bool, probe Prober, warn func(string)) (Engine, error) {
	switch requested {
	case Docker:
		if !usable(Docker, probe) {
			return "", fmt.Errorf("docker was requested but is not usable (is the docker daemon running?)")
		}
		return Docker, nil
	case Podman:
		switch rootless, err := podmanRootless(probe); {
		case err != nil:
			if explicit {
				return "", fmt.Errorf("podman was requested but is not usable: %w", err)
			}
			warn(fmt.Sprintf("podman is not usable (%v); falling back to docker", err))
		case rootless:
			if explicit {
				return "", fmt.Errorf("podman was requested but is running rootless, which rooket does not support")
			}
			warn("podman is running rootless, which rooket does not support; falling back to docker")
		default:
			return Podman, nil
		}
		if usable(Docker, probe) {
			return Docker, nil
		}
		return "", fmt.Errorf("no suitable container engine found: rooket needs rootful podman or a running docker")
	default:
		return "", fmt.Errorf("unsupported container engine %q (must be %q or %q)", requested, Podman, Docker)
	}
}
