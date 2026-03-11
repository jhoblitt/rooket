// Package registry manages a local OCI registry container via podman.
package registry

import (
	"fmt"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
)

const (
	RegistryImage       = "docker.io/library/registry:2"
	RegistryInternalPort = 5000
)

// Config holds registry configuration.
type Config struct {
	// Name is the podman container name for the registry.
	Name string
	// HostPort is the port bound on the host (e.g. 5001).
	HostPort int
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
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// Create starts the registry container if it does not already exist.
func Create(cfg Config) error {
	if Exists(cfg.Name) {
		fmt.Printf("registry container %q already exists, skipping creation\n", cfg.Name)
		return nil
	}
	return run.Cmd("podman", "run",
		"-d",
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HostPort, RegistryInternalPort),
		"--name", cfg.Name,
		RegistryImage,
	)
}

// ConnectNetwork connects the registry container to the named podman network so
// that kind nodes on that network can pull images by container name.
func ConnectNetwork(containerName, network string) error {
	// Check if already connected.
	out, err := run.Output("podman", "inspect", containerName, "--format", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}}\n{{end}}")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == network {
				fmt.Printf("registry already connected to network %q\n", network)
				return nil
			}
		}
	}
	return run.Cmd("podman", "network", "connect", network, containerName)
}

// Delete stops and removes the registry container.
func Delete(name string) error {
	if !Exists(name) {
		return nil
	}
	if err := run.Cmd("podman", "stop", name); err != nil {
		return err
	}
	return run.Cmd("podman", "rm", name)
}
