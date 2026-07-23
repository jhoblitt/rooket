package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/jhoblitt/rooket/internal/values"
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
	deployWith         []string
	deployWithOnly     []string
	deployWithOnlySet  bool
	deployValueFiles   []string
	deploySets         []string
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
		dir, active, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephOperator(dir, active); err != nil {
			return err
		}
		// rook-ceph-cluster's CRs (CephCluster, pools, object store, ...) need
		// the operator running to reconcile them, so cluster waits on the
		// operator install (invariant 1).
		if err := installRookCephCluster(dir, active); err != nil {
			return err
		}
		// Profile resources reference cluster-chart resources — e.g. a
		// CephObjectStoreUser's object store, a StorageClass's PVC binds — so
		// profiles waits on cluster (invariant 1).
		if err := installProfilesChart(dir, active); err != nil {
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
		dir, active, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephOperator(dir, active); err != nil {
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
		dir, active, err := deploySetup(cmd)
		if err != nil {
			return err
		}
		if err := installRookCephCluster(dir, active); err != nil {
			return err
		}
		// Profile resources reference cluster-chart resources — e.g. a
		// CephObjectStoreUser's object store, a StorageClass's PVC binds — so
		// profiles waits on cluster (invariant 1).
		if err := installProfilesChart(dir, active); err != nil {
			return err
		}
		switchKubectlNamespace("rook-ceph")
		return nil
	},
}

// deploySetup resolves everything a deploy needs: the cluster name (pointing
// $KUBECONFIG at its kubeconfig), the kubectl context, the registry port, the
// rook source directory, and the active profile set. It returns the source
// directory and the resolved profiles.
//
// The profile set is resolved here — once — rather than by each chart
// installer, so every chart in this deploy and the profiles release itself
// see the same selection even if .rooket/config.yaml changes mid-deploy.
func deploySetup(cmd *cobra.Command) (string, []profiles.Profile, error) {
	name, err := useCluster(deployName)
	if err != nil {
		return "", nil, err
	}
	deployName = name
	if deployKubeContext == "" {
		deployKubeContext = "kind-" + name
	}
	applyWithOnlyGuard(cmd.Flags().Changed("with-only"))
	if deployHelmEnv, err = helmEnv(name, "rooket"); err != nil {
		return "", nil, err
	}
	port, err := resolveRegistryPort(name, deployRegistryPort, cmd.Flags().Changed("registry-port"))
	if err != nil {
		return "", nil, err
	}
	deployRegistryPort = port

	dir := deployDir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", nil, fmt.Errorf("get working directory: %w", err)
		}
		dir = cwd
	}

	cloneDir := clone.Open(dir)
	if err := cloneDir.Ensure(); err != nil {
		return "", nil, err
	}
	names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
	if err != nil {
		return "", nil, err
	}
	active, err := loadProfiles(names)
	if err != nil {
		return "", nil, err
	}

	return dir, active, nil
}

// applyWithOnlyGuard sets deployWithOnlySet when deployCmd's own --with-only
// flag was changed. When it was not changed, an already-true deployWithOnlySet
// is left alone: 'rooket up' sets it directly before calling deployCmd.RunE,
// and deployCmd's own flag is unset on that path, so an unconditional
// assignment here would erase what 'up' forwarded and silently deploy the
// wrong profiles.
func applyWithOnlyGuard(changed bool) {
	if changed {
		deployWithOnlySet = true
	}
}

func installRookCephOperator(dir string, active []profiles.Profile) error {
	gitRef, err := gitHeadRef(dir)
	if err != nil {
		return fmt.Errorf("determine git ref in %s: %w", dir, err)
	}

	registry := fmt.Sprintf("localhost:%d", deployRegistryPort)
	imageRepo := fmt.Sprintf("%s/%s/%s", registry, deployNamespace, deployImageName)
	imageTag := gitRef // already sanitized by gitHeadRef

	chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph")
	// Shares the "make" purpose helm home (see helmEnv) with
	// installRookCephCluster's ensureChartDeps call — the two must never run
	// concurrently (invariant 2). They already can't: this whole operator
	// install (including ceph-csi-drivers) completes before cluster starts.
	if err := ensureChartDeps(dir, "rook-ceph"); err != nil {
		return err
	}

	run.Printf("==> deploying rook-ceph operator\n")
	run.Printf("    chart:      %s\n", chartPath)
	run.Printf("    image:      %s:%s\n", imageRepo, imageTag)
	run.Printf("    release:    %s\n", deployOperatorName)
	run.Printf("    namespace:  rook-ceph\n")

	base := values.OperatorBase(values.OperatorInput{
		ImageRepo: imageRepo,
		ImageTag:  imageTag,
		Digest:    digestOrEmpty(deployRegistryPort, deployNamespace+"/"+deployImageName, imageTag),
	})
	valuesPath, err := writeComposed(chartOperator, base, dir, active)
	if err != nil {
		return err
	}

	args := append([]string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployOperatorName, chartPath,
	}, helmValueArgs(valuesPath, deploySets)...)
	if err := run.CmdWithEnv(deployHelmEnv, "helm", args...); err != nil {
		return err
	}

	// ceph-csi-drivers needs the csi.ceph.io CRDs the operator chart's
	// ceph-csi-operator subchart installs, so it waits on the operator
	// (invariant 1); see the retry loop in installCephCsiDrivers.
	return installCephCsiDrivers(dir, active)
}

func digestOrEmpty(port int, repo, tag string) string {
	digest, ok := manifestDigest(port, repo, tag)
	if !ok {
		return ""
	}
	return digest
}

