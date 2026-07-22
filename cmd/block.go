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
		steps := buildISCSISteps(initIQN, disks, diskSizeGB, !initiatorNameCurrent(initiatorNamePath, initIQN))
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

	var disks []iscsiDisk
	for w := 0; w < blockTeardownWorkers; w++ {
		for d := 0; d < blockTeardownDiskCount; d++ {
			id := fmt.Sprintf("worker%d-disk%d", w, d)
			disks = append(disks, iscsiDisk{
				workerIdx:     w,
				diskIdx:       d,
				imgPath:       filepath.Join(dataDir, id+".img"),
				backstoreName: fmt.Sprintf("%s-%s", blockTeardownName, id),
				targetIQN:     fmt.Sprintf("iqn.%s.local.rooket:%s-%s", blockTeardownIQNDate, blockTeardownName, id),
			})
		}
	}

	run.Printf("==> tearing down iSCSI targets\n")
	steps := buildISCSITeardownSteps(disks)
	if err := runPrivileged(os.Stdout, steps); err != nil {
		return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
	}

	if blockTeardownDeleteDisks {
		run.Printf("==> deleting disk images\n")
		for _, d := range disks {
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
