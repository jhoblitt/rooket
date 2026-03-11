// Package cluster manages kind Kubernetes clusters configured for Rook testing.
package cluster

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jhoblitt/rooket/internal/disks"
	"github.com/jhoblitt/rooket/internal/run"
)

// Config holds all parameters needed to create a kind cluster for Rook.
type Config struct {
	// Name is the kind cluster name.
	Name string
	// Workers is the number of worker nodes.
	Workers int
	// RegistryName is the podman container name of the local OCI registry.
	RegistryName string
	// RegistryHostPort is the port the registry listens on on the host.
	RegistryHostPort int
	// WorkerDisks maps worker index → Disk descriptors.
	// Each disk's HostPath (/dev/loopN) is bind-mounted into the corresponding
	// worker node. crun adds the device to the container's cgroup device
	// allowlist automatically for bind-mounted device files.
	WorkerDisks map[int][]disks.Disk
}

// kindConfigTmpl is the kind cluster configuration template.
var kindConfigTmpl = template.Must(template.New("kind").Parse(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
{{- range $i, $w := .Workers}}
- role: worker
  {{- if $w.Disks}}
  extraMounts:
  {{- range $w.Disks}}
  - hostPath: {{.HostPath}}
    containerPath: {{.ContainerPath}}
    propagation: HostToContainer
  {{- end}}
  {{- end}}
{{- end}}
`))

type workerNode struct {
	Disks []disks.Disk
}

type kindConfigData struct {
	Workers []workerNode
}

// GenerateConfig renders the kind cluster YAML configuration.
func GenerateConfig(cfg Config) ([]byte, error) {
	var workers []workerNode
	for i := 0; i < cfg.Workers; i++ {
		workers = append(workers, workerNode{Disks: cfg.WorkerDisks[i]})
	}
	data := kindConfigData{Workers: workers}

	var buf bytes.Buffer
	if err := kindConfigTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Exists returns true if a kind cluster with the given name already exists.
func Exists(name string) (bool, error) {
	out, err := run.Output("kind", "get", "clusters")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// Create creates the kind cluster using the provided configuration.
func Create(cfg Config) error {
	configBytes, err := GenerateConfig(cfg)
	if err != nil {
		return fmt.Errorf("generate kind config: %w", err)
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("rooket-kind-%s.yaml", cfg.Name))
	if err := os.WriteFile(tmpFile, configBytes, 0o600); err != nil {
		return fmt.Errorf("write kind config: %w", err)
	}
	defer os.Remove(tmpFile)

	fmt.Printf("kind cluster config:\n%s\n", configBytes)

	return run.CmdWithEnv(
		[]string{"KIND_EXPERIMENTAL_PROVIDER=podman"},
		"kind", "create", "cluster",
		"--name", cfg.Name,
		"--config", tmpFile,
	)
}

// Delete deletes the named kind cluster.
func Delete(name string) error {
	return run.CmdWithEnv(
		[]string{"KIND_EXPERIMENTAL_PROVIDER=podman"},
		"kind", "delete", "cluster", "--name", name,
	)
}

// Nodes returns the list of node container names for the cluster.
func Nodes(clusterName string) ([]string, error) {
	out, err := run.Output("kind", "get", "nodes", "--name", clusterName)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// ConfigureRegistry creates the containerd registry hosts.toml on every node.
func ConfigureRegistry(clusterName, registryName string, hostPort int) error {
	nodes, err := Nodes(clusterName)
	if err != nil {
		return err
	}

	hostsToml := fmt.Sprintf(`server = "http://%s:5000"

[host."http://%s:5000"]
  capabilities = ["pull", "resolve", "push"]
`, registryName, registryName)

	dir := fmt.Sprintf("/etc/containerd/certs.d/localhost:%d", hostPort)

	for _, node := range nodes {
		if err := run.Cmd("podman", "exec", node, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("mkdir on node %s: %w", node, err)
		}
		hostsPath := dir + "/hosts.toml"
		if err := run.CmdWithStdin(
			strings.NewReader(hostsToml),
			"podman", "exec", "-i", node,
			"sh", "-c", "cat > "+hostsPath,
		); err != nil {
			return fmt.Errorf("write hosts.toml on node %s: %w", node, err)
		}
		fmt.Printf("configured registry on node %s\n", node)
	}
	return nil
}

// ApplyRegistryConfigMap creates the standard registry ConfigMap.
func ApplyRegistryConfigMap(clusterName, registryName string, hostPort int) error {
	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:%d"
    hostFromContainerRuntime: "%s:5000"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
`, hostPort, registryName)

	return run.CmdWithStdin(
		strings.NewReader(cm),
		"kubectl", "apply",
		"--context", "kind-"+clusterName,
		"-f", "-",
	)
}
