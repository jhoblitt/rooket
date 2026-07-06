package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root, names, err := stateDirNames()
	if err != nil {
		t.Fatalf("stateDirNames with no root: %v", err)
	}
	if want := filepath.Join(home, ".local", "share", "rooket"); root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
	if len(names) != 0 {
		t.Errorf("names for missing root = %v, want none", names)
	}

	for _, d := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_, names, err = stateDirNames()
	if err != nil {
		t.Fatalf("stateDirNames: %v", err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("names = %v, want [alpha beta]", names)
	}
}

func TestStateDirDisks(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"worker0-disk0.img", "worker2-disk1.img", "kubeconfig", "stray.img"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	disks := stateDirDisks("myclus", dir, "2003-01")
	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2: %+v", len(disks), disks)
	}
	want := []iscsiDisk{
		{
			imgPath:       filepath.Join(dir, "worker0-disk0.img"),
			backstoreName: "myclus-worker0-disk0",
			targetIQN:     "iqn.2003-01.local.rooket:myclus-worker0-disk0",
		},
		{
			imgPath:       filepath.Join(dir, "worker2-disk1.img"),
			backstoreName: "myclus-worker2-disk1",
			targetIQN:     "iqn.2003-01.local.rooket:myclus-worker2-disk1",
		},
	}
	for i, w := range want {
		if disks[i] != w {
			t.Errorf("disk[%d] = %+v, want %+v", i, disks[i], w)
		}
	}

	if got := stateDirDisks("myclus", filepath.Join(dir, "nonexistent"), "2003-01"); len(got) != 0 {
		t.Errorf("disks for missing dir = %+v, want none", got)
	}
}
