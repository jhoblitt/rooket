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
	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
)

// scopeTeardownSet decides which clusters 'down --all' acts on. Every state dir
// is rooket's, so all of them are included. A live kind cluster is included
// only when it is rooket-owned — it has a state dir, or owns() finds a rooket
// registry container for it — or inclUnmanaged is set; otherwise it is a
// foreign kind cluster (someone else's 'kind create cluster') and is returned
// in unmanaged and left alone. This keeps 'down --all --force' from deleting
// clusters rooket never created.
func scopeTeardownSet(live map[string][]engine.Engine, stateNames []string, inclUnmanaged bool, owns func(string, []engine.Engine) bool) (set map[string]bool, unmanaged []string) {
	set = map[string]bool{}
	hasState := map[string]bool{}
	for _, n := range stateNames {
		hasState[n] = true
		set[n] = true
	}
	for n, engs := range live {
		switch {
		case hasState[n] || owns(n, engs) || inclUnmanaged:
			set[n] = true
		default:
			unmanaged = append(unmanaged, n)
		}
	}
	sort.Strings(unmanaged)
	return set, unmanaged
}

var (
	downAll           bool
	downForce         bool
	downDryRun        bool
	downInclUnmanaged bool
)

// downAllRun tears down every cluster rooket can see: kind clusters live under
// any installed engine (rooket-created or not) plus every state directory,
// including orphans left by deleted clones. Without --delete-disks it preserves
// disk images and iSCSI targets like a plain down; with it, every cluster's
// target teardown is batched into one privileged run so the whole sweep costs
// at most a single prompt (or none, with rooket's sudoers rule installed).
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
	owns := func(name string, engs []engine.Engine) bool {
		for _, eng := range engs {
			if registry.Exists(os.Stdout, eng, registry.ContainerName(name)) {
				return true
			}
		}
		return false
	}
	all, unmanaged := scopeTeardownSet(live, stateNames, downInclUnmanaged, owns)
	if len(unmanaged) > 0 {
		fmt.Fprintf(os.Stderr,
			"skipping %d unmanaged kind cluster(s) with no rooket state or registry: %s\n"+
				"  (pass --include-unmanaged to tear these down too)\n",
			len(unmanaged), strings.Join(unmanaged, ", "))
	}
	if len(all) == 0 {
		run.Printf("nothing to tear down\n")
		return nil
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)

	run.Printf("The following clusters will be torn down:\n")
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
		run.Printf("Full teardown: iSCSI targets, disk images, and state directories will be removed.\n")
	} else {
		run.Printf("Disk images and iSCSI targets will be preserved (pass --delete-disks to remove them).\n")
	}
	if downDryRun {
		return nil
	}
	if !downForce {
		run.Printf("Proceed? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			run.Printf("aborted\n")
			return nil
		}
	}

	// blocked marks clusters that survived a failed delete: their disks may still
	// be in use, so nothing downstream may zap, teardown, or remove their state.
	blocked := map[string]bool{}
	for _, n := range names {
		engs := live[n]
		if len(engs) == 0 {
			continue
		}
		run.Printf("==> deleting cluster %q\n", n)
		kc, _ := kubeconfigPath(n)
		for _, eng := range engs {
			if err := cluster.DeleteWith(eng, n, kc); err != nil {
				run.Printf("warning: delete cluster %q under %s: %v\n", n, eng, err)
			}
			if err := registry.Delete(os.Stdout, eng, registry.ContainerName(n)); err != nil {
				run.Printf("warning: delete registry for %q under %s: %v\n", n, eng, err)
			}
		}
		// Confirm the cluster is actually gone before anything truncates or
		// removes its disks; a survivor still holding them must be left intact.
		if stillLive(engs, n) {
			blocked[n] = true
			fmt.Fprintf(os.Stderr, "warning: cluster %q is still present after delete; leaving its disks and state alone\n", n)
			continue
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
			if hasState[n] && !blocked[n] {
				disks = append(disks, stateDirDisks(n, filepath.Join(root, n), downIQNDate)...)
			}
		}
		if len(disks) > 0 {
			run.Printf("==> tearing down iSCSI targets (all clusters in one privileged run)\n")
			steps := buildISCSITeardownSteps(disks)
			if err := runPrivileged(steps); err != nil {
				return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
			}
		}
		for _, n := range names {
			if !hasState[n] || blocked[n] {
				continue
			}
			dir := filepath.Join(root, n)
			if err := os.RemoveAll(dir); err != nil {
				run.Printf("warning: remove state dir %s: %v\n", dir, err)
			} else {
				run.Printf("removed state dir %s\n", dir)
			}
		}
	} else if downDeleteDisks {
		run.Printf("block teardown skipped by --skip-block; disk images and state dirs preserved\n")
	}

	if len(blocked) > 0 {
		names := make([]string, 0, len(blocked))
		for n := range blocked {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("could not delete %d cluster(s), left intact: %s", len(blocked), strings.Join(names, ", "))
	}

	run.Printf("\nrooket down --all complete.\n")
	return nil
}

// stillLive reports whether a kind cluster is still present under any of the
// given engines — used after a delete attempt to decide whether its disks are
// safe to zap.
func stillLive(engs []engine.Engine, name string) bool {
	for _, eng := range engs {
		if ok, err := cluster.Exists(os.Stdout, eng, name); err == nil && ok {
			return true
		}
	}
	return false
}

func init() {
	downCmd.Flags().BoolVar(&downAll, "all", false, "tear down every rooket cluster: rooket-owned clusters live under any engine, plus all state dirs")
	downCmd.Flags().BoolVar(&downForce, "force", false, "with --all: skip the confirmation prompt")
	downCmd.Flags().BoolVar(&downDryRun, "dry-run", false, "with --all: list what would be torn down, then exit")
	downCmd.Flags().BoolVar(&downInclUnmanaged, "include-unmanaged", false, "with --all: also tear down live kind clusters that have no rooket state or registry")
}
