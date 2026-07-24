package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/lio"
	"github.com/jhoblitt/rooket/internal/run"
)

var (
	blockSetupName       string
	blockSetupWorkers    int
	blockSetupDiskCount  int
	blockSetupDiskSizeGB int
	blockSetupDataDir    string
	blockSetupIQNDate    string
)

var (
	blockTeardownName        string
	blockTeardownWorkers     int
	blockTeardownDiskCount   int
	blockTeardownDataDir     string
	blockTeardownIQNDate     string
	blockTeardownDeleteDisks bool
)

type iscsiDisk struct {
	workerIdx     int
	diskIdx       int
	imgPath       string
	backstoreName string
	targetIQN     string
}

var blockCmd = &cobra.Command{
	Use:   "block",
	Short: "Manage iSCSI block devices for Rook testing",
}

var blockSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create disk images and configure iSCSI targets on the host",
	Long: `setup creates sparse disk image files and exposes each one as an iSCSI
target via targetcli and iscsiadm. The resulting /dev/sdX block devices are
bind-mounted into kind worker nodes as Rook OSD devices.

Privilege requirements: targetcli, iscsiadm, and systemctl require root.
rooket runs itemized through sudo -n when a passwordless grant is available
(rooket's own rule, installed via 'rooket sudoers install', or any other
passwordless sudo), otherwise falls back to a single pkexec prompt.
`,
	RunE: blockSetupRun,
}

func blockSetupRun(_ *cobra.Command, _ []string) error {
	blockSetupName = clusterName(blockSetupName)
	if err := validateIQNDate(blockSetupIQNDate); err != nil {
		return err
	}
	dataDir, err := blockDataDir(blockSetupName, blockSetupDataDir)
	if err != nil {
		return err
	}
	return blockSetupRunTo(os.Stdout, blockSetupName, dataDir, blockSetupIQNDate,
		blockSetupWorkers, blockSetupDiskCount, blockSetupDiskSizeGB)
}

