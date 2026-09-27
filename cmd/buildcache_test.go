package cmd

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloneKeyResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a, err := cloneKey(real)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cloneKey(link)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("one clone reached through a symlink got two keys: %s, %s", a, b)
	}
	c, err := cloneKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("two clones share a key")
	}
}

// Two trees fingerprintDiff tells apart must never share a cached build.
func TestFingerprintKeyCoversEveryField(t *testing.T) {
	base := treeFP{Head: "h", Describe: "d", DiffSum: "ds", StatusSum: "ss", UntrackedSum: "us", BuildEnv: "be"}
	variants := map[string]func(*treeFP){
		"Head":         func(f *treeFP) { f.Head = "x" },
		"Describe":     func(f *treeFP) { f.Describe = "x" },
		"DiffSum":      func(f *treeFP) { f.DiffSum = "x" },
		"StatusSum":    func(f *treeFP) { f.StatusSum = "x" },
		"UntrackedSum": func(f *treeFP) { f.UntrackedSum = "x" },
		"BuildEnv":     func(f *treeFP) { f.BuildEnv = "x" },
	}
	for field, mutate := range variants {
		fp := base
		mutate(&fp)
		if fingerprintDiff(base, fp) == "" {
			t.Fatalf("%s: fingerprintDiff misses the change, so this case proves nothing", field)
		}
		if fingerprintKey(fp) == fingerprintKey(base) {
			t.Errorf("changing %s left the key unchanged", field)
		}
	}
	if fingerprintKey(base) != fingerprintKey(base) {
		t.Error("key is not deterministic")
	}
}

func TestPinnedRef(t *testing.T) {
	got := pinnedRef("c10e", "f00d", "build-98fc4431/ceph-amd64")
	if want := "rooket-build/c10e/ceph-amd64:f00d"; got != want {
		t.Errorf("pinnedRef = %q, want %q", got, want)
	}
}

