package cmd

import (
	"fmt"
	"os"

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
  2. rooket block teardown  — only with --delete-disks: log out iSCSI sessions,
     remove targets, delete the disk images and the cluster's state dir

By default the disk images AND their iSCSI targets are preserved, so a plain
down needs no root and the next up reuses the devices without prompting either.
Pass --delete-disks for the full teardown (this is the step that needs
sudo/pkexec). Use --skip-cluster to omit the cluster step.

Example:
  rooket down                 # cluster gone, disks kept: no root needed
  rooket down --delete-disks  # full teardown incl. iSCSI targets and images
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

		// The iSCSI targets are host-level config pointing at the preserved
		// images: removing them is the only step that needs root, and keeping
		// them lets the next up skip its privileged block setup too. So a plain
		// down leaves them alone; --delete-disks is the full, privileged teardown.
		if downSkipBlock || downDiskCount == 0 || !downDeleteDisks {
			fmt.Println("==> [2/2] block teardown (skipped; disk images and iSCSI targets preserved — pass --delete-disks to remove them)")
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

			// With the disks gone, the cluster's state dir holds only leftovers
			// (kubeconfig, registry-port marker) — remove it entirely.
			if downDeleteDisks {
				if dir, err := stateDirPath(downName); err == nil {
					if err := os.RemoveAll(dir); err != nil {
						fmt.Printf("warning: remove state dir %s: %v\n", dir, err)
					} else {
						fmt.Printf("removed state dir %s\n", dir)
					}
				}
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
	downCmd.Flags().BoolVar(&downDeleteDisks, "delete-disks", false, "full teardown: remove iSCSI targets and delete the disk images and state dir (needs root)")
	downCmd.Flags().BoolVar(&downSkipBlock, "skip-block", false, "skip block teardown even with --delete-disks")
	downCmd.Flags().BoolVar(&downSkipCluster, "skip-cluster", false, "skip cluster delete")
}
