package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jhoblitt/rooket/internal/lio"
)

// writeFakeLIO materializes a configfs-shaped tree holding one fileio object
// per entry of objs (name -> backing path), each exported by a target named
// with rooket's IQN convention unless its name is listed in noTarget.
func writeFakeLIO(t *testing.T, objs map[string]string, noTarget ...string) string {
	t.Helper()
	root := t.TempDir()
	skip := map[string]bool{}
	for _, n := range noTarget {
		skip[n] = true
	}
	names := make([]string, 0, len(objs))
	for n := range objs {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, name := range names {
		objDir := filepath.Join(root, "core", fmt.Sprintf("fileio_%d", i), name)
		if err := os.MkdirAll(objDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(objDir, "udev_path"), []byte(objs[name]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if skip[name] {
			continue
		}
		lun0 := filepath.Join(root, "iscsi", "iqn.2003-01.local.rooket:"+name, "tpgt_1", "lun", "lun_0")
		if err := os.MkdirAll(lun0, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(objDir, filepath.Join(lun0, "0deadbeef")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func diskKeys(disks []iscsiDisk) []string {
	out := make([]string, len(disks))
	for i, d := range disks {
		out[i] = d.backstoreName + "|" + d.targetIQN + "|" + d.imgPath
	}
	sort.Strings(out)
	return out
}

// The regression this whole path exists for: a cluster brought up with more
// workers than the teardown is told about. The flag grid names three disks
// and the state dir holds three images, but the host is still configured for
// ten — and once the state directory goes, nothing else can name the other
// seven. Every source must be unioned, or those seven become orphans that
// break backstore creation for every cluster on the host.
func TestTeardownDisksCoversDisksBeyondTheFlagGrid(t *testing.T) {
	dataDir := t.TempDir()
	objs := map[string]string{}
	for w := range 10 {
		img := filepath.Join(dataDir, fmt.Sprintf("worker%d-disk0.img", w))
		objs[fmt.Sprintf("c-worker%d-disk0", w)] = img
		if w < 3 {
			if err := os.WriteFile(img, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	lioRoot := writeFakeLIO(t, objs)

	disks := teardownDisks(lioRoot, "c", dataDir, "2003-01", 3, 1)
	if len(disks) != 10 {
		t.Fatalf("teardownDisks named %d disk(s):\n%v\nwant all 10 the host is configured for", len(disks), diskKeys(disks))
	}
	for w := range 10 {
		want := fmt.Sprintf("c-worker%d-disk0", w)
		found := false
		for _, d := range disks {
			if d.backstoreName == want {
				found = true
				if d.imgPath != filepath.Join(dataDir, fmt.Sprintf("worker%d-disk0.img", w)) {
					t.Errorf("%s: imgPath = %q, want the image path so --delete-disks can remove it", want, d.imgPath)
				}
			}
		}
		if !found {
			t.Errorf("%s missing from the teardown set", want)
		}
	}

	// Without the kernel's view, the grid and the state dir between them see
	// only three — which is exactly how the other seven were orphaned.
	if blind := teardownDisks(filepath.Join(t.TempDir(), "absent"), "c", dataDir, "2003-01", 3, 1); len(blind) != 3 {
		t.Errorf("without the kernel's configuration teardownDisks named %d disk(s), want 3", len(blind))
	}
}

// With no counts to trust (down --all rejects them) the grid contributes
// nothing and the other two sources still name everything.
func TestTeardownDisksWithoutCounts(t *testing.T) {
	dataDir := t.TempDir()
	lioRoot := writeFakeLIO(t, map[string]string{
		"c-worker0-disk0": filepath.Join(dataDir, "worker0-disk0.img"),
	})
	disks := teardownDisks(lioRoot, "c", dataDir, "2003-01", 0, 0)
	if len(disks) != 1 || disks[0].backstoreName != "c-worker0-disk0" {
		t.Errorf("disks = %v, want the one disk the kernel holds", diskKeys(disks))
	}
}

// Another cluster's disks must never join this cluster's teardown.
func TestTeardownDisksIgnoresOtherClusters(t *testing.T) {
	dataDir := t.TempDir()
	lioRoot := writeFakeLIO(t, map[string]string{
		"c-worker0-disk0":     filepath.Join(dataDir, "worker0-disk0.img"),
		"other-worker0-disk0": "/somewhere/else/worker0-disk0.img",
	})
	for _, d := range teardownDisks(lioRoot, "c", dataDir, "2003-01", 0, 0) {
		if d.backstoreName == "other-worker0-disk0" {
			t.Errorf("teardown set includes another cluster's disk: %v", diskKeys([]iscsiDisk{d}))
		}
	}
}

func TestLIOClusterDisks(t *testing.T) {
	st := lio.State{
		StorageObjects: []lio.StorageObject{
			// Exported normally.
			fileio("c-worker0-disk0", "/data/c/worker0-disk0.img"),
			// Left behind by a teardown that removed the target first.
			fileio("c-worker1-disk0", "/data/c/worker1-disk0.img"),
			// Not something rooket could have created.
			fileio("vmstore", "/srv/vm/disk.img"),
			// A cluster component that is not a valid cluster name.
			fileio("Bad_Name-worker0-disk0", "/data/x.img"),
		},
		Targets: []lio.Target{
			target("iqn.2003-01.local.rooket:c-worker0-disk0", "c-worker0-disk0"),
			// A target whose backstore create failed: no LUN, no image.
			target("iqn.2003-01.local.rooket:c-worker2-disk0"),
			target("iqn.2005-03.com.example:vmstore", "vmstore"),
		},
	}
	got := lioClusterDisks(st, "2003-01")
	if len(got) != 1 {
		t.Fatalf("clusters = %v, want only c", got)
	}
	want := []string{
		"c-worker0-disk0|iqn.2003-01.local.rooket:c-worker0-disk0|/data/c/worker0-disk0.img",
		"c-worker1-disk0|iqn.2003-01.local.rooket:c-worker1-disk0|/data/c/worker1-disk0.img",
		"c-worker2-disk0|iqn.2003-01.local.rooket:c-worker2-disk0|",
	}
	gotKeys := diskKeys(got["c"])
	if len(gotKeys) != len(want) {
		t.Fatalf("disks = %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Errorf("disk %d = %q, want %q", i, gotKeys[i], want[i])
		}
	}
}

func TestParseRooketIQN(t *testing.T) {
	cases := []struct {
		iqn       string
		cluster   string
		backstore string
		ok        bool
	}{
		{"iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker3-disk0", "home-jhoblitt-github-rook", "home-jhoblitt-github-rook-worker3-disk0", true},
		{"iqn.2003-01.local.rooket:c-worker0-disk1", "c", "c-worker0-disk1", true},
		{"iqn.2005-03.com.example:disk0", "", "", false},
		{"iqn.2003-1.local.rooket:c-worker0-disk0", "", "", false},
		{"iqn.2003-01.local.rooket:c-worker0", "", "", false},
		{"iqn.2003-01.local.rooket:UPPER-worker0-disk0", "", "", false},
		{"not an iqn", "", "", false},
	}
	for _, c := range cases {
		disk, cluster, ok := parseRooketIQN(c.iqn)
		if ok != c.ok || cluster != c.cluster || disk.backstoreName != c.backstore {
			t.Errorf("parseRooketIQN(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.iqn, disk.backstoreName, cluster, ok, c.backstore, c.cluster, c.ok)
		}
	}
}

// The first set to name a disk wins, so the sources that know the worker
// index and a live image path are not overwritten by the ones that don't.
func TestUnionDisks(t *testing.T) {
	rich := iscsiDisk{workerIdx: 2, imgPath: "/data/c/worker2-disk0.img", backstoreName: "c-worker2-disk0", targetIQN: "iqn.2003-01.local.rooket:c-worker2-disk0"}
	poor := iscsiDisk{backstoreName: "c-worker2-disk0", targetIQN: "iqn.2003-01.local.rooket:c-worker2-disk0"}
	extra := iscsiDisk{backstoreName: "c-worker9-disk0", targetIQN: "iqn.2003-01.local.rooket:c-worker9-disk0"}

	got := unionDisks([]iscsiDisk{rich}, []iscsiDisk{poor, extra})
	if len(got) != 2 {
		t.Fatalf("unionDisks = %v, want 2 disks", diskKeys(got))
	}
	if got[0] != rich {
		t.Errorf("disk[0] = %+v, want the first source's entry %+v", got[0], rich)
	}
	if got[1] != extra {
		t.Errorf("disk[1] = %+v, want %+v", got[1], extra)
	}
}
