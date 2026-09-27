package cmd

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// lockHelperEnv makes a re-exec of this test binary act as a second rooket
// process: it takes a cluster lock, announces it, and holds it until killed.
const lockHelperEnv = "ROOKET_TEST_LOCK_HELPER"

// TestClusterLockIsExclusiveAcrossProcesses is the property the whole design
// exists for, and it cannot be shown in one process: flock is owned by the open
// file description, so a same-process second acquisition proves nothing about
// two rookets racing.
//
// It also covers why this is a flock and not a pid file — the lock must be gone
// the moment the holder dies, including under a signal it cannot handle.
func TestClusterLockIsExclusiveAcrossProcesses(t *testing.T) {
	const name = "lock-exclusion"

	if os.Getenv(lockHelperEnv) != "" {
		release, err := LockCluster(name)
		if err != nil {
			os.Stdout.WriteString("FAILED " + err.Error() + "\n")
			os.Exit(1)
		}
		defer release()
		os.Stdout.WriteString("LOCKED\n")
		// Hold it until the parent kills us; reading a stdin that never closes
		// parks without burning CPU.
		os.Stdin.Read(make([]byte, 1))
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	helper := exec.Command(os.Args[0], "-test.run=TestClusterLockIsExclusiveAcrossProcesses")
	helper.Env = append(os.Environ(), lockHelperEnv+"=1", "HOME="+home)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if _, err := helper.StdinPipe(); err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("helper did not take the lock (line %q, err %v)", line, err)
	}

	if release, err := LockCluster(name); err == nil {
		release()
		t.Fatalf("took a lock another process is holding")
	} else if !strings.Contains(err.Error(), "locked by another rooket") {
		t.Errorf("error should say the cluster is locked, got: %v", err)
	}

	// The holder dies without unwinding anything. The kernel must drop the lock
	// regardless, or an interrupted run would wedge the cluster until a human
	// deleted a file they were never told about.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = helper.Wait()

	release, err := LockCluster(name)
	if err != nil {
		t.Fatalf("lock not released when its holder was killed: %v", err)
	}
	release()
}

// A command that runs another command's body — 'down' invokes cluster delete
// and block teardown — must not block on a lock it already holds.
func TestClusterLockIsReentrantWithinOneProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const name = "lock-reentrant"

	outer, err := LockCluster(name)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	inner, err := LockCluster(name)
	if err != nil {
		t.Fatalf("nested lock deadlocked or failed: %v", err)
	}
	// The nested release is a no-op: the outer holder still owns the cluster.
	inner()
	if _, held := heldFile(name); !held {
		t.Errorf("nested release dropped the lock the outer caller still holds")
	}
	outer()
	if _, held := heldFile(name); held {
		t.Errorf("outer release left the lock held")
	}
}

// The lock must not live inside the directory 'down --delete-disks', 'down
// --all', and 'prune' remove: unlinking it while held lets the next caller lock
// a different inode and both proceed.
func TestClusterLockFileSitsOutsideTheStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	root, err := stateDirRoot()
	if err != nil {
		t.Fatalf("stateDirRoot: %v", err)
	}
	lock, err := clusterLockPath(root, "rook")
	if err != nil {
		t.Fatalf("clusterLockPath: %v", err)
	}
	stateDir, err := stateDirPath("rook")
	if err != nil {
		t.Fatalf("stateDirPath: %v", err)
	}
	if strings.HasPrefix(lock, stateDir+string(filepath.Separator)) {
		t.Errorf("lock %q is inside the removable state dir %q", lock, stateDir)
	}
	if filepath.Dir(lock) != filepath.Dir(stateDir) {
		t.Errorf("lock %q should sit beside the state dir %q", lock, stateDir)
	}
	if _, err := clusterLockPath(root, "../escape"); err == nil {
		t.Errorf("an invalid cluster name must not produce a lock path")
	}
}

