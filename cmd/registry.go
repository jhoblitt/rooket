package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
)

// registryConfigPath returns a cluster's generated zot config path, beside the
// other per-cluster files in its state dir.
//
// The config carries no per-cluster content, but one shared copy would break
// the change detection that guards it: the first cluster to notice a change
// would rewrite the file and recreate its own container, leaving every other
// cluster's registry on the old config with nothing left to detect. Teardown
// reclaims this copy with the rest of the cluster's state.
func registryConfigPath(name string) (string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "registry-config.json"), nil
}

// writeRegistryConfig renders this cluster's registry config, installs it, and
// reports whether it differs from what was already there. Whether the
// container must be recreated to pick the change up is decided from the
// container itself — see registry.Create — not from what this reports.
//
// The install is atomic — write a temp file, rename it over the target — for
// the same reason the build stamp is: the file is read by something other than
// the writer. Here that reader is the registry container, which bind-mounts it
// by inode, so a truncating rewrite could hand a running zot a half-written
// config, and a container created in that window would mount one it cannot
// parse. Rename leaves the mounted inode untouched and swaps the path in one
// step.
func writeRegistryConfig(name string) (string, bool, error) {
	if _, err := ensureStateDir(name); err != nil {
		return "", false, err
	}
	path, err := registryConfigPath(name)
	if err != nil {
		return "", false, err
	}
	want, err := registry.GenerateConfig()
	if err != nil {
		return "", false, err
	}
	got, readErr := os.ReadFile(path)
	if readErr == nil && bytes.Equal(got, want) {
		return path, false, nil
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return "", false, fmt.Errorf("write registry config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", false, fmt.Errorf("install registry config: %w", err)
	}
	return path, true, nil
}

// setupRegistry installs this cluster's registry config and brings its
// registry container up against it.
//
// Installing the config and reconciling the container are deliberately not
// sequenced against each other here. registry.Create compares the container's
// recorded config with the one on disk, so an interrupt between the two leaves
// a state the next run still repairs — where a recreate driven by "this run
// wrote a new config" would not, since the second run finds the file already
// current and has nothing left to act on.
func setupRegistry(out io.Writer, name string, cfg registry.Config) error {
	path, changed, err := writeRegistryConfig(name)
	if err != nil {
		return err
	}
	if changed {
		run.Fprintf(out, "installed registry config %s\n", path)
	}
	cfg.HostConfigPath = path
	return registry.Create(out, cfg)
}
