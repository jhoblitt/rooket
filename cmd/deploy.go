package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	deployDir          string
	deployRegistryPort int
	deployNamespace    string
	deployImageName    string
	deployOperatorName string
	deployClusterName  string
	deployKubeContext  string
	deployName         string
	deployHelmEnv      []string
	deployWorkers      int
	deployDiskCount    int
	deployDiskSizeGB   int
	deployIQNDate      string
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy the rook-ceph and rook-ceph-cluster helm charts into the kind cluster",
	Long: `deploy installs both the rook-ceph operator and the rook-ceph-cluster
helm charts from the rook source directory. The operator image tag is derived
from the current git branch of that directory — the same logic used by
'rooket build' — so the chart always references whatever was last pushed to
the local registry.

Run the 'operator' or 'cluster' subcommand to install only one of the charts.

Example:
  rooket deploy --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephOperator(dir); err != nil {
			return err
		}
		if err := installRookCephCluster(dir); err != nil {
			return err
		}
		switchKubectlNamespace("rook-ceph")
		return nil
	},
}

var deployOperatorCmd = &cobra.Command{
	Use:   "operator",
	Short: "Deploy the rook-ceph operator helm chart using the image from the local registry",
	Long: `deploy operator runs 'helm upgrade --install' for the rook-ceph operator chart
found in the rook source directory. The image tag is derived from the current
git branch of that directory — the same logic used by 'rooket build' — so the
chart always references whatever was last pushed to the local registry.

Example:
  rooket deploy operator --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephOperator(dir); err != nil {
			return err
		}
		switchKubectlNamespace("rook-ceph")
		return nil
	},
}

var deployClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Deploy the rook-ceph-cluster helm chart",
	Long: `deploy cluster runs 'helm upgrade --install' for the rook-ceph-cluster chart
found in the rook source directory.

Example:
  rooket deploy cluster --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephCluster(dir); err != nil {
			return err
		}
		switchKubectlNamespace("rook-ceph")
		return nil
	},
}

// deploySetup resolves everything a deploy needs: the cluster name (pointing
// $KUBECONFIG at its kubeconfig), the kubectl context, the registry port, and
// finally the rook source directory, which it returns.
func deploySetup(cmd *cobra.Command) (string, error) {
	name, err := useCluster(deployName)
	if err != nil {
		return "", err
	}
	deployName = name
	if deployKubeContext == "" {
		deployKubeContext = "kind-" + name
	}
	if deployHelmEnv, err = helmEnv(name, "rooket"); err != nil {
		return "", err
	}
	port, err := resolveRegistryPort(name, deployRegistryPort, cmd.Flags().Changed("registry-port"))
	if err != nil {
		return "", err
	}
	deployRegistryPort = port

	if deployDir != "" {
		return deployDir, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return cwd, nil
}

func installRookCephOperator(dir string) error {
	gitRef, err := gitHeadRef(dir)
	if err != nil {
		return fmt.Errorf("determine git ref in %s: %w", dir, err)
	}

	registry := fmt.Sprintf("localhost:%d", deployRegistryPort)
	imageRepo := fmt.Sprintf("%s/%s/%s", registry, deployNamespace, deployImageName)
	imageTag := gitRef // already sanitized by gitHeadRef

	chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph")
	if err := ensureChartDeps(dir, "rook-ceph"); err != nil {
		return err
	}

	fmt.Printf("==> deploying rook-ceph operator\n")
	fmt.Printf("    chart:      %s\n", chartPath)
	fmt.Printf("    image:      %s:%s\n", imageRepo, imageTag)
	fmt.Printf("    release:    %s\n", deployOperatorName)
	fmt.Printf("    namespace:  rook-ceph\n")

	args := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployOperatorName,
		chartPath,
		"--set", fmt.Sprintf("image.repository=%s", imageRepo),
		"--set", fmt.Sprintf("image.tag=%s", imageTag),
		// One provisioner per driver is plenty for a dev cluster, and the HA
		// pair starves small hosts. Consumed only by refs where rook manages
		// the CSI drivers itself (<= v1.19); newer refs take the drivers
		// chart's default of one replica.
		"--set", "csi.provisionerReplicas=1",
	}
	// The deploy tag is a mutable branch name and the chart defaults to
	// IfNotPresent, so a rebuild that pushes the same tag would neither roll
	// the Deployment nor beat a node-cached image. Pinning the registry's
	// current digest as a pod-template annotation makes the operator roll
	// exactly when image content changed.
	if digest, ok := manifestDigest(deployRegistryPort, deployNamespace+"/"+deployImageName, imageTag); ok {
		// Always-pull is required for the roll to matter: with IfNotPresent a
		// replacement pod happily reuses the node-cached image behind the
		// same mutable tag. The registry is on localhost, so the pull check
		// is cheap.
		args = append(args,
			"--set-string", "annotations.rooket-image-digest="+digest,
			"--set", "image.pullPolicy=Always")
	}
	if err := run.CmdWithEnv(deployHelmEnv, "helm", args...); err != nil {
		return err
	}

	return installCephCsiDrivers(dir)
}

// csiDriversValues configures the ceph-csi-drivers chart: the RBD and CephFS
// driver names must carry the operator-namespace prefix that the
// rook-ceph-cluster chart's StorageClasses use as their provisioner; snapshot
// support stays off (kind clusters have no VolumeSnapshot CRDs, and the
// chart's cephfs driver defaults it on); and the nfs and nvmeof drivers,
// enabled by chart default, are not deployed by rooket.
const csiDriversValues = `operatorConfig:
  namespace: rook-ceph
