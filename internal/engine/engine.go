// Package engine selects the container engine — podman or docker — that rooket
// drives for image builds, the local OCI registry, and the kind provider.
package engine

import "fmt"

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
