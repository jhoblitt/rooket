// Package disks creates sparse disk image files and attaches them as loop
// devices on the HOST. The devices are then bind-mounted into kind worker
// nodes via kind's extraMounts, which causes crun to add them to the
// container's cgroup device allowlist.
//
// # Privilege requirements
//
// Loop device creation (/dev/loop-control) requires membership in the disk
// group or root. After attachment the device must be owned by the calling
// user so that, when bind-mounted into a rootless-podman kind container, the
// container root process (which maps to the host user) can open it.
//
// rooket tries the following strategies in order:
//
//  1. Direct losetup   – works if the user is in the disk group.
//  2. sudo -n losetup  – works if NOPASSWD sudo is configured.
//  3. pkexec           – shows a GUI auth dialog (Wayland/X11 session required).
//
// If all strategies fail, a human-readable error with copy-paste commands is
// returned so the user can run the setup once in a regular terminal.
package disks

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/run"
)

// Disk describes a single OSD disk image and its loop device on the host.
type Disk struct {
	// HostPath is the path to the loop device on the host (e.g. /dev/loop0).
	HostPath string
	// ContainerPath is where the device appears inside the node container.
	ContainerPath string
	// ImagePath is the backing sparse image file on the host.
	ImagePath string
}

// Config describes the disk layout for a single worker node.
type Config struct {
	// DataDir is the host directory where disk image files are stored.
	DataDir string
	// WorkerIndex identifies the worker node (0-based).
	WorkerIndex int
	// Count is the number of disks to create for this worker.
	Count int
	// SizeGB is the size of each disk image in gigabytes.
	SizeGB int
}

// ImagePath returns the host path of the i-th disk image file for this worker.
func (c *Config) ImagePath(i int) string {
	return filepath.Join(c.DataDir, fmt.Sprintf("worker%d-disk%d.img", c.WorkerIndex, i))
}

// attachedLoopDevice returns the /dev/loopN path already attached to the
// image, or "" if none. Reads from /sys — no privileges required.
func attachedLoopDevice(imagePath string) string {
	out, err := run.Output("losetup", "-j", imagePath)
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	// Output: /dev/loopN: []: (/path/to/file)
	return strings.SplitN(strings.TrimSpace(out), ":", 2)[0]
}

// attachViaUdisksctl attaches imagePath as a loop device using udisksctl
// (D-Bus/udisks2), which requires no password or graphical session.
// udisks2 sets ownership to the calling user via udev; we wait up to 3 s for
// the device node to become accessible.
func attachViaUdisksctl(imagePath string) (string, error) {
	out, err := run.Output("udisksctl", "loop-setup", "-f", imagePath)
	if err != nil || out == "" {
		return "", fmt.Errorf("udisksctl loop-setup: %w", err)
	}
	// Output: "Mapped file /path as /dev/loopN."
	// Extract the device path.
	parts := strings.Fields(out)
	if len(parts) == 0 {
		return "", fmt.Errorf("unexpected udisksctl output: %q", out)
	}
	dev := strings.TrimSuffix(parts[len(parts)-1], ".")
	if !strings.HasPrefix(dev, "/dev/loop") {
		return "", fmt.Errorf("unexpected device from udisksctl: %q", dev)
	}
	// Wait for udev to process the device and udisks to transfer ownership.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f, err := os.Open(dev); err == nil {
			f.Close()
			return dev, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return dev, nil // return path even if not yet accessible; caller may still work
}

