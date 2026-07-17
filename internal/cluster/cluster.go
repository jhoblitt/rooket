// Package cluster manages kind Kubernetes clusters configured for Rook testing.
package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

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

// DeleteWith deletes the named kind cluster under an explicit engine's kind
// provider and kubeconfig file, overriding the ambient environment. 'down
// --all' uses it to remove clusters owned by an engine other than the
// session's resolved one.
func DeleteWith(eng engine.Engine, name, kubeconfig string) error {
	env := []string{"KIND_EXPERIMENTAL_PROVIDER=" + eng.String()}
	if kubeconfig != "" {
		env = append(env, "KUBECONFIG="+kubeconfig)
	}
	return run.CmdWithEnv(env, "kind", "delete", "cluster", "--name", name)
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
//   - installs lvm2 and cryptsetup when missing (absent from kindest/node
//     images, but required for LVM-backed and encrypted OSDs),
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
// Failures across all nodes are collected and returned together (not just
// logged), so creation aborts before deploying onto a mis-prepared node — for
// example one whose foreign-device mask failed and can still see another
// worker's OSD disk. The /run/udev and /dev/disk bind-mounts live in the kind
// config, not here.
//
// Each node runs ONE shell script (piped over stdin, so the trace stays
// short) and all nodes run concurrently; a node's output is buffered and
// flushed as a block afterwards, in node order, so logs stay readable.
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

	return forEachNode(nodes, func(node string, out *bytes.Buffer) []error {
		keep := map[string]bool{}
		for _, d := range ownDevsByNode[node] {
			keep[d] = true
		}
		var foreign []string
		for dev := range allOSDDevs {
			if !keep[dev] {
				foreign = append(foreign, dev)
			}
		}
		sort.Strings(foreign)
		return runNodeScript(eng, out, node, nodePrepScript(foreign))
	})
}

// runNodeScript executes a marker-protocol script inside a node and converts
// the outcome into per-operation errors.
//
// The script travels over stdin, but the shell must not READ it from stdin:
// children like apt-get inherit the shell's fd 0, and a child that reads
// stdin would consume the rest of the script — silently skipping the
// safety-critical device masking. `eval "$(cat)"` slurps the whole script
// before executing it, so children only ever see the drained stream.
//
// The trailing ROOKET_DONE sentinel proves the script ran to its end; a run
// without it (exec died, or something still consumed the script) is retried
// — every script operation is idempotent — and is an error even when earlier
// markers were already emitted.
func runNodeScript(eng engine.Engine, out *bytes.Buffer, node, script string) []error {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		var buf bytes.Buffer
		err := run.CmdWithStdinTo(&buf, strings.NewReader(script),
			eng.String(), "exec", "-i", node, "sh", "-c", `eval "$(cat)"`)
		out.Write(buf.Bytes())
		output := buf.String()
		if strings.Contains(output, "ROOKET_DONE") {
			return nodeScriptErrors(node, output, err)
		}
		if attempt == attempts {
			errs := nodeScriptErrors(node, output, nil)
			if err == nil {
				err = fmt.Errorf("script output ended before ROOKET_DONE")
			}
			return append(errs, fmt.Errorf("node script did not complete on node %s after %d attempts: %w", node, attempts, err))
		}
		run.Fprintf(out, "node script did not complete on node %s (attempt %d/%d); retrying\n", node, attempt, attempts)
		time.Sleep(5 * time.Second)
	}
}

// forEachNode runs fn for every node concurrently, buffering each node's
// output and flushing the blocks in node order once all are done. The
// per-node error lists are combined into one error.
func forEachNode(nodes []string, fn func(node string, out *bytes.Buffer) []error) error {
	outs := make([]bytes.Buffer, len(nodes))
	errLists := make([][]error, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errLists[i] = fn(node, &outs[i])
		}()
	}
	wg.Wait()
	var errs []error
	for i := range nodes {
		os.Stdout.Write(outs[i].Bytes())
		errs = append(errs, errLists[i]...)
	}
	return errors.Join(errs...)
}

// nodePrepScript renders the per-node preparation script. Operations do NOT
// stop at the first failure — a failed remount must not skip the
// safety-critical device masking — so each failure emits a ROOKET_FAIL
// marker that nodeScriptErrors converts back into a per-operation error.
//
// lvm2 and cryptsetup are absent from kindest/node images but required for
// LVM-backed and encrypted OSDs; nodes of a reused cluster already carry
// them from their first prep, so the probe skips apt — and its
// deb.debian.org network dependency — when both are present. apt retries
// ride out transient network failures.
func nodePrepScript(foreignDevs []string) string {
	var b strings.Builder
	b.WriteString(`rc=0
mount -o remount,rw /sys || { echo "ROOKET_FAIL:remount /sys read-write"; rc=1; }
if command -v vgs >/dev/null && command -v cryptsetup >/dev/null; then
  echo "lvm2 and cryptsetup already present"
else
  ok=""
  for attempt in 1 2 3; do
    if apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y lvm2 cryptsetup; then ok=1; break; fi
    if [ "$attempt" -lt 3 ]; then echo "apt install attempt $attempt/3 failed; retrying"; sleep 5; fi
  done
  [ -n "$ok" ] || { echo "ROOKET_FAIL:install lvm2/cryptsetup"; rc=1; }
fi
dmsetup mknodes || { echo "ROOKET_FAIL:dmsetup mknodes"; rc=1; }
`)
	for _, dev := range foreignDevs {
		marker := "ROOKET_FAIL:mask foreign OSD device " + dev
		fmt.Fprintf(&b, "rm -f %s || { echo %s; rc=1; }\n", shellQuote(dev), shellQuote(marker))
	}
	b.WriteString("echo ROOKET_DONE\nexit $rc\n")
	return b.String()
}

