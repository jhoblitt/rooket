package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/run"
)

// The build cache remembers every recent build of a rook clone, not just the
// last one, so returning to a tree already built — reverting an edit, switching
// back to a branch — republishes that build instead of running make again.
//
// It is keyed by clone rather than by cluster because a build is a fact about
// a clone: rook's make names its output after the clone's path, and two
// clusters deployed from one clone can share each other's builds. It lives
// beside the cluster state directories, so it survives 'down --all' and
// 'prune', which remove those.
//
// Each cached build is pinned under a local tag of its own. make's output tag
// is mutable — the next build in the clone retags it — so a record naming it
// would soon point at the wrong image.
//
// A record is only ever a hint: every use re-checks the pinned image's ID with
// the engine, and anything missing or mismatched means "run make". A stale
// record costs a build, never a wrong image.

// buildCacheDirName is dot-led so it can never collide with a cluster's state
// directory (a cluster name is a DNS label), and so stateDirNames — through
// which 'list', 'prune', and 'down --all' find clusters — skips it.
const buildCacheDirName = ".builds"

const buildCacheVersion = 1

// buildCacheKeep bounds the builds kept per clone. Builds of one clone share
// all but their topmost layers, so each extra one costs little disk.
const buildCacheKeep = 5

// cloneLockWait bounds the wait for another rooket's build in the same clone.
// A rook make can take many minutes, and the wait usually ends in a cache hit,
// so waiting beats refusing.
const cloneLockWait = time.Hour

// pinnedRepo is the local repository every cached build is tagged under. It is
// never pushed anywhere; the engine resolves it as a local name.
const pinnedRepo = "rooket-build"

type cachedImage struct {
	// Built is make's name for the image (e.g. build-xxxx/ceph-amd64): the
	// input deriveTag turns into the registry ref.
	Built string `json:"built"`
	// Pinned is this build's own local tag for the image.
	Pinned string `json:"pinned"`
	// ID is the image ID Pinned named when the build was recorded.
	ID string `json:"id"`
}

type cachedBuild struct {
	Version     int           `json:"version"`
	Dir         string        `json:"dir"`
	Fingerprint treeFP        `json:"fingerprint"`
	Images      []cachedImage `json:"images"`
	BuiltAt     string        `json:"builtAt"`
}

// cloneKey identifies a clone by its absolute, symlink-resolved path, so one
// clone reached through two paths shares one cache.
func cloneKey(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8]), nil
}

// fingerprintKey names a tree state. Every field takes part, so two trees that
// fingerprintDiff tells apart never share a key.
func fingerprintKey(fp treeFP) string {
	data, _ := json.Marshal(fp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// pinnedRef is the local tag a build's image is pinned under.
func pinnedRef(clone, fpKey, built string) string {
	base := built
	if i := strings.LastIndex(built, "/"); i >= 0 {
		base = built[i+1:]
	}
	return fmt.Sprintf("%s/%s/%s:%s", pinnedRepo, clone, base, fpKey)
}

// buildCache is one clone's cache directory, held under that clone's lock.
type buildCache struct {
	dir   string
	clone string
}

// buildCacheLockPath is beside the clone's cache directory, not inside it, for
// the reason clusterLockPath gives: removing a directory holding a lock file
// lets a waiter lock a fresh inode at the same path.
func buildCacheLockPath(root, clone string) string {
	return filepath.Join(root, buildCacheDirName, clone+".lock")
}

// lockBuildCache takes the exclusive lock on the cache of the clone at dir and
// returns it with its release. Two clusters built from one clone each hold only
// their own cluster lock, yet their makes write the same output tag; this is
// what serializes them. It is always taken after the cluster lock, never before,
// so the two cannot deadlock.
func lockBuildCache(out io.Writer, dir string) (*buildCache, func(), error) {
	root, err := stateDirRoot()
	if err != nil {
		return nil, nil, err
	}
	return lockBuildCacheIn(out, root, dir)
}

func lockBuildCacheIn(out io.Writer, root, dir string) (*buildCache, func(), error) {
	clone, err := cloneKey(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve rook clone path: %w", err)
	}
	cacheDir := filepath.Join(root, buildCacheDirName, clone)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create the build cache directory: %w", err)
	}
	path := buildCacheLockPath(root, clone)
	f, err := acquireFlock(path, 0)
	if errors.Is(err, errLockBusy) {
		run.Fprintf(out, "==> waiting for another rooket building in %s%s\n", dir, lockOwnerAt(path))
		f, err = acquireFlock(path, cloneLockWait)
	}
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, nil, fmt.Errorf("another rooket has been building in %s for over %s%s; "+
				"if it is wedged, kill it and retry", dir, cloneLockWait, lockOwnerAt(path))
		}
		return nil, nil, fmt.Errorf("lock the build cache for %s: %w", dir, err)
	}
	writeLockOwner(f)
	return &buildCache{dir: cacheDir, clone: clone}, func() { f.Close() }, nil
}

