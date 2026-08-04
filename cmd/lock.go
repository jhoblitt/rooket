package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// held records the cluster locks this process already owns.
//
// A flock belongs to the open file description, not to the path, so a second
// os.Open of the same lock file blocks against this process's own lock and
// deadlocks it. That is reachable through ordinary composition: 'down' runs
// 'cluster delete' and 'block teardown' by calling their command bodies, and
// each of those locks on its own when invoked directly. So a nested acquisition
// returns a no-op release and the outermost caller keeps the only real one —
// correct because the releases are deferred and therefore unwind innermost
// first.
var (
	heldMu sync.Mutex
	held   = map[string]*os.File{}
)

// LockCluster takes the host-wide exclusive lock for a cluster and returns the
// function that releases it.
//
// Every command that mutates a cluster holds this for its whole run, rather
// than guarding the individual dangerous sections. Two concurrent runs against
// one cluster are a mistake, never a workflow — the cluster name is derived
// from the rook clone's path, so this is two terminals in the same clone — and
// the interleavings are not otherwise containable: node prep exec'ing into the
// same nodes twice, two helm installs of one release, two registry port
// re-picks, and above all the create path's delete-zap-create, where one run
// can truncate the OSD images of a cluster the other has just rebuilt onto
// them.
//
// The lock is advisory and only binds rooket to rooket; nothing stops a user
// removing a container by hand.
func LockCluster(name string) (release func(), err error) {
	root, err := stateDirRoot()
	if err != nil {
		return nil, err
	}
	return lockClusterIn(root, name)
}

// lockClusterIn is LockCluster against an explicit state root, for callers that
// were handed one — prune operates on the root it is given, and locking the
// ambient one instead would guard a different directory than the one it is
// about to remove.
func lockClusterIn(root, name string) (release func(), err error) {
	heldMu.Lock()
	defer heldMu.Unlock()
	if _, ok := held[name]; ok {
		return func() {}, nil
	}

	path, err := clusterLockPath(root, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create the rooket state directory: %w", err)
	}
	// Not waited on: a cluster lock is held for a whole command, so waiting
	// would silently park an interactive run behind one that may be minutes
	// from finishing.
	f, err := acquireFlock(path, 0)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("cluster %q is locked by another rooket%s; wait for it to finish, "+
				"or work on a different cluster with --name", name, lockOwnerAt(path))
		}
		return nil, fmt.Errorf("lock cluster %q: %w", name, err)
	}
	writeLockOwner(f)

	held[name] = f
	return func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		delete(held, name)
		// Closing the descriptor releases the flock. The kernel does the same on
		// exit, including a kill, so an interrupted run never strands the lock —
		// which is the whole reason this is not a pid file.
		f.Close()
	}, nil
}

// clusterLockPath is deliberately beside the cluster's state directory rather
// than inside it: 'down --delete-disks', 'down --all', and 'prune' all
// os.RemoveAll that directory, and unlinking a locked file is silent and legal.
// A waiter would then create a fresh file at the same path and lock a different
// inode, leaving two runs each convinced it holds the cluster.
//
// For the same reason nothing ever deletes these: unlinking a lock file while
// holding it reopens exactly that hole. They are a few dozen bytes each, one
// per cluster name ever used, and stateDirNames only counts directories, so a
// leftover is invisible to 'list', 'down --all', and 'prune'.
func clusterLockPath(root, name string) (string, error) {
	if err := validateClusterName(name); err != nil {
		return "", err
	}
	return filepath.Join(root, name+".lock"), nil
}

// writeLockOwner records who holds the lock, for the message the next caller
// gets. It is diagnostic only: a failure to write costs a clearer error and
// nothing else, since the kernel's lock is what actually excludes.
func writeLockOwner(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), strings.Join(os.Args, " "))
}

// errLockBusy reports that another process holds the lock, as opposed to the
// lock file being unusable — the caller phrases those very differently.
var errLockBusy = errors.New("lock is held by another process")

// portsLockWait bounds the wait for the registry-port allocation lock. Unlike a
// cluster lock this one is held for a few filesystem reads and a handful of
// bind probes, so waiting is right where refusing would be absurd: two clusters
// starting together is the case the lock exists for, and failing one of them
// would be the very collision it is meant to prevent.
const portsLockWait = 30 * time.Second

// LockPorts serializes registry host-port allocation across every cluster on
// the host. Cluster locks cannot cover this: the clusters contending for a port
// are different ones, each already holding its own lock.
func LockPorts() (release func(), err error) {
	root, err := stateDirRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create the rooket state directory: %w", err)
	}
	// The leading dot is load-bearing: cluster locks are "<name>.lock" beside
	// this one, and a cluster may legitimately be named "ports". A name must be
	// a DNS label, so no cluster can ever produce a file starting with a dot.
	path := filepath.Join(root, ".ports.lock")
	f, err := acquireFlock(path, portsLockWait)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("another rooket has been allocating a registry port for over %s%s; "+
				"if it is wedged, kill it and retry", portsLockWait, lockOwnerAt(path))
		}
		return nil, fmt.Errorf("lock registry port allocation: %w", err)
	}
	writeLockOwner(f)
	return func() { f.Close() }, nil
}

// acquireFlock opens path and takes an exclusive flock on it. wait of zero
// tries once; otherwise it retries until wait elapses, since flock itself has
// no timeout and a blocking one could never be interrupted.
func acquireFlock(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, errLockBusy
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// lockOwnerAt renders the holder recorded in a lock file we failed to take.
// Reading needs no lock and races the holder's own write, so anything
// unexpected yields no attribution rather than a guess.
func lockOwnerAt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return formatLockOwner(string(buf[:n]))
}

// formatLockOwner turns a lock file's recorded "<pid> <argv>" into a clause for
// the busy error, or "" when there is nothing trustworthy to report.
func formatLockOwner(content string) string {
	line := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	pid, argv, ok := strings.Cut(line, " ")
	if !ok || pid == "" || strings.TrimSpace(argv) == "" {
		return ""
	}
	for _, r := range pid {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return fmt.Sprintf(" (pid %s: %s)", pid, strings.TrimSpace(argv))
}
