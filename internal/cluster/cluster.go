// Package cluster manages kind Kubernetes clusters configured for Rook testing.
package cluster

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/jhoblitt/rooket/internal/run"
)

// Disk describes a single OSD block device to bind-mount into a worker node.
type Disk struct {
	// HostPath is the path to the block device on the host (e.g. /dev/sdb).
	HostPath string
	// ContainerPath is where the device appears inside the node container.
	ContainerPath string
}

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
	// WorkerDisks maps worker index → Disk descriptors to bind-mount into
	// each worker node. crun adds device files to the container's cgroup
	// device allowlist automatically for bind-mounted device files.
	WorkerDisks map[int][]Disk
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
  # ceph-volume reads /run/udev to inventory disks; without it Rook fails with
  # "No udev data could be retrieved" and skips the device.
  - hostPath: /run/udev
    containerPath: /run/udev
    propagation: HostToContainer
  # /dev/disk/by-path carries the stable iSCSI symlinks Rook pins OSDs to; the
  # node runs no udevd, so without the host's /dev/disk these are absent.
  - hostPath: /dev/disk
    containerPath: /dev/disk
    propagation: HostToContainer
  {{- range $w.Disks}}
  - hostPath: {{.HostPath}}
    containerPath: {{.ContainerPath}}
    propagation: HostToContainer
  {{- end}}
  {{- end}}
{{- end}}
`))

type workerNode struct {
	Disks []Disk
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
	for line := range strings.SplitSeq(out, "\n") {
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
	for line := range strings.SplitSeq(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// PrepareNodes readies every node for Rook OSD provisioning:
//   - remounts /sys read-write (kind mounts it read-only, but ceph-volume and
//     kernel RBD mapping must write it),
//   - installs lvm2 and cryptsetup (absent from kindest/node images, but
//     required for LVM-backed and encrypted OSDs),
//   - runs 'dmsetup mknodes' to create /dev/mapper entries for the host's
//     device-mapper devices: the node shares the host's /sys (which advertises
//     those dm devices) but not its /dev/mapper, so ceph-volume's full-device
//     scan would otherwise crash on the missing nodes and provision no OSDs.
//   - creates extra /dev/loopN nodes: the kubelet maps each local-PV block
//     volume (the OSD storageClassDeviceSet) through a loop device, but rootless
//     podman's image-layer mounts already consume most of the host's ~22 loop
//     devices, so losetup -f runs out and OSDs fail to start. max_loop=0 lets
//     the kernel allocate more on use; this just provides the device nodes.
//
// Per-node failures are logged, not fatal. The /run/udev and /dev/disk
// bind-mounts live in the kind config, not here.
func PrepareNodes(clusterName string) error {
	nodes, err := Nodes(clusterName)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := run.Cmd("podman", "exec", node, "mount", "-o", "remount,rw", "/sys"); err != nil {
			fmt.Printf("warning: remount /sys read-write on node %s: %v\n", node, err)
		}
		if err := installNodePackages(node); err != nil {
			fmt.Printf("warning: install lvm2/cryptsetup on node %s: %v\n", node, err)
		}
		if err := run.Cmd("podman", "exec", node, "dmsetup", "mknodes"); err != nil {
			fmt.Printf("warning: dmsetup mknodes on node %s: %v\n", node, err)
		}
		if err := run.Cmd("podman", "exec", node, "sh", "-c",
			"for i in $(seq 0 127); do [ -e /dev/loop$i ] || mknod /dev/loop$i b 7 $i; done"); err != nil {
			fmt.Printf("warning: create loop device nodes on node %s: %v\n", node, err)
		}
	}
	return nil
}

// installNodePackages installs lvm2 and cryptsetup into a kind node, retrying to
// ride out transient apt failures.
func installNodePackages(node string) error {
	const script = "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y lvm2 cryptsetup"
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = run.Cmd("podman", "exec", node, "sh", "-c", script); err == nil {
			return nil
		}
		if attempt < 3 {
			fmt.Printf("apt install attempt %d/3 on node %s failed: %v; retrying\n", attempt, node, err)
			time.Sleep(5 * time.Second)
		}
	}
	return err
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

// InstallPrometheusOperatorCRDs installs the prometheus-operator-crds helm
// chart from the prometheus-community repository into the cluster. The install
// is idempotent: the repo is added with --force-update and the chart is
// applied with 'helm upgrade --install'.
func InstallPrometheusOperatorCRDs(clusterName, releaseName, version string) error {
	if err := run.Cmd(
		"helm", "repo", "add", "--force-update",
		"prometheus-community",
		"https://prometheus-community.github.io/helm-charts",
	); err != nil {
		return fmt.Errorf("add prometheus-community helm repo: %w", err)
	}

	if err := run.Cmd(
		"helm", "repo", "update", "prometheus-community",
	); err != nil {
		return fmt.Errorf("update prometheus-community helm repo: %w", err)
	}

	return run.Cmd(
		"helm",
		"--kube-context", "kind-"+clusterName,
		"upgrade", "--install",
		releaseName,
		"prometheus-community/prometheus-operator-crds",
		"--version", version,
	)
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
