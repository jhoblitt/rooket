package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jhoblitt/rooket/internal/engine"
)

func TestParseStrandedByPathLink(t *testing.T) {
	cases := []struct {
		name          string
		link          string
		wantOK        bool
		wantCluster   string
		wantBackstore string
		wantTargetIQN string
	}{
		{
			name:          "cluster name containing dashes",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0-lun-0",
			wantOK:        true,
			wantCluster:   "home-jhoblitt-github-rook",
			wantBackstore: "home-jhoblitt-github-rook-worker0-disk0",
			wantTargetIQN: "iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0",
		},
		{
			name:          "short cluster name",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker0-disk0-lun-0",
			wantOK:        true,
			wantCluster:   "rook3",
			wantBackstore: "rook3-worker0-disk0",
			wantTargetIQN: "iqn.2003-01.local.rooket:rook3-worker0-disk0",
		},
		{
			name:          "multi-worker multi-disk index",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook6-worker2-disk3-lun-0",
			wantOK:        true,
			wantCluster:   "rook6",
			wantBackstore: "rook6-worker2-disk3",
			wantTargetIQN: "iqn.2003-01.local.rooket:rook6-worker2-disk3",
		},
		{
			name:   "non-rooket iSCSI target ignored",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.com.example:cluster-worker0-disk0-lun-0",
			wantOK: false,
		},
		{
			name:   "non-iSCSI by-path entry ignored",
			link:   "pci-0000:00:1f.2-ata-1.0-part1",
			wantOK: false,
		},
		{
			name:   "malformed: no cluster before worker/disk suffix",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:worker0-disk0-lun-0",
			wantOK: false,
		},
		{
			name:   "malformed: empty backstore name",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:-lun-0",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disk, cluster, ok := parseStrandedByPathLink(tc.link)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (disk=%+v cluster=%q)", ok, tc.wantOK, disk, cluster)
			}
			if !ok {
				if cluster != "" {
					t.Errorf("cluster = %q on a rejected link, want empty", cluster)
				}
				return
			}
			if cluster != tc.wantCluster {
				t.Errorf("cluster = %q, want %q", cluster, tc.wantCluster)
			}
			if disk.backstoreName != tc.wantBackstore {
				t.Errorf("backstoreName = %q, want %q", disk.backstoreName, tc.wantBackstore)
			}
			if disk.targetIQN != tc.wantTargetIQN {
				t.Errorf("targetIQN = %q, want %q", disk.targetIQN, tc.wantTargetIQN)
			}
		})
	}
}

func TestDiscoverStrandedByPath(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker0-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker1-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.com.example:other-worker0-disk0-lun-0", // non-rooket, ignored
		"pci-0000:00:1f.2-ata-1.0-part1", // non-iSCSI, ignored
	}
	for _, n := range names {
		// Real by-path entries are symlinks to /dev/sdX; only the entry name is
		// consulted here, but making the fixture a regular file would silently
		// stop catching a change that starts following the link.
		if err := os.Symlink("/dev/sdz", filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}

	found, err := discoverStrandedByPath(dir)
	if err != nil {
		t.Fatalf("discoverStrandedByPath: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d clusters, want 2: %+v", len(found), found)
	}
	if len(found["rook3"]) != 2 {
		t.Errorf("rook3 disks = %d, want 2", len(found["rook3"]))
	}
	if len(found["home-jhoblitt-github-rook"]) != 1 {
		t.Errorf("home-jhoblitt-github-rook disks = %d, want 1", len(found["home-jhoblitt-github-rook"]))
	}

	t.Run("missing directory is not an error", func(t *testing.T) {
		found, err := discoverStrandedByPath(filepath.Join(dir, "nonexistent"))
		if err != nil {
			t.Fatalf("discoverStrandedByPath on missing dir: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("found = %+v, want none", found)
		}
	})
}

// The two strandable names are chosen so their insertion order into the map
// (irrelevant — Go randomizes map iteration) cannot coincide with the
// asserted sorted order by luck across runs: comparing directly against a
// sorted "want" with no re-sort on the "got" side is what actually exercises
// strandableClusters' own sort, rather than the test's.
func TestStrandableClusters(t *testing.T) {
	found := map[string][]iscsiDisk{
		"zeta":      {{targetIQN: "iqn.2003-01.local.rooket:zeta-worker0-disk0"}},
		"alpha":     {{targetIQN: "iqn.2003-01.local.rooket:alpha-worker0-disk0"}},
		"live":      {{targetIQN: "iqn.2003-01.local.rooket:live-worker0-disk0"}},
		"has-state": {{targetIQN: "iqn.2003-01.local.rooket:has-state-worker0-disk0"}},
	}
	live := map[string][]engine.Engine{
		"live": {engine.Podman},
	}
	hasState := map[string]bool{
		"has-state": true,
	}

	got := strandableClusters(found, live, hasState)
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("strandableClusters = %v, want %v", got, want)
	}
}

// diskSet is an order-independent comparison key for []iscsiDisk in tests
// below: prunePlan's disks order depends on strandableClusters' internal map
// iteration within a cluster's own found[c] slice construction order, which
// is deterministic per cluster but not a contract worth pinning here.
func diskSet(disks []iscsiDisk) map[string]bool {
	s := map[string]bool{}
	for _, d := range disks {
		s[d.targetIQN] = true
	}
	return s
}