// nodeScriptErrors converts a node script's ROOKET_FAIL markers back into
// per-operation errors. A failed run with no markers (e.g. the exec itself
// failed) yields one generic error.
func nodeScriptErrors(node, output string, runErr error) []error {
	var errs []error
	for _, line := range strings.Split(output, "\n") {
		if op, ok := strings.CutPrefix(strings.TrimSpace(line), "ROOKET_FAIL:"); ok {
			errs = append(errs, fmt.Errorf("%s on node %s", op, node))
		}
	}
	if runErr != nil && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("node script on node %s: %w", node, runErr))
	}
	return errs
}

// shellQuote single-quotes s for safe interpolation into a shell script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ConfigureRegistry creates the containerd registry hosts.toml on every node,
// one script per node, all nodes concurrently (see PrepareNodes).
func ConfigureRegistry(eng engine.Engine, clusterName, registryName string, hostPort int) error {
	nodes, err := Nodes(clusterName)
	if err != nil {
		return err
	}
	script := registryScript(registryName, hostPort)
	return forEachNode(nodes, func(node string, out *bytes.Buffer) []error {
		errs := runNodeScript(eng, out, node, script)
		if len(errs) == 0 {
			run.Fprintf(out, "configured registry on node %s\n", node)
		}
		return errs
	})
}

// registryScript renders the script that wires a node's containerd to the
// local registry; the hosts.toml content is embedded via a quoted heredoc.
func registryScript(registryName string, hostPort int) string {
	hostsToml := fmt.Sprintf(`server = "http://%s:5000"

[host."http://%s:5000"]
  capabilities = ["pull", "resolve", "push"]
`, registryName, registryName)

	dir := fmt.Sprintf("/etc/containerd/certs.d/localhost:%d", hostPort)
	return fmt.Sprintf(`rc=0
mkdir -p %[1]s || { echo %[3]s; rc=1; }
[ "$rc" = 0 ] && { cat > %[1]s/hosts.toml <<'ROOKET_EOF' || { echo "ROOKET_FAIL:write hosts.toml"; rc=1; }
%[2]sROOKET_EOF
}
echo ROOKET_DONE
exit $rc
`, shellQuote(dir), hostsToml, shellQuote("ROOKET_FAIL:mkdir "+dir))
}

// InstallPrometheusOperatorCRDs installs the prometheus-operator-crds helm
// chart directly from the prometheus-community repository URL (no repo
// entry needed). The upgrade is skipped when promCRDsCurrent proves the
// release is already deployed at the requested version with its CRDs
// intact; otherwise 'helm upgrade --install' reconciles it.
func InstallPrometheusOperatorCRDs(clusterName, releaseName, version string, extraEnv []string) error {
	if promCRDsCurrent(clusterName, releaseName, version, extraEnv) {
		fmt.Printf("prometheus-operator-crds %s already deployed with its CRDs present, skipping\n", version)
		return nil
	}
	// Pin the release namespace: rooket later switches the kube-context's default
	// namespace to rook-ceph, so an unpinned install lands in a different namespace
	// on re-run and collides with the CRDs' helm ownership annotations.
	return run.CmdWithEnv(extraEnv,
		"helm",
		"--kube-context", "kind-"+clusterName,
		"-n", "rook-ceph",
		"upgrade", "--install",
		releaseName,
		"prometheus-operator-crds",
		"--repo", "https://prometheus-community.github.io/helm-charts",
		"--version", version,
		"--create-namespace",
	)
}

// promCRDsCurrent reports whether the prometheus-operator-crds release is
// already deployed at the requested chart version AND every CRD in its
// manifest still exists. A deployed release alone doesn't prove its
// resources survived — the upgrade is the reconciler — so both halves must
// hold before the install is skipped. Any probe failure means "not current".
func promCRDsCurrent(clusterName, releaseName, version string, extraEnv []string) bool {
	out, err := run.OutputWithEnv(extraEnv, "helm",
		"--kube-context", "kind-"+clusterName,
		"-n", "rook-ceph",
		"list", "-o", "json",
		"--filter", "^"+regexp.QuoteMeta(releaseName)+"$",
	)
	if err != nil {
		return false
	}
	var releases []struct {
		Name   string `json:"name"`
		Chart  string `json:"chart"`
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &releases) != nil {
		return false
	}
	deployed := false
	for _, r := range releases {
		if r.Name == releaseName && r.Status == "deployed" &&
			r.Chart == "prometheus-operator-crds-"+version {
			deployed = true
		}
	}
	if !deployed {
		return false
	}

	manifest, err := run.OutputWithEnv(extraEnv, "helm",
		"--kube-context", "kind-"+clusterName,
		"-n", "rook-ceph",
		"get", "manifest", releaseName,
	)
	if err != nil {
		return false
	}
	crds, err := manifestCRDNames(manifest)
	if err != nil || len(crds) == 0 {
		return false
	}
	args := append([]string{"get", "crd", "--context", "kind-" + clusterName, "-o", "name"}, crds...)
	_, err = run.Output("kubectl", args...)
	return err == nil
}

// manifestCRDNames extracts the metadata names of every
// CustomResourceDefinition document in a rendered helm manifest. Documents
// are decoded structurally; any document that fails to decode, or a CRD
// without a metadata.name, is an error so the caller cannot skip based on a
// partial view of the chart's CRDs.
func manifestCRDNames(manifest string) ([]string, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	var names []string
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode manifest document: %w", err)
		}
		if doc.Kind != "CustomResourceDefinition" {
			continue
		}
		if doc.Metadata.Name == "" {
			return nil, fmt.Errorf("CustomResourceDefinition document without metadata.name")
		}
		names = append(names, doc.Metadata.Name)
	}
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