// newTestBuildCache locks a cache for a fresh clone under a fresh state root.
func newTestBuildCache(t *testing.T) (*buildCache, string) {
	t.Helper()
	root := t.TempDir()
	c, release, err := lockBuildCacheIn(io.Discard, root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return c, root
}

func TestBuildCacheLookup(t *testing.T) {
	c, _ := newTestBuildCache(t)
	fp := treeFP{Head: "h", DiffSum: "d"}
	b := &cachedBuild{
		Version:     buildCacheVersion,
		Fingerprint: fp,
		Images:      []cachedImage{{Built: "build-x/ceph-amd64", Pinned: "rooket-build/c/ceph-amd64:k", ID: "sha256:one"}},
	}
	if err := c.record(b); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"rooket-build/c/ceph-amd64:k": "sha256:one"}
	idOf := func(ref string) string { return ids[ref] }

	got := c.lookup(fp, idOf)
	if got == nil || got.Images[0].ID != "sha256:one" {
		t.Fatalf("lookup of a recorded build = %+v", got)
	}
	if c.lookup(treeFP{Head: "other"}, idOf) != nil {
		t.Error("served a build of a different tree")
	}

	// The record is only a hint; the engine is the authority on the image.
	ids["rooket-build/c/ceph-amd64:k"] = "sha256:two"
	if c.lookup(fp, idOf) != nil {
		t.Error("served a build whose pinned tag now names a different image")
	}
	delete(ids, "rooket-build/c/ceph-amd64:k")
	if c.lookup(fp, idOf) != nil {
		t.Error("served a build whose image is gone")
	}

	ids["rooket-build/c/ceph-amd64:k"] = "sha256:one"
	b.Version = buildCacheVersion + 1
	if err := c.record(b); err != nil {
		t.Fatal(err)
	}
	if c.lookup(fp, idOf) != nil {
		t.Error("served a record of another format version")
	}

	if err := os.WriteFile(c.recordPath(fingerprintKey(fp)), []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.lookup(fp, idOf) != nil {
		t.Error("served a corrupt record")
	}
}

func TestBuildCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := newTestBuildCache(t)
	idOf := func(string) string { return "sha256:id" }
	old := time.Now().Add(-time.Hour)
	var fps []treeFP
	for i := range buildCacheKeep + 2 {
		fp := treeFP{Head: strings.Repeat("h", i+1)}
		fps = append(fps, fp)
		b := &cachedBuild{Version: buildCacheVersion, Fingerprint: fp, Images: []cachedImage{{Pinned: "p", ID: "sha256:id"}}}
		if err := c.record(b); err != nil {
			t.Fatal(err)
		}
		mod := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(c.recordPath(fingerprintKey(fp)), mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	// Using the oldest build makes it the most recent.
	if c.lookup(fps[0], idOf) == nil {
		t.Fatal("lookup missed")
	}

	got := c.evictable(buildCacheKeep)
	want := []string{c.recordPath(fingerprintKey(fps[1])), c.recordPath(fingerprintKey(fps[2]))}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("evictable =\n%s\nwant (oldest first)\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The lock must outlive the directory it guards, or a waiter could lock a
// fresh inode at the same path once the directory was removed.
func TestBuildCacheLockSitsOutsideItsDir(t *testing.T) {
	c, root := newTestBuildCache(t)
	lock := buildCacheLockPath(root, c.clone)
	if rel, err := filepath.Rel(c.dir, lock); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("lock %s is inside the cache dir %s", lock, c.dir)
	}
	if err := os.RemoveAll(c.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("removing the cache dir removed its lock: %v", err)
	}
}

// The helper is told the state root and the clone to lock.
const (
	buildCacheLockHelperRootEnv  = "ROOKET_TEST_BUILD_CACHE_LOCK_ROOT"
	buildCacheLockHelperCloneEnv = "ROOKET_TEST_BUILD_CACHE_LOCK_CLONE"
)

// Two clusters built from one clone hold different cluster locks; only this
// lock keeps their makes apart, and that is a property of two processes.
func TestBuildCacheLockIsExclusiveAcrossProcesses(t *testing.T) {
	if root := os.Getenv(buildCacheLockHelperRootEnv); root != "" {
		_, release, err := lockBuildCacheIn(io.Discard, root, os.Getenv(buildCacheLockHelperCloneEnv))
		if err != nil {
			os.Stdout.WriteString("FAILED " + err.Error() + "\n")
			os.Exit(1)
		}
		defer release()
		os.Stdout.WriteString("LOCKED\n")
		os.Stdin.Read(make([]byte, 1))
		return
	}

	root, dir := t.TempDir(), t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=TestBuildCacheLockIsExclusiveAcrossProcesses")
	helper.Env = append(os.Environ(), buildCacheLockHelperRootEnv+"="+root, buildCacheLockHelperCloneEnv+"="+dir)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := helper.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("helper did not take the lock (line %q, err %v)", line, err)
	}

	clone, err := cloneKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := buildCacheLockPath(root, clone)
	if f, err := acquireFlock(path, 0); !errors.Is(err, errLockBusy) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("took a build-cache lock another process holds (err %v)", err)
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	f, err := acquireFlock(path, 5*time.Second)
	if err != nil {
		t.Fatalf("lock not released when its holder was killed: %v", err)
	}
	f.Close()
}

// An edit made while make runs may or may not be in the images; recording them
// under the pre-make tree would let a revert to that tree reuse them.
func TestPinBuildRefusesATreeThatChangedDuringMake(t *testing.T) {
	dir := gitFixture(t)
	fp, err := treeFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edited.txt"), []byte("mid-make edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := newTestBuildCache(t)

	imgs, stampable := pinBuild(io.Discard, c, dir, fp, nil, []string{"build-x/ceph-amd64"})
	if stampable {
		t.Error("a build whose tree changed during make was declared stampable")
	}
	if len(imgs) != 1 || imgs[0].Built != "build-x/ceph-amd64" || imgs[0].Pinned != "" {
		t.Errorf("images = %+v, want the unpinned make output", imgs)
	}
	if got := c.evictable(0); len(got) != 0 {
		t.Errorf("recorded a build of a tree that changed during make: %v", got)
	}

	if _, stampable := pinBuild(io.Discard, c, dir, treeFP{}, errors.New("no fingerprint"), []string{"build-x/ceph-amd64"}); stampable {
		t.Error("a build with no fingerprint was declared stampable")
	}
}

func TestStateDirNamesSkipsTheBuildCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, err := stateDirRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"mycluster", buildCacheDirName} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, names, err := stateDirNames()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "mycluster" {
		t.Errorf("stateDirNames = %v, want only [mycluster]", names)
	}
}

func TestStampedSourcePrefersThePinnedTag(t *testing.T) {
	if got := stampedSource(stampImage{Source: "build-x/ceph-amd64", Pinned: "rooket-build/c/ceph-amd64:k"}); got != "rooket-build/c/ceph-amd64:k" {
		t.Errorf("got %q", got)
	}
	// Stamps from before the build cache have no pinned tag.
	if got := stampedSource(stampImage{Source: "build-x/ceph-amd64"}); got != "build-x/ceph-amd64" {
		t.Errorf("got %q", got)
	}
}
