// Package disks creates sparse disk image files and attaches them as loop
// devices so that kind worker nodes can use them as raw block devices for
// Ceph OSDs.
package disks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
)

// Config describes the disk layout for a single worker node.
type Config struct {
	// DataDir is the directory where disk image files are stored.
	DataDir string
	// WorkerIndex identifies the worker node (0-based).
	WorkerIndex int
	// Count is the number of disks to create for this worker.
	Count int
	// SizeGB is the size of each disk image in gigabytes.
	SizeGB int
}

// DiskPath returns the path to the i-th disk image file for this worker.
func (c *Config) DiskPath(i int) string {
	return filepath.Join(c.DataDir, fmt.Sprintf("worker%d-disk%d.img", c.WorkerIndex, i))
}

// LoopDevice returns the /dev/loop path attached to the i-th disk image, or
// an empty string if it is not attached.
func (c *Config) LoopDevice(i int) string {
	path := c.DiskPath(i)
	out, err := run.Output("losetup", "-j", path)
	if err != nil || out == "" {
		return ""
	}
	// Output format: /dev/loopN: []: (/path/to/file)
	return strings.SplitN(out, ":", 2)[0]
}

// Create creates the disk image files (if they don't exist) and attaches them
// as loop devices. It returns the list of loop device paths in order.
func Create(cfg Config) ([]string, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	var devices []string
	for i := 0; i < cfg.Count; i++ {
		path := cfg.DiskPath(i)

		// Create sparse image file if it doesn't exist.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			sizeStr := fmt.Sprintf("%dG", cfg.SizeGB)
			if err := run.Cmd("truncate", "-s", sizeStr, path); err != nil {
				return nil, fmt.Errorf("create disk image %s: %w", path, err)
			}
		}

		// Attach as loop device if not already attached.
		dev := cfg.LoopDevice(i)
		if dev == "" {
			if err := run.Cmd("losetup", "--find", "--show", path); err != nil {
				return nil, fmt.Errorf("losetup %s: %w", path, err)
			}
			dev = cfg.LoopDevice(i)
			if dev == "" {
				return nil, fmt.Errorf("losetup succeeded but could not find loop device for %s", path)
			}
		}
		fmt.Printf("disk %s attached as %s\n", path, dev)
		devices = append(devices, dev)
	}
	return devices, nil
}

// Detach detaches all loop devices associated with this worker's disk images.
func Detach(cfg Config) error {
	var firstErr error
	for i := 0; i < cfg.Count; i++ {
		dev := cfg.LoopDevice(i)
		if dev == "" {
			continue
		}
		if err := run.Cmd("losetup", "-d", dev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove deletes the disk image files from disk.
func Remove(cfg Config) error {
	var firstErr error
	for i := 0; i < cfg.Count; i++ {
		path := cfg.DiskPath(i)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
