// Package registry manages a local OCI registry container via podman.
package registry

import (
	"fmt"
	"strings" // for Exists

	"github.com/jhoblitt/rooket/internal/run"
)

const (
	RegistryImage        = "docker.io/library/registry:2"
	RegistryInternalPort = 5000
)

// Config holds registry configuration.
type Config struct {
	// Name is the podman container name for the registry.
	Name string
	// HostPort is the port bound on the host (e.g. 5001).
	HostPort int
	// Network is the podman network to attach the container to (e.g. "kind")
	// so cluster nodes can reach the registry by name.
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
// kind nodes share a podman network and can reach the registry container by name.
func (c *Config) InClusterAddr() string {
	return fmt.Sprintf("%s:%d", c.Name, RegistryInternalPort)
}

// Exists returns true if the registry container already exists (running or stopped).
func Exists(name string) bool {
	out, err := run.Output("podman", "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
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
// ("kind") already exists; attaching at creation time makes it reachable by
// name from the cluster nodes.
func Create(cfg Config) error {
	if Exists(cfg.Name) {
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
	return run.Cmd("podman", args...)
}

// Delete stops and removes the registry container. The -v removes the
// container's anonymous volume (registry:2 declares VOLUME /var/lib/registry);
// without it each create/delete cycle leaks a ~600MB volume.
func Delete(name string) error {
	if !Exists(name) {
		return nil
	}
	return run.Cmd("podman", "rm", "-f", "-v", name)
}
