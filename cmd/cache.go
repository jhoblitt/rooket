package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/cache"
	"github.com/jhoblitt/rooket/internal/run"
)

// cacheConfigPath returns the generated zot config path. It lives under
// ~/.config rather than the state root because stateDirNames treats every
// directory under ~/.local/share/rooket as a cluster name — a cache directory
// there would be misread by 'rooket prune' as an orphaned cluster.
func cacheConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "rooket", "cache-config.json"), nil
}

// setupCache writes the zot config and starts the shared cache container,
// recreating an existing container when the generated config has changed (a
// rooket upgrade adding an upstream, say). Only the container is replaced: the
// named volume carries the cached blobs across the recreation.
//
// Concurrent 'rooket up' runs converge here rather than serialize: identical
// binaries generate byte-identical config, so the recreate branch is not taken
// and cache.Create absorbs the lost creation race. Mismatched binaries can
// briefly flap the container between their two configs; the cost is a pull
// falling back upstream, which is the same soft-dependency behavior as a cache
// that is simply down.
func setupCache(out io.Writer) error {
	path, err := cacheConfigPath()
	if err != nil {
		return err
	}
	want, err := cache.GenerateConfig(nil)
	if err != nil {
		return err
	}
	got, readErr := os.ReadFile(path)
	changed := readErr != nil || !bytes.Equal(got, want)

	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create cache config dir: %w", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			return fmt.Errorf("write cache config: %w", err)
		}
		if cache.Exists(out, containerEngine) {
			run.Fprintf(out, "cache config changed; recreating %s (cached images are preserved in volume %s)\n",
				cache.ContainerName, cache.VolumeName)
			if err := cache.RemoveContainer(out, containerEngine); err != nil {
				return fmt.Errorf("recreate cache container: %w", err)
			}
		}
	}

	return cache.Create(out, cache.Config{
		Engine:         containerEngine,
		Network:        "kind",
		HostConfigPath: path,
	})
}

// teardownCache removes the shared cache container and its volume. It is only
// ever reached via an explicit --delete-cache: rooket's teardown paths match
// "<cluster>-registry" by exact name, so the cache is invisible to them and
// survives every 'down' by construction.
func teardownCache(out io.Writer) error {
	if err := cache.Delete(out, containerEngine); err != nil {
		return err
	}
	run.Fprintf(out, "removed image cache %s and volume %s\n", cache.ContainerName, cache.VolumeName)
	return nil
}

// noteCachePreserved mirrors how a plain 'down' reports preserved disk images,
// so several GB of cache are never left behind silently.
func noteCachePreserved(out io.Writer) {
	if cache.Exists(out, containerEngine) {
		run.Fprintf(out, "image cache preserved (pass --delete-cache to remove it)\n")
	}
}
