package cmd

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/values"
)

const (
	chartOperator = "rook-ceph"
	chartCluster  = "rook-ceph-cluster"
	chartCSI      = "ceph-csi-drivers"
)

var chartShortNames = map[string]string{
	"operator": chartOperator,
	"cluster":  chartCluster,
	"csi":      chartCSI,
}

// allCharts is every chart rooket composes values for.
var allCharts = []string{chartOperator, chartCluster, chartCSI}

// isProfilePath reports whether a --with/--with-only value names a profile
// directory rather than a profile.
func isProfilePath(s string) bool {
	return strings.Contains(s, "/") || s == "." || s == ".."
}

func chartName(short string) (string, error) {
	if full, ok := chartShortNames[short]; ok {
		return full, nil
	}
	for _, full := range chartShortNames {
		if short == full {
			return full, nil
		}
	}
	return "", fmt.Errorf("unknown chart %q (want operator, cluster, or csi)", short)
}

func userProfileDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(cfg, "rooket", "profiles"), nil
}

// activeProfileNames resolves the configuration home's sticky list against
// the flags: --with appends to it, --with-only replaces it. withOnlySet
// distinguishes an unset flag from --with-only "", which clears the
// selection.
//
// pflag's StringArray has no syntax for an empty list, so `--with-only ""`
// arrives here as []string{""} rather than nil; empty entries are dropped so
// that still clears the selection instead of becoming an unknown profile
// named "".
func activeProfileNames(cloneDir clone.Dir, with, withOnly []string, withOnlySet bool) ([]string, error) {
	if withOnlySet {
		return dropEmpty(withOnly), nil
	}
	sticky, err := cloneDir.Profiles()
	if err != nil {
		return nil, err
	}
	// A relative path here would resolve against whichever directory rooket
	// happened to run from, so the sticky list takes names only.
	for _, s := range sticky {
		if isProfilePath(s) {
			return nil, fmt.Errorf("%s lists %q, a path: profile directories are accepted only on --with and --with-only",
				filepath.Join(cloneDir.Path(), "config.yaml"), s)
		}
	}
	return append(sticky, dropEmpty(with)...), nil
}

func dropEmpty(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// loadProfiles resolves each selected profile, by name or by directory, and
// rejects what composition would otherwise get silently wrong: a values file
// no chart looks up, and two different profiles sharing the name that
// prefixes their templates.
func loadProfiles(names []string) ([]profiles.Profile, error) {
	dir, err := userProfileDir()
	if err != nil {
		return nil, err
	}
	out := make([]profiles.Profile, 0, len(names))
	byName := make(map[string]profiles.Profile, len(names))
	for _, n := range names {
		var p profiles.Profile
		if isProfilePath(n) {
			p, err = profiles.LoadDir(n)
		} else {
			p, err = profiles.Load(dir, n)
		}
		if err != nil {
			return nil, err
		}
		if err := checkValueCharts(p); err != nil {
			return nil, err
		}
		if prev, ok := byName[p.Name]; ok && !prev.SameSource(p) {
			return nil, fmt.Errorf("two active profiles are named %q: %s and %s", p.Name, prev.Label(), p.Label())
		}
		byName[p.Name] = p
		out = append(out, p)
	}
	return out, nil
}

// checkValueCharts rejects a profile values file named for no chart:
// composeChart looks profile values up by chart name, so such a file would
// load and then never apply.
func checkValueCharts(p profiles.Profile) error {
	for _, chart := range slices.Sorted(maps.Keys(p.Values)) {
		if !slices.Contains(allCharts, chart) {
			return fmt.Errorf("profile %s: values/%s.* is named for no chart (want one of %s)",
				p.Label(), chart, strings.Join(allCharts, ", "))
		}
	}
	return nil
}

type composed struct {
	Merged     map[string]any
	Provenance map[string]string
}

// composeChart stacks every layer for one chart, lowest first: rooket's
// generated base, the configuration home's sticky file if there is one, then
// each active profile in selection order.
func composeChart(chart string, base map[string]any, cloneDir clone.Dir,
	active []profiles.Profile) (composed, error) {

	layers := []values.Layer{{Name: "rooket base", Values: base}}

	if p := cloneDir.ValuesPath(chart); p != "" {
		sticky, err := values.LoadFile(p)
		if err != nil {
			return composed{}, err
		}
		if sticky != nil {
			layers = append(layers, values.Layer{Name: stickyLayerName(cloneDir), Values: sticky})
		}
	}

	for _, p := range active {
		if v, ok := p.Values[chart]; ok {
			layers = append(layers, values.Layer{Name: "profile:" + p.Label(), Values: v})
		}
	}

	merged, prov := values.Merge(layers)
	return composed{Merged: merged, Provenance: prov}, nil
}

// stickyLayerName labels composeChart's sticky-values layer by where it came
// from: a clone's own .rooket keeps that familiar name, while a user-named
// --config-dir is labeled by its role instead, so 'values show --layers'
// never attributes a key to a .rooket the user does not have.
func stickyLayerName(cloneDir clone.Dir) string {
	if cloneDir.Named() {
		return "--config-dir values"
	}
	return ".rooket/values"
}

func (c composed) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := values.Encode(c.Merged)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
