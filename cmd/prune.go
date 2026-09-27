package cmd

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/lio"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/spf13/cobra"
)

var (
	pruneForce      bool
	pruneDryRun     bool
	pruneIQNDate    string
	pruneInclParked bool
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove state directories of clusters that no longer exist, and their iSCSI targets",
	Long: `prune deletes ~/.local/share/rooket/<name> directories left behind once
every owner that created them is gone — removed without 'rooket down' — and
removes their iSCSI targets first, while the state directory's
worker*-disk*.img filenames can still be used to reconstruct them. All
targets are torn down in one privileged run, so the whole prune costs at most
a single authentication.

A cluster whose owner still exists is parked, not abandoned: a plain
'rooket down' keeps its disk images and iSCSI targets on purpose so the next
'up' reuses them without root. A cluster built from a clone is owned by that
clone alone; one deploying a released Rook is instead owned by its recorded
configuration directory and by any clone it happens to have been created in,
and is abandoned only once every recorded owner is gone. prune reports every
parked cluster and leaves it alone; 'rooket down --delete-disks' is how you
reclaim one, or --include-parked sweeps them here too. A clone-built
directory that records no clone at all — created before rooket recorded one
— counts as abandoned; a released cluster recording no owner at all is
instead always kept, since nothing on disk can ever show it was abandoned,
until 'rooket down --delete-disks' or --include-parked removes it.

prune also sweeps iSCSI targets and backstores left behind by an earlier
deletion of their state directory, read straight from the kernel's own
configuration (the world-readable configfs tree the target subsystem
exports), so it finds targets with no active session and backstores whose
target is already gone. That view also backstops an orphan whose state
directory has lost its worker*-disk*.img files, or was built with a
different --iqn-date than this run's: its targets are torn down from
whichever source names them.

This assumes rooket is the only user on this host driving iSCSI targets: the
kernel's target configuration is host-global, not per-user, so on a host
where two users each run rooket against their own per-user container engine,
one user's prune would see the other's targets too. rooket's usual rootful
podman/docker setup makes every cluster visible to any querying user
regardless, so this does not add a new restriction there.

  rooket prune --dry-run   # list what would be removed
  rooket prune             # prompt, then remove
  rooket prune --force     # remove without prompting
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateIQNDate(pruneIQNDate); err != nil {
			return err
		}

		root, stateNames, err := stateDirNames()
		if err != nil {
			return err
		}
		hasState := map[string]bool{}
		for _, n := range stateNames {
			hasState[n] = true
		}

		// Query every installed engine, not just the session's resolved one: a
		// cluster living under the other engine must not be pruned as orphaned.
		live, consulted, failed := liveClusters()
		if len(consulted) == 0 {
			return fmt.Errorf("cannot determine live clusters (no queryable container engine); not pruning")
		}
		for _, eng := range failed {
			run.Printf("warning: %s is installed but could not be queried; "+
				"its clusters (if any) would be misread as orphaned — not pruning\n", eng)
		}
		if len(failed) > 0 {
			return fmt.Errorf("refusing to prune with an unqueryable engine present")
		}

		strandedFound, err := discoverStranded(hostLIORoot(), iscsiByPathDir, pruneIQNDate)
		if err != nil {
			return fmt.Errorf("scan %s: %w", iscsiByPathDir, err)
		}

		orphans, parked, disks := prunePlan(root, stateNames, live, hasState, strandedFound, pruneInclParked)
		stranded := strandableClusters(strandedFound, live, hasState)

		for _, p := range parked {
			run.Printf("keeping %s: %s, so it is parked by 'rooket down', not abandoned "+
				"(remove it with 'rooket down --delete-disks', or sweep it here with --include-parked)\n",
				filepath.Join(root, p), parkedBecause(filepath.Join(root, p)))
		}

		if len(orphans) == 0 && len(stranded) == 0 {
			run.Printf("nothing to prune\n")
			return nil
		}

		// Reconstructed up front, before the prompt, so it can report an
		// accurate total — and while each orphan's worker*-disk*.img filenames
		// still exist to name it; nothing can reconstruct them once the state
		// dir is gone.
		for _, o := range orphans {
			disks[o] = append(stateDirDisks(o, filepath.Join(root, o), pruneIQNDate), disks[o]...)
		}
		nDisks := 0
		for _, d := range disks {
			nDisks += len(d)
		}

		engNames := make([]string, len(consulted))
		for i, eng := range consulted {
			engNames[i] = eng.String()
		}

		if len(orphans) > 0 {
			run.Printf("Orphaned cluster state directories (no live kind cluster under %s):\n",
				strings.Join(engNames, " or "))
			for _, o := range orphans {
				run.Printf("  %s\n", filepath.Join(root, o))
			}
		}
		if len(stranded) > 0 {
			run.Printf("Stranded iSCSI targets with no state directory (no live kind cluster under %s):\n",
				strings.Join(engNames, " or "))
			for _, c := range stranded {
				for _, d := range strandedFound[c] {
					run.Printf("  %s\n", d.targetIQN)
				}
			}
		}
		if nDisks > 0 {
			run.Printf("The iSCSI targets listed above will be removed too, in one privileged run.\n")
		}

		if pruneDryRun {
			return nil
		}
		if !pruneForce {
			run.Printf("Remove %d state director(y/ies) and %d iSCSI target(s)? [y/N] ",
				len(orphans), nDisks)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				run.Printf("aborted\n")
				return nil
			}
		}

		isOrphan := map[string]bool{}
		for _, o := range orphans {
			isOrphan[o] = true
		}
		recheck := func(planned []string) (map[string]string, error) {
			return pruneRecheck(root, isOrphan, pruneInclParked, planned)
		}
		return pruneExecute(root, orphans, disks, recheck, teardownISCSI, os.RemoveAll, os.Stdout)
	},
}

// pruneRecheck asks again, of each cluster prune is about to touch, what the
// scan asked to choose it, with the scan's own probes, and returns why each
// that no longer passes must be left alone. An orphan must still have no live
// kind cluster and, unless parked clusters are in scope, no owner; a stranded
// cluster must still have no live kind cluster and no state dir. Like the
// scan, it will not answer without every installed engine.
func pruneRecheck(root string, orphans map[string]bool, includeParked bool, planned []string) (map[string]string, error) {
	live, consulted, failed := liveClusters()
	if len(consulted) == 0 || len(failed) > 0 {
		return nil, fmt.Errorf("cannot check again which clusters are live (a container engine could not be queried); nothing was pruned")
	}
	changed := map[string]string{}
	for _, n := range planned {
		dir := filepath.Join(root, n)
		if _, ok := live[n]; ok {
			changed[n] = "its kind cluster came up after prune looked"
			continue
		}
		if orphans[n] {
			if !includeParked && !ownerGone(dir) {
				changed[n] = "it is parked now: " + parkedBecause(dir)
			}
			continue
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			changed[n] = "it has a state directory now"
		}
	}
	return changed, nil
}

// teardownISCSI runs disks' privileged teardown, wrapping a failure with the
// manual-recovery script — the same shape prune, down --all, and block
// teardown all render on a privileged-run failure.
func teardownISCSI(disks []iscsiDisk) error {
	steps := buildISCSITeardownSteps(disks)
	if err := runPrivileged(os.Stdout, steps); err != nil {
		return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
	}
	return nil
}

// pruneExecute performs the two side-effecting steps of a prune run: tear
// down the iSCSI targets of disks, each orphan's and each stranded cluster's
// filed under its cluster, in one batch, then, only once that succeeds, remove
// each orphan's state directory. teardown and remove are injected so this can
// be tested against fakes instead of real privilege escalation or disk I/O.
//
// Preserving order here — and returning before any remove call when teardown
// fails — is the whole point: an orphan's state directory is prune's only
// remaining record of its targets, so it must never be deleted ahead of, or
// despite a failure of, the teardown that names them.
//
// Both steps run under the lock of every cluster they touch, taken before the
// first and held through the second (see lockSweep). The plan came from a
// scan that has since gone stale, and a concurrent 'up' may be building one of
// these very clusters: iSCSI targets but no kind cluster yet is just what an
// orphan or a stranded cluster looks like. Its lock is the only sign of that,
// so a cluster prune cannot lock keeps its targets as well as its state dir,
// and is reported; prune still succeeds, as it always has for one it skips. A
// state dir whose name cannot be a cluster's has no lock anyone could hold,
// and is pruned unlocked.
//
// A lock shows only a rooket still at work: an up that finished between the
// scan and the lock left a live cluster and nothing to show for it. So, with
// the locks held, recheck asks each cluster again what the scan asked, and one
// that no longer passes is let go at once, reported, and left alone. The
// answer cannot go stale again, since whatever could bring a held cluster up
// needs its lock. A re-check that cannot be answered stops prune before it
// touches anything.
func pruneExecute(root string, orphans []string, disks map[string][]iscsiDisk, recheck func(planned []string) (map[string]string, error), teardown func([]iscsiDisk) error, remove func(string) error, out io.Writer) error {
	touched := map[string]bool{}
	for n := range disks {
		touched[n] = true
	}
	for _, o := range orphans {
		touched[o] = true
	}
	names := slices.Sorted(maps.Keys(touched))
	var toLock []string
	for _, n := range names {
		if validateClusterName(n) == nil {
			toLock = append(toLock, n)
		}
	}
	locks, skipped := lockSweep(out, root, toLock)
	defer locks.releaseAll()

	var planned []string
	for _, n := range names {
		if !skipped[n] {
			planned = append(planned, n)
		}
	}
	changed, err := recheck(planned)
	if err != nil {
		return err
	}
	for _, n := range planned {
		if why, ok := changed[n]; ok {
			fmt.Fprintf(out, "skipping cluster %q: %s\n", n, why)
			locks.release(n)
			skipped[n] = true
		}
	}

	var batch []iscsiDisk
	for _, n := range names {
		if !skipped[n] {
			batch = append(batch, disks[n]...)
		}
	}
	if len(batch) > 0 {
		fmt.Fprintf(out, "==> tearing down iSCSI targets (all clusters in one privileged run)\n")
		if err := teardown(batch); err != nil {
			return err
		}
	}
	for _, o := range orphans {
		if skipped[o] {
			continue
		}
		p := filepath.Join(root, o)
		if err := remove(p); err != nil {
			fmt.Fprintf(out, "warning: remove %s: %v\n", p, err)
		} else {
			fmt.Fprintf(out, "removed %s\n", p)
		}
	}
	var tornDown []string
	for _, n := range names {
		if !skipped[n] {
			tornDown = append(tornDown, n)
		}
	}
	removeStatelessLockFiles(root, tornDown)
	return nil
}

// prunePlan decides which state-dir clusters are orphaned and which
// by-path-discovered disks the run's privileged teardown batch must include
// for them, in addition to whatever the caller reconstructs from each
// orphan's state dir via stateDirDisks. The disks are filed by cluster, since
// prune tears a cluster's disks down only while it holds that cluster's lock.
// The clusters it declines to orphan because they are merely parked are
// returned separately, so the caller can say why they survived.
//
// Orphaned means more than "not live": a plain 'rooket down' leaves a state
// dir with no live kind cluster on purpose, its disk images and iSCSI targets
// preserved so the next 'up' reuses them without root. Sweeping that would
// destroy the very thing down set out to keep, so a cluster whose owner —
// its rook clone, or for a released cluster its configuration directory —
// still exists is parked and left alone unless includeParked says otherwise;
// see ownerGone.
//
// The by-path union matters because reconstruction alone can miss real
// targets: a state dir whose worker*-disk*.img files were already removed
// (by an earlier partial teardown, or by hand) globs to nothing, and a
// cluster built with a --iqn-date other than this run's reconstructs IQNs
// that match nothing either. In both cases the by-path symlink still carries
// the correct IQN, so it is unioned in rather than discarded. Duplicates
// across the two sources are harmless: buildISCSITeardownSteps is
// idempotent and best-effort.
//
// A cluster with neither a state dir nor a live entry is handled separately,
// by strandableClusters — this function only adds by-path disks for
// clusters that already have a state dir (are in stateNames); it does not
// itself decide the no-state-dir "stranded" bucket. A live cluster's by-path
// entries are never included here or there.
func prunePlan(root string, stateNames []string, live map[string][]engine.Engine, hasState map[string]bool, strandedFound map[string][]iscsiDisk, includeParked bool) (orphans, parked []string, disks map[string][]iscsiDisk) {
	disks = map[string][]iscsiDisk{}
	for _, n := range stateNames {
		if _, ok := live[n]; ok {
			continue
		}
		if !includeParked && !ownerGone(filepath.Join(root, n)) {
			parked = append(parked, n)
			continue
		}
		orphans = append(orphans, n)
		if found := strandedFound[n]; len(found) > 0 {
			disks[n] = found
		}
	}
	for _, c := range strandableClusters(strandedFound, live, hasState) {
		disks[c] = strandedFound[c]
	}
	return orphans, parked, disks
}

// strandedByPathRE matches a rooket iSCSI by-path symlink for LUN 0 and
// captures the target IQN.
var strandedByPathRE = regexp.MustCompile(
	"^" + regexp.QuoteMeta(iscsiByPathPrefix) + `(iqn\..+)` + regexp.QuoteMeta(iscsiByPathSuffix) + "$")

// parseStrandedByPathLink parses one /dev/disk/by-path entry name (not a full
// path) as a rooket iSCSI target, returning the disk's teardown identity and
// the cluster it belongs to. ok is false for anything else: a non-iSCSI
// by-path entry, a non-rooket iSCSI target, or a rooket-shaped IQN that does
// not resolve to a valid cluster name — none of which are safe to treat as a
// rooket cluster.
func parseStrandedByPathLink(name string) (disk iscsiDisk, cluster string, ok bool) {
	m := strandedByPathRE.FindStringSubmatch(name)
	if m == nil {
		return iscsiDisk{}, "", false
	}
	return parseRooketIQN(m[1])
}

// discoverStranded groups every rooket iSCSI disk still configured on the
// host by cluster name, from the kernel's own configuration unioned with the
// /dev/disk/by-path symlinks. Neither source needs privileges.
//
// The kernel's view is the complete one — it holds targets with no session
// and backstores with no target, which is exactly what a partial teardown
// leaves behind and what the by-path scan alone can never see. by-path is
// kept as a fallback for a host whose configfs cannot be read at all.
func discoverStranded(lioRoot, byPathDir, iqnDate string) (map[string][]iscsiDisk, error) {
	fromLIO := map[string][]iscsiDisk{}
	if st, err := lio.Read(lioRoot); err != nil {
		run.Printf("warning: could not read the host's iSCSI configuration (%v); "+
			"falling back to the /dev/disk/by-path scan, which cannot see targets with no active session\n", err)
	} else {
		fromLIO = lioClusterDisks(st, iqnDate)
	}
	byPath, err := discoverStrandedByPath(byPathDir)
	if err != nil {
		return nil, err
	}
	for cluster, disks := range byPath {
		fromLIO[cluster] = unionDisks(fromLIO[cluster], disks)
	}
	return fromLIO, nil
}

// discoverStrandedByPath scans dir (normally /dev/disk/by-path) for rooket
// iSCSI LUN-0 symlinks and groups their teardown-ready disk identities by
// cluster name. This needs no privileges: the symlinks are world-readable
// (see iscsiByPathLink / resolveDeviceLink). A missing directory (no iSCSI
// devices ever attached) is not an error.
func discoverStrandedByPath(dir string) (map[string][]iscsiDisk, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	found := map[string][]iscsiDisk{}
	for _, e := range entries {
		disk, cluster, ok := parseStrandedByPathLink(e.Name())
		if !ok {
			continue
		}
		found[cluster] = append(found[cluster], disk)
	}
	return found, nil
}

// strandableClusters returns the cluster names in found that are strandable:
// discovered via by-path but with neither a live kind cluster nor a state
// directory. Either one means the cluster is already handled elsewhere (as
// live, or as an orphan whose state dir drives its own teardown), so it must
// not be swept here too. The result is sorted for stable output.
func strandableClusters(found map[string][]iscsiDisk, live map[string][]engine.Engine, hasState map[string]bool) []string {
	var out []string
	for c := range found {
		if hasState[c] {
			continue
		}
		if _, ok := live[c]; ok {
			continue
		}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func init() {
	rootCmd.AddCommand(pruneCmd)
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "list what would be removed without removing it")
	pruneCmd.Flags().BoolVar(&pruneForce, "force", false, "remove without prompting")
	pruneCmd.Flags().BoolVar(&pruneInclParked, "include-parked", false,
		"also remove clusters whose owner still exists (parked by 'rooket down', not abandoned)")
	pruneCmd.Flags().StringVar(&pruneIQNDate, "iqn-date", "2003-01", "date component for reconstructing an orphan's IQNs (YYYY-MM)")
}
