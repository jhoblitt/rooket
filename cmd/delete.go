package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
)

var deleteName string

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete the kind cluster and associated registry",
	Long: `delete tears down the cluster created by 'rooket cluster create':

  1. Delete the kind cluster.
  2. Stop and remove the local OCI registry container.

iSCSI block devices set up by 'rooket block setup' are not affected and must
be torn down separately if desired.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regName := registry.ContainerName(deleteName)

		// --- Step 1: kind cluster ---
		fmt.Println("==> deleting kind cluster")
		if err := cluster.Delete(deleteName); err != nil {
			fmt.Printf("warning: delete cluster: %v\n", err)
		}

		// --- Step 2: registry container ---
		fmt.Println("==> deleting local OCI registry")
		if err := registry.Delete(regName); err != nil {
			fmt.Printf("warning: delete registry: %v\n", err)
		}

		fmt.Printf("cluster %q deleted\n", deleteName)
		return nil
	},
}

func init() {
	clusterCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().StringVar(&deleteName, "name", "rook", "kind cluster name")
}