// writeComposed stacks every layer for chart and writes the result into the
// cluster's state dir, where it survives a failed deploy for inspection.
// active is the deploy's profile set, resolved once by deploySetup.
func writeComposed(chart string, base map[string]any, rookDir string, active []profiles.Profile) (string, error) {
	cloneDir := clone.Open(rookDir)
	if err := cloneDir.Ensure(); err != nil {
		return "", err
	}
	c, err := composeChart(chart, base, cloneDir, active, deployValueFiles)
	if err != nil {
		return "", err
	}
	dir, err := deployValuesDir(deployName)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, chart+".yaml")
	return path, c.write(path)
}

// helmValueArgs returns the "-f valuesPath" pair followed by a "--set entry"
// pair for each set entry, in that order. --set is deliberately not merged
// into rooket's own layering (see composeChart) — it is passed through to
// helm verbatim so it keeps helm's own highest-precedence behavior, and
// ordering it after -f here reflects that.
func helmValueArgs(valuesPath string, sets []string) []string {
	args := make([]string, 0, 2+2*len(sets))
	args = append(args, "-f", valuesPath)
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	return args
}

func deployValuesDir(cluster string) (string, error) {
	state, err := stateDirPath(cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "values"), nil
}

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
func installCephCsiDrivers(dir string, active []profiles.Profile) error {
	chartYAML := filepath.Join(dir, "deploy", "charts", "rook-ceph", "Chart.yaml")
	version, condition, err := cephCsiOperatorDep(chartYAML)
	if err != nil {
		return err
	}
	if condition != "csi.installCsiOperator" {
		run.Printf("==> rook ref predates the ceph-csi-drivers chart (rook < 1.20); its operator manages CSI itself\n")
		return nil
	}
	if version == "" {
		return fmt.Errorf("ceph-csi-operator dependency in %s has no version, so the matching ceph-csi-drivers chart version is unknown", chartYAML)
	}

	run.Printf("==> deploying ceph-csi-drivers %s (Driver CRs and driver RBAC the rook-ceph chart does not ship)\n", version)
	valuesPath, err := writeComposed(chartCSI, values.CSIBase(), dir, active)
	if err != nil {
		return err
	}

	csiArgs := append([]string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install",
		"ceph-csi-drivers", "ceph-csi-drivers",
		"--repo", "https://ceph.github.io/ceph-csi-operator",
		"--version", version,
	}, helmValueArgs(valuesPath, deploySets)...)

	var installErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if installErr = run.CmdWithEnv(deployHelmEnv, "helm", csiArgs...); installErr == nil {
			return nil
		}
		// The csi.ceph.io CRDs arrive with the rook-ceph chart applied moments
		// earlier and may not be established yet.
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

func installRookCephCluster(dir string, active []profiles.Profile) error {
	chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph-cluster")
	// Shares the "make" purpose helm home with installRookCephOperator's
	// ensureChartDeps call — must stay sequential with it, never concurrent
	// (invariant 2).
	if err := ensureChartDeps(dir, "rook-ceph-cluster"); err != nil {
		return err
	}

	run.Printf("==> deploying rook-ceph-cluster\n")
	run.Printf("    chart:      %s\n", chartPath)
	run.Printf("    release:    %s\n", deployClusterName)
	run.Printf("    namespace:  rook-ceph\n")

	var nodes []values.StorageNode
	if deployWorkers > 0 && deployDiskCount > 0 {
		run.Printf("    storage:    %d node-device OSD(s) (one per worker)\n", deployWorkers*deployDiskCount)
		var err error
		nodes, err = clusterStorageNodes(deployName, deployWorkers, deployDiskCount, waitForISCSIDevice)
		if err != nil {
			return err
		}
	}
	base := values.ClusterBase(values.ClusterInput{OperatorNamespace: "rook-ceph", Nodes: nodes})
	valuesPath, err := writeComposed(chartCluster, base, dir, active)
	if err != nil {
		return err
	}

	clusterArgs := append([]string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName, chartPath,
	}, helmValueArgs(valuesPath, deploySets)...)
	return run.CmdWithEnv(deployHelmEnv, "helm", clusterArgs...)
}

// clusterStorageNodes resolves each worker's iSCSI disks to the device paths
// rook should claim. resolve is injectable so the mapping can be tested without
// an iSCSI session. An unresolved device aborts the deploy: rooket exists to
// give each worker a real OSD on its own disk, and silently deploying with
// fewer OSDs than requested would hide exactly the failure an operator needs
// to see immediately.
func clusterStorageNodes(cluster string, workers, disks int,
	resolve func(iqn string) (string, error)) ([]values.StorageNode, error) {

	out := make([]values.StorageNode, 0, workers)
	for i := 0; i < workers; i++ {
		node := values.StorageNode{Name: workerNodeName(cluster, i)}
		for d := 0; d < disks; d++ {
			iqn := fmt.Sprintf("iqn.%s.local.rooket:%s-worker%d-disk%d", deployIQNDate, cluster, i, d)
			dev, err := resolve(iqn)
			if err != nil {
				return nil, fmt.Errorf("resolve iSCSI device for worker %d disk %d: %w", i, d, err)
			}
			node.Devices = append(node.Devices, dev)
		}
		out = append(out, node)
	}
	return out, nil
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
	pf.StringArrayVar(&deployWith, "with", nil, "profile to enable, in addition to the clone's sticky list (repeatable)")
	pf.StringArrayVar(&deployWithOnly, "with-only", nil, "profile to enable, replacing the clone's sticky list (repeatable)")
	pf.StringArrayVarP(&deployValueFiles, "values", "f", nil, "additional values file, applied above profiles (repeatable)")
	pf.StringArrayVar(&deploySets, "set", nil, "value passed straight through to helm, applied above every layer (repeatable)")
}
