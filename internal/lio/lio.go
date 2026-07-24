// Package lio reads the kernel's LIO (SCSI target) configuration out of
// configfs.
//
// Everything under /sys/kernel/config/target is world-readable, so this
// enumerates the host's storage objects and iSCSI targets with no privileges
// at all — where `targetcli ls` would need root, and asking for root is the
// one thing rooket's day-to-day loop is built to avoid. That makes a
// diagnosis of the target state possible BEFORE deciding whether a privileged
// run is needed, and lets teardown name objects that no longer have any other
// record on the host.
package lio

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultRoot is where the LIO target subsystem exports its configuration.
const DefaultRoot = "/sys/kernel/config/target"

// StorageObject is one LIO backstore, i.e. <root>/core/<plugin>_<n>/<name>.
type StorageObject struct {
	Name   string // the backstore name, as targetcli addresses it
	Plugin string // "fileio", "block", "pscsi", "rd_mcp", "user"
	Path   string // udev_path: the file or device backing the object, "" if it has none
}

// Target is one iSCSI target and the storage objects its LUNs export.
type Target struct {
	IQN            string
	StorageObjects []string
}

// State is a snapshot of the host's LIO configuration.
type State struct {
	StorageObjects []StorageObject
	Targets        []Target
}

// pluginDirRE matches a storage-object plugin directory (e.g. "fileio_3") and
// captures the plugin name. It excludes the sibling "alua" directory, which
// holds LU group settings rather than storage objects.
var pluginDirRE = regexp.MustCompile(`^([a-z_]+)_[0-9]+$`)

// Read snapshots the LIO configuration under root (normally DefaultRoot).
//
// A root that does not exist is an empty State rather than an error: on a host
// where no target has ever been configured the LIO modules are not loaded and
// configfs has no target directory, which is a legitimate "nothing here", not
// a failure to read.
func Read(root string) (State, error) {
	objs, err := readStorageObjects(filepath.Join(root, "core"))
	if err != nil {
		return State{}, err
	}
	targets, err := readTargets(filepath.Join(root, "iscsi"))
	if err != nil {
		return State{}, err
	}
	return State{StorageObjects: objs, Targets: targets}, nil
}

// TargetIQNs returns the IQNs of the targets exporting the named storage
// object through one of their LUNs.
func (s State) TargetIQNs(objectName string) []string {
	var iqns []string
	for _, t := range s.Targets {
		for _, so := range t.StorageObjects {
			if so == objectName {
				iqns = append(iqns, t.IQN)
				break
			}
		}
	}
	return iqns
}

// readDirIfExists lists dir, treating a missing directory as empty. Callers
// walk a tree the kernel materializes on demand, so an absent level means the
// host has none of that thing rather than a read failure.
func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

func readStorageObjects(coreDir string) ([]StorageObject, error) {
	plugins, err := readDirIfExists(coreDir)
	if err != nil {
		return nil, err
	}
	var objs []StorageObject
	for _, p := range plugins {
		m := pluginDirRE.FindStringSubmatch(p.Name())
		if !p.IsDir() || m == nil {
			continue
		}
		named, err := readDirIfExists(filepath.Join(coreDir, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, n := range named {
			if !n.IsDir() {
				continue
			}
			// A plugin with no backing path (a ramdisk) reports no udev_path;
			// it is still a storage object and still occupies its name.
			path, _ := os.ReadFile(filepath.Join(coreDir, p.Name(), n.Name(), "udev_path"))
			objs = append(objs, StorageObject{
				Name:   n.Name(),
				Plugin: m[1],
				Path:   strings.TrimSpace(string(path)),
			})
		}
	}
	return objs, nil
}

func readTargets(iscsiDir string) ([]Target, error) {
	entries, err := readDirIfExists(iscsiDir)
	if err != nil {
		return nil, err
	}
	var targets []Target
	for _, e := range entries {
		// The iscsi directory also holds fabric-wide settings
		// (discovery_auth, lio_version); only IQNs are targets.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "iqn.") {
			continue
		}
		objs, err := readTargetLUNs(filepath.Join(iscsiDir, e.Name()))
		if err != nil {
			return nil, err
		}
		targets = append(targets, Target{IQN: e.Name(), StorageObjects: objs})
	}
	return targets, nil
}

// readTargetLUNs collects the storage objects a target exports: each mapped
// LUN holds a symlink into <root>/core, whose basename is the object's name.
func readTargetLUNs(targetDir string) ([]string, error) {
	tpgs, err := readDirIfExists(targetDir)
	if err != nil {
		return nil, err
	}
	var objs []string
	for _, tpg := range tpgs {
		if !tpg.IsDir() || !strings.HasPrefix(tpg.Name(), "tpgt_") {
			continue
		}
		lunDir := filepath.Join(targetDir, tpg.Name(), "lun")
		luns, err := readDirIfExists(lunDir)
		if err != nil {
			return nil, err
		}
		for _, lun := range luns {
			if !lun.IsDir() || !strings.HasPrefix(lun.Name(), "lun_") {
				continue
			}
			links, err := readDirIfExists(filepath.Join(lunDir, lun.Name()))
			if err != nil {
				return nil, err
			}
			for _, l := range links {
				if l.Type()&os.ModeSymlink == 0 {
					continue
				}
				dest, err := os.Readlink(filepath.Join(lunDir, lun.Name(), l.Name()))
				if err != nil {
					continue
				}
				objs = append(objs, filepath.Base(dest))
			}
		}
	}
	return objs, nil
}
