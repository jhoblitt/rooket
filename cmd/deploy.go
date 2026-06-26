package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	deployDir            string
	deployRegistryPort   int
	deployNamespace      string
	deployImageName      string
	deployOperatorName   string
	deployClusterName    string
	deployKubeContext    string
	deployName           string
	deployWorkers        int
	deployDiskCount      int
	deployDiskSizeGB     int
	deployIQNDate        string
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
		dir, err := resolveDeployDir()
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
		dir, err := resolveDeployDir()
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
		dir, err := resolveDeployDir()
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

func resolveDeployDir() (string, error) {
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

	fmt.Printf("==> deploying rook-ceph operator\n")
	fmt.Printf("    chart:      %s\n", chartPath)
	fmt.Printf("    image:      %s:%s\n", imageRepo, imageTag)
	fmt.Printf("    release:    %s\n", deployOperatorName)
	fmt.Printf("    namespace:  rook-ceph\n")

	return run.Cmd(
		"helm",
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployOperatorName,
		chartPath,
		"--set", fmt.Sprintf("image.repository=%s", imageRepo),
		"--set", fmt.Sprintf("image.tag=%s", imageTag),
	)
}

func installRookCephCluster(dir string) error {
	chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph-cluster")

	args := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName,
		chartPath,
		"--set", "operatorNamespace=rook-ceph",
		"--set", "toolbox.enabled=true",
	}

	fmt.Printf("==> deploying rook-ceph-cluster\n")
	fmt.Printf("    chart:      %s\n", chartPath)
	fmt.Printf("    release:    %s\n", deployClusterName)
	fmt.Printf("    namespace:  rook-ceph\n")

	// The kind nodes are privileged and share the host's /dev, so a node-local
	// device list lets every worker see (and rook mis-attribute) every disk.
	// Instead back each OSD with a node-pinned local PV: the PV's nodeAffinity
	// fixes placement to one OSD per worker, and PVC-backed OSDs scope
	// ceph-volume's listing to the claim rather than the whole host.
	if deployWorkers > 0 && deployDiskCount > 0 {
		if err := applyLocalStorage(); err != nil {
			return err
		}
		valuesPath, err := writeClusterStorageValues()
		if err != nil {
			return err
		}
		defer os.Remove(valuesPath)
		fmt.Printf("    storage:    %d local-PV OSD(s) via storageClassDeviceSet (one per worker)\n", deployWorkers*deployDiskCount)
		args = append(args, "-f", valuesPath)
	}

	return run.Cmd("helm", args...)
}

// writeClusterStorageValues renders a Helm values file configuring OSDs as a
// storageClassDeviceSet over the node-pinned local PVs created by
// applyLocalStorage. PVC-backed OSDs scope ceph-volume's listing to the claim,
// so each worker ends up with exactly one OSD on its own iSCSI disk. Returns the
// file path; the caller removes it.
func writeClusterStorageValues() (string, error) {
	var sb strings.Builder
	sb.WriteString("cephClusterSpec:\n")
	sb.WriteString("  storage:\n")
	sb.WriteString("    useAllNodes: false\n")
	sb.WriteString("    useAllDevices: false\n")
	sb.WriteString("    storageClassDeviceSets:\n")
	sb.WriteString("      - name: rooket\n")
	sb.WriteString(fmt.Sprintf("        count: %d\n", deployWorkers*deployDiskCount))
	sb.WriteString("        portable: false\n")
	sb.WriteString("        volumeClaimTemplates:\n")
	sb.WriteString("          - metadata:\n")
	sb.WriteString("              name: data\n")
	sb.WriteString("            spec:\n")
	sb.WriteString("              storageClassName: rooket-local\n")
	sb.WriteString("              volumeMode: Block\n")
	sb.WriteString("              accessModes:\n")
	sb.WriteString("                - ReadWriteOnce\n")
	sb.WriteString("              resources:\n")
	sb.WriteString("                requests:\n")
	sb.WriteString(fmt.Sprintf("                  storage: %dGi\n", deployDiskSizeGB))

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

// applyLocalStorage creates the no-provisioner StorageClass and one block-mode
// local PersistentVolume per iSCSI disk. Each PV's nodeAffinity pins it to the
// worker that owns the disk, so the storageClassDeviceSet's WaitForFirstConsumer
// claims bind one OSD to each node.
func applyLocalStorage() error {
	fmt.Printf("==> applying local-PV StorageClass and PersistentVolumes\n")
	return run.CmdWithStdin(strings.NewReader(localStorageManifest()),
		"kubectl", "apply", "--context", deployKubeContext, "-f", "-")
}

// localStorageManifest renders the no-provisioner StorageClass and a node-pinned,
// block-mode local PV for each worker's iSCSI disk(s).
func localStorageManifest() string {
	var sb strings.Builder
	sb.WriteString(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: rooket-local
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Retain
`)
	for i := 0; i < deployWorkers; i++ {
		node := workerNodeName(deployName, i)
		for d := 0; d < deployDiskCount; d++ {
			byPath := fmt.Sprintf(
				"/dev/disk/by-path/ip-127.0.0.1:3260-iscsi-iqn.%s.local.rooket:%s-worker%d-disk%d-lun-0",
				deployIQNDate, deployName, i, d)
			sb.WriteString(fmt.Sprintf(`---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: rooket-osd-%s-disk%d
spec:
  capacity:
    storage: %dGi
  volumeMode: Block
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  storageClassName: rooket-local
  local:
    path: %q
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/hostname
              operator: In
              values:
                - %s
`, node, d, deployDiskSizeGB, byPath, node))
		}
	}
	return sb.String()
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
	pf.StringVar(&deployKubeContext, "context", "kind-rook", "kubectl context to use")
	pf.IntVar(&deployRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	pf.StringVar(&deployNamespace, "namespace", "rook", "image namespace in the registry")
	pf.StringVar(&deployImageName, "image-name", "ceph", "image name without architecture suffix")
	pf.StringVar(&deployOperatorName, "operator-release", "rook-ceph", "rook-ceph operator helm release name")
	pf.StringVar(&deployClusterName, "cluster-release", "rook-ceph-cluster", "rook-ceph-cluster helm release name")
	pf.StringVar(&deployName, "name", "rook", "kind cluster name (for node-name and iSCSI by-path derivation)")
	pf.IntVar(&deployWorkers, "workers", 3, "worker node count (for per-node OSD device pinning)")
	pf.IntVar(&deployDiskCount, "disk-count", 1, "iSCSI disks per worker (0 disables OSD device pinning)")
	pf.IntVar(&deployDiskSizeGB, "disk-size", 10, "disk size in GiB (must match 'rooket block setup'; used for local-PV capacity)")
	pf.StringVar(&deployIQNDate, "iqn-date", "2003-01", "IQN date component matching 'rooket block setup'")
}
