package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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

	fmt.Printf("==> deploying rook-ceph-cluster\n")
	fmt.Printf("    chart:      %s\n", chartPath)
	fmt.Printf("    release:    %s\n", deployClusterName)
	fmt.Printf("    namespace:  rook-ceph\n")

	return run.Cmd(
		"helm",
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName,
		chartPath,
		"--set", "operatorNamespace=rook-ceph",
	)
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
}
