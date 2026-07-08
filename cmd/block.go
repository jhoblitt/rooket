package cmd

import (
	"fmt"
	"os"
	"os/exec"
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
rooket tries sudo -n first, then pkexec.
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
	fmt.Println("==> creating disk images")
	for _, d := range disks {
		if _, err := os.Stat(d.imgPath); os.IsNotExist(err) {
			if err := run.Cmd("truncate", "-s", fmt.Sprintf("%dG", blockSetupDiskSizeGB), d.imgPath); err != nil {
				return fmt.Errorf("create image %s: %w", d.imgPath, err)
			}
			fmt.Printf("created %s (%dGiB)\n", d.imgPath, blockSetupDiskSizeGB)
		} else {
			fmt.Printf("image %s already exists, reusing\n", d.imgPath)
		}
	}

	// Step 2: Privileged iSCSI setup via a single shell script, unless every
	// target is already present. Checking the (world-readable) by-path symlinks
	// first means a re-run with nothing to do skips the privileged step and its
	// sudo/pkexec prompt.
	if allISCSIDevicesPresent(disks) {
		fmt.Println("==> iSCSI targets already present, skipping privileged setup")
	} else {
		fmt.Println("==> configuring iSCSI targets")
		script := buildISCSIScript(initIQN, disks, blockSetupDiskSizeGB)
		if err := runPrivilegedScript(script); err != nil {
			return fmt.Errorf("iSCSI setup failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", script, err)
		}
	}

	// Step 3: Wait for block devices to appear and print their paths.
	fmt.Println("==> waiting for block devices")
	var missing []string
	for _, d := range disks {
		dev, err := waitForISCSIDevice(d.targetIQN)
		if err != nil {
			fmt.Printf("warning: %v\n", err)
			missing = append(missing, d.targetIQN)
			continue
		}
		fmt.Printf("worker%d disk%d: %s\n", d.workerIdx, d.diskIdx, dev)
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

Privilege requirements: iscsiadm and targetcli require root. rooket tries
sudo -n first, then pkexec.
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

	fmt.Println("==> tearing down iSCSI targets")
	script := buildISCSITeardownScript(disks)
	if err := runPrivilegedScript(script); err != nil {
		return fmt.Errorf("iSCSI teardown failed.\n\nRun the following script manually with root privileges:\n\n%s\nError: %w", script, err)
	}

	if blockTeardownDeleteDisks {
		fmt.Println("==> deleting disk images")
		for _, d := range disks {
			if err := os.Remove(d.imgPath); err == nil {
				fmt.Printf("removed %s\n", d.imgPath)
			} else if !os.IsNotExist(err) {
				fmt.Printf("warning: remove %s: %v\n", d.imgPath, err)
			}
		}
	} else {
		fmt.Println("disk images preserved (pass --delete-disks to remove them)")
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

// buildISCSITeardownScript generates the privileged shell script that logs
// out of iSCSI sessions, deletes node records, and removes targets and
// backstores via targetcli. Every step is best-effort so a partial setup
// can still be cleaned up.
func buildISCSITeardownScript(disks []iscsiDisk) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	for _, d := range disks {
		sb.WriteString(fmt.Sprintf("iscsiadm -m node -T %s -u 2>/dev/null || true\n", shQuote(d.targetIQN)))
		sb.WriteString(fmt.Sprintf("iscsiadm -m node -T %s -o delete 2>/dev/null || true\n", shQuote(d.targetIQN)))
	}
	for _, d := range disks {
		sb.WriteString(fmt.Sprintf("targetcli /iscsi delete %s 2>/dev/null || true\n", shQuote(d.targetIQN)))
		sb.WriteString(fmt.Sprintf("targetcli /backstores/fileio delete %s 2>/dev/null || true\n", shQuote(d.backstoreName)))
	}
	sb.WriteString("targetcli saveconfig\n")
	return sb.String()
}

// shQuote single-quotes s for safe interpolation into a /bin/sh script,
// escaping any embedded single quote. The iSCSI scripts run through
// sudo/pkexec, so every dynamic operand — cluster-derived names, IQNs, and
// image paths — must be quoted rather than pasted in raw.
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

// buildISCSIScript generates the privileged shell script that:
//  1. Starts iscsid
//  2. Sets the initiator name
//  3. Creates fileio backstores, iSCSI targets, LUNs, and ACLs via targetcli
//  4. Saves the targetcli config
//  5. Discovers targets and logs in with iscsiadm
func buildISCSIScript(initIQN string, disks []iscsiDisk, sizeGB int) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	sb.WriteString("systemctl start iscsid\n")
	sb.WriteString(fmt.Sprintf("printf 'InitiatorName=%%s\\n' %s | tee /etc/iscsi/initiatorname.iscsi > /dev/null\n", shQuote(initIQN)))

	for _, d := range disks {
		lunsPath := fmt.Sprintf("/iscsi/%s/tpg1/luns", d.targetIQN)
		aclsPath := fmt.Sprintf("/iscsi/%s/tpg1/acls", d.targetIQN)
		aclPath := fmt.Sprintf("/iscsi/%s/tpg1/acls/%s", d.targetIQN, initIQN)
		backstoreRef := fmt.Sprintf("/backstores/fileio/%s", d.backstoreName)
		sb.WriteString(fmt.Sprintf(
			"targetcli /backstores/fileio create %s %s %dG 2>/dev/null || true\n",
			shQuote(d.backstoreName), shQuote(d.imgPath), sizeGB))
		sb.WriteString(fmt.Sprintf(
			"targetcli /iscsi create %s 2>/dev/null || true\n",
			shQuote(d.targetIQN)))
		sb.WriteString(fmt.Sprintf(
			"targetcli %s create %s 2>/dev/null || true\n",
			shQuote(lunsPath), shQuote(backstoreRef)))
		sb.WriteString(fmt.Sprintf(
			"targetcli %s create %s 2>/dev/null || true\n",
			shQuote(aclsPath), shQuote(initIQN)))
		sb.WriteString(fmt.Sprintf(
			"targetcli %s create tpg_lun_or_backstore=lun0 mapped_lun=0 2>/dev/null || true\n",
			shQuote(aclPath)))
	}

	sb.WriteString("targetcli saveconfig\n")
	sb.WriteString("systemctl restart iscsid && sleep 1\n")
	sb.WriteString("iscsiadm -m discovery -t sendtargets -p 127.0.0.1\n")
	sb.WriteString("iscsiadm -m node --login || true\n")
	return sb.String()
}

// runPrivilegedScript runs a shell script with root privileges: directly when
// already root, otherwise via sudo -n (which never prompts) and finally pkexec
// (through a temp file so pkexec can find it). pkexec is the only step that can
// prompt, so callers should skip this entirely when there is no work to do.
func runPrivilegedScript(script string) error {
	if os.Geteuid() == 0 {
		return run.CmdWithStdin(strings.NewReader(script), "sh")
	}
	if err := run.CmdWithStdin(strings.NewReader(script), "sudo", "-n", "sh"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("pkexec"); err == nil {
		f, err := os.CreateTemp("", "rooket-iscsi-*.sh")
		if err != nil {
			return fmt.Errorf("create temp script: %w", err)
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(script); err != nil {
			f.Close()
			return err
		}
		f.Close()
		if err := os.Chmod(f.Name(), 0o700); err != nil {
			return err
		}
		fmt.Println("==> requesting root via pkexec (you may be prompted to authenticate)")
		if err := run.Cmd("pkexec", "sh", f.Name()); err == nil {
			return nil
		}
	}
	return fmt.Errorf("all privilege-escalation strategies failed")
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