// A full teardown deletes its cluster's lock file, and another run may have
// opened that file just before. The flock it then takes is on an inode no path
// names, while a third run creates a fresh file at the path and locks that one.
// A locker has to notice, and lock the file the path names now.
func TestAcquireFlockLocksTheFileThePathNamesNow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.lock")
	keep(t, &betweenOpenAndFlock)
	opens := 0
	betweenOpenAndFlock = func(p string) {
		opens++
		if opens > 1 {
			return
		}
		// Between this open and its flock, the holder deletes the file and
		// another run creates one in its place.
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	f, err := acquireFlock(path, 0)
	if err != nil {
		t.Fatalf("acquireFlock: %v", err)
	}
	defer f.Close()
	locked, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(locked, current) {
		t.Fatalf("acquireFlock locked a file the path no longer names")
	}
	other, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Errorf("flock of the file at the path = %v, want EWOULDBLOCK: it should already be held", err)
	}
}

// A lock file is deleted only as its holder lets go, so a nested release —
// which lets nothing go — must not delete it.
func TestClusterLockFileRemovedByTheOutermostRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const name = "lock-removed"
	root, err := stateDirRoot()
	if err != nil {
		t.Fatal(err)
	}
	path, err := clusterLockPath(root, name)
	if err != nil {
		t.Fatal(err)
	}

	outer, err := LockCluster(name)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	inner, err := LockCluster(name)
	if err != nil {
		t.Fatalf("nested lock: %v", err)
	}
	removeClusterLockOnRelease(name)
	inner()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a nested release deleted the lock file the outer holder still holds: %v", err)
	}
	outer()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock file survived the release that was to remove it (stat: %v)", err)
	}

	again, err := LockCluster(name)
	if err != nil {
		t.Fatalf("lock after the file was removed: %v", err)
	}
	again()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a plain release removed the lock file: %v", err)
	}
}

func TestFormatLockOwner(t *testing.T) {
	if got := formatLockOwner("4321 rooket up --workers 3\n"); got != " (pid 4321: rooket up --workers 3)" {
		t.Errorf("formatLockOwner = %q", got)
	}
	// Anything unexpected yields no attribution rather than a guess: the read
	// races the holder's own truncate-and-write.
	for _, bad := range []string{"", "\n", "4321", "4321 ", "not-a-pid rooket up", "  "} {
		if got := formatLockOwner(bad); got != "" {
			t.Errorf("formatLockOwner(%q) = %q, want empty", bad, got)
		}
	}
}

// heldFile reports whether this process currently owns a cluster's lock.
func heldFile(name string) (*os.File, bool) {
	heldMu.Lock()
	defer heldMu.Unlock()
	l, ok := held[name]
	if !ok {
		return nil, false
	}
	return l.f, true
}

// lockClusterExternally simulates a second process already holding cluster
// name's lock, for a test in the same process: it takes the flock through its
// own open file description, one lockClusterIn never sees, so this process's
// own held map stays empty and a later LockCluster(name) contends against the
// kernel for real instead of taking the reentrant no-op path.
func lockClusterExternally(t *testing.T, name string) {
	t.Helper()
	root, err := stateDirRoot()
	if err != nil {
		t.Fatalf("stateDirRoot: %v", err)
	}
	path, err := clusterLockPath(root, name)
	if err != nil {
		t.Fatalf("clusterLockPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	f, err := acquireFlock(path, 0)
	if err != nil {
		t.Fatalf("acquireFlock: %v", err)
	}
	t.Cleanup(func() { f.Close() })
}

// pruneExecute is handed the state root it operates on, and its unit tests pass
// a fake one with an injected remove func. A lock that resolved the ambient
// $HOME instead would both guard the wrong directory and, in those tests, write
// real files into the developer's own state root.
func TestPruneExecuteLocksTheRootItWasGiven(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	root := t.TempDir()

	// Read while the orphan is removed: that is when the lock is held, and
	// the release that follows deletes its file.
	var lockedAt string
	remove := func(string) error {
		heldMu.Lock()
		defer heldMu.Unlock()
		if l, ok := held["orphan"]; ok {
			lockedAt = l.path
		}
		return nil
	}
	if err := pruneExecute(root, []string{"orphan"}, nil,
		func([]iscsiDisk) error { return nil }, remove, io.Discard); err != nil {
		t.Fatalf("pruneExecute: %v", err)
	}

	if want := filepath.Join(root, "orphan.lock"); lockedAt != want {
		t.Errorf("prune removed the orphan holding the lock at %q, want %q in the root it was given", lockedAt, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(realHome, ".local", "share", "rooket")); len(entries) != 0 {
		t.Errorf("prune wrote %d entries into the ambient state root", len(entries))
	}
}
