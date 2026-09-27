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
// so an interrupted pull never leaves one behind; a run that loses that rename
// to a concurrent one uses the winner's entry.
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