func (c *buildCache) recordPath(fpKey string) string {
	return filepath.Join(c.dir, fpKey+".json")
}

// lookup returns the cached build of the tree fp names whose pinned images
// still carry the IDs recorded for them, or nil. idOf reports an image's
// local ID, "" when it is absent.
func (c *buildCache) lookup(fp treeFP, idOf func(ref string) string) *cachedBuild {
	p := c.recordPath(fingerprintKey(fp))
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var b cachedBuild
	if json.Unmarshal(data, &b) != nil || !b.usableFor(fp, idOf) {
		return nil
	}
	// Recency, for eviction: a build in use is not one to drop.
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return &b
}

// usableFor reports whether b is a build of the tree fp names whose images are
// all still present locally as recorded.
func (b *cachedBuild) usableFor(fp treeFP, idOf func(ref string) string) bool {
	if b.Version != buildCacheVersion || len(b.Images) == 0 || fingerprintDiff(b.Fingerprint, fp) != "" {
		return false
	}
	for _, img := range b.Images {
		if img.ID == "" || idOf(img.Pinned) != img.ID {
			return false
		}
	}
	return true
}

// record writes b atomically: a torn write leaves an unreadable record, which
// lookup treats as absent, never a wrong one.
func (c *buildCache) record(b *cachedBuild) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	fpKey := fingerprintKey(b.Fingerprint)
	tmp, err := os.CreateTemp(c.dir, fpKey+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.recordPath(fpKey))
}

// evictable returns the records beyond the keep most recently used, oldest
// first.
func (c *buildCache) evictable(keep int) []string {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	type rec struct {
		path string
		mod  time.Time
	}
	var recs []rec
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		recs = append(recs, rec{filepath.Join(c.dir, e.Name()), info.ModTime()})
	}
	if len(recs) <= keep {
		return nil
	}
	slices.SortFunc(recs, func(a, b rec) int { return b.mod.Compare(a.mod) })
	paths := make([]string, 0, len(recs)-keep)
	for i := len(recs) - 1; i >= keep; i-- {
		paths = append(paths, recs[i].path)
	}
	return paths
}

// evict drops the builds beyond buildCacheKeep. Removing a pinned tag only
// untags: an image another tag still names — a registry ref, make's output tag
// — stays. A cluster whose stamp named an evicted build just rebuilds.
func (c *buildCache) evict(out io.Writer) {
	for _, p := range c.evictable(buildCacheKeep) {
		if data, err := os.ReadFile(p); err == nil {
			var b cachedBuild
			if json.Unmarshal(data, &b) == nil {
				for _, img := range b.Images {
					if img.Pinned != "" {
						_, _ = run.OutputTo(out, containerEngine.String(), "rmi", img.Pinned)
					}
				}
			}
		}
		_ = os.Remove(p)
	}
}
