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
	// NodeImage is the kindest/node image passed to `kind create cluster
	// --image`. Pinning it (rather than letting kind pick its built-in default)
	// keeps the Kubernetes version reproducible and lets the caller pre-pull the
	// exact ref concurrently with other work; empty means use kind's default.
	NodeImage string
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
func List(w io.Writer, eng engine.Engine) ([]string, error) {
	out, err := run.OutputWithEnvTo(w,
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
func Exists(w io.Writer, eng engine.Engine, name string) (bool, error) {
	names, err := List(w, eng)
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
func Create(w io.Writer, cfg Config) error {
	configBytes, err := GenerateConfig(cfg)
	if err != nil {
		return fmt.Errorf("generate kind config: %w", err)
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("rooket-kind-%s.yaml", cfg.Name))
	if err := os.WriteFile(tmpFile, configBytes, 0o600); err != nil {
		return fmt.Errorf("write kind config: %w", err)
	}
	defer os.Remove(tmpFile)

	run.Fprintf(w, "kind cluster config:\n%s\n", configBytes)

	// The kind provider comes from KIND_EXPERIMENTAL_PROVIDER, which the root
	// command exports from the selected engine.
	args := []string{
		"create", "cluster",
		"--name", cfg.Name,
		"--config", tmpFile,
	}
	// A pinned node image keeps the Kubernetes version reproducible and lets the
	// caller pre-pull the exact ref; kind picks its built-in default when unset.
	if cfg.NodeImage != "" {
		args = append(args, "--image", cfg.NodeImage)
	}
	return run.CmdTo(w, "kind", args...)
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
// image files in dataDir (the cluster's state directory). Output goes to w so a
// caller zapping several clusters concurrently can buffer each cluster's lines.
func ZapISCSIDisks(w io.Writer, eng engine.Engine, clusterName, dataDir string) {
	imgs, _ := filepath.Glob(filepath.Join(dataDir, "*.img"))
	if len(imgs) == 0 {
		run.Fprintf(w, "no OSD disk images for cluster %q; skipping zap\n", clusterName)
		return
	}

	run.Fprintf(w, "==> zapping OSD disks (re-sparsifying backing images)\n")
	for _, img := range imgs {
		fi, err := os.Stat(img)
		if err != nil {
			run.Fprintf(w, "warning: stat %s: %v\n", img, err)
			continue
		}
		if err := os.Truncate(img, 0); err != nil {
			run.Fprintf(w, "warning: truncate %s to 0: %v\n", img, err)
			continue
		}
		if err := os.Truncate(img, fi.Size()); err != nil {
			run.Fprintf(w, "warning: truncate %s to %d: %v\n", img, fi.Size(), err)
			continue
		}
		run.Fprintf(w, "zapped %s\n", img)
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
		_ = run.CmdTo(w, eng.String(), "run", "--rm", "--privileged",
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
func Nodes(w io.Writer, clusterName string) ([]string, error) {
	out, err := run.OutputTo(w, "kind", "get", "nodes", "--name", clusterName)
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
//   - raises systemd DefaultTasksMax so a pod's thread pool (notably rgw's) is
//     not capped at the node's derived default,
//   - installs lvm2 and cryptsetup when missing (absent from kindest/node
//     images, but required for LVM-backed and encrypted OSDs),
//   - prunes this node's /dev to an allowlist plus the node's own OSD disk(s),
//     removing every other block or char device node. kind runs the node
//     privileged and podman (or docker) then fills its /dev with a node for
//     EVERY host device — the host's root LVM and system nvme, other clusters'
//     iSCSI disks, the char passthrough paths to them (/dev/sg*, the nvme
//     controller), and unrelated host hardware (/dev/mem, /dev/kvm, ...) — all
//     openable from the node and from the pods that bind-mount its /dev. Keeping
//     only allowedDevs (see nodePrepScript) plus the node's own disk holds the
//     host's real devices out of reach AND stops Rook mis-attributing OSDs: its
//     non-PVC inventory is a global ceph-volume scan that would otherwise adopt
//     every visible disk and pin all OSDs onto one node. /dev is a per-node
//     tmpfs and no udevd runs, so the prune is node-local and stays put; rooket
//     OSDs are raw bluestore on the whole disk (no LVM child), so keeping the
//     node's own /dev/sdX suffices. ownDevsByNode maps node name -> the OSD
//     device(s) it keeps; a node absent from the map (the control-plane) keeps
//     only the allowlist.
//
// Failures across all nodes are collected and returned together (not just
// logged), so creation aborts before deploying onto a mis-prepared node — for
// example one whose device mask failed and can still see another worker's OSD
// disk or a host disk. The /run/udev and /dev/disk bind-mounts live in the kind
// config, not here.
//
// Each node runs ONE shell script (piped over stdin, so the trace stays
// short) and all nodes run concurrently; a node's output is buffered and
// flushed as a block afterwards, in node order, so logs stay readable.
func PrepareNodes(w io.Writer, eng engine.Engine, clusterName string, ownDevsByNode map[string][]string) error {
	nodes, err := Nodes(w, clusterName)
	if err != nil {
		return err
	}

	return forEachNode(w, nodes, func(node string, out *bytes.Buffer) []error {
		keep := append([]string(nil), ownDevsByNode[node]...)
		sort.Strings(keep)
		return runNodeScript(eng, out, node, nodePrepScript(keep))
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
func forEachNode(w io.Writer, nodes []string, fn func(node string, out *bytes.Buffer) []error) error {
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
		w.Write(outs[i].Bytes())
		errs = append(errs, errLists[i]...)
	}
	return errors.Join(errs...)
}

// allowedDevs is the allowlist of device-node paths kept in every node's /dev;
// nodePrepScript removes every other block or char node, plus per node the
// node's own OSD disk. It is the minimal set a kind node needs: the OCI standard
// devices (null/zero/full/random/urandom/tty/console/ptmx) plus the kernel-log
// (kmsg), FUSE, CNI tun, device-mapper-control and loop-control nodes. None of
// these is a host-storage, -memory, or -hardware access path — unlike the host
// disks, /dev/mem, /dev/kvm, /dev/sg*, etc. that the prune strips. Runtime nodes
// like CSI's /dev/rbdN are created after prep and so are never pruned.
const allowedDevs = "/dev/null /dev/zero /dev/full /dev/random /dev/urandom " +
	"/dev/tty /dev/console /dev/ptmx /dev/kmsg /dev/fuse /dev/net/tun " +
	"/dev/mapper/control /dev/loop-control"

// nodePrepScript renders the per-node preparation script. Operations do NOT
// stop at the first failure — a failed remount must not skip the
// safety-critical device masking — so each failure emits a ROOKET_FAIL
// marker that nodeScriptErrors converts back into a per-operation error.
//
// The tail of the script is the device mask. kind runs the node privileged and
// podman — and docker — then populate its /dev with a node for EVERY host
// device: not just the block disks but the char passthrough paths to them
// (/dev/sg*, the NVMe controller) and unrelated host hardware (/dev/mem,
// /dev/kvm, /dev/snapshot, ...). Rather than deny-list those, the prune keeps an
// allowlist — allowedDevs, the minimal set kubelet, containerd, the CNI and
// ceph-volume need — plus the node's own OSD disk(s), and removes every other
// block or char device node. 'find -xdev' stays on the /dev tmpfs, so the
// devpts/shm/mqueue submounts (and the ptys of live exec sessions) are left
// alone; a shell 'case' membership test against the space-padded allow+keep set
// spares the kept nodes. An empty keep-set (the control-plane) keeps only the
// allowlist. rooket OSDs are whole raw disks, so an exact /dev/sdX match
// suffices — no realpath needed.
//
// lvm2 and cryptsetup are absent from kindest/node images but required for
// LVM-backed and encrypted OSDs; nodes of a reused cluster already carry
// them from their first prep, so the probe skips apt — and its
// deb.debian.org network dependency — when both are present. apt retries
// ride out transient network failures.
//
// The DefaultTasksMax drop-in lifts the per-pod PID/task ceiling. The node's
// systemd derives DefaultTasksMax as 15% of the tightest PID limit it sees —
// podman caps the node container at 2048 pids by default, so that is 307 —
// and stamps it onto every pod's cgroup scope. rgw (rgw_thread_pool_size
// defaults to 512, plus RADOS/messenger threads) then dies at startup with
// "Resource temporarily unavailable" when pthread_create hits 307. Writing
// the drop-in during node prep — before the Rook cluster is deployed — lets
// every pod scope inherit the raised default; the node container's own 2048
// pids cap remains the real ceiling. The 90- prefix keeps a stock kind-image
// drop-in from lexicographically overriding it, and the post-reload check
// fails loudly if something did. Guarded so warm reruns skip the reload.
func nodePrepScript(keepDevs []string) string {
	var b strings.Builder
	b.WriteString(`rc=0
mount -o remount,rw /sys || { echo "ROOKET_FAIL:remount /sys read-write"; rc=1; }
if [ "$(systemctl show -p DefaultTasksMax --value)" = infinity ]; then
  echo "systemd DefaultTasksMax already unlimited"
elif mkdir -p /etc/systemd/system.conf.d &&
     printf '[Manager]\nDefaultTasksMax=infinity\n' > /etc/systemd/system.conf.d/90-rooket-tasksmax.conf &&
     systemctl daemon-reload &&
     [ "$(systemctl show -p DefaultTasksMax --value)" = infinity ]; then
  echo "systemd DefaultTasksMax set to unlimited"
else
  echo "ROOKET_FAIL:raise systemd DefaultTasksMax"; rc=1
fi
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
`)
	keep := allowedDevs + " " + strings.Join(keepDevs, " ")
	b.WriteString("for dev in $(find /dev -xdev \\( -type b -o -type c \\) 2>/dev/null); do\n")
	fmt.Fprintf(&b, "  case \" %s \" in *\" $dev \"*) continue ;; esac\n", keep)
	b.WriteString(`  rm -f "$dev" || { echo "ROOKET_FAIL:mask host device $dev"; rc=1; }
done
`)
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
func ConfigureRegistry(w io.Writer, eng engine.Engine, clusterName, registryName string, hostPort int) error {
	nodes, err := Nodes(w, clusterName)
	if err != nil {
		return err
	}
	script := registryScript(registryName, hostPort)
	return forEachNode(w, nodes, func(node string, out *bytes.Buffer) []error {
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
func InstallPrometheusOperatorCRDs(w io.Writer, clusterName, releaseName, version string, extraEnv []string) error {
	if promCRDsCurrent(w, clusterName, releaseName, version, extraEnv) {
		run.Fprintf(w, "prometheus-operator-crds %s already deployed with its CRDs present, skipping\n", version)
		return nil
	}
	// Pin the release namespace: rooket later switches the kube-context's default
	// namespace to rook-ceph, so an unpinned install lands in a different namespace
	// on re-run and collides with the CRDs' helm ownership annotations.
	return run.CmdWithEnvTo(w, extraEnv,
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
func promCRDsCurrent(w io.Writer, clusterName, releaseName, version string, extraEnv []string) bool {
	out, err := run.OutputWithEnvTo(w, extraEnv, "helm",
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

	manifest, err := run.OutputWithEnvTo(w, extraEnv, "helm",
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
	_, err = run.OutputTo(w, "kubectl", args...)
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
func ApplyRegistryConfigMap(w io.Writer, clusterName, registryName string, hostPort int) error {
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

	return run.CmdWithStdinTo(
		w,
		strings.NewReader(cm),
		"kubectl", "apply",
		"--context", "kind-"+clusterName,
		"-f", "-",
	)
}
