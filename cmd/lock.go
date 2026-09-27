package cmd

import (
	"errors"
	"fmt"
	"io/fs"
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
	held   = map[string]*clusterLock{}
)

// clusterLock is one cluster lock this process holds.
type clusterLock struct {
	f      *os.File
	path   string
	remove bool // the release deletes the lock file; see removeClusterLockOnRelease
}

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

	l := &clusterLock{f: f, path: path}
	held[name] = l
	return func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		delete(held, name)
		// The unlink must come before the close. Once the flock is dropped,
		// another rooket can lock this inode and pass acquireFlock's check,
		// since the path still names it; unlinking after that would pull the
		// file out from under a live holder and let a third run create and
		// lock a fresh one. A failed unlink costs a stray file and nothing else.
		if l.remove {
			_ = os.Remove(l.path)
		}
		// Closing the descriptor releases the flock. The kernel does the same on
		// exit, including a kill, so an interrupted run never strands the lock —
		// which is the whole reason this is not a pid file.
		f.Close()
	}, nil
}

// removeClusterLockOnRelease has the release of this process's lock on a
// cluster delete the lock file as well, for a run that leaves nothing of the
// cluster behind. The deletion waits for the release that actually lets go, a
// nested one staying a no-op, and nothing happens unless this process holds the
// lock: only the holder may delete a lock file, and only as it lets go (see
// acquireFlock).
func removeClusterLockOnRelease(name string) {
	heldMu.Lock()
	defer heldMu.Unlock()
	if l, ok := held[name]; ok {
		l.remove = true
	}
}

// clusterLockPath is deliberately beside the cluster's state directory rather
// than inside it: 'down --delete-disks', 'down --all', and 'prune' all
// os.RemoveAll that directory, and unlinking a locked file is silent and legal.
// Inside it, the lock file would go whenever the directory did rather than as
// its holder lets go, the one moment it is safe to delete (see acquireFlock):
// the next locker could create a fresh file at the same path, lock it, and pass
// acquireFlock's check while the holder was still at work, leaving two runs
// each convinced it holds the cluster.
//
// Only down and prune delete a lock file: with the cluster's state dir, or, for
// down, on finding nothing of the cluster at all. A leftover is harmless:
// stateDirNames only counts directories, so it is invisible to 'list',
// 'down --all', and 'prune'.
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
//
// A flock is taken on an inode, not a path, and down and prune delete cluster
// lock files (see removeClusterLockOnRelease). A locker that opened the file
// just before one was deleted would go on to lock an inode no path names,
// while the next locker creates a fresh file at the path and locks that one:
// two runs, each holding "the" lock. So a flock counts only once the path is
// seen, with the flock held, to still name the inode it was taken on;
// otherwise the file is opened and locked afresh.
//
// That check is enough because of the rule on the other side: a lock file is
// deleted only by the process holding its flock, and only as it lets go. While
// the holder keeps the flock nothing else unlinks the path or puts another file
// there — O_CREATE never replaces a file — so a locker that passes the check
// holds a flock on the one inode the path names, and that flock excludes every
// other locker that passes it. Once the holder deletes the file, a locker still
// on the old inode fails the check and moves to whatever the path names next.
// A retry therefore follows the end of another run's hold, so the loop cannot
// spin on its own; and it retries at once, even when wait is zero, because it
// is not waiting for the lock but finding the file.
func acquireFlock(path string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		betweenOpenAndFlock(path)
		if err := flockUntil(f, deadline); err != nil {
			f.Close()
			return nil, err
		}
		current, err := namesFile(path, f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if current {
			return f, nil
		}
		f.Close()
	}
}

// betweenOpenAndFlock runs in the window where the lock file acquireFlock has
// just opened can be deleted under it. Tests set it to land that race on
// demand.
var betweenOpenAndFlock = func(path string) {}

// flockUntil takes an exclusive flock on f, retrying while another process
// holds it until deadline passes.
func flockUntil(f *os.File, deadline time.Time) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errLockBusy
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// namesFile reports whether path currently names the file f has open.
func namesFile(path string, f *os.File) (bool, error) {
	open, err := f.Stat()
	if err != nil {
		return false, err
	}
	now, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(open, now), nil
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
