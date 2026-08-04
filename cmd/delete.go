package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
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

		// --- Steps 1 and 2: kind cluster (releasing the OSD disks), then zap ---
		if err := deleteClusterAndZap(os.Stdout, deleteName, deleteZap); err != nil {
			return err
		}

		// --- Step 3: registry container ---
		run.Printf("==> deleting local OCI registry\n")
		if err := registry.Delete(os.Stdout, containerEngine, regName); err != nil {
			run.Printf("warning: delete registry: %v\n", err)
		}

		run.Printf("cluster %q deleted\n", deleteName)
		return nil
	},
}

// deleteClusterAndZap deletes the kind cluster, drops its kubeconfig, and — only
// once the cluster is confirmed gone — re-sparsifies its OSD disk images. Both
// 'cluster delete' and the create path's drift recovery go through it, so the
// rule that decides whether user data is destroyed has one implementation
// instead of a copy per caller.
func deleteClusterAndZap(out io.Writer, name string, zap bool) error {
	run.Fprintf(out, "==> deleting kind cluster\n")
	if err := cluster.Delete(out, containerEngine, name, ""); err != nil {
		// kind delete is a no-op success on an absent cluster, so an error
		// means something went wrong. The zap below truncates the OSD disk
		// images; doing that while the cluster still holds them corrupts a
		// live cluster. Only continue if the cluster is confirmed gone.
		exists, exErr := cluster.Exists(out, containerEngine, name)
		if exErr != nil {
			return fmt.Errorf("delete cluster %q: %w; could not verify it was removed (%v), so not zapping its disks", name, err, exErr)
		}
		if exists {
			return fmt.Errorf("delete cluster %q: %w; cluster still present, not zapping its disks", name, err)
		}
		run.Fprintf(out, "warning: delete cluster returned an error but the cluster is gone: %v\n", err)
	}
	// kind delete strips the cluster's entries but leaves the (now empty)
	// kubeconfig file behind; remove it so 'rooket k' reports "is it up?"
	// instead of letting kubectl chase an empty config. The file is
	// per-cluster, so nothing else lives in it.
	if kc, err := kubeconfigPath(name); err == nil {
		_ = os.Remove(kc)
	}
	if !zap {
		return nil
	}
	dir, err := stateDirPath(name)
	if err != nil {
		return fmt.Errorf("locate the state directory of cluster %q to zap its OSD disks: %w", name, err)
	}
	if err := cluster.ZapISCSIDisks(out, containerEngine, name, dir); err != nil {
		return fmt.Errorf("zap the OSD disks of cluster %q: %w", name, err)
	}
	return nil
}

func init() {
	clusterCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().StringVar(&deleteName, "name", "", "kind cluster name")
	deleteCmd.Flags().BoolVar(&deleteZap, "zap", true, "re-sparsify (wipe) the OSD disk images during teardown so the next bring-up starts clean")
}
