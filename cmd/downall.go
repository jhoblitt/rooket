package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	downAll    bool
	downForce  bool
	downDryRun bool
)

// downAllRun tears down every cluster rooket can see: kind clusters live under
// any installed engine (rooket-created or not) plus every state directory,
// including orphans left by deleted clones. Without --delete-disks it preserves
// disk images and iSCSI targets like a plain down; with it, every cluster's
// target teardown is batched into one privileged script so the whole sweep
// costs at most a single sudo/pkexec prompt.
func downAllRun(cmd *cobra.Command) error {
	for _, f := range []string{"name", "workers", "disk-count", "skip-cluster"} {
		if cmd.Flags().Changed(f) {
			return fmt.Errorf("--%s cannot be combined with --all", f)
		}
	}

	live, consulted, failed := liveClusters()
	for _, eng := range failed {
		fmt.Fprintf(os.Stderr, "warning: %s is installed but could not be queried\n", eng)
	}
	if len(consulted) == 0 {
		return fmt.Errorf("cannot determine live clusters (no queryable container engine)")
	}
	if len(failed) > 0 {
		return fmt.Errorf("refusing --all with an unqueryable engine present: its clusters (if any) could not be torn down")
	}

	root, stateNames, err := stateDirNames()
	if err != nil {
		return err
	}
	hasState := map[string]bool{}
	for _, n := range stateNames {
		hasState[n] = true
	}
	all := map[string]bool{}
	for n := range live {
		all[n] = true
	}
	for _, n := range stateNames {
		all[n] = true
	}
	if len(all) == 0 {
		fmt.Println("nothing to tear down")
		return nil
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Println("The following clusters will be torn down:")
	w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tLIVE\tSTATE DIR")
	for _, n := range names {
		liveCol := "-"
		if engs := live[n]; len(engs) > 0 {
			ss := make([]string, len(engs))
			for i, e := range engs {
				ss[i] = e.String()
			}
			liveCol = strings.Join(ss, ",")
		}
		dirCol := "-"
		if hasState[n] {
			dirCol = filepath.Join(root, n)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", n, liveCol, dirCol)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if downDeleteDisks {
		fmt.Println("Full teardown: iSCSI targets, disk images, and state directories will be removed.")
	} else {
		fmt.Println("Disk images and iSCSI targets will be preserved (pass --delete-disks to remove them).")
	}
	if downDryRun {
		return nil
	}
	if !downForce {
		fmt.Printf("Proceed? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			fmt.Println("aborted")
			return nil
		}
	}

	for _, n := range names {
		engs := live[n]
		if len(engs) == 0 {
			continue
		}
		fmt.Printf("==> deleting cluster %q\n", n)
		kc, _ := kubeconfigPath(n)
		for _, eng := range engs {
			if err := cluster.DeleteWith(eng, n, kc); err != nil {
				fmt.Printf("warning: delete cluster %q under %s: %v\n", n, eng, err)
			}
			if err := registry.Delete(eng, registry.ContainerName(n)); err != nil {
				fmt.Printf("warning: delete registry for %q under %s: %v\n", n, eng, err)
			}
		}
		if kc != "" {
			_ = os.Remove(kc)
		}
		// Preserved images must still be zapped so the next up starts clean;
		// images about to be deleted don't need it.
		if !downDeleteDisks && hasState[n] {
			cluster.ZapISCSIDisks(engs[0], n, filepath.Join(root, n))
		}
	}

	if downDeleteDisks && !downSkipBlock {
		var disks []iscsiDisk
		for _, n := range names {
			if hasState[n] {
				disks = append(disks, stateDirDisks(n, filepath.Join(root, n), downIQNDate)...)
			}
		}
		if len(disks) > 0 {
			fmt.Println("==> tearing down iSCSI targets (all clusters in one privileged run)")
			script := buildISCSITeardownScript(disks)
			if err := runPrivilegedScript(script); err != nil {
				return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", script, err)
			}
		}
		for _, n := range names {
			if !hasState[n] {
				continue
			}
			dir := filepath.Join(root, n)
			if err := os.RemoveAll(dir); err != nil {
				fmt.Printf("warning: remove state dir %s: %v\n", dir, err)
			} else {
				fmt.Printf("removed state dir %s\n", dir)
			}
		}
	} else if downDeleteDisks {
		fmt.Println("block teardown skipped by --skip-block; disk images and state dirs preserved")
	}

	fmt.Println("\nrooket down --all complete.")
	return nil
}

func init() {
	downCmd.Flags().BoolVar(&downAll, "all", false, "tear down every cluster: live under any engine, plus all state dirs")
	downCmd.Flags().BoolVar(&downForce, "force", false, "with --all: skip the confirmation prompt")
	downCmd.Flags().BoolVar(&downDryRun, "dry-run", false, "with --all: list what would be torn down, then exit")
}
