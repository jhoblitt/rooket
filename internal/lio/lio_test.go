package lio

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeRoot builds a configfs-shaped tree: two fileio objects, a ramdisk with
// no udev_path, the "alua" sibling that is not a plugin directory, one target
// exporting an object, one target with no LUN, and the fabric-wide entries
// that share the iscsi directory with real targets.
func fakeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mkObj := func(plugin, name, path string) {
		dir := filepath.Join(root, "core", plugin, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if path != "" {
			if err := os.WriteFile(filepath.Join(dir, "udev_path"), []byte(path+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkObj("fileio_3", "c-worker3-disk0", "/data/c/worker3-disk0.img")
	mkObj("fileio_4", "c-worker4-disk0", "/data/c/worker4-disk0.img")
	mkObj("rd_mcp_0", "ram0", "")
	if err := os.MkdirAll(filepath.Join(root, "core", "alua", "lu_gps", "default_lu_gp"), 0o755); err != nil {
		t.Fatal(err)
	}

	mkTarget := func(iqn, exports string) {
		lunDir := filepath.Join(root, "iscsi", iqn, "tpgt_1", "lun")
		if exports == "" {
			if err := os.MkdirAll(lunDir, 0o755); err != nil {
				t.Fatal(err)
			}
			return
		}
		lun0 := filepath.Join(lunDir, "lun_0")
		if err := os.MkdirAll(lun0, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lun0, "alua_tg_pt_gp"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../../../../target/core/fileio_3/"+exports, filepath.Join(lun0, "476c6a1ecd")); err != nil {
			t.Fatal(err)
		}
	}
	mkTarget("iqn.2003-01.local.rooket:c-worker3-disk0", "c-worker3-disk0")
	mkTarget("iqn.2003-01.local.rooket:c-worker9-disk0", "")
	if err := os.MkdirAll(filepath.Join(root, "iscsi", "discovery_auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "iscsi", "lio_version"), []byte("v5.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReadStorageObjects(t *testing.T) {
	st, err := Read(fakeRoot(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []StorageObject{
		{Name: "c-worker3-disk0", Plugin: "fileio", Path: "/data/c/worker3-disk0.img"},
		{Name: "c-worker4-disk0", Plugin: "fileio", Path: "/data/c/worker4-disk0.img"},
		{Name: "ram0", Plugin: "rd_mcp", Path: ""},
	}
	if !slices.Equal(st.StorageObjects, want) {
		t.Errorf("storage objects = %+v, want %+v", st.StorageObjects, want)
	}
}

func TestReadTargets(t *testing.T) {
	st, err := Read(fakeRoot(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(st.Targets) != 2 {
		t.Fatalf("targets = %+v, want 2 (discovery_auth and lio_version are not targets)", st.Targets)
	}
	got := st.TargetIQNs("c-worker3-disk0")
	want := []string{"iqn.2003-01.local.rooket:c-worker3-disk0"}
	if !slices.Equal(got, want) {
		t.Errorf("TargetIQNs = %v, want %v", got, want)
	}
	// A target whose backstore create failed has a TPG but no LUN, and must
	// not be reported as exporting anything.
	for _, tg := range st.Targets {
		if tg.IQN == "iqn.2003-01.local.rooket:c-worker9-disk0" && len(tg.StorageObjects) != 0 {
			t.Errorf("LUN-less target exports %v, want none", tg.StorageObjects)
		}
	}
	if iqns := st.TargetIQNs("c-worker4-disk0"); len(iqns) != 0 {
		t.Errorf("TargetIQNs for an unexported object = %v, want none", iqns)
	}
}

// A host that has never configured a target has no configfs target directory;
// that is an empty configuration, not a read failure.
func TestReadMissingRoot(t *testing.T) {
	st, err := Read(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(st.StorageObjects) != 0 || len(st.Targets) != 0 {
		t.Errorf("state = %+v, want empty", st)
	}
}
