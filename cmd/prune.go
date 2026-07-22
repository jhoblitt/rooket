package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/spf13/cobra"
)

var (
	pruneForce   bool
	pruneDryRun  bool
	pruneIQNDate string
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove state directories of clusters that no longer exist, and their iSCSI targets",
	Long: `prune deletes ~/.local/share/rooket/<name> directories whose kind cluster is
no longer running — e.g. a clone removed without 'rooket down' — and removes
their iSCSI targets first, while the state directory's worker*-disk*.img
filenames can still be used to reconstruct them. All targets are torn down in
one privileged run, so the whole prune costs at most a single authentication.

prune also sweeps iSCSI targets left behind by an earlier deletion of their
state directory, found via the world-readable /dev/disk/by-path symlinks
iscsiadm creates for each logged-in session. A target with no active session
has no such symlink and will not be found this way; see 'targetcli ls' to
check by hand. The same by-path scan also backstops an orphan whose state
directory has lost its worker*-disk*.img files, or was built with a
different --iqn-date than this run's: its targets are torn down from
whichever source names them.

This assumes rooket is the only user on this host driving iSCSI targets:
/dev/disk/by-path is host-global, not per-user, so on a host where two users
each run rooket against their own per-user container engine, one user's
prune would see the other's targets too. rooket's usual rootful podman/docker
setup makes every cluster visible to any querying user regardless, so this
does not add a new restriction there.

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

		strandedFound, err := discoverStrandedByPath(iscsiByPathDir)
		if err != nil {
			return fmt.Errorf("scan %s: %w", iscsiByPathDir, err)
		}

		orphans, byPathDisks := prunePlan(stateNames, live, hasState, strandedFound)
		stranded := strandableClusters(strandedFound, live, hasState)

		if len(orphans) == 0 && len(stranded) == 0 {
			run.Printf("nothing to prune\n")
			return nil
		}

		// Reconstructed up front, before the prompt, so it can report an
		// accurate total — and while each orphan's worker*-disk*.img filenames
		// still exist to name it; nothing can reconstruct them once the state
		// dir is gone.
		var disks []iscsiDisk
		for _, o := range orphans {
			disks = append(disks, stateDirDisks(o, filepath.Join(root, o), pruneIQNDate)...)
		}
		disks = append(disks, byPathDisks...)

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
		if len(disks) > 0 {
			run.Printf("The iSCSI targets listed above will be removed too, in one privileged run.\n")
		}

		if pruneDryRun {
			return nil
		}
		if !pruneForce {
			run.Printf("Remove %d state director(y/ies) and %d iSCSI target(s)? [y/N] ",
				len(orphans), len(disks))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				run.Printf("aborted\n")
				return nil
			}
		}

		return pruneExecute(root, orphans, disks, teardownISCSI, os.RemoveAll, os.Stdout)
	},
}

// teardownISCSI runs disks' privileged teardown, wrapping a failure with the
// manual-recovery script — the same shape prune, down --all, and block
// teardown all render on a privileged-run failure.
func teardownISCSI(disks []iscsiDisk) error {
	steps := buildISCSITeardownSteps(disks)
	if err := runPrivileged(steps); err != nil {
		return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
	}
	return nil
}

// pruneExecute performs the two side-effecting steps of a prune run: tear
// down disks' iSCSI targets (if any), then, only once that succeeds, remove
// each orphan's state directory. teardown and remove are injected so this can
// be tested against fakes instead of real privilege escalation or disk I/O.
//
// Preserving order here — and returning before any remove call when teardown
// fails — is the whole point: an orphan's state directory is prune's only
// remaining record of its targets, so it must never be deleted ahead of, or
// despite a failure of, the teardown that names them.
func pruneExecute(root string, orphans []string, disks []iscsiDisk, teardown func([]iscsiDisk) error, remove func(string) error, out io.Writer) error {
	if len(disks) > 0 {
		fmt.Fprintf(out, "==> tearing down iSCSI targets (all clusters in one privileged run)\n")
		if err := teardown(disks); err != nil {
			return err
		}
	}
	for _, o := range orphans {
		p := filepath.Join(root, o)
		if err := remove(p); err != nil {
			fmt.Fprintf(out, "warning: remove %s: %v\n", p, err)
		} else {
			fmt.Fprintf(out, "removed %s\n", p)
		}
	}
	return nil
}

// prunePlan decides which state-dir clusters are orphaned (no live kind
// cluster) and which by-path-discovered disks the run's privileged teardown
// batch must include for them, in addition to whatever the caller
// reconstructs from each orphan's state dir via stateDirDisks.
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
func prunePlan(stateNames []string, live map[string][]engine.Engine, hasState map[string]bool, strandedFound map[string][]iscsiDisk) (orphans []string, disks []iscsiDisk) {
	for _, n := range stateNames {
		if _, ok := live[n]; ok {
			continue
		}
		orphans = append(orphans, n)
		disks = append(disks, strandedFound[n]...)
	}
	for _, c := range strandableClusters(strandedFound, live, hasState) {
		disks = append(disks, strandedFound[c]...)
	}
	return orphans, disks
}

// strandedByPathRE matches a rooket iSCSI by-path symlink for LUN 0 and
// captures the target IQN (group 1) and the "<cluster>-worker<N>-disk<M>"
// name after the "local.rooket:" prefix (group 2) — the same string used as
// the backstore name.
var strandedByPathRE = regexp.MustCompile(
	"^" + regexp.QuoteMeta(iscsiByPathPrefix) +
		`(iqn\.[0-9]{4}-[0-9]{2}\.local\.rooket:(.+))` +
		regexp.QuoteMeta(iscsiByPathSuffix) + "$")

// strandedBackstoreRE splits a "<cluster>-worker<N>-disk<M>" backstore name
// into the cluster name, anchored on the fixed "-worker<N>-disk<M>" suffix so
// a cluster name that itself contains dashes (e.g.
// "home-jhoblitt-github-rook") is not mis-split.
var strandedBackstoreRE = regexp.MustCompile(`^(.+)-worker[0-9]+-disk[0-9]+$`)

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
	targetIQN, backstoreName := m[1], m[2]

	cm := strandedBackstoreRE.FindStringSubmatch(backstoreName)
	if cm == nil {
		return iscsiDisk{}, "", false
	}
	cluster = cm[1]
	if err := validateClusterName(cluster); err != nil {
		return iscsiDisk{}, "", false
	}
	return iscsiDisk{backstoreName: backstoreName, targetIQN: targetIQN}, cluster, true
}

// discoverStrandedByPath scans dir (normally /dev/disk/by-path) for rooket
// iSCSI LUN-0 symlinks and groups their teardown-ready disk identities by
// cluster name. This needs no privileges: the symlinks are world-readable
// (see iscsiByPathLink / resolveDeviceLink). Its blind spot: a target
// configured in LIO but with no active iscsiadm session has no by-path
// symlink, so it will not be found this way. A missing directory (no iSCSI
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
	pruneCmd.Flags().StringVar(&pruneIQNDate, "iqn-date", "2003-01", "date component for reconstructing an orphan's IQNs (YYYY-MM)")
}
