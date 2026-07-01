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
	deployDir          string
	deployRegistryPort int
	deployNamespace    string
	deployImageName    string
	deployOperatorName string
	deployClusterName  string
	deployKubeContext  string
	deployName         string
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

	// Pin one OSD to each worker's own iSCSI disk with an explicit per-node device
	// list. Every privileged kind node sees every host disk, so naming the device
	// per node is what keeps Rook from mis-attributing OSDs; this puts the OSD on
	// Rook's direct device path, with no local PV or kubelet loop device.
	if deployWorkers > 0 && deployDiskCount > 0 {
		valuesPath, err := writeClusterStorageValues()
		if err != nil {
			return err
		}
		defer os.Remove(valuesPath)
		fmt.Printf("    storage:    %d node-device OSD(s) (one per worker)\n", deployWorkers*deployDiskCount)
		args = append(args, "-f", valuesPath)
	}

	return run.Cmd("helm", args...)
}

// writeClusterStorageValues renders a Helm values file pinning one OSD to each
// worker's own iSCSI disk with an explicit per-node device list. Naming the
// device per node keeps Rook from mis-attributing OSDs (every privileged kind
// node sees every host disk), so each worker gets exactly one OSD on its own
// disk via Rook's direct device path — no local PV, no kubelet loop. Returns the
// file path; the caller removes it.
func writeClusterStorageValues() (string, error) {
	var sb strings.Builder
	sb.WriteString("cephClusterSpec:\n")
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
