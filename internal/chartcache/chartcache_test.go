package chartcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakePull unpacks a stand-in chart the way helm pull --untar does, counting
// calls per chart.
func fakePull(calls map[string]int) Puller {
	return func(dir, chart, version string) error {
		calls[chart]++
		c := filepath.Join(dir, chart)
		if err := os.MkdirAll(c, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c, "Chart.yaml"), []byte("name: "+chart+"\nversion: "+version+"\n"), 0o644)
	}
}

// entriesIn lists root without its lock file, which outlives every pull.
func entriesIn(t *testing.T, root string) []string {
	t.Helper()
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		if de.Name() != lockName {
			names = append(names, de.Name())
		}
	}
	return names
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"v1.20.7", "v1.21.0-beta.0"} {
		if err := ValidVersion(v); err != nil {
			t.Errorf("ValidVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"", "1.20.7", "v1.20", "v1.20.x", "~1.20", "v1.20.7 "} {
		if err := ValidVersion(v); err == nil {
			t.Errorf("ValidVersion(%q) = nil, want it rejected", v)
		}
	}
}

func TestEnsurePullsEachChartOnce(t *testing.T) {
	root := t.TempDir()
	calls := map[string]int{}

	entry, err := Ensure(root, "v1.20.7", fakePull(calls))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != filepath.Join(root, "v1.20.7") {
		t.Errorf("entry = %q, want %q", entry, filepath.Join(root, "v1.20.7"))
	}
	for _, c := range Charts {
		if _, err := os.Stat(filepath.Join(entry, "deploy", "charts", c, "Chart.yaml")); err != nil {
			t.Errorf("chart %s not laid out like a clone's: %v", c, err)
		}
	}
	if _, err := Ensure(root, "v1.20.7", fakePull(calls)); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	for _, c := range Charts {
		if calls[c] != 1 {
			t.Errorf("%s pulled %d times, want once: a present entry is reused", c, calls[c])
		}
	}
}

// A pull that fails part way must leave nothing a later run would trust.
func TestEnsureLeavesNoEntryAfterAFailedPull(t *testing.T) {
	root := t.TempDir()
	boom := errors.New("network down")
	pull := func(dir, chart, version string) error {
		if chart == "rook-ceph-cluster" {
			return boom
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	if _, err := Ensure(root, "v1.20.7", pull); !errors.Is(err, boom) {
		t.Fatalf("Ensure = %v, want the pull's error", err)
	}
	if left := entriesIn(t, root); len(left) != 0 {
		t.Errorf("root holds %v after a failed pull, want nothing", left)
	}
}

// An entry that appears mid-pull through no lock — made by hand, or by a
// process that does not take the lock — is used rather than fought over.
func TestEnsureYieldsToAConcurrentWinner(t *testing.T) {
	root := t.TempDir()
	winner := filepath.Join(root, "v1.20.7")
	pull := func(dir, chart, version string) error {
		if chart == Charts[len(Charts)-1] {
			for _, c := range Charts {
				cd := filepath.Join(winner, "deploy", "charts", c)
				if err := os.MkdirAll(cd, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cd, "Chart.yaml"), []byte("name: "+c+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	entry, err := Ensure(root, "v1.20.7", pull)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != winner {
		t.Errorf("entry = %q, want the winner's %q", entry, winner)
	}
	if left := entriesIn(t, root); len(left) != 1 {
		t.Errorf("root holds %v, want only the winner's entry: the loser's copy is discarded", left)
	}
}

func TestEnsureRejectsAnIncompleteEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v1.20.7", "deploy", "charts", "rook-ceph"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(root, "v1.20.7", fakePull(map[string]int{}))
	if err == nil {
		t.Fatal("Ensure = nil error, want the incomplete entry reported")
	}
	if want := filepath.Join(root, "v1.20.7"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the entry %s to delete", err, want)
	}
}

// pullRecorder is a Puller that counts pulls per version and chart, notes
// whether two ever ran at once, and fails the first pull when told to.
type pullRecorder struct {
	mu        sync.Mutex
	calls     map[string]int // by "<version>/<chart>"
	active    int
	overlap   bool
	failFirst bool
}

func (r *pullRecorder) pull(dir, chart, version string) error {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[version+"/"+chart]++
	r.active++
	if r.active > 1 {
		r.overlap = true
	}
	fail := r.failFirst
	r.failFirst = false
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()

	// Long enough that a second pull let through alongside lands inside this one.
	time.Sleep(20 * time.Millisecond)
	if fail {
		return errors.New("network down")
	}
	return fakePull(map[string]int{})(dir, chart, version)
}

// missTogether holds each of n Ensure calls just after it finds no entry until
// all n have, so they contend for the pull instead of a late one simply finding
// an early one's entry. The wait is bounded because an Ensure that took the
// lock before reaching the barrier would otherwise hang the test, not fail it.
func missTogether(t *testing.T, n int) {
	var arrived sync.WaitGroup
	arrived.Add(n)
	all := make(chan struct{})
	go func() {
		arrived.Wait()
		close(all)
	}()
	prev := afterMiss
	afterMiss = func() {
		arrived.Done()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			t.Error("the Ensure calls never all missed together: one holds the lock before it misses")
		}
	}
	t.Cleanup(func() { afterMiss = prev })
}

// ensureAll runs Ensure for each version concurrently.
func ensureAll(root string, pull Puller, versions ...string) ([]string, []error) {
	entries := make([]string, len(versions))
	errs := make([]error, len(versions))
	var wg sync.WaitGroup
	for i, v := range versions {
		wg.Go(func() { entries[i], errs[i] = Ensure(root, v, pull) })
	}
	wg.Wait()
	return entries, errs
}

// holdLock takes the pull lock as another process would, until the test ends.
func holdLock(t *testing.T, root string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take the pull lock: %v", err)
	}
}

func shortenLockWait(t *testing.T, d time.Duration) {
	prev := lockWait
	lockWait = d
	t.Cleanup(func() { lockWait = prev })
}

// Goroutines stand in for rookets here and below: a flock taken through
// separate opens of one file excludes within a process as it does across them.
func TestEnsureSerializesPullsOfOneVersion(t *testing.T) {
	root := t.TempDir()
	missTogether(t, 2)
	var r pullRecorder

	entries, errs := ensureAll(root, r.pull, "v1.20.7", "v1.20.7")

	want := filepath.Join(root, "v1.20.7")
	for i := range entries {
		if errs[i] != nil {
			t.Errorf("Ensure %d: %v", i, errs[i])
		} else if entries[i] != want {
			t.Errorf("Ensure %d = %q, want %q", i, entries[i], want)
		}
	}
	if r.overlap {
		t.Error("two pulls ran at once through the shared helm home")
	}
	for _, c := range Charts {
		if n := r.calls["v1.20.7/"+c]; n != 1 {
			t.Errorf("%s pulled %d times, want once: the run that waited must use the entry the other installed", c, n)
		}
	}
}

// Different versions get different entries but pull through the same helm
// home, so they are serialized too.
func TestEnsureSerializesPullsOfDifferentVersions(t *testing.T) {
	root := t.TempDir()
	missTogether(t, 2)
	var r pullRecorder

	versions := []string{"v1.20.7", "v1.19.0"}
	entries, errs := ensureAll(root, r.pull, versions...)

	for i, v := range versions {
		if errs[i] != nil {
			t.Errorf("Ensure %s: %v", v, errs[i])
		} else if want := filepath.Join(root, v); entries[i] != want {
			t.Errorf("Ensure %s = %q, want %q", v, entries[i], want)
		}
		for _, c := range Charts {
			if n := r.calls[v+"/"+c]; n != 1 {
				t.Errorf("%s %s pulled %d times, want once", c, v, n)
			}
		}
	}
	if r.overlap {
		t.Error("two pulls ran at once through the shared helm home")
	}
}

// A run that waited out a failed pull finds no entry and pulls it itself.
func TestEnsurePullsAfterTheHolderFails(t *testing.T) {
	root := t.TempDir()
	missTogether(t, 2)
	r := pullRecorder{failFirst: true}

	entries, errs := ensureAll(root, r.pull, "v1.20.7", "v1.20.7")

	var failed, got int
	for i := range entries {
		switch {
		case errs[i] != nil:
			failed++
		case entries[i] == filepath.Join(root, "v1.20.7"):
			got++
		}
	}
	if failed != 1 || got != 1 {
		t.Errorf("errors %v, entries %q: want the first puller failed and the second to have pulled the entry", errs, entries)
	}
	if r.overlap {
		t.Error("two pulls ran at once through the shared helm home")
	}
}

// A cached version is returned without the lock, so a run whose charts are
// already here never queues behind another's pull.
func TestEnsureHitTakesNoLock(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, "v1.20.7", fakePull(map[string]int{})); err != nil {
		t.Fatalf("seed the entry: %v", err)
	}
	holdLock(t, root)
	shortenLockWait(t, 50*time.Millisecond)

	entry, err := Ensure(root, "v1.20.7", func(dir, chart, version string) error {
		t.Errorf("pulled %s %s, which is cached", chart, version)
		return nil
	})
	if err != nil {
		t.Fatalf("Ensure with the lock held elsewhere = %v, want the cached entry", err)
	}
	if want := filepath.Join(root, "v1.20.7"); entry != want {
		t.Errorf("entry = %q, want %q", entry, want)
	}
}

func TestEnsureGivesUpOnAPullThatNeverFinishes(t *testing.T) {
	root := t.TempDir()
	holdLock(t, root)
	shortenLockWait(t, 50*time.Millisecond)

	_, err := Ensure(root, "v1.20.7", func(dir, chart, version string) error {
		t.Errorf("pulled %s %s while another held the lock", chart, version)
		return nil
	})
	if err == nil {
		t.Fatal("Ensure = nil error, want it to give up waiting for the lock")
	}
	if lock := filepath.Join(root, lockName); !strings.Contains(err.Error(), lock) {
		t.Errorf("error %q does not name the lock file %s", err, lock)
	}
	if left := entriesIn(t, root); len(left) != 0 {
		t.Errorf("root holds %v after giving up, want nothing", left)
	}
}