drivers:
  rbd:
    name: rook-ceph.rbd.csi.ceph.com
    snapshotPolicy: none
  cephfs:
    name: rook-ceph.cephfs.csi.ceph.com
    snapshotPolicy: none
  nfs:
    enabled: false
  nvmeof:
    enabled: false
`

// installCephCsiDrivers installs the ceph-csi-drivers helm chart for rook
// refs that need it. Since v1.20, rook's helm chart installs the
// ceph-csi-operator but rook no longer creates the csi.ceph.io Driver CRs —
// they and the driver pods' ServiceAccounts and RBAC moved to the separate
// ceph-csi-drivers chart, a documented new v1.20 requirement for helm
// installs; without it no CSI driver pods run and every PVC pends. Earlier
// refs must NOT get the chart: v1.18/v1.19 deploy a csi-operator too, but
// their rook still creates the Driver CRs itself and would fight the chart
// over them. The flow is read from the rook-ceph chart's ceph-csi-operator
// dependency: v1.20 renamed its gating condition to csi.installCsiOperator
// in the same move that took Driver creation out of rook, so the condition
// name identifies the flow, and the dependency's version pin — released in
// lockstep with the drivers chart — supplies the matching chart version.
func installCephCsiDrivers(dir string) error {
	chartYAML := filepath.Join(dir, "deploy", "charts", "rook-ceph", "Chart.yaml")
	version, condition, err := cephCsiOperatorDep(chartYAML)
	if err != nil {
		return err
	}
	if condition != "csi.installCsiOperator" {
		fmt.Println("==> rook ref predates the ceph-csi-drivers chart (rook < 1.20); its operator manages CSI itself")
		return nil
	}
	if version == "" {
		return fmt.Errorf("ceph-csi-operator dependency in %s has no version, so the matching ceph-csi-drivers chart version is unknown", chartYAML)
	}

	fmt.Printf("==> deploying ceph-csi-drivers %s (Driver CRs and driver RBAC the rook-ceph chart does not ship)\n", version)
	var installErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if installErr = run.CmdWithStdinEnv(strings.NewReader(csiDriversValues), deployHelmEnv,
			"helm",
			"--kube-context", deployKubeContext,
			"-n", "rook-ceph",
			"upgrade", "--install",
			"ceph-csi-drivers", "ceph-csi-drivers",
			"--repo", "https://ceph.github.io/ceph-csi-operator",
			"--version", version,
			"-f", "-",
		); installErr == nil {
			return nil
		}
		// The csi.ceph.io CRDs arrive with the rook-ceph chart applied
		// moments earlier and may not be established yet.
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("install ceph-csi-drivers chart: %w", installErr)
}

// cephCsiOperatorDep returns the version and gating condition of the
// rook-ceph chart's ceph-csi-operator dependency, or empty strings when the
// chart has no such dependency.
func cephCsiOperatorDep(chartYAML string) (version, condition string, err error) {
	deps, err := chartDeps(chartYAML)
	if err != nil {
		return "", "", err
	}
	for _, d := range deps {
		if d.name == "ceph-csi-operator" {
			return d.version, d.condition, nil
		}
	}
	return "", "", nil
}

func installRookCephCluster(dir string) error {
	chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph-cluster")
	if err := ensureChartDeps(dir, "rook-ceph-cluster"); err != nil {
		return err
	}

	args := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName,
		chartPath,
		"--set", "operatorNamespace=rook-ceph",
		"--set", "toolbox.enabled=true",
		// A standby mgr adds nothing to a disposable dev cluster and its
		// requests eat a node's cpu budget — enough to leave the mds and the
		// detect-version jobs unschedulable on a 4-vCPU host.
		"--set", "cephClusterSpec.mgr.count=1",
	}

	fmt.Printf("==> deploying rook-ceph-cluster\n")
	fmt.Printf("    chart:      %s\n", chartPath)
	fmt.Printf("    release:    %s\n", deployClusterName)
	fmt.Printf("    namespace:  rook-ceph\n")

	if deployWorkers > 0 && deployDiskCount > 0 {
		fmt.Printf("    storage:    %d node-device OSD(s) (one per worker)\n", deployWorkers*deployDiskCount)
	}
	valuesPath, err := writeClusterValues()
	if err != nil {
		return err
	}
	defer os.Remove(valuesPath)
	args = append(args, "-f", valuesPath)

	return run.CmdWithEnv(deployHelmEnv, "helm", args...)
}

// writeClusterValues renders the rook-ceph-cluster Helm values file. It pins
// one OSD to each worker's own iSCSI disk with an explicit per-node device
// list — naming the device per node keeps Rook from mis-attributing OSDs
// (every privileged kind node sees every host disk), so each worker gets
// exactly one OSD on its own disk via Rook's direct device path, no local PV,
// no kubelet loop. It also trims the chart's production-HA cpu requests
// (1 cpu per mon and per OSD): on a small host those fill each node's request
// budget until later components — the detect-version jobs, the mds — cannot
// schedule at all, seen as a wedged cluster on 4-vCPU CI runners. Memory
// requests are left alone (rook derives osd_memory_target from them). Returns
// the file path; the caller removes it.
func writeClusterValues() (string, error) {
	var sb strings.Builder
	sb.WriteString("cephClusterSpec:\n")
	sb.WriteString("  resources:\n")
	sb.WriteString("    mon:\n")
	sb.WriteString("      requests:\n")
	sb.WriteString("        cpu: 500m\n")
	sb.WriteString("    osd:\n")
	sb.WriteString("      requests:\n")
	sb.WriteString("        cpu: 500m\n")
	sb.WriteString("    mgr:\n")
	sb.WriteString("      requests:\n")
	sb.WriteString("        cpu: 300m\n")
	if deployWorkers > 0 && deployDiskCount > 0 {
		sb.WriteString("  storage:\n")
		sb.WriteString("    useAllNodes: false\n")
		sb.WriteString("    useAllDevices: false\n")
		sb.WriteString("    nodes:\n")
		for i := 0; i < deployWorkers; i++ {
			node := workerNodeName(deployName, i)
			sb.WriteString(fmt.Sprintf("      - name: %s\n", node))
			sb.WriteString("        devices:\n")
			for d := 0; d < deployDiskCount; d++ {
				iqn := fmt.Sprintf("iqn.%s.local.rooket:%s-worker%d-disk%d",
					deployIQNDate, deployName, i, d)
				dev, err := waitForISCSIDevice(iqn)
				if err != nil {
					return "", fmt.Errorf("resolve iSCSI device for worker %d disk %d: %w", i, d, err)
				}
				sb.WriteString(fmt.Sprintf("          - name: %s\n", dev))
			}
		}
	}

	f, err := os.CreateTemp("", "rooket-cluster-values-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create cluster values file: %w", err)
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		f.Close()
		return "", fmt.Errorf("write cluster values file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	fmt.Printf("%s", sb.String())
	return f.Name(), nil
}

// workerNodeName returns the kind/k8s node name for worker index i. kind names
// the first worker "<cluster>-worker" and the rest "<cluster>-worker2",
// "<cluster>-worker3", ...
func workerNodeName(cluster string, i int) string {
	if i == 0 {
		return cluster + "-worker"
	}
	return fmt.Sprintf("%s-worker%d", cluster, i+1)
}

// switchKubectlNamespace sets the default kubectl namespace if the krew ns
// plugin is available; failures are non-fatal.
func switchKubectlNamespace(ns string) {
	if out, err := run.Output("kubectl", "ns", ns); err == nil {
		fmt.Println(out)
	}
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.AddCommand(deployOperatorCmd)
	deployCmd.AddCommand(deployClusterCmd)

	pf := deployCmd.PersistentFlags()
	pf.StringVar(&deployDir, "dir", "", "path to the rook source directory (default: current directory)")
	pf.StringVar(&deployKubeContext, "context", "", "kubectl context to use (default: kind-<cluster-name>)")
	pf.IntVar(&deployRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	pf.StringVar(&deployNamespace, "namespace", "rook", "image namespace in the registry")
	pf.StringVar(&deployImageName, "image-name", "ceph", "image name without architecture suffix")
	pf.StringVar(&deployOperatorName, "operator-release", "rook-ceph", "rook-ceph operator helm release name")
	pf.StringVar(&deployClusterName, "cluster-release", "rook-ceph-cluster", "rook-ceph-cluster helm release name")
	pf.StringVar(&deployName, "name", "", "kind cluster name (for node-name and iSCSI by-path derivation)")
	pf.IntVar(&deployWorkers, "workers", 3, "worker node count (for per-node OSD device pinning)")
	pf.IntVar(&deployDiskCount, "disk-count", 1, "iSCSI disks per worker (0 disables OSD device pinning)")
	pf.IntVar(&deployDiskSizeGB, "disk-size", 10, "disk size in GiB (matches 'rooket block setup')")
	pf.StringVar(&deployIQNDate, "iqn-date", "2003-01", "IQN date component matching 'rooket block setup'")
}
