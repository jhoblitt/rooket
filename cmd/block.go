package cmd

import (
	"fmt"
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

	dataDir := blockSetupDataDir
	if dataDir == "" {
		var err error
		dataDir, err = stateDirPath(blockSetupName)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	initIQN := fmt.Sprintf("iqn.%s.local.rooket:initiator", blockSetupIQNDate)

	// Build the disk list.
	var disks []iscsiDisk
	for w := 0; w < blockSetupWorkers; w++ {
		for d := 0; d < blockSetupDiskCount; d++ {
			id := fmt.Sprintf("worker%d-disk%d", w, d)
			disks = append(disks, iscsiDisk{
				workerIdx:     w,
				diskIdx:       d,
				imgPath:       filepath.Join(dataDir, id+".img"),
				backstoreName: fmt.Sprintf("%s-%s", blockSetupName, id),
				targetIQN:     fmt.Sprintf("iqn.%s.local.rooket:%s-%s", blockSetupIQNDate, blockSetupName, id),
			})
		}
	}

	// Step 1: Create sparse image files (no privilege needed).
	run.Printf("==> creating disk images\n")
	for _, d := range disks {
		if _, err := os.Stat(d.imgPath); os.IsNotExist(err) {
			if err := run.Cmd("truncate", "-s", fmt.Sprintf("%dG", blockSetupDiskSizeGB), d.imgPath); err != nil {
				return fmt.Errorf("create image %s: %w", d.imgPath, err)
			}
			run.Printf("created %s (%dGiB)\n", d.imgPath, blockSetupDiskSizeGB)
		} else {
			run.Printf("image %s already exists, reusing\n", d.imgPath)
		}
	}

	// Step 2: Privileged iSCSI setup, unless every target is already present.
	// Checking the (world-readable) by-path symlinks first means a re-run with
	// nothing to do skips the privileged step and its sudo/pkexec prompt.
	if allISCSIDevicesPresent(disks) {
		run.Printf("==> iSCSI targets already present, skipping privileged setup\n")
	} else {
		run.Printf("==> configuring iSCSI targets\n")
		steps := buildISCSISteps(initIQN, disks, blockSetupDiskSizeGB, !initiatorNameCurrent(initiatorNamePath, initIQN))
		if err := runPrivileged(steps); err != nil {
			return fmt.Errorf("iSCSI setup failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", renderScript(steps), err)
		}
	}

	// Step 3: Wait for block devices to appear and print their paths.
	run.Printf("==> waiting for block devices\n")
	var missing []string
	for _, d := range disks {
		dev, err := waitForISCSIDevice(d.targetIQN)
		if err != nil {
			run.Printf("warning: %v\n", err)
			missing = append(missing, d.targetIQN)
			continue
		}
		run.Printf("worker%d disk%d: %s\n", d.workerIdx, d.diskIdx, dev)
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
	if err := runPrivileged(steps); err != nil {
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
func buildISCSITeardownSteps(disks []iscsiDisk) []privStep {
	var steps []privStep
	for _, d := range disks {
		steps = append(steps,
			privStep{argv: []string{"iscsiadm", "-m", "node", "-T", d.targetIQN, "-u"}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"iscsiadm", "-m", "node", "-T", d.targetIQN, "-o", "delete"}, quietStderr: true, ignoreErr: true},
		)
	}
	for _, d := range disks {
		steps = append(steps,
			privStep{argv: []string{"targetcli", "/iscsi", "delete", d.targetIQN}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"targetcli", "/backstores/fileio", "delete", d.backstoreName}, quietStderr: true, ignoreErr: true},
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
	for _, d := range disks {
		tpg := "/iscsi/" + d.targetIQN + "/tpg1"
		steps = append(steps,
			privStep{argv: []string{"targetcli", "/backstores/fileio", "create", d.backstoreName, d.imgPath, fmt.Sprintf("%dG", sizeGB)}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"targetcli", "/iscsi", "create", d.targetIQN}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"targetcli", tpg + "/luns", "create", "/backstores/fileio/" + d.backstoreName}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"targetcli", tpg + "/acls", "create", initIQN}, quietStderr: true, ignoreErr: true},
			privStep{argv: []string{"targetcli", tpg + "/acls/" + initIQN, "create", "tpg_lun_or_backstore=lun0", "mapped_lun=0"}, quietStderr: true, ignoreErr: true},
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

// iscsiByPathLink returns the /dev/disk/by-path symlink for a target's LUN 0.
func iscsiByPathLink(targetIQN string) string {
	return fmt.Sprintf("/dev/disk/by-path/ip-127.0.0.1:3260-iscsi-%s-lun-0", targetIQN)
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
