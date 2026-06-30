package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	downName        string
	downWorkers     int
	downDiskCount   int
	downIQNDate     string
	downDeleteDisks bool
	downSkipBlock   bool
	downSkipCluster bool
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Tear down everything brought up by 'rooket up'",
	Long: `down reverses the work of 'rooket up':

  1. rooket cluster delete  — delete the kind cluster and the local registry
  2. rooket block teardown  — log out iSCSI sessions and remove targets

Use --skip-cluster or --skip-block to omit a step. Disk image files are
preserved by default — pass --delete-disks to remove them too.

Example:
  rooket down
  rooket down --delete-disks
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		downName = clusterName(downName)

		if downSkipCluster {
			fmt.Println("==> [1/2] cluster delete (skipped)")
		} else {
			fmt.Println("==> [1/2] cluster delete")
			deleteName = downName
			if err := deleteCmd.RunE(deleteCmd, nil); err != nil {
				return fmt.Errorf("cluster delete: %w", err)
			}
		}

		if downSkipBlock || downDiskCount == 0 {
			fmt.Println("==> [2/2] block teardown (skipped)")
		} else {
			fmt.Println("==> [2/2] block teardown")
			blockTeardownName = downName
			blockTeardownWorkers = downWorkers
			blockTeardownDiskCount = downDiskCount
			blockTeardownIQNDate = downIQNDate
			blockTeardownDeleteDisks = downDeleteDisks
			if err := blockTeardownRun(nil, nil); err != nil {
				return fmt.Errorf("block teardown: %w", err)
			}
		}

		fmt.Println("\nrooket down complete.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(downCmd)

	downCmd.Flags().StringVar(&downName, "name", "", "kind cluster name")
	downCmd.Flags().IntVar(&downWorkers, "workers", 3, "number of worker nodes (must match 'up')")
	downCmd.Flags().IntVar(&downDiskCount, "disk-count", 1, "iSCSI disks per worker (0 skips block teardown)")
	downCmd.Flags().StringVar(&downIQNDate, "iqn-date", "2003-01", "IQN date component (YYYY-MM)")
	downCmd.Flags().BoolVar(&downDeleteDisks, "delete-disks", false, "also delete disk image files")
	downCmd.Flags().BoolVar(&downSkipBlock, "skip-block", false, "skip block teardown")
	downCmd.Flags().BoolVar(&downSkipCluster, "skip-cluster", false, "skip cluster delete")
}
