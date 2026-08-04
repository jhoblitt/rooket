package cmd

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// stateDirNames counts only directories, so the lock file must stay a file
	// or it would start showing up as a cluster.
	if _, err := clusterLockPath(root, "../escape"); err == nil {
		t.Errorf("an invalid cluster name must not produce a lock path")
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
	f, ok := held[name]
	return f, ok
}

// pruneExecute is handed the state root it operates on, and its unit tests pass
// a fake one with an injected remove func. A lock that resolved the ambient
// $HOME instead would both guard the wrong directory and, in those tests, write
// real files into the developer's own state root.
func TestPruneExecuteLocksTheRootItWasGiven(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	root := t.TempDir()

	if err := pruneExecute(root, []string{"orphan"}, nil,
		func([]iscsiDisk) error { return nil },
		func(string) error { return nil }, io.Discard); err != nil {
		t.Fatalf("pruneExecute: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "orphan.lock")); err != nil {
		t.Errorf("expected the lock in the root prune was given: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(realHome, ".local", "share", "rooket")); len(entries) != 0 {
		t.Errorf("prune wrote %d entries into the ambient state root", len(entries))
	}
}
