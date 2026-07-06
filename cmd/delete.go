package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	deleteName string
	deleteZap  bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete the kind cluster and associated registry",
	Long: `delete tears down the cluster created by 'rooket cluster create':

  1. Delete the kind cluster (releasing the OSD disks).
  2. Zap the OSD disks (unless --zap=false) so the next bring-up starts clean:
     re-create the iSCSI disk images as sparse and refresh the udev cache.
  3. Stop and remove the local OCI registry container.

The iSCSI targets themselves set up by 'rooket block setup' are not removed and
must be torn down separately if desired.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := useCluster(deleteName)
		if err != nil {
			return err
		}
		deleteName = name
		regName := registry.ContainerName(deleteName)

		// --- Step 1: kind cluster (releases the OSD disks) ---
		fmt.Println("==> deleting kind cluster")
		if err := cluster.Delete(deleteName); err != nil {
			fmt.Printf("warning: delete cluster: %v\n", err)
		}
		// kind delete strips the cluster's entries but leaves the (now empty)
		// kubeconfig file behind; remove it so 'rooket k' reports "is it up?"
		// instead of letting kubectl chase an empty config. The file is
		// per-cluster, so nothing else lives in it.
		if kc, err := kubeconfigPath(deleteName); err == nil {
			_ = os.Remove(kc)
		}

		// --- Step 2: zap OSD disks now that the nodes have released them ---
		if deleteZap {
			if dir, err := stateDirPath(deleteName); err == nil {
				cluster.ZapISCSIDisks(containerEngine, deleteName, dir)
			} else {
				fmt.Printf("warning: zap OSD disks: %v\n", err)
			}
		}

		// --- Step 3: registry container ---
		fmt.Println("==> deleting local OCI registry")
		if err := registry.Delete(containerEngine, regName); err != nil {
			fmt.Printf("warning: delete registry: %v\n", err)
		}

		fmt.Printf("cluster %q deleted\n", deleteName)
		return nil
	},
}

func init() {
	clusterCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().StringVar(&deleteName, "name", "", "kind cluster name")
	deleteCmd.Flags().BoolVar(&deleteZap, "zap", true, "re-sparsify (wipe) the OSD disk images during teardown so the next bring-up starts clean")
}
