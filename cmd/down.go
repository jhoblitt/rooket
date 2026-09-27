package cmd

import (
	"fmt"
	"os"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/lio"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/spf13/cobra"
)

var (
	downName        string
	downWorkers     int
	downDiskCount   int
	downIQNDate     string
	downDeleteDisks bool
	downDeleteCache bool
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
Pass --delete-disks for the full teardown (this is the step that needs root —
see 'rooket sudoers install' to remove the prompt). Use --skip-cluster to omit
the cluster step.

A cluster with no recorded shape has its iSCSI disks found rather than
assumed from a worker count: the images in its state directory and the targets
the kernel still holds for it, plus those of the workers an explicit --workers
names. A cluster with nothing left at all is reported as not found, and down
succeeds, so a teardown is safe to repeat.

The shared OCI pull-through cache is host-wide, not per-cluster: it is what
makes the next 'up' — in this clone or any other — skip re-downloading upstream
images, so no teardown touches it unless you pass --delete-cache. That flag is
separate from --delete-disks because the two remove different things:
--delete-disks reclaims this cluster's iSCSI targets and disk images, while
--delete-cache discards images shared by every cluster on the host.

--all tears down every rooket cluster at once instead of one: every state
directory under ~/.local/share/rooket (orphans included) plus the kind clusters
rooket owns that are live under any installed engine — a live cluster counts as
rooket's only if it has a state dir or a rooket registry container, so a
foreign 'kind create cluster' is left alone (add --include-unmanaged to sweep
those too). It prompts with the full plan first; --force skips the prompt and
--dry-run stops at it. With --delete-disks all iSCSI target teardowns are
batched into a single privileged run, so freeing the whole machine costs at
most one prompt (or none, with rooket's sudoers rule installed).

Example:
  rooket down                      # cluster gone, disks kept: no root needed
  rooket down --delete-disks       # full teardown incl. iSCSI targets and images
  rooket down --all --delete-disks # destroy every cluster and all state
  rooket down --delete-cache       # also discard the host-wide image cache
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if downAll {
			return downAllRun(cmd)
		}
		if downForce {
			return fmt.Errorf("--force requires --all")
		}
		if downDryRun {
			return fmt.Errorf("--dry-run requires --all")
		}
		if downInclUnmanaged {
			return fmt.Errorf("--include-unmanaged requires --all")
		}
		// --workers 0 is down's default, not a count: the recorded one, or with
		// no record the disks that can be found.
		if err := checkShapeFlags(cmd, 0); err != nil {
			return err
		}

		name, err := clusterName(downName)
		if err != nil {
			return err
		}
		downName = name
		release, err := LockCluster(downName)
		if err != nil {
			return err
		}
		defer release()
		if err := useRecordedShape(downName, cmd.Flags().Changed, matchShape, &downWorkers, &downDiskCount, &downIQNDate); err != nil {
			return err
		}
		// Decided under the lock, so no up can be creating the cluster between
		// this check and the report that it does not exist.
		if _, recorded := readShape(downName); !recorded && !clusterLeftovers(downName, hostLIORoot(), downIQNDate) {
			run.Printf("cluster %q not found: no kind cluster, registry, state directory, or iSCSI targets; nothing to tear down\n", downName)
			// Nothing is left for its lock file to guard.
			removeClusterLockOnRelease(downName)
			if downDeleteCache {
				return removeSharedCache()
			}
			return nil
		}

		if downSkipCluster {
			run.Printf("==> [1/2] cluster delete (skipped)\n")
		} else {
			run.Printf("==> [1/2] cluster delete\n")
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
			run.Printf("==> [2/2] block teardown (skipped; disk images and iSCSI targets preserved — pass --delete-disks to remove them)\n")
		} else {
			run.Printf("==> [2/2] block teardown\n")
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
						run.Printf("warning: remove state dir %s: %v\n", dir, err)
					} else {
						run.Printf("removed state dir %s\n", dir)
						removeClusterLockOnRelease(downName)
					}
				}
			}
		}

		if downDeleteCache {
			if err := removeSharedCache(); err != nil {
				return err
			}
		} else {
			noteCachePreserved(os.Stdout)
		}

		run.Printf("\nrooket down complete.\n")
		return nil
	},
}

// removeSharedCache is down's --delete-cache step. The cache is host-wide, so
// it runs even for a cluster that has nothing else to tear down.
func removeSharedCache() error {
	run.Printf("==> removing the shared image cache\n")
	if err := teardownCache(os.Stdout); err != nil {
		return fmt.Errorf("remove image cache: %w", err)
	}
	return nil
}

// clusterLeftovers reports whether anything of a cluster is left for down to
// remove, looking everywhere up leaves something: the state directory, the
// kernel's iSCSI configuration, the kind cluster, and its registry container.
// A probe that cannot answer counts as something left, so a cluster is only
// reported absent when it has been shown to be.
func clusterLeftovers(name, lioRoot, iqnDate string) bool {
	dir, err := stateDirPath(name)
	if err != nil {
		return true
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		return true
	}
	st, err := lio.Read(lioRoot)
	if err != nil || len(lioClusterDisks(st, iqnDate)[name]) > 0 {
		return true
	}
	if live, err := cluster.Exists(os.Stdout, containerEngine, name); err != nil || live {
		return true
	}
	found, err := registry.Lookup(os.Stdout, containerEngine, registry.ContainerName(name))
	return err != nil || found
}

// hostLIORoot is where down, down --all, and block teardown read the host's LIO
// configuration. Tests point it at a fake tree, so that no test of those
// commands depends on the iSCSI targets of the machine it runs on.
var hostLIORoot = func() string { return lio.DefaultRoot }

func init() {
	rootCmd.AddCommand(downCmd)

	downCmd.Flags().StringVar(&downName, "name", "", "kind cluster name")
	downCmd.Flags().IntVar(&downWorkers, "workers", 0, "number of worker nodes; unset, the cluster's recorded value, which a set flag must match (with no record, only the iSCSI disks that can be found are torn down)")
	downCmd.Flags().IntVar(&downDiskCount, "disk-count", 1, "iSCSI disks per worker, 0 skips block teardown; unset, the cluster's recorded value, which a set flag must match")
	downCmd.Flags().StringVar(&downIQNDate, "iqn-date", "2003-01", "IQN date component (YYYY-MM); unset, the cluster's recorded value, which a set flag must match")
	downCmd.Flags().BoolVar(&downDeleteDisks, "delete-disks", false, "full teardown: remove iSCSI targets and delete the disk images and state dir (needs root)")
	downCmd.Flags().BoolVar(&downDeleteCache, "delete-cache", false, "also remove the host-wide OCI pull-through cache container and its volume (shared by every rooket cluster)")
	downCmd.Flags().BoolVar(&downSkipBlock, "skip-block", false, "skip block teardown even with --delete-disks")
	downCmd.Flags().BoolVar(&downSkipCluster, "skip-cluster", false, "skip cluster delete")
}
