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

	"github.com/jhoblitt/rooket/internal/engine"
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
	// RegistryName is the container name of the local OCI registry.
	RegistryName string
	// RegistryHostPort is the port the registry listens on on the host.
	RegistryHostPort int
	// WorkerDisks maps worker index → Disk descriptors to bind-mount into each
	// worker node. kind runs node containers privileged, so a bind-mounted
	// device file is usable inside the node under either engine (podman's crun
	// also adds such devices to the cgroup device allowlist automatically).
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

// List returns the names of all kind clusters known to the given engine's kind
// provider. The provider is passed explicitly (overriding the ambient
// KIND_EXPERIMENTAL_PROVIDER) so callers like prune and list can query engines
// other than the session's resolved one.
func List(eng engine.Engine) ([]string, error) {
	out, err := run.OutputWithEnv(
		[]string{"KIND_EXPERIMENTAL_PROVIDER=" + eng.String()},
		"kind", "get", "clusters")
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// Exists returns true if a kind cluster with the given name already exists
// under the given engine's kind provider.
func Exists(eng engine.Engine, name string) (bool, error) {
	names, err := List(eng)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
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

	// The kind provider comes from KIND_EXPERIMENTAL_PROVIDER, which the root
	// command exports from the selected engine.
	return run.Cmd(
		"kind", "create", "cluster",
		"--name", cfg.Name,
		"--config", tmpFile,
	)
}

// Delete deletes the named kind cluster. The kind provider comes from
// KIND_EXPERIMENTAL_PROVIDER, which the root command exports from the engine.
func Delete(name string) error {
	return run.Cmd("kind", "delete", "cluster", "--name", name)
}

// ZapISCSIDisks wipes this cluster's iSCSI OSD disks so the next bring-up sees
// clean devices. It re-creates each backing image as a fresh sparse file
// (truncate to 0, then back to its size), which clears every Ceph bluestore
// label while keeping the image sparse — the LIO fileio backstore supports
// neither UNMAP/blkdiscard nor (on ext2) hole-punching, so zeroing through the
// device would permanently inflate the image instead. It then refreshes the host
// udev DB (the truncate doesn't notify the kernel, so lsblk/ceph-volume would
// otherwise see a stale "ceph_bluestore" signature and skip the disk). Run AFTER
// the kind cluster is deleted, when the disks are idle. Best-effort; targets the
// image files in dataDir (the cluster's state directory).
func ZapISCSIDisks(eng engine.Engine, clusterName, dataDir string) {
	imgs, _ := filepath.Glob(filepath.Join(dataDir, "*.img"))
	if len(imgs) == 0 {
		fmt.Printf("no OSD disk images for cluster %q; skipping zap\n", clusterName)
		return
	}

	fmt.Println("==> zapping OSD disks (re-sparsifying backing images)")
	for _, img := range imgs {
		fi, err := os.Stat(img)
		if err != nil {
			fmt.Printf("warning: stat %s: %v\n", img, err)
			continue
		}
		if err := os.Truncate(img, 0); err != nil {
			fmt.Printf("warning: truncate %s to 0: %v\n", img, err)
			continue
		}
		if err := os.Truncate(img, fi.Size()); err != nil {
			fmt.Printf("warning: truncate %s to %d: %v\n", img, fi.Size(), err)
			continue
		}
		fmt.Printf("zapped %s\n", img)
	}

	// Refresh udev so lsblk/ceph-volume don't see the stale "ceph_bluestore"
	// signature. Needs privileges, so use a throwaway privileged container, with
	// the host's /run/udev mounted so udevadm shares real udev state (else settle
	// hangs and the re-probe is unreliable). blockdev --flushbufs first drops the
	// iSCSI initiator's stale block-device cache (the truncate happened on the
	// backstore file, behind the initiator) so the re-probe reads the now-zeroed
	// device rather than cached bluestore blocks.
	if cimg := kindNodeImageID(eng); cimg != "" {
		script := fmt.Sprintf(`for dev in /dev/disk/by-path/ip-127.0.0.1:3260-iscsi-iqn.*.local.rooket:%s-worker*-disk*-lun-0; do
  [ -e "$dev" ] || continue
  blockdev --flushbufs "$dev" 2>/dev/null || true
done
udevadm trigger --action=change --subsystem-match=block >/dev/null 2>&1 || true
udevadm settle >/dev/null 2>&1 || true`, clusterName)
		_ = run.Cmd(eng.String(), "run", "--rm", "--privileged",
			"-v", "/dev:/dev", "-v", "/run/udev:/run/udev",
			"--entrypoint", "sh", cimg, "-c", script)
	}
}

// kindNodeImageID returns the image ID of a locally-present kindest/node image,
// used as a throwaway privileged container for disk zapping. Empty if none.
func kindNodeImageID(eng engine.Engine) string {
	out, err := run.Output(eng.String(), "images", "--format", "{{.ID}} {{.Repository}}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "kindest/node") {
			if f := strings.Fields(line); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
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
//   - removes every other worker's OSD device node from this node's /dev so each
//     node sees only its own disk. The nodes share the host's /dev, so otherwise
//     each node's global ceph-volume scan adopts all OSDs and Rook mis-attributes
//     them onto one node. /dev is a per-container tmpfs (so the removal is
//     node-local) and Rook's OSD pods bind-mount the node's /dev, so the mask
//     reaches ceph-volume. ownDevsByNode maps node name -> the OSD device(s) it keeps.
//
// Per-node failures are logged, not fatal. The /run/udev and /dev/disk
// bind-mounts live in the kind config, not here.
func PrepareNodes(eng engine.Engine, clusterName string, ownDevsByNode map[string][]string) error {
	nodes, err := Nodes(clusterName)
	if err != nil {
		return err
	}

	allOSDDevs := map[string]bool{}
	for _, devs := range ownDevsByNode {
		for _, d := range devs {
			allOSDDevs[d] = true
		}
	}

	for _, node := range nodes {
		if err := run.Cmd(eng.String(), "exec", node, "mount", "-o", "remount,rw", "/sys"); err != nil {
			fmt.Printf("warning: remount /sys read-write on node %s: %v\n", node, err)
		}
		if err := installNodePackages(eng, node); err != nil {
			fmt.Printf("warning: install lvm2/cryptsetup on node %s: %v\n", node, err)
		}
		if err := run.Cmd(eng.String(), "exec", node, "dmsetup", "mknodes"); err != nil {
			fmt.Printf("warning: dmsetup mknodes on node %s: %v\n", node, err)
		}
		keep := map[string]bool{}
		for _, d := range ownDevsByNode[node] {
			keep[d] = true
		}
		for dev := range allOSDDevs {
			if keep[dev] {
				continue
			}
			if err := run.Cmd(eng.String(), "exec", node, "rm", "-f", dev); err != nil {
				fmt.Printf("warning: mask foreign OSD device %s on node %s: %v\n", dev, node, err)
			}
		}
	}
	return nil
}

// installNodePackages installs lvm2 and cryptsetup into a kind node, retrying to
// ride out transient apt failures.
func installNodePackages(eng engine.Engine, node string) error {
	const script = "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y lvm2 cryptsetup"
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = run.Cmd(eng.String(), "exec", node, "sh", "-c", script); err == nil {
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
func ConfigureRegistry(eng engine.Engine, clusterName, registryName string, hostPort int) error {
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
		if err := run.Cmd(eng.String(), "exec", node, "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("mkdir on node %s: %w", node, err)
		}
		hostsPath := dir + "/hosts.toml"
		if err := run.CmdWithStdin(
			strings.NewReader(hostsToml),
			eng.String(), "exec", "-i", node,
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

	// Pin the release namespace: rooket later switches the kube-context's default
	// namespace to rook-ceph, so an unpinned install lands in a different namespace
	// on re-run and collides with the CRDs' helm ownership annotations.
	return run.Cmd(
		"helm",
		"--kube-context", "kind-"+clusterName,
		"-n", "rook-ceph",
		"upgrade", "--install",
		releaseName,
		"prometheus-community/prometheus-operator-crds",
		"--version", version,
		"--create-namespace",
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
