package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/disks"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	deleteName      string
	deleteDataDir   string
	deleteWorkers   int
	deleteDiskCount int
	deleteKeepDisks bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete the kind cluster and associated registry and disk images",
	Long: `delete tears down the cluster created by 'rooket create':

  1. Delete the kind cluster (loop devices inside nodes are cleaned up automatically).
  2. Stop and remove the local OCI registry container.
  3. Unless --keep-disks, remove the disk image files from the host.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regName := registry.ContainerName(deleteName)

		dataDir := deleteDataDir
		if dataDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home dir: %w", err)
			}
			dataDir = filepath.Join(home, ".local", "share", "rooket", deleteName)
		}

		// --- Step 1: kind cluster ---
		// Deleting the cluster removes the node containers, which automatically
		// cleans up any loop devices that were set up inside them.
		fmt.Println("==> deleting kind cluster")
		if err := cluster.Delete(deleteName); err != nil {
			fmt.Printf("warning: delete cluster: %v\n", err)
		}

		// --- Step 2: registry container ---
		fmt.Println("==> deleting local OCI registry")
		if err := registry.Delete(regName); err != nil {
			fmt.Printf("warning: delete registry: %v\n", err)
		}

		// --- Step 3: loop devices and disk image files ---
		// The cluster deletion above releases the bind-mounts, so the loop
		// devices are no longer held open. They are owned by the current user
		// (transferred via sudo chown during create), so no sudo is needed here.
		if deleteDiskCount > 0 {
			fmt.Println("==> detaching loop devices")
			for i := 0; i < deleteWorkers; i++ {
				diskCfg := disks.Config{
					DataDir:     dataDir,
					WorkerIndex: i,
					Count:       deleteDiskCount,
				}
				if err := disks.Detach(diskCfg); err != nil {
					fmt.Printf("warning: detach loop devices for worker %d: %v\n", i, err)
				}
			}
		}
		if !deleteKeepDisks && deleteDiskCount > 0 {
			fmt.Println("==> removing disk image files")
			for i := 0; i < deleteWorkers; i++ {
				diskCfg := disks.Config{
					DataDir:     dataDir,
					WorkerIndex: i,
					Count:       deleteDiskCount,
				}
				if err := disks.Remove(diskCfg); err != nil {
					fmt.Printf("warning: remove disk images for worker %d: %v\n", i, err)
				}
			}
			_ = os.Remove(dataDir)
		}

		fmt.Printf("cluster %q deleted\n", deleteName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().StringVar(&deleteName, "name", "rook", "kind cluster name")
	deleteCmd.Flags().StringVar(&deleteDataDir, "data-dir", "", "directory containing disk images (default: ~/.local/share/rooket/<name>)")
	deleteCmd.Flags().IntVar(&deleteWorkers, "workers", 3, "number of workers (must match create)")
	deleteCmd.Flags().IntVar(&deleteDiskCount, "disk-count", 1, "disks per worker (must match create; 0 to skip)")
	deleteCmd.Flags().BoolVar(&deleteKeepDisks, "keep-disks", false, "keep disk image files on disk")
}
