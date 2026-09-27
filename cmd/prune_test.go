package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
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

// The by-path scan sees a target only while a session is logged in, which is
// the state a partial teardown does not leave things in. Reading the kernel's
// configuration is what finds the leftovers: a target whose session is gone,
// and a backstore whose target was already deleted.
func TestDiscoverStrandedFindsWhatByPathCannot(t *testing.T) {
	byPath := t.TempDir()
	if err := os.Symlink("/dev/sdz", filepath.Join(byPath,
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:c-worker0-disk0-lun-0")); err != nil {
		t.Fatal(err)
	}
	lioRoot := writeFakeLIO(t, map[string]string{
		"c-worker0-disk0": "/data/c/worker0-disk0.img", // also seen via by-path
		"c-worker1-disk0": "/data/c/worker1-disk0.img", // logged out: no symlink
		"c-worker2-disk0": "/data/c/worker2-disk0.img", // no target at all
	}, "c-worker2-disk0")

	found, err := discoverStranded(lioRoot, byPath, "2003-01")
	if err != nil {
		t.Fatalf("discoverStranded: %v", err)
	}
	if len(found["c"]) != 3 {
		t.Fatalf("c disks = %v, want all 3 the kernel holds", diskKeys(found["c"]))
	}

	t.Run("falls back to by-path when the kernel cannot be read", func(t *testing.T) {
		found, err := discoverStranded(filepath.Join(t.TempDir(), "absent"), byPath, "2003-01")
		if err != nil {
			t.Fatalf("discoverStranded: %v", err)
		}
		if len(found["c"]) != 1 {
			t.Errorf("c disks = %v, want the one by-path names", diskKeys(found["c"]))
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

// allDisks flattens prunePlan's per-cluster disks.
func allDisks(byCluster map[string][]iscsiDisk) []iscsiDisk {
	var disks []iscsiDisk
	for _, d := range byCluster {
		disks = append(disks, d...)
	}
	return disks
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

	orphans, parked, disks := prunePlan(t.TempDir(), stateNames, live, hasState, strandedFound, false)

	if len(parked) != 0 {
		t.Errorf("parked = %v, want none", parked)
	}
	if want := []string{"orphan", "untouched"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}

	got := diskSet(allDisks(disks))
	want := diskSet([]iscsiDisk{orphanDisk, strandedDisk, untouchedOrphanImg})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("disks = %v, want %v", got, want)
	}
	// Each disk is filed under its own cluster: prune tears a cluster's disks
	// down only while it holds that cluster's lock.
	wantByCluster := map[string][]iscsiDisk{
		"orphan":    {orphanDisk},
		"stranded":  {strandedDisk},
		"untouched": {untouchedOrphanImg},
	}
	if !reflect.DeepEqual(disks, wantByCluster) {
		t.Errorf("disks by cluster = %+v, want %+v", disks, wantByCluster)
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

	orphans, _, disks := prunePlan(t.TempDir(), stateNames, live, hasState, strandedFound, false)
	if want := []string{"orphan"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	if d := disks["orphan"]; len(d) != 1 || d[0].targetIQN != "iqn.1999-01.local.rooket:orphan-worker0-disk0" {
		t.Errorf("disks = %+v, want the orphan's by-path disk", disks)
	}
}

func TestPrunePlanNoFindings(t *testing.T) {
	orphans, _, disks := prunePlan(t.TempDir(), []string{"solo"}, map[string][]engine.Engine{}, map[string]bool{"solo": true}, nil, false)
	if want := []string{"solo"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	if len(disks) != 0 {
		t.Errorf("disks = %+v, want none", disks)
	}
}

// A plain 'rooket down' deliberately leaves the state dir, its disk images,
// and its iSCSI targets behind so the next 'up' reuses them. That parked
// cluster is indistinguishable from garbage by liveness alone, so prune
// judges it by its clone: while the clone it was created from still exists,
// the cluster is parked, not abandoned, and neither it nor its targets may
// be swept.
func TestPrunePlanKeepsClustersWhoseCloneStillExists(t *testing.T) {
	root := t.TempDir()
	clone := t.TempDir()
	mkState := func(name, clonePath string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if clonePath != "" {
			writeFile(t, filepath.Join(dir, clonePathFile), clonePath+"\n")
		}
	}
	mkState("parked", clone)
	mkState("abandoned", filepath.Join(t.TempDir(), "deleted-clone"))
	mkState("unknown", "")

	parkedDisk := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:parked-worker0-disk0"}
	abandonedDisk := iscsiDisk{targetIQN: "iqn.2003-01.local.rooket:abandoned-worker0-disk0"}
	stateNames := []string{"abandoned", "parked", "unknown"}
	hasState := map[string]bool{"abandoned": true, "parked": true, "unknown": true}
	strandedFound := map[string][]iscsiDisk{
		"parked":    {parkedDisk},
		"abandoned": {abandonedDisk},
	}

	orphans, parked, disks := prunePlan(root, stateNames, map[string][]engine.Engine{}, hasState, strandedFound, false)

	if want := []string{"abandoned", "unknown"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}
	if want := []string{"parked"}; !reflect.DeepEqual(parked, want) {
		t.Errorf("parked = %v, want %v", parked, want)
	}
	if diskSet(allDisks(disks))[parkedDisk.targetIQN] {
		t.Error("parked cluster's iSCSI target leaked into the teardown batch")
	}

	// --include-parked is the escape hatch: it puts them back in scope.
	orphans, parked, disks = prunePlan(root, stateNames, map[string][]engine.Engine{}, hasState, strandedFound, true)
	if want := []string{"abandoned", "parked", "unknown"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans with --include-parked = %v, want %v", orphans, want)
	}
	if len(parked) != 0 {
		t.Errorf("parked with --include-parked = %v, want none", parked)
	}
	if !diskSet(allDisks(disks))[parkedDisk.targetIQN] {
		t.Error("--include-parked did not bring the parked cluster's target into the teardown batch")
	}
}

// A released cluster may have no rook clone at all; prune must still judge
// it parked while its recorded configuration directory exists, exactly as a
// clone-built cluster is judged by its clone (see
// TestPrunePlanKeepsClustersWhoseCloneStillExists). This is the regression
// prunePlan calling cloneGone instead of ownerGone would reintroduce: with
// no clone recorded at all, cloneGone reads every released cluster as
// abandoned regardless of its configuration directory.
func TestPrunePlanKeepsReleasedClusterWhoseConfigDirStillExists(t *testing.T) {
	root := t.TempDir()
	configDir := t.TempDir()
	dir := filepath.Join(root, "released")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(clusterSource{RookVersion: "v1.20.7", ConfigDir: configDir})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, sourceFile), string(data))

	stateNames := []string{"released"}
	hasState := map[string]bool{"released": true}

	orphans, parked, _ := prunePlan(root, stateNames, map[string][]engine.Engine{}, hasState, nil, false)
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none", orphans)
	}
	if want := []string{"released"}; !reflect.DeepEqual(parked, want) {
		t.Errorf("parked = %v, want %v", parked, want)
	}

	// --include-parked is the escape hatch here too.
	orphans, parked, _ = prunePlan(root, stateNames, map[string][]engine.Engine{}, hasState, nil, true)
	if want := []string{"released"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans with --include-parked = %v, want %v", orphans, want)
	}
	if len(parked) != 0 {
		t.Errorf("parked with --include-parked = %v, want none", parked)
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
		disks := map[string][]iscsiDisk{"orphan-a": {{targetIQN: "iqn.x"}}}
		err := pruneExecute(t.TempDir(), []string{"orphan-a", "orphan-b"}, disks, teardown, remove, io.Discard)
		if err == nil {
			t.Fatal("pruneExecute = nil error, want the teardown failure")
		}
		if !reflect.DeepEqual(calls, []string{"teardown:iqn.x"}) {
			t.Errorf("calls = %v, want only the teardown call — no removal must follow a teardown failure", calls)
		}
		for _, n := range []string{"orphan-a", "orphan-b"} {
			if _, ok := heldFile(n); ok {
				t.Errorf("prune still holds the lock of %s after its teardown failed", n)
			}
		}
	})

	t.Run("no disks skips teardown entirely but still removes every orphan", func(t *testing.T) {
		// A real root: the orphan loop takes each cluster's lock beside the
		// directory it is about to remove, so this cannot be a fake path.
		root := t.TempDir()
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
		if err := pruneExecute(root, []string{"a", "b"}, nil, teardown, remove, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		if teardownCalled {
			t.Error("teardown called with no disks to tear down")
		}
		want := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
		if !reflect.DeepEqual(removed, want) {
			t.Errorf("removed = %v, want %v", removed, want)
		}
	})

	t.Run("a removal failure warns but does not block the next orphan's removal", func(t *testing.T) {
		root := t.TempDir()
		var removed []string
		remove := func(p string) error {
			removed = append(removed, p)
			if p == filepath.Join(root, "a") {
				return errors.New("permission denied")
			}
			return nil
		}
		if err := pruneExecute(root, []string{"a", "b"}, nil, func([]iscsiDisk) error { return nil }, remove, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		want := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
		if !reflect.DeepEqual(removed, want) {
			t.Errorf("removed = %v, want both attempted despite the first's failure: %v", want, removed)
		}
	})

	// As with down, a lock file left beside a state dir that is gone would
	// outlive everything else of its cluster.
	t.Run("an orphan's lock file goes with its state dir, and stays with one that survives", func(t *testing.T) {
		root := t.TempDir()
		for _, n := range []string{"gone", "kept"} {
			if err := os.Mkdir(filepath.Join(root, n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		remove := func(p string) error {
			if p == filepath.Join(root, "kept") {
				return errors.New("permission denied")
			}
			return os.RemoveAll(p)
		}
		if err := pruneExecute(root, []string{"gone", "kept"}, nil, func([]iscsiDisk) error { return nil }, remove, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		lock := func(name string) string {
			p, err := clusterLockPath(root, name)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		if _, err := os.Stat(lock("gone")); !os.IsNotExist(err) {
			t.Errorf("the lock file of the orphan prune removed survived (stat: %v)", err)
		}
		if _, err := os.Stat(lock("kept")); err != nil {
			t.Errorf("the lock file of the orphan whose state dir survived was removed: %v", err)
		}
	})

	t.Run("teardown sees the full disks batch in one call", func(t *testing.T) {
		var batches [][]iscsiDisk
		teardown := func(d []iscsiDisk) error {
			batches = append(batches, d)
			return nil
		}
		a := []iscsiDisk{{targetIQN: "iqn.a0"}, {targetIQN: "iqn.a1"}}
		b := []iscsiDisk{{targetIQN: "iqn.b0"}}
		disks := map[string][]iscsiDisk{"a": a, "b": b}
		if err := pruneExecute(t.TempDir(), nil, disks, teardown, func(string) error { return nil }, io.Discard); err != nil {
			t.Fatalf("pruneExecute: %v", err)
		}
		if want := [][]iscsiDisk{slices.Concat(a, b)}; !reflect.DeepEqual(batches, want) {
			t.Errorf("teardown saw %v, want %v (one call for every cluster, not one per cluster or disk)", batches, want)
		}
	})
}

// clusterLockHeld reports whether anyone, this process included, holds the lock
// of cluster name under root. It tries the flock through an open file
// description of its own, which contends with every other one, this process's
// included, and lets go at once.
func clusterLockHeld(t *testing.T, root, name string) bool {
	t.Helper()
	path, err := clusterLockPath(root, name)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

// prune tears down a cluster's iSCSI targets, and removes its state dir, only
// while it holds the cluster's lock, a stranded cluster with no state dir
// included. A cluster another rooket holds — an up that has set up its targets
// but not yet created its kind cluster, say — keeps its targets and its state,
// and prune reports it and still succeeds.
func TestPruneExecuteTearsDownOnlyTheClustersItHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := stateDirRoot()
	if err != nil {
		t.Fatal(err)
	}
	const orphan, stranded, busyOrphan, busyStranded = "w5-orphan", "w5-stranded", "w5-busy-orphan", "w5-busy-stranded"
	for _, n := range []string{orphan, busyOrphan} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lockClusterExternally(t, busyOrphan)
	lockClusterExternally(t, busyStranded)
	disks := map[string][]iscsiDisk{}
	for _, n := range []string{orphan, stranded, busyOrphan, busyStranded} {
		id := n + "-worker0-disk0"
		disks[n] = []iscsiDisk{{backstoreName: id, targetIQN: "iqn.2003-01.local.rooket:" + id}}
	}

	var tornDown []string
	teardown := func(batch []iscsiDisk) error {
		for _, d := range batch {
			_, n, ok := parseRooketIQN(d.targetIQN)
			if !ok {
				t.Fatalf("%q is not one of rooket's target IQNs", d.targetIQN)
			}
			tornDown = append(tornDown, n)
			if !clusterLockHeld(t, root, n) {
				t.Errorf("the targets of %s were torn down while its lock was not held", n)
			}
		}
		return nil
	}
	var removed []string
	remove := func(p string) error {
		removed = append(removed, filepath.Base(p))
		return os.RemoveAll(p)
	}
	var out strings.Builder
	if err := pruneExecute(root, []string{busyOrphan, orphan}, disks, teardown, remove, &out); err != nil {
		t.Fatalf("pruneExecute = %v, want success: a cluster prune cannot lock is reported, not an error", err)
	}

	slices.Sort(tornDown)
	if want := []string{orphan, stranded}; !slices.Equal(tornDown, want) {
		t.Errorf("tore down the targets of %v, want only the unheld clusters' %v", tornDown, want)
	}
	if want := []string{orphan}; !slices.Equal(removed, want) {
		t.Errorf("removed the state dirs of %v, want only %v", removed, want)
	}
	for _, n := range []string{busyOrphan, busyStranded} {
		if !strings.Contains(out.String(), fmt.Sprintf("skipping cluster %q", n)) {
			t.Errorf("prune did not report skipping %s, which another rooket holds:\n%s", n, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, busyOrphan)); err != nil {
		t.Errorf("the state dir of a cluster another rooket holds was removed: %v", err)
	}
	// Neither cluster prune tore down has a state dir left for its lock file to
	// stand beside; the held clusters' lock files are their holders'.
	for _, n := range []string{orphan, stranded} {
		if _, err := os.Stat(filepath.Join(root, n+".lock")); !os.IsNotExist(err) {
			t.Errorf("the lock file of %s survived its teardown (stat: %v)", n, err)
		}
	}
	for _, n := range []string{busyOrphan, busyStranded} {
		if _, err := os.Stat(filepath.Join(root, n+".lock")); err != nil {
			t.Errorf("the lock file another rooket holds for %s was removed: %v", n, err)
		}
	}
}

// A state dir whose name cannot be a cluster's — made by hand, say — has no
// lock anyone could hold, so prune tears it down and removes it unlocked, as
// down --all does.
func TestPruneExecuteRemovesAStateDirNoClusterCouldOwn(t *testing.T) {
	root := t.TempDir()
	const name = "Hand_Made"
	if validateClusterName(name) == nil {
		t.Fatalf("%q is a valid cluster name", name)
	}
	if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
	disks := map[string][]iscsiDisk{name: {{targetIQN: "iqn.2003-01.local.rooket:" + name + "-worker0-disk0"}}}
	var tornDown []iscsiDisk
	teardown := func(batch []iscsiDisk) error {
		tornDown = batch
		return nil
	}

	var out strings.Builder
	if err := pruneExecute(root, []string{name}, disks, teardown, os.RemoveAll, &out); err != nil {
		t.Fatalf("pruneExecute = %v, want success", err)
	}
	if !reflect.DeepEqual(tornDown, disks[name]) {
		t.Errorf("tore down %v, want %v", tornDown, disks[name])
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("state root holds %v, want %s removed and nothing left:\n%s", entries, name, out.String())
	}
}
