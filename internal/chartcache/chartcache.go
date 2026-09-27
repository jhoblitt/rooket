// Package chartcache keeps the charts of released Rook versions on disk, one
// entry per version, laid out like a rook clone's deploy/charts so everything
// that reads a clone's charts reads an entry the same way.
package chartcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Repo is where released Rook charts are published.
const Repo = "https://charts.rook.io/release"

// Charts are the charts a released Rook version is installed from.
var Charts = []string{"rook-ceph", "rook-ceph-cluster"}

// versionRE matches one released tag. A range would resolve anew on each
// deploy and drift under a cluster whose record says what it runs.
var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// ValidVersion reports whether v names exactly one released Rook version.
func ValidVersion(v string) error {
	if !versionRE.MatchString(v) {
		return fmt.Errorf("rook version %q is not a single released version such as v1.20.7", v)
	}
	return nil
}

// Puller unpacks one chart at one version into dir, as dir/<chart>.
type Puller func(dir, chart, version string) error

// DefaultRoot is the host-wide cache, shared by every cluster.
func DefaultRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	return filepath.Join(dir, "rooket", "charts"), nil
}

// Ensure returns the cache entry for version under root, pulling it first when
// absent. An entry is pulled into a temporary sibling and renamed into place,
// so an interrupted pull never leaves one behind and an entry that exists is
// whole. One that appears mid-pull anyway, made by hand or by a process that
// does not take the lock below, is used rather than fought over.
//
// Pulls under one root run one at a time, across processes. Different versions
// get different entries but not a different helm: rooket's Puller gives helm one
// home under root for every pull on the host, and helm writes the config and
// cache files it keeps there non-atomically (see cmd's helmEnv). So a miss
// takes an exclusive flock on root's lock file before it pulls (see lockPulls).
// A hit takes none: it runs no Puller, and the rename means the entry it finds
// was never half-written.
//
// Holding the lock, a miss looks for the entry again, and that re-check is what
// makes it one pull per version rather than one per waiting run. The holder
// renames its entry into place before it lets go, so a run that waited out a
// successful pull finds that entry and returns it; it pulls only when the pull
// it waited on failed or was killed, which leaves no entry.
func Ensure(root, version string, pull Puller) (string, error) {
	if err := ValidVersion(version); err != nil {
		return "", err
	}
	entry := filepath.Join(root, version)
	if _, err := os.Stat(entry); err == nil {
		return entry, complete(entry)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create chart cache %s: %w", root, err)
	}
	afterMiss()
	release, err := lockPulls(root)
	if err != nil {
		return "", err
	}
	defer release()
	if _, err := os.Stat(entry); err == nil {
		return entry, complete(entry)
	}

	tmp, err := os.MkdirTemp(root, "."+version+"-")
	if err != nil {
		return "", fmt.Errorf("create chart cache entry for %s: %w", version, err)
	}
	defer os.RemoveAll(tmp)

	charts := filepath.Join(tmp, "deploy", "charts")
	if err := os.MkdirAll(charts, 0o755); err != nil {
		return "", err
	}
	for _, c := range Charts {
		if err := pull(charts, c, version); err != nil {
			return "", fmt.Errorf("pull chart %s %s from %s: %w", c, version, Repo, err)
		}
	}
	if err := os.Rename(tmp, entry); err != nil {
		if _, statErr := os.Stat(entry); statErr == nil {
			return entry, complete(entry)
		}
		return "", fmt.Errorf("install chart cache entry %s: %w", entry, err)
	}
	return entry, nil
}

// complete checks that an entry still holds every chart. Entries are only
// ever renamed into place whole, so a missing chart means something changed
// the entry afterwards.
func complete(entry string) error {
	for _, c := range Charts {
		_, err := os.Stat(filepath.Join(entry, "deploy", "charts", c, "Chart.yaml"))
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("chart cache entry %s has no %s chart; delete the entry to pull it again", entry, c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// afterMiss runs between Ensure finding no entry and its taking the lock.
// Tests set it to make concurrent misses contend for the lock on demand.
var afterMiss = func() {}

// lockName is the file in root whose flock serializes pulls. The leading dot
// keeps it out of the entries' namespace: an entry is named for a version, and
// ValidVersion makes every version start with "v".
const lockName = ".pull.lock"

// lockWait bounds a miss's wait for another process's pull, so a wedged helm
// cannot park every later first pull on the host indefinitely. It is generous
// because giving up on a pull that is merely slow only sends the run back to
// queue behind it again. Tests shorten it.
var lockWait = 5 * time.Minute

// lockPulls takes the exclusive flock that serializes pulls under root and
// returns the function that releases it. It polls, since a blocking flock has
// no timeout.
//
// Unlike cmd's cluster locks, this needs no check after the flock that the
// path still names the locked file: rooket never deletes or replaces it, so
// every locker opens and locks the same inode. Only deleting the cache itself
// removes it, and that also deletes the helm home a running pull is using,
// which no lock inside the cache could protect. The kernel drops a flock when
// its holder exits, however it exits, so a killed pull never strands the lock.
func lockPulls(root string) (release func(), err error) {
	path := filepath.Join(root, lockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open chart cache lock: %w", err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() { f.Close() }, nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, fmt.Errorf("lock chart cache %s: %w", path, err)
		case !time.Now().Before(deadline):
			f.Close()
			return nil, fmt.Errorf("another rooket has been pulling released Rook charts for over %s, holding %s; "+
				"if it is wedged, kill it and retry", lockWait, path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
