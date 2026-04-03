package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	deployDir          string
	deployRegistryPort int
	deployNamespace    string
	deployImageName    string
	deployName         string
	deployKubeContext  string
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy components into the kind cluster",
}

var deployRookCephCmd = &cobra.Command{
	Use:   "rook-ceph",
	Short: "Deploy the rook-ceph operator helm chart using the image from the local registry",
	Long: `deploy rook-ceph runs 'helm upgrade --install' for the rook-ceph operator chart
found in the rook source directory. The image tag is derived from the current
git branch of that directory — the same logic used by 'rooket build' — so the
chart always references whatever was last pushed to the local registry.

Example:
  rooket deploy rook-ceph --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := deployDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
		}

		gitRef, err := gitHeadRef(dir)
		if err != nil {
			return fmt.Errorf("determine git ref in %s: %w", dir, err)
		}

		registry := fmt.Sprintf("localhost:%d", deployRegistryPort)
		imageRepo := fmt.Sprintf("%s/%s/%s", registry, deployNamespace, deployImageName)
		imageTag := gitRef // already sanitized by gitHeadRef

		chartPath := filepath.Join(dir, "deploy", "charts", "rook-ceph")

		fmt.Printf("==> deploying rook-ceph\n")
		fmt.Printf("    chart:      %s\n", chartPath)
		fmt.Printf("    image:      %s:%s\n", imageRepo, imageTag)
		fmt.Printf("    release:    %s\n", deployName)
		fmt.Printf("    namespace:  rook-ceph\n")

		if err := run.Cmd(
			"helm",
			"--kube-context", deployKubeContext,
			"-n", "rook-ceph",
			"upgrade", "--install", "--create-namespace",
			deployName,
			chartPath,
			"--set", fmt.Sprintf("image.repository=%s", imageRepo),
			"--set", fmt.Sprintf("image.tag=%s", imageTag),
		); err != nil {
			return err
		}

		// Switch the default namespace to rook-ceph if the krew ns plugin is available.
		if nsOut, err := run.Output("kubectl", "ns", "rook-ceph"); err == nil {
			fmt.Println(nsOut)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.AddCommand(deployRookCephCmd)

	deployRookCephCmd.Flags().StringVar(&deployDir, "dir", "", "path to the rook source directory (default: current directory)")
	deployRookCephCmd.Flags().IntVar(&deployRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	deployRookCephCmd.Flags().StringVar(&deployNamespace, "namespace", "rook", "image namespace in the registry")
	deployRookCephCmd.Flags().StringVar(&deployImageName, "image-name", "ceph", "image name without architecture suffix")
	deployRookCephCmd.Flags().StringVar(&deployName, "name", "rook-ceph", "helm release name")
	deployRookCephCmd.Flags().StringVar(&deployKubeContext, "context", "kind-rook", "kubectl context to use")
}
