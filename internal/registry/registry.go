// Package registry manages a local OCI registry container via the configured
// container engine (podman or docker).
package registry

import (
	"fmt"
	"strings" // for Exists

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/run"
)

const (
	RegistryImage        = "docker.io/library/registry:2"
	RegistryInternalPort = 5000
)

// Config holds registry configuration.
type Config struct {
	// Engine is the container engine (podman or docker) that runs the registry.
	Engine engine.Engine
	// Name is the registry container name.
	Name string
	// HostPort is the port bound on the host (e.g. 5001).
	HostPort int
	// Network is the container network to attach the registry to (e.g. "kind").
	// Membership is declared at container creation because rootless podman's
	// default "pasta" mode does not support attaching afterwards; doing it at
	// creation also works for docker.
	Network string
}

// ContainerName returns the registry container name for a given cluster name.
func ContainerName(clusterName string) string {
	return clusterName + "-registry"
}

// HostAddr returns the host-accessible registry address (e.g. "localhost:5001").
func (c *Config) HostAddr() string {
	return fmt.Sprintf("localhost:%d", c.HostPort)
}

// InClusterAddr returns the address reachable from inside cluster nodes.
// kind nodes share a container network and can reach the registry by name.
func (c *Config) InClusterAddr() string {
	return fmt.Sprintf("%s:%d", c.Name, RegistryInternalPort)
}

// Exists returns true if the registry container already exists (running or stopped).
func Exists(eng engine.Engine, name string) bool {
	out, err := run.Output(eng.String(), "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// Create starts the registry container if it does not already exist.
// The registry must be created after the kind cluster so that cfg.Network
// ("kind") already exists. Network membership is declared at creation time
// because rootless podman cannot attach a container to a network afterwards
// in its default "pasta" mode (and doing it at creation also works for docker).
func Create(cfg Config) error {
	if Exists(cfg.Engine, cfg.Name) {
		fmt.Printf("registry container %q already exists, skipping creation\n", cfg.Name)
		return nil
	}
	args := []string{
		"run", "-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HostPort, RegistryInternalPort),
		"--name", cfg.Name,
	}
	if cfg.Network != "" {
		args = append(args, "--network="+cfg.Network)
	}
	args = append(args, RegistryImage)
	return run.Cmd(cfg.Engine.String(), args...)
}

// Delete stops and removes the registry container. The -v removes the
// container's anonymous volume (registry:2 declares VOLUME /var/lib/registry);
// without it each create/delete cycle leaks a ~600MB volume.
func Delete(eng engine.Engine, name string) error {
	if !Exists(eng, name) {
		return nil
	}
	return run.Cmd(eng.String(), "rm", "-f", "-v", name)
}