// attachLoopDevice tries to attach imagePath as a loop device and chown it
// to username using the first available privilege-escalation mechanism.
// Returns the /dev/loopN path on success.
func attachLoopDevice(imagePath, username string) (string, error) {
	script := fmt.Sprintf(
		`DEV=$(losetup --find --show %q) && chown %s "$DEV" && echo "$DEV"`,
		imagePath, username,
	)

	// Strategy 1: direct (works if user is in the disk group).
	if out, err := run.Output("sh", "-c", script); err == nil && out != "" {
		return out, nil
	}

	// Strategy 2: udisksctl (D-Bus/udisks2; no password or graphical session needed).
	if _, err := exec.LookPath("udisksctl"); err == nil {
		if out, err := attachViaUdisksctl(imagePath); err == nil && out != "" {
			return out, nil
		}
	}

	// Strategy 3: sudo -n (no password; works with NOPASSWD configuration).
	if out, err := run.OutputInteractive("sudo", "-n", "sh", "-c", script); err == nil && out != "" {
		return out, nil
	}

	// Strategy 4: pkexec (PolicyKit GUI dialog; requires a graphical session).
	if _, err := exec.LookPath("pkexec"); err == nil {
		if out, err := run.OutputInteractive("pkexec", "sh", "-c", script); err == nil && out != "" {
			return out, nil
		}
	}

	return "", fmt.Errorf("all privilege-escalation strategies failed")
}

// Create creates sparse image files on the host and attaches each as a loop
// device. Existing attachments are reused. Returns a Disk descriptor per image.
func Create(cfg Config) ([]Disk, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("get current user: %w", err)
	}

	var result []Disk
	// Accumulate copy-paste commands on failure (two variants: udisksctl and sudo).
	var udisksCmds []string
	var sudoCmds []string

	for i := 0; i < cfg.Count; i++ {
		imgPath := cfg.ImagePath(i)

		// Create the sparse image file if needed.
		if _, err := os.Stat(imgPath); os.IsNotExist(err) {
			if err := run.Cmd("truncate", "-s", fmt.Sprintf("%dG", cfg.SizeGB), imgPath); err != nil {
				return nil, fmt.Errorf("create image %s: %w", imgPath, err)
			}
			fmt.Printf("created disk image %s (%dGiB)\n", imgPath, cfg.SizeGB)
		} else {
			fmt.Printf("disk image %s already exists, reusing\n", imgPath)
		}

		// Check if already attached.
		loopDev := attachedLoopDevice(imgPath)
		if loopDev != "" {
			fmt.Printf("loop device %s already attached for %s\n", loopDev, imgPath)
			// Ensure ownership is still ours; skip chown if already accessible.
			if f, err := os.Open(loopDev); err == nil {
				f.Close()
			} else {
				_, _ = run.OutputInteractive("sudo", "-n", "chown", u.Username, loopDev)
			}
		} else {
			fmt.Printf("attaching %s as a loop device...\n", imgPath)
			loopDev, err = attachLoopDevice(imgPath, u.Username)
			if err != nil {
				udisksCmds = append(udisksCmds, fmt.Sprintf("  udisksctl loop-setup -f %q", imgPath))
				sudoCmds = append(sudoCmds,
					fmt.Sprintf("  sudo sh -c 'DEV=$(losetup --find --show %q); chown %s $DEV'", imgPath, u.Username),
				)
				continue
			}
			fmt.Printf("loop device %s attached\n", loopDev)
		}

		result = append(result, Disk{
			HostPath:      loopDev,
			ContainerPath: loopDev,
			ImagePath:     imgPath,
		})
	}

	if len(udisksCmds) > 0 {
		return nil, fmt.Errorf(
			"could not attach loop device(s) automatically.\n\n"+
				"Option A – udisksctl (no password or terminal required):\n\n%s\n\n"+
				"Option B – sudo (run in a terminal that can prompt for a password):\n\n%s\n\n"+
				"Then re-run 'rooket create', or skip: rooket create --skip-disks",
			strings.Join(udisksCmds, "\n"),
			strings.Join(sudoCmds, "\n"),
		)
	}

	return result, nil
}

// Detach detaches all loop devices associated with this worker's disk images.
// After Create the devices are owned by the current user, so no sudo is needed.
func Detach(cfg Config) error {
	var firstErr error
	for i := 0; i < cfg.Count; i++ {
		dev := attachedLoopDevice(cfg.ImagePath(i))
		if dev == "" {
			continue
		}
		if err := run.Cmd("losetup", "-d", dev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove deletes disk image files from the host.
func Remove(cfg Config) error {
	var firstErr error
	for i := 0; i < cfg.Count; i++ {
		path := cfg.ImagePath(i)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