func TestPrunePlan(t *testing.T) {
	orphanDisk := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:orphan-worker0-disk0"}
	liveDisk := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:live-worker0-disk0"}
	strandedDisk := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:stranded-worker0-disk0"}
	untouchedOrphanImg := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:untouched-worker0-disk0"}

	stateNames := []string{"live", "orphan", "untouched"}
	live := map[string][]engine.Engine{
		"live": {engine.Podman},
	}
	hasState := map[string]bool{
		"live":      true,
		"orphan":    true,
		"untouched": true,
	}
	strandedFound := map[string][]iscsiDisk{
		"live":      {liveDisk},
		"orphan":    {orphanDisk},
		"stranded":  {strandedDisk},
		"untouched": {untouchedOrphanImg},
	}

	orphans, disks := prunePlan(stateNames, live, hasState, strandedFound)

	if want := []string{"orphan", "untouched"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}

	got := diskSet(disks)
	want := diskSet([]iscsiDisk{orphanDisk, strandedDisk, untouchedOrphanImg})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("disks = %v, want %v", got, want)
	}
	// The concrete regression: a live cluster's by-path entries must never
	// enter the teardown batch, no matter how it was discovered (it has a
	// state dir here, but the same must hold via strandableClusters too).
	if got[liveDisk.targetIQN] {
		t.Error("live cluster's by-path disk leaked into the teardown batch")
	}
}

// A hasState cluster's by-path disks must be unioned in even when its
// worker*-disk*.img reconstruction would find nothing (images already
// removed) or would reconstruct the wrong IQN (a different --iqn-date) —
// prunePlan can't see either failure mode from the caller's side, so it must
// always union, not conditionally prefer one source.
func TestPrunePlanUnionsOrphanByPathEvenWithNoReconstructableImages(t *testing.T) {
	stateNames := []string{"orphan"}
	live := map[string][]engine.Engine{}
	hasState := map[string]bool{"orphan": true}
	strandedFound := map[string][]iscsiDisk{
		"orphan": {{targetIQN: "iqn.1999-01.local.rooket:orphan-worker0-disk0"}},
	}

	orphans, disks := prunePlan(stateNames, live, hasState, strandedFound)
	if want := []string{"orphan"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	if len(disks) != 1 || disks[0].targetIQN != "iqn.1999-01.local.rooket:orphan-worker0-disk0" {
		t.Errorf("disks = %+v, want the orphan's by-path disk", disks)
	}
}

func TestPrunePlanNoFindings(t *testing.T) {
	orphans, disks := prunePlan([]string{"solo"}, map[string][]engine.Engine{}, map[string]bool{"solo": true}, nil)
	if want := []string{"solo"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	if len(disks) != 0 {
		t.Errorf("disks = %+v, want none", disks)
	}
}

func TestPruneExecute(t *testing.T) {
	t.Run("teardown runs before removal, and its failure blocks every removal", func(t *testing.T) {
		var calls []string
		teardown := func(d []iscsiDisk) error {
			calls = append(calls, "teardown:"+d[0].targetIQN)
			return errors.New("targetcli boom")
		}
		remove := func(p string) error {
			calls = append(calls, "remove:"+p)
			return nil
		}
		disks := []iscsiDisk{{targetIQN: "iqn.x"}}
		err := pruneExecute("/root", []string{"orphan-a", "orphan-b"}, disks, teardown, remove, io.Discard)
		if err == nil {
			t.Fatal("pruneExecute = nil error, want the teardown failure")
		}
		if !reflect.DeepEqual(calls, []string{"teardown:iqn.x"}) {
			t.Errorf("calls = %v, want only the teardown call — no removal must follow a teardown failure", calls)
		}
	})

	t.Run("no disks skips teardown entirely but still removes every orphan", func(t *testing.T) {
		teardownCalled := false
		teardown := func(d []iscsiDisk) error {
			teardownCalled = true
			return nil
		}
		var removed []string
		remove := func(p string) error {
			removed = append(removed, p)
			return nil
		}
		if err := pruneExecute("/root", []string{"a", "b"}, nil, teardown, remove, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		if teardownCalled {
			t.Error("teardown called with no disks to tear down")
		}
		want := []string{filepath.Join("/root", "a"), filepath.Join("/root", "b")}
		if !reflect.DeepEqual(removed, want) {
			t.Errorf("removed = %v, want %v", removed, want)
		}
	})

	t.Run("a removal failure warns but does not block the next orphan's removal", func(t *testing.T) {
		var removed []string
		remove := func(p string) error {
			removed = append(removed, p)
			if p == filepath.Join("/root", "a") {
				return errors.New("permission denied")
			}
			return nil
		}
		if err := pruneExecute("/root", []string{"a", "b"}, nil, func([]iscsiDisk) error { return nil }, remove, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		want := []string{filepath.Join("/root", "a"), filepath.Join("/root", "b")}
		if !reflect.DeepEqual(removed, want) {
			t.Errorf("removed = %v, want both attempted despite the first's failure: %v", want, removed)
		}
	})

	t.Run("teardown sees the full disks batch in one call", func(t *testing.T) {
		var gotDisks []iscsiDisk
		teardown := func(d []iscsiDisk) error {
			gotDisks = d
			return nil
		}
		disks := []iscsiDisk{{targetIQN: "iqn.a"}, {targetIQN: "iqn.b"}}
		if err := pruneExecute("/root", nil, disks, teardown, func(string) error { return nil }, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		if !reflect.DeepEqual(gotDisks, disks) {
			t.Errorf("teardown saw %v, want %v (one call, not per-disk)", gotDisks, disks)
		}
	})
}
