package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	upName            string
	upWorkers         int
	upDiskCount       int
	upDiskSizeGB     int
	upRegistryPort    int
	upIQNDate         string
	upRookDir         string
	upPromVersion     string
	upPromRelease     string
	upOperatorRelease string
	upClusterRelease  string
	upSkipBlock       bool
	upSkipBuild       bool
	upSkipDeploy      bool
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Bring up a complete rook-on-kind environment in one step",
	Long: `up runs the full bring-up sequence:

  1. rooket block setup    — create disk images and iSCSI targets
  2. rooket cluster create — start the kind cluster with the local registry
  3. rooket build          — build rook and push to the local registry
  4. rooket deploy         — install the rook-ceph and rook-ceph-cluster charts

Use --skip-block, --skip-build, or --skip-deploy to omit individual steps.
Setting --disk-count 0 also skips the block-setup step automatically.

Example:
  rooket up --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve the rook source dir up front so a missing clone fails fast,
		// before we stand up a cluster. Only build and deploy consume it.
		var rookDir string
		if !upSkipBuild || !upSkipDeploy {
			var err error
			rookDir, err = resolveRookDir(upRookDir)
			if err != nil {
				return err
			}
		}

		// --- Step 1: block setup ---
		if upSkipBlock || upDiskCount == 0 {
			fmt.Println("==> [1/4] block setup (skipped)")
		} else {
			fmt.Println("==> [1/4] block setup")
			blockSetupName = upName
			blockSetupWorkers = upWorkers
			blockSetupDiskCount = upDiskCount
			blockSetupDiskSizeGB = upDiskSizeGB
			blockSetupIQNDate = upIQNDate
			if err := blockSetupRun(nil, nil); err != nil {
				return fmt.Errorf("block setup: %w", err)
			}
		}

		// --- Step 2: cluster create ---
		fmt.Println("==> [2/4] cluster create")
		createName = upName
		createWorkers = upWorkers
		createRegistryPort = upRegistryPort
		createDiskCount = upDiskCount
		createISCSIQNDate = upIQNDate
		createPromCRDsVersion = upPromVersion
		createPromCRDsRelease = upPromRelease
		if err := createCmd.RunE(createCmd, nil); err != nil {
			return fmt.Errorf("cluster create: %w", err)
		}

		// --- Step 3: build ---
		if upSkipBuild {
			fmt.Println("==> [3/4] build (skipped)")
		} else {
			fmt.Println("==> [3/4] build")
			buildDir = rookDir
			buildRegistryPort = upRegistryPort
			if err := buildCmd.RunE(buildCmd, nil); err != nil {
				return fmt.Errorf("build: %w", err)
			}
		}

		// --- Step 4: deploy ---
		if upSkipDeploy {
			fmt.Println("==> [4/4] deploy (skipped)")
		} else {
			fmt.Println("==> [4/4] deploy")
			deployDir = rookDir
			deployRegistryPort = upRegistryPort
			deployKubeContext = "kind-" + upName
			deployOperatorName = upOperatorRelease
			deployClusterName = upClusterRelease
			deployName = upName
			deployWorkers = upWorkers
			deployDiskCount = upDiskCount
			deployDiskSizeGB = upDiskSizeGB
			deployIQNDate = upIQNDate
			if err := deployCmd.RunE(deployCmd, nil); err != nil {
				return fmt.Errorf("deploy: %w", err)
			}
		}

		fmt.Printf("\nrooket up complete. cluster %q is ready (context: kind-%s).\n",
			upName, upName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(upCmd)

	upCmd.Flags().StringVar(&upName, "name", "rook", "kind cluster name")
	upCmd.Flags().IntVar(&upWorkers, "workers", 3, "number of worker nodes")
	upCmd.Flags().IntVar(&upDiskCount, "disk-count", 1, "iSCSI disks per worker (0 skips block setup)")
	upCmd.Flags().IntVar(&upDiskSizeGB, "disk-size", 10, "disk size in GiB")
	upCmd.Flags().IntVar(&upRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	upCmd.Flags().StringVar(&upIQNDate, "iqn-date", "2003-01", "IQN date component (YYYY-MM)")
	upCmd.Flags().StringVar(&upRookDir, "dir", "", "path to the rook source directory (default: $ROOK_DIR, else the rook clone found by walking up from the current directory)")
	upCmd.Flags().StringVar(&upPromVersion, "prometheus-operator-crds-version", "29.0.0", "version of the prometheus-operator-crds helm chart")
	upCmd.Flags().StringVar(&upPromRelease, "prometheus-operator-crds-release", "my-prometheus-operator-crds", "helm release name for prometheus-operator-crds")
	upCmd.Flags().StringVar(&upOperatorRelease, "operator-release", "rook-ceph", "rook-ceph operator helm release name")
	upCmd.Flags().StringVar(&upClusterRelease, "cluster-release", "rook-ceph-cluster", "rook-ceph-cluster helm release name")
	upCmd.Flags().BoolVar(&upSkipBlock, "skip-block", false, "skip 'block setup'")
	upCmd.Flags().BoolVar(&upSkipBuild, "skip-build", false, "skip 'build'")
	upCmd.Flags().BoolVar(&upSkipDeploy, "skip-deploy", false, "skip 'deploy'")
}
