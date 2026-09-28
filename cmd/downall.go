package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
			liveCol = engineNames(engs)
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
	if downDeleteCache {
		run.Printf("The host-wide OCI image cache will be removed.\n")
	} else {
		run.Printf("The host-wide OCI image cache will be preserved (pass --delete-cache to remove it).\n")
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

	// Every cluster the sweep will touch is locked before anything of any of
	// them is removed, and stays locked until the sweep is done with it (see
	// lockSweep). A cluster with no kind cluster may be an up that has not
	// created it yet, and one released after its delete could be taken by an
	// up before the batched teardown below reached its targets. A cluster
	// someone else is working on is skipped rather than failing the sweep — the
	// point of --all is to clear whatever it can.
	tearDownDisks := downDeleteDisks && !downSkipBlock
	var toLock []string
	for _, n := range names {
		// A cluster with only a state dir is touched only by the disk teardown,
		// and one whose name LockCluster refuses has no lock anyone could hold,
		// so it goes unlocked.
		if len(live[n]) == 0 && (!tearDownDisks || validateClusterName(n) != nil) {
			continue
		}
		toLock = append(toLock, n)
	}
	// blocked marks clusters that another rooket holds, or that survived a
	// failed delete or no listing could show gone after it: their disks may
	// still be in use, so nothing downstream may zap, teardown, or remove their
	// state.
	locks, blocked := lockSweep(os.Stdout, root, toLock)
	defer locks.releaseAll()

	// The scan's view of a cluster may be stale by the time the sweep holds it:
	// a down and an up that ran to completion in between leave nothing in the
	// lock to show for it. The cluster may have gained a kind cluster since, or
	// been brought back up under the other engine, where a delete and a
	// confirm-gone asked of the scan's engines would find nothing; either way
	// the zap or the batched teardown would then reach disks its nodes use. So
	// every held cluster is asked again, with the scan's own probe, and is
	// deleted, confirmed gone, and zapped by that answer. Held, it cannot come
	// up or move after it.
	var held []string
	for _, n := range toLock {
		if !blocked[n] {
			held = append(held, n)
		}
	}
	if len(held) > 0 {
		now, consulted, failed := liveClusters()
		if len(consulted) == 0 || len(failed) > 0 {
			unqueried := "no container engine"
			if len(failed) > 0 {
				unqueried = engineNames(failed)
			}
			return fmt.Errorf("cannot check again which clusters are live (%s could not be queried); nothing was torn down", unqueried)
		}
		for _, n := range held {
			was, is := live[n], now[n]
			switch {
			case len(was) == 0 && len(is) > 0:
				run.Printf("cluster %q came up since the sweep looked; deleting it as a live cluster\n", n)
			case len(is) == 0 && len(was) > 0:
				run.Printf("cluster %q went down since the sweep looked\n", n)
			case !slices.Equal(was, is):
				run.Printf("cluster %q is now live under %s, not %s; deleting it there\n", n, engineNames(is), engineNames(was))
			}
			live[n] = is
		}
	}

	// The clusters share no kind cluster, registry, or disk, so they are deleted
	// concurrently — N deletes cost roughly one delete's wallclock, not N — with
	// each cluster's blocked-ness recorded into its own slot and merged after the
	// join. The batched iSCSI teardown below is the barrier that needs every
	// cluster gone first. (Per-cluster this preserves the sequential path's
	// best-effort delete-then-remove-registry behavior; unlike single-cluster
	// delete, --all already removed each registry regardless of delete success.)
	blockedByIdx := make([]bool, len(names))
	delFns := make([]func(io.Writer) error, len(names))
	for i, n := range names {
		delFns[i] = func(w io.Writer) error {
			engs := live[n]
			if len(engs) == 0 || blocked[n] {
				return nil
			}
			run.Fprintf(w, "==> deleting cluster %q\n", n)
			kc, _ := kubeconfigPath(n)
			for _, eng := range engs {
				if err := cluster.Delete(w, eng, n, kc); err != nil {
					run.Fprintf(w, "warning: delete cluster %q under %s: %v\n", n, eng, err)
				}
				if err := registry.Delete(w, eng, registry.ContainerName(n)); err != nil {
					run.Fprintf(w, "warning: delete registry for %q under %s: %v\n", n, eng, err)
				}
			}
			// Confirm the cluster is actually gone before anything truncates or
			// removes its disks; a survivor still holding them must be left
			// intact, and so must one no listing could show gone.
			switch live, err := stillLive(engs, n); {
			case err != nil:
				blockedByIdx[i] = true
				run.Fprintf(w, "warning: could not tell whether cluster %q is gone (%v); leaving its disks and state alone\n", n, err)
				return nil
			case live:
				blockedByIdx[i] = true
				run.Fprintf(w, "warning: cluster %q is still present after delete; leaving its disks and state alone\n", n)
				return nil
			}
			if kc != "" {
				_ = os.Remove(kc)
			}
			// Preserved images must still be zapped so the next up starts clean;
			// images about to be deleted don't need it.
			// A sweep reports and keeps going: the next 'up' on that clone hits
			// the same failure with the cluster in front of it.
			if !downDeleteDisks && hasState[n] {
				if err := cluster.ZapISCSIDisks(w, engs[0], n, filepath.Join(root, n)); err != nil {
					run.Fprintf(w, "warning: zap OSD disks of cluster %q: %v\n", n, err)
				}
			}
			return nil
		}
	}
	if err := runConcurrent(os.Stdout, delFns...); err != nil {
		return err
	}
	for i, n := range names {
		if blockedByIdx[i] {
			blocked[n] = true
		}
	}

	if tearDownDisks {
		var disks []iscsiDisk
		for _, n := range names {
			// --all rejects --workers/--disk-count, so there are no per-cluster
			// counts to name a grid from: each cluster's disks come from its
			// state dir and from what the kernel still holds for it.
			if !blocked[n] {
				disks = append(disks, teardownDisks(hostLIORoot(), n, filepath.Join(root, n), downIQNDate, 0, 0)...)
			}
		}
		if len(disks) > 0 {
			run.Printf("==> tearing down iSCSI targets (all clusters in one privileged run)\n")
			steps := buildISCSITeardownSteps(disks)
			if err := runPrivileged(os.Stdout, steps); err != nil {
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
	var tornDown []string
	for _, n := range names {
		if !blocked[n] {
			tornDown = append(tornDown, n)
		}
	}
	removeStatelessLockFiles(root, tornDown)
	locks.releaseAll()

	// Safe to run even with clusters left behind: the cache is a soft
	// dependency, so a node that outlives it falls back to pulling upstream.
	if downDeleteCache {
		run.Printf("==> removing the shared image cache\n")
		if err := teardownCache(os.Stdout); err != nil {
			run.Printf("warning: remove image cache: %v\n", err)
		}
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

// engineNames renders engines as a comma-separated list, as down --all's table
// shows where a cluster is live.
func engineNames(engs []engine.Engine) string {
	ss := make([]string, len(engs))
	for i, e := range engs {
		ss[i] = e.String()
	}
	return strings.Join(ss, ",")
}

// stillLive reports whether a kind cluster is still present under any of the
// given engines — used after a delete attempt to decide whether its disks are
// safe to zap. A listing that fails shows nothing either way, so unless another
// engine lists the cluster it is an error, naming the engine, and never a "no":
// the cluster may still be running on the disks.
func stillLive(engs []engine.Engine, name string) (bool, error) {
	var err error
	for _, eng := range engs {
		ok, listErr := cluster.Exists(os.Stdout, eng, name)
		if listErr != nil {
			listErr = fmt.Errorf("kind get clusters under %s: %w", eng, listErr)
			if err == nil {
				err = listErr
			} else {
				err = fmt.Errorf("%w; %w", err, listErr)
			}
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, err
}

func init() {
	downCmd.Flags().BoolVar(&downAll, "all", false, "tear down every rooket cluster: rooket-owned clusters live under any engine, plus all state dirs")
	downCmd.Flags().BoolVar(&downForce, "force", false, "with --all: skip the confirmation prompt")
	downCmd.Flags().BoolVar(&downDryRun, "dry-run", false, "with --all: list what would be torn down, then exit")
	downCmd.Flags().BoolVar(&downInclUnmanaged, "include-unmanaged", false, "with --all: also tear down live kind clusters that have no rooket state or registry")
}
