package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/chartcache"
	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/run"
)

// sourceFile names the record, in a cluster's state directory, of where its
// Rook comes from and where its configuration lives. Every command after the
// one that set them reads it, so neither has to be repeated.
const sourceFile = "source.json"

// clusterSource is what a cluster was last deployed from. An empty
// RookVersion means a rook clone. ConfigDir is set only when a configuration
// directory was named, and is always absolute.
type clusterSource struct {
	RookVersion string `json:"rookVersion,omitempty"`
	ConfigDir   string `json:"configDir,omitempty"`
}

func readSource(name string) (clusterSource, bool) {
	dir, err := stateDirPath(name)
	if err != nil {
		return clusterSource{}, false
	}
	return readSourceAt(dir)
}

// readSourceAt reads the record from a state directory, for prune, which
// walks directories rather than names.
func readSourceAt(stateDir string) (clusterSource, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, sourceFile))
	if err != nil {
		return clusterSource{}, false
	}
	var s clusterSource
	if json.Unmarshal(data, &s) != nil {
		return clusterSource{}, false
	}
	return s, true
}

// writeSource records a cluster's source atomically (temp+rename), so a torn
// write leaves an unreadable record rather than a wrong one.
func writeSource(name string, s clusterSource) error {
	dir, err := ensureStateDir(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, sourceFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("record cluster source: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("record cluster source: %w", err)
	}
	return nil
}

// resolveSource settles a command's rook version and configuration directory.
// The version is the flag when the user passed it, else the record. The
// directory is the flag, else $ROOKET_CONFIG_DIR, else the record; it must
// exist, and a named one is made absolute so the record means the same thing
// from wherever the next command runs. It also reports whether the result
// differs from the record, which the commands that deploy then write back.
func resolveSource(name, version string, versionSet bool, configDir string, configSet bool) (clusterSource, bool, error) {
	rec, _ := readSource(name)
	out := rec
	if versionSet {
		if err := chartcache.ValidVersion(version); err != nil {
			return clusterSource{}, false, err
		}
		out.RookVersion = version
	}
	dir := ""
	switch {
	case configSet:
		dir = configDir
	case os.Getenv("ROOKET_CONFIG_DIR") != "":
		dir = os.Getenv("ROOKET_CONFIG_DIR")
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return clusterSource{}, false, fmt.Errorf("resolve configuration directory %s: %w", dir, err)
		}
		if err := checkConfigDir(abs); err != nil {
			return clusterSource{}, false, err
		}
		out.ConfigDir = abs
	} else if out.ConfigDir != "" {
		// A recorded directory can vanish, as when a checkout switches to a
		// branch without it; unchecked, the command would run unconfigured.
		if err := checkConfigDir(out.ConfigDir); err != nil {
			return clusterSource{}, false, fmt.Errorf("recorded %w; pass --config-dir to name another", err)
		}
	}
	return out, out != rec, nil
}

// checkConfigDir reports why dir cannot serve as a configuration directory, or
// nil if it can.
func checkConfigDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("configuration directory %s does not exist", dir)
	case err != nil:
		return fmt.Errorf("configuration directory %s: %w", dir, err)
	case !fi.IsDir():
		return fmt.Errorf("configuration directory %s is not a directory", dir)
	}
	return nil
}

// configHome is where a command's sticky values, profile list, and templates
// come from: a named directory, else the .rooket of the rook clone the command
// runs against or within, else nowhere.
func configHome(src clusterSource, rookDir string) clone.Dir {
	if src.ConfigDir != "" {
		return clone.At(src.ConfigDir)
	}
	if rookDir != "" {
		return clone.Open(rookDir)
	}
	return clone.Dir{}
}

// chartPuller unpacks one released chart with helm; tests replace it.
var chartPuller = func(env []string) chartcache.Puller {
	return func(dir, chart, version string) error {
		return run.CmdWithEnv(env, "helm", "pull", chart,
			"--repo", chartcache.Repo, "--version", version, "--untar", "--untardir", dir)
	}
}

// releasedCharts returns the chart cache entry for a released Rook version,
// pulling it on first use. The pull gets a helm home of its own inside the
// cache, because the cache outlives any one cluster's state dir. Every rooket on
// the host pulls through that one home, which Ensure's lock keeps to one pull
// at a time.
func releasedCharts(version string) (string, error) {
	root, err := chartcache.DefaultRoot()
	if err != nil {
		return "", err
	}
	env, err := helmEnvAt(filepath.Join(root, ".helm"))
	if err != nil {
		return "", err
	}
	return chartcache.Ensure(os.Stdout, root, version, chartPuller(env))
}