// blockDataDir resolves the disk-image directory — the cluster's state dir by
// default — and ensures it exists.
func blockDataDir(name, override string) (string, error) {
	dataDir := override
	if dataDir == "" {
		var err error
		dataDir, err = stateDirPath(name)
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	return dataDir, nil
}

// iscsiDiskList builds the iSCSI disk descriptors for a cluster's workers and
// per-worker disks. Shared by block setup and by the up command's pre-flight
// overlap check so both name the same images, backstores, and target IQNs.
func iscsiDiskList(name, dataDir, iqnDate string, workers, diskCount int) []iscsiDisk {
	var disks []iscsiDisk
	for w := 0; w < workers; w++ {
		for d := 0; d < diskCount; d++ {
			id := fmt.Sprintf("worker%d-disk%d", w, d)
			disks = append(disks, iscsiDisk{
				workerIdx:     w,
				diskIdx:       d,
				imgPath:       filepath.Join(dataDir, id+".img"),
				backstoreName: fmt.Sprintf("%s-%s", name, id),
				targetIQN:     fmt.Sprintf("iqn.%s.local.rooket:%s-%s", iqnDate, name, id),
			})
		}
	}
	return disks
}

// blockSetupPromptFree reports whether block setup for these disks will finish
// without a pkexec prompt — either because every device is already attached (so
// the privileged step is skipped entirely) or because a root or passwordless-
// sudo path is available. Only then is it safe to overlap block setup with a
// make that owns the terminal: otherwise the pkexec prompt would compete with
// make's stream for the terminal, so the caller keeps block setup serial and in
// front. The probes are the same ones runPrivileged itself branches on.
func blockSetupPromptFree(disks []iscsiDisk) bool {
	return allISCSIDevicesPresent(disks) ||
		os.Geteuid() == 0 ||
		sudoersGrantLive() ||
		sudoNoPasswordAvailable()
}

// blockSetupRunTo is the block-setup core, writing every rooket-emitted line and
// child stream to out so a caller can buffer it while another phase (make) owns
// the terminal. It must not mutate process-global state, so a caller can run it
// concurrently with other phases.
func blockSetupRunTo(out io.Writer, name, dataDir, iqnDate string, workers, diskCount, diskSizeGB int) error {
	initIQN := fmt.Sprintf("iqn.%s.local.rooket:initiator", iqnDate)
	disks := iscsiDiskList(name, dataDir, iqnDate, workers, diskCount)

	// Step 1: Create sparse image files (no privilege needed).
	run.Fprintf(out, "==> creating disk images\n")
	for _, d := range disks {
		if _, err := os.Stat(d.imgPath); os.IsNotExist(err) {
			if err := run.CmdTo(out, "truncate", "-s", fmt.Sprintf("%dG", diskSizeGB), d.imgPath); err != nil {
				return fmt.Errorf("create image %s: %w", d.imgPath, err)
			}
			run.Fprintf(out, "created %s (%dGiB)\n", d.imgPath, diskSizeGB)
		} else {
			run.Fprintf(out, "image %s already exists, reusing\n", d.imgPath)
		}
	}

	// Step 2: Privileged iSCSI setup, unless every target is already present.
	// Checking the (world-readable) by-path symlinks first means a re-run with
	// nothing to do skips the privileged step and its sudo/pkexec prompt.
	if allISCSIDevicesPresent(disks) {
		run.Fprintf(out, "==> iSCSI targets already present, skipping privileged setup\n")
	} else {
		run.Fprintf(out, "==> configuring iSCSI targets\n")
		repair, err := lioRepairPreflight(out, disks)
		if err != nil {
			return err
		}
		steps := append(repair, buildISCSISteps(initIQN, disks, diskSizeGB, !initiatorNameCurrent(initiatorNamePath, initIQN))...)
		if err := runPrivileged(out, steps); err != nil {
			return fmt.Errorf("iSCSI setup failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
		}
	}

	// Step 3: Wait for block devices to appear and print their paths.
	run.Fprintf(out, "==> waiting for block devices\n")
	var missing []string
	for _, d := range disks {
		dev, err := waitForISCSIDevice(d.targetIQN)
		if err != nil {
			run.Fprintf(out, "warning: %v\n", err)
			missing = append(missing, d.targetIQN)
			continue
		}
		run.Fprintf(out, "worker%d disk%d: %s\n", d.workerIdx, d.diskIdx, dev)
	}
	if len(missing) > 0 {
		return fmt.Errorf("block devices not found for targets: %s", strings.Join(missing, ", "))
	}

	return nil
}

// lioOrphan is a storage object the kernel still holds whose backing file is
// gone.
//
// One of these anywhere on the host breaks EVERY fileio backstore creation,
// for every cluster and every user: targetcli's create walks all existing
// storage objects and calls os.path.samefile against each one's recorded
// path, which raises ENOENT on the missing file and aborts the create before
// it starts. What the run then shows is a target with no LUN and, ten seconds
// later, "block devices not found" — nothing that names the object actually
// responsible, which is why rooket looks for them itself.
type lioOrphan struct {
	object lio.StorageObject
	iqns   []string // the targets exporting it, which must go before it can
}

// repairableBackstores maps a configfs plugin directory to the targetcli path
// that addresses it. Only fileio is listed: it is the only kind of backstore
// rooket creates, so it is the only kind rooket removes. An orphan of any
// other kind is reported for a human to deal with, even if rooket's own IQN
// names it.
var repairableBackstores = map[string]string{"fileio": "/backstores/fileio"}

// rooketIQNRE matches one of rooket's target IQNs and captures the
// "<cluster>-worker<N>-disk<M>" identity after the namespace prefix — the
// same string rooket uses as the backstore name.
var rooketIQNRE = regexp.MustCompile(`^iqn\.[0-9]{4}-[0-9]{2}\.local\.rooket:(.+)$`)

// findLIOOrphans returns the storage objects whose recorded backing path no
// longer exists. Only a definitively missing path counts: a path rooket
// cannot stat for any other reason (one inside another user's home, say) is
// still there as far as the root-run targetcli is concerned.
func findLIOOrphans(st lio.State) []lioOrphan {
	var orphans []lioOrphan
	for _, so := range st.StorageObjects {
		if so.Path == "" {
			continue
		}
		if _, err := os.Stat(so.Path); !os.IsNotExist(err) {
			continue
		}
		orphans = append(orphans, lioOrphan{object: so, iqns: st.TargetIQNs(so.Name)})
	}
	return orphans
}

// rooketOwned reports whether the orphan is rooket's to remove: the kernel
// exports it through a target in rooket's own IQN namespace, or its backing
// file was inside the state root rooket owns. Either is proof rooket created
// it. An orphan matching neither belongs to something else on this host, and
// removing it would destroy a stranger's storage — so it is only reported.
func (o lioOrphan) rooketOwned(stateRoot string) bool {
	for _, iqn := range o.iqns {
		if rooketIQNRE.MatchString(iqn) {
			return true
		}
	}
	return pathUnder(stateRoot, o.object.Path)
}

// pathUnder reports whether p lies inside dir.
func pathUnder(dir, p string) bool {
	if dir == "" || p == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// lioRepairPreflight finds the orphaned storage objects that would make every
// backstore creation in this run fail, and returns the privileged steps that
// remove the ones rooket owns, to be prepended to the run's own steps.
//
// Reading the kernel's configuration needs no privileges (see internal/lio),
// so this costs nothing and cannot prompt. It runs only on the path that is
// about to create backstores: a run whose devices are all attached creates
// nothing an orphan could poison.
func lioRepairPreflight(out io.Writer, disks []iscsiDisk) ([]privStep, error) {
	st, err := lio.Read(lio.DefaultRoot)
	if err != nil {
		run.Fprintf(out, "warning: could not read the host's iSCSI configuration (%v); continuing\n", err)
		return nil, nil
	}
	stateRoot, err := stateDirRoot()
	if err != nil {
		return nil, err
	}
	return lioRepairSteps(out, st, stateRoot, disks)
}

// lioRepairSteps splits the orphans of a configuration into the ones rooket
// owns — returned as removal steps — and the ones it will not touch, which
// stop a run that still has a backstore to create.
func lioRepairSteps(out io.Writer, st lio.State, stateRoot string, disks []iscsiDisk) ([]privStep, error) {
	orphans := findLIOOrphans(st)
	if len(orphans) == 0 {
		return nil, nil
	}
	var mine, foreign []lioOrphan
	for _, o := range orphans {
		if _, ok := repairableBackstores[o.object.Plugin]; ok && o.rooketOwned(stateRoot) {
			mine = append(mine, o)
		} else {
			foreign = append(foreign, o)
		}
	}
	// An orphan only bites a run that creates a backstore. One whose
	// backstores all exist already — reattaching a logged-out device, say —
	// is not blocked by anything rooket refuses to remove, so it is warned
	// about rather than stopped.
	if len(foreign) > 0 {
		if backstoresMissing(st, disks) {
			return nil, foreignOrphanError(foreign)
		}
		run.Fprintf(out, "warning: %d storage object(s) on this host have no backing file and are not rooket's to remove;\n"+
			"  they will block the next run that has to create a backstore. See 'targetcli ls'.\n", len(foreign))
	}
	if len(mine) == 0 {
		return nil, nil
	}
	run.Fprintf(out, "==> removing %d orphaned backstore(s) whose disk image is gone (they would block every backstore creation)\n", len(mine))
	for _, o := range mine {
		run.Fprintf(out, "    %s (%s)\n", o.object.Name, o.object.Path)
	}
	return buildLIORepairSteps(mine), nil
}

// backstoresMissing reports whether any of the run's disks still needs its
// backstore created — the only step an orphan can block.
func backstoresMissing(st lio.State, disks []iscsiDisk) bool {
	have := make(map[string]bool, len(st.StorageObjects))
	for _, so := range st.StorageObjects {
		have[so.Name] = true
	}
	for _, d := range disks {
		if !have[d.backstoreName] {
			return true
		}
	}
	return false
}

// foreignOrphanError explains why a run cannot proceed past orphans rooket
// will not touch, and hands over the exact commands that clear them.
func foreignOrphanError(foreign []lioOrphan) error {
	var what, how strings.Builder
	for _, o := range foreign {
		fmt.Fprintf(&what, "  %s (%s) -> %s\n", o.object.Name, o.object.Plugin, o.object.Path)
		for _, iqn := range o.iqns {
			fmt.Fprintf(&how, "  targetcli /iscsi delete %s\n", iqn)
		}
		fmt.Fprintf(&how, "  targetcli /backstores/%s delete %s\n", o.object.Plugin, o.object.Name)
	}
	return fmt.Errorf("the host's iSCSI configuration holds storage object(s) whose backing file no longer exists:\n\n%s\n"+
		"targetcli stats every storage object's path when creating one, so it aborts every backstore creation while\n"+
		"these exist — no disk of this cluster can be attached until they are gone. rooket did not create them, so it\n"+
		"will not remove them. Remove them with root privileges:\n\n%s", what.String(), how.String())
}

// buildLIORepairSteps removes orphaned storage objects, and the targets that
// export them, ahead of the run's own creates. Order is forced: the initiator
// must log out before its session's device disappears, and the kernel refuses
// to delete a storage object a LUN still exports.
//
// The backstore delete is fatal rather than warnOnFailure, unlike the same
// command in teardown: an orphan left in place guarantees every create in
// this run fails, so stopping here — with the manual script the caller
// renders — beats letting the run continue to its misleading "block devices
// not found".
func buildLIORepairSteps(orphans []lioOrphan) []privStep {
	if len(orphans) == 0 {
		return nil
	}
	// A logout needs iscsid, and a kernel session outlives a stopped one: the
	// repair runs ahead of the setup steps that start it, and must not delete
	// a target out from under a session it failed to log out of.
	steps := []privStep{{argv: []string{"systemctl", "start", "iscsid"}}}
	for _, o := range orphans {
		for _, iqn := range o.iqns {
			steps = append(steps,
				privStep{argv: []string{"iscsiadm", "-m", "node", "-T", iqn, "-u"}, ignoreErr: true},
				privStep{argv: []string{"iscsiadm", "-m", "node", "-T", iqn, "-o", "delete"}, ignoreErr: true},
				privStep{argv: []string{"targetcli", "/iscsi", "delete", iqn}, warnOnFailure: true},
			)
		}
		steps = append(steps, privStep{argv: []string{"targetcli", repairableBackstores[o.object.Plugin], "delete", o.object.Name}})
	}
	return steps
}

var blockTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Tear down iSCSI targets and optionally delete disk images",
	Long: `teardown logs out of all iSCSI sessions for this cluster, removes targets
and backstores via targetcli, and (with --delete-disks) deletes the underlying
disk image files.

Privilege requirements: iscsiadm and targetcli require root. rooket runs
itemized through sudo -n when a passwordless grant is available (rooket's own
rule, installed via 'rooket sudoers install', or any other passwordless sudo),
otherwise falls back to a single pkexec prompt.
`,
	RunE: blockTeardownRun,
}

func blockTeardownRun(_ *cobra.Command, _ []string) error {
	blockTeardownName = clusterName(blockTeardownName)
	if err := validateIQNDate(blockTeardownIQNDate); err != nil {
		return err
	}

	dataDir := blockTeardownDataDir
	if dataDir == "" {
		var err error
		dataDir, err = stateDirPath(blockTeardownName)
		if err != nil {
			return err
		}
	}

	disks := teardownDisks(lio.DefaultRoot, blockTeardownName, dataDir, blockTeardownIQNDate,
		blockTeardownWorkers, blockTeardownDiskCount)

	run.Printf("==> tearing down iSCSI targets\n")
	steps := buildISCSITeardownSteps(disks)
	if err := runPrivileged(os.Stdout, steps); err != nil {
		return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
	}

	if blockTeardownDeleteDisks {
		run.Printf("==> deleting disk images\n")
		for _, d := range disks {
			// A disk named only by the kernel's configuration — its image
			// already deleted — has no path left to remove.
			if d.imgPath == "" {
				continue
			}
			if err := os.Remove(d.imgPath); err == nil {
				run.Printf("removed %s\n", d.imgPath)
			} else if !os.IsNotExist(err) {
				run.Printf("warning: remove %s: %v\n", d.imgPath, err)
			}
		}
	} else {
		run.Printf("disk images preserved (pass --delete-disks to remove them)\n")
	}
	return nil
}

// stateDirDisks reconstructs a cluster's iscsiDisk entries from the
// worker*-disk*.img images in its state directory, so teardown can name the
// matching backstores and target IQNs without knowing the --workers and
// --disk-count the cluster was set up with.
func stateDirDisks(clusterName, dir, iqnDate string) []iscsiDisk {
	imgs, _ := filepath.Glob(filepath.Join(dir, "worker*-disk*.img"))
	var disks []iscsiDisk
	for _, img := range imgs {
		id := strings.TrimSuffix(filepath.Base(img), ".img")
		disks = append(disks, iscsiDisk{
			imgPath:       img,
			backstoreName: fmt.Sprintf("%s-%s", clusterName, id),
			targetIQN:     fmt.Sprintf("iqn.%s.local.rooket:%s-%s", iqnDate, clusterName, id),
		})
	}
	return disks
}

// teardownDisks assembles a cluster's teardown set from every source that can
// name one of its disks: the requested --workers/--disk-count grid, the
// images its state directory actually holds, and the objects the kernel still
// holds for it. Passing 0 workers or disks contributes nothing from the grid,
// for a caller (down --all) that has no per-cluster counts to trust.
//
// The grid alone is not enough, and getting that wrong is how the host
// accumulates iSCSI configuration nothing can name again: a cluster brought
// up with more workers than the teardown is told about leaves its extra
// targets and backstores behind, and removing the state directory then
// deletes the images that were the only remaining record of them. An orphan
// like that goes on to break backstore creation for every cluster on the
// host, so teardown deliberately over-collects — a name with nothing behind
// it costs one warned-about, best-effort targetcli step.
func teardownDisks(lioRoot, name, dataDir, iqnDate string, workers, diskCount int) []iscsiDisk {
	return unionDisks(
		iscsiDiskList(name, dataDir, iqnDate, workers, diskCount),
		stateDirDisks(name, dataDir, iqnDate),
		lioDisks(lioRoot, name, iqnDate),
	)
}

// unionDisks concatenates disk sets, keeping the first entry that names a
// given backstore and target. Earlier sets therefore win on the fields the
// later ones cannot know (the worker and disk indices, and an image path that
// is still on disk).
func unionDisks(sets ...[]iscsiDisk) []iscsiDisk {
	seen := map[string]bool{}
	var out []iscsiDisk
	for _, set := range sets {
		for _, d := range set {
			key := d.backstoreName + "\x00" + d.targetIQN
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	return out
}

// lioDisks returns the disks the kernel still holds for one cluster. A read
// failure yields nothing: this is an extra source for teardown, never the
// authority on whether a cluster has disks.
func lioDisks(lioRoot, name, iqnDate string) []iscsiDisk {
	st, err := lio.Read(lioRoot)
	if err != nil {
		return nil
	}
	return lioClusterDisks(st, iqnDate)[name]
}

// lioClusterDisks groups the iSCSI disks the kernel holds by rooket cluster,
// reading the configuration itself rather than any host artefact a partial
// teardown can erase. It sees strictly more than the /dev/disk/by-path scan:
// a target with no active session has no by-path symlink, and a backstore
// whose target was already deleted has neither a symlink nor an image.
//
// Storage objects are collected before targets so that a disk named by both
// keeps the backing path the object records. A backstore no target exports
// cannot have its IQN recovered from the configuration, so iqnDate
// reconstructs it — the same assumption prune makes for an orphaned state
// directory.
func lioClusterDisks(st lio.State, iqnDate string) map[string][]iscsiDisk {
	found := map[string][]iscsiDisk{}
	seen := map[string]bool{}
	add := func(cluster string, d iscsiDisk) {
		key := d.backstoreName + "\x00" + d.targetIQN
		if seen[key] {
			return
		}
		seen[key] = true
		found[cluster] = append(found[cluster], d)
	}
	for _, so := range st.StorageObjects {
		cluster, ok := clusterOfBackstore(so.Name)
		if !ok {
			continue
		}
		iqns := st.TargetIQNs(so.Name)
		if len(iqns) == 0 {
			iqns = []string{fmt.Sprintf("iqn.%s.local.rooket:%s", iqnDate, so.Name)}
		}
		for _, iqn := range iqns {
			add(cluster, iscsiDisk{imgPath: so.Path, backstoreName: so.Name, targetIQN: iqn})
		}
	}
	for _, t := range st.Targets {
		if d, cluster, ok := parseRooketIQN(t.IQN); ok {
			add(cluster, d)
		}
	}
	return found
}

// parseRooketIQN parses one of rooket's target IQNs into the disk identity a
// teardown needs and the cluster it belongs to. ok is false for an IQN
// outside rooket's namespace, one whose name does not carry the
// worker/disk shape, or one whose cluster component is not a valid cluster
// name — none of which are safe to treat as rooket's.
func parseRooketIQN(iqn string) (disk iscsiDisk, cluster string, ok bool) {
	m := rooketIQNRE.FindStringSubmatch(iqn)
	if m == nil {
		return iscsiDisk{}, "", false
	}
	cluster, ok = clusterOfBackstore(m[1])
	if !ok {
		return iscsiDisk{}, "", false
	}
	return iscsiDisk{backstoreName: m[1], targetIQN: iqn}, cluster, true
}

// backstoreNameRE splits a "<cluster>-worker<N>-disk<M>" backstore name into
// the cluster name, anchored on the fixed "-worker<N>-disk<M>" suffix so a
// cluster name that itself contains dashes (e.g. "home-jhoblitt-github-rook")
// is not mis-split.
var backstoreNameRE = regexp.MustCompile(`^(.+)-worker[0-9]+-disk[0-9]+$`)

// clusterOfBackstore returns the cluster a backstore name belongs to, and
// whether the name is one rooket could have created.
func clusterOfBackstore(name string) (string, bool) {
	m := backstoreNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	if err := validateClusterName(m[1]); err != nil {
		return "", false
	}
	return m[1], true
}

// buildISCSITeardownSteps generates the privileged steps that log out of
// iSCSI sessions, delete node records, and remove targets and backstores via
// targetcli. Every step is best-effort so a partial setup can still be
// cleaned up.
//
// The targetcli deletes carry warnOnFailure rather than ignoreErr: a single
// stale backstore (e.g. left by an unrelated cluster) aborts every targetcli
// mutation, including these, and a caller that deletes state right after this
// runs (prune) must not let that failure go unreported — silently discarding
// it is what let a target survive with nothing left to name it again. The
// iscsiadm logout/delete steps stay ignoreErr: unlike targetcli, they fail
// routinely and harmlessly (no session or node record ever existed, e.g. a
// setup that never reached iscsiadm login), so warning on every one of them
// would bury the signal the targetcli warnings are meant to surface.
func buildISCSITeardownSteps(disks []iscsiDisk) []privStep {
	var steps []privStep
	for _, d := range disks {
		steps = append(steps,
			privStep{argv: []string{"iscsiadm", "-m", "node", "-T", d.targetIQN, "-u"}, ignoreErr: true},
			privStep{argv: []string{"iscsiadm", "-m", "node", "-T", d.targetIQN, "-o", "delete"}, ignoreErr: true},
		)
	}
	for _, d := range disks {
		steps = append(steps,
			privStep{argv: []string{"targetcli", "/iscsi", "delete", d.targetIQN}, warnOnFailure: true},
			privStep{argv: []string{"targetcli", "/backstores/fileio", "delete", d.backstoreName}, warnOnFailure: true},
		)
	}
	return append(steps, privStep{argv: []string{"targetcli", "saveconfig"}})
}

// shQuote single-quotes s for safe interpolation into a /bin/sh script,
// escaping any embedded single quote. renderScript uses this for every
// dynamic operand — cluster-derived names, IQNs, and image paths — in the
// script rendered for the pkexec fallback; the itemized sudo executor passes
// argv directly to exec, so no shell is involved there.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// iqnDateRE matches the YYYY-MM date component of an IQN.
var iqnDateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)

// validateIQNDate rejects a malformed --iqn-date. The value lands in every
// target IQN; constraining it to YYYY-MM keeps a stray character out of the
// IQNs (and, with shQuote, out of the privileged script) and matches the IQN
// naming convention.
func validateIQNDate(date string) error {
	if !iqnDateRE.MatchString(date) {
		return fmt.Errorf("invalid --iqn-date %q: want YYYY-MM (e.g. 2003-01)", date)
	}
	return nil
}

const initiatorNamePath = "/etc/iscsi/initiatorname.iscsi"

// initiatorNameCurrent reports whether the file already declares exactly
// wantIQN. The file is world-readable, so this needs no privileges — and when
// it is current, both the write and the iscsid restart that follows it can be
// skipped. iscsid is host-global and shared by every other cluster's live
// sessions, so restarting it to apply a change that did not happen disrupts
// clusters this run has nothing to do with.
func initiatorNameCurrent(path, wantIQN string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ok := strings.CutPrefix(line, "InitiatorName=")
		if !ok {
			continue
		}
		if found || strings.TrimSpace(name) != wantIQN {
			return false
		}
		found = true
	}
	return found
}

// buildISCSISteps generates the privileged steps that:
//  1. Start iscsid
//  2. Set the initiator name, if writeInitiator
//  3. Create fileio backstores, iSCSI targets, LUNs, and ACLs via targetcli
//  4. Save the targetcli config
//  5. Restart iscsid, if writeInitiator, to pick up the new name
//  6. Discover targets and log in with iscsiadm
//  7. Rescan each target's LUNs with iscsiadm
func buildISCSISteps(initIQN string, disks []iscsiDisk, sizeGB int, writeInitiator bool) []privStep {
	steps := []privStep{{argv: []string{"systemctl", "start", "iscsid"}}}
	if writeInitiator {
		steps = append(steps, privStep{
			argv:        []string{"tee", initiatorNamePath},
			stdinLine:   "InitiatorName=" + initIQN,
			quietStdout: true,
		})
	}
	// These create steps tolerate "already exists" on a re-run, but that is
	// not the only way targetcli can fail: an unrelated stale backstore (e.g.
	// referencing another cluster's deleted image file) makes every targetcli
	// mutation abort, including these. warnOnFailure keeps that failure from
	// being silently discarded — quietStderr+ignoreErr once hid exactly this,
	// leaving "block devices not found" as the only, misleading symptom.
	for _, d := range disks {
		tpg := "/iscsi/" + d.targetIQN + "/tpg1"
		steps = append(steps,
			privStep{argv: []string{"targetcli", "/backstores/fileio", "create", d.backstoreName, d.imgPath, fmt.Sprintf("%dG", sizeGB)}, warnOnFailure: true},
			privStep{argv: []string{"targetcli", "/iscsi", "create", d.targetIQN}, warnOnFailure: true},
			privStep{argv: []string{"targetcli", tpg + "/luns", "create", "/backstores/fileio/" + d.backstoreName}, warnOnFailure: true},
			privStep{argv: []string{"targetcli", tpg + "/acls", "create", initIQN}, warnOnFailure: true},
			privStep{argv: []string{"targetcli", tpg + "/acls/" + initIQN, "create", "tpg_lun_or_backstore=lun0", "mapped_lun=0"}, warnOnFailure: true},
		)
	}
	steps = append(steps, privStep{argv: []string{"targetcli", "saveconfig"}})
	if writeInitiator {
		steps = append(steps, privStep{argv: []string{"systemctl", "restart", "iscsid"}, settle: time.Second})
	}
	steps = append(steps,
		privStep{argv: []string{"iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", "127.0.0.1"}},
		privStep{argv: []string{"iscsiadm", "-m", "node", "--login"}, ignoreErr: true},
	)
	// --login is a no-op on a target the initiator already has a session
	// with: it prints nothing and scans nothing. So a target created (or
	// given a new LUN) after that session was established — e.g. a prior run
	// that created the target but failed before adding its backstore — never
	// gets its LUN scanned in, and no /dev/disk/by-path symlink ever appears.
	// -R forces the existing session to rescan, which is the actual recovery;
	// like --login, it fails harmlessly on a target with no session.
	for _, d := range disks {
		steps = append(steps, privStep{argv: []string{"iscsiadm", "-m", "node", "-T", d.targetIQN, "-R"}, ignoreErr: true})
	}
	return steps
}

const (
	iscsiByPathDir    = "/dev/disk/by-path"
	iscsiByPathPrefix = "ip-127.0.0.1:3260-iscsi-"
	iscsiByPathSuffix = "-lun-0"
)

// iscsiByPathLink returns the /dev/disk/by-path symlink for a target's LUN 0.
func iscsiByPathLink(targetIQN string) string {
	return filepath.Join(iscsiByPathDir, iscsiByPathPrefix+targetIQN+iscsiByPathSuffix)
}

// resolveDeviceLink reads a symlink and returns its target as an absolute path,
// or "" if the link does not exist. No privileges are required: the by-path
// symlinks are world-readable.
func resolveDeviceLink(link string) string {
	target, err := os.Readlink(link)
	if err != nil || target == "" {
		return ""
	}
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Clean(filepath.Join(filepath.Dir(link), target))
}

// iscsiDevicePresent reports whether the target's LUN is already attached: the
// by-path symlink resolves and the device node it points to exists (the os.Stat
// guards against a dangling symlink to a removed device).
func iscsiDevicePresent(targetIQN string) bool {
	dev := resolveDeviceLink(iscsiByPathLink(targetIQN))
	if dev == "" {
		return false
	}
	_, err := os.Stat(dev)
	return err == nil
}

// allISCSIDevicesPresent reports whether every disk's iSCSI device is already
// attached, so the privileged setup step can be skipped.
func allISCSIDevicesPresent(disks []iscsiDisk) bool {
	for _, d := range disks {
		if !iscsiDevicePresent(d.targetIQN) {
			return false
		}
	}
	return len(disks) > 0
}

// waitForISCSIDevice waits up to 10 s for the /dev/disk/by-path symlink
// for the given target IQN to appear, then returns the resolved device path.
func waitForISCSIDevice(targetIQN string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dev := resolveDeviceLink(iscsiByPathLink(targetIQN)); dev != "" {
			return dev, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("device for %s not found after 10s (expected symlink at %s)", targetIQN, iscsiByPathLink(targetIQN))
}

func init() {
	rootCmd.AddCommand(blockCmd)
	blockCmd.AddCommand(blockSetupCmd)
	blockCmd.AddCommand(blockTeardownCmd)

	blockSetupCmd.Flags().StringVar(&blockSetupName, "name", "", "cluster name (used in iSCSI IQN naming)")
	blockSetupCmd.Flags().IntVar(&blockSetupWorkers, "workers", 3, "number of workers")
	blockSetupCmd.Flags().IntVar(&blockSetupDiskCount, "disk-count", 1, "disks per worker")
	blockSetupCmd.Flags().IntVar(&blockSetupDiskSizeGB, "disk-size", 10, "disk size in GiB")
	blockSetupCmd.Flags().StringVar(&blockSetupDataDir, "data-dir", "", "directory for disk images (default: ~/.local/share/rooket/<name>)")
	blockSetupCmd.Flags().StringVar(&blockSetupIQNDate, "iqn-date", "2003-01", "date component for IQNs (YYYY-MM)")

	blockTeardownCmd.Flags().StringVar(&blockTeardownName, "name", "", "cluster name (used in iSCSI IQN naming)")
	blockTeardownCmd.Flags().IntVar(&blockTeardownWorkers, "workers", 3, "number of workers")
	blockTeardownCmd.Flags().IntVar(&blockTeardownDiskCount, "disk-count", 1, "disks per worker")
	blockTeardownCmd.Flags().StringVar(&blockTeardownDataDir, "data-dir", "", "directory for disk images (default: ~/.local/share/rooket/<name>)")
	blockTeardownCmd.Flags().StringVar(&blockTeardownIQNDate, "iqn-date", "2003-01", "date component for IQNs (YYYY-MM)")
	blockTeardownCmd.Flags().BoolVar(&blockTeardownDeleteDisks, "delete-disks", false, "also delete disk image files")
}
