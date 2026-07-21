package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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

// activeProfileNames resolves the clone's sticky list against the flags:
// --with appends to it, --with-only replaces it. withOnlySet distinguishes an
// unset flag from --with-only "", which clears the selection.
func activeProfileNames(cloneDir clone.Dir, with, withOnly []string, withOnlySet bool) ([]string, error) {
	if withOnlySet {
		return withOnly, nil
	}
	sticky, err := cloneDir.Profiles()
	if err != nil {
		return nil, err
	}
	return append(sticky, with...), nil
}

func loadProfiles(names []string) ([]profiles.Profile, error) {
	dir, err := userProfileDir()
	if err != nil {
		return nil, err
	}
	out := make([]profiles.Profile, 0, len(names))
	for _, n := range names {
		p, err := profiles.Load(dir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

type composed struct {
	Merged     map[string]any
	Provenance map[string]string
}

// composeChart stacks every layer for one chart, lowest first: rooket's
// generated base, the clone's sticky file, each active profile in selection
// order, then any -f files. --set is not represented here; helm applies it
// above everything rooket writes.
func composeChart(chart string, base map[string]any, cloneDir clone.Dir,
	active []profiles.Profile, extraFiles []string) (composed, error) {

	layers := []values.Layer{{Name: "rooket base", Values: base}}

	sticky, err := values.LoadFile(cloneDir.ValuesPath(chart))
	if err != nil {
		return composed{}, err
	}
	if sticky != nil {
		layers = append(layers, values.Layer{Name: ".rooket/values", Values: sticky})
	}

	for _, p := range active {
		if v, ok := p.Values[chart]; ok {
			layers = append(layers, values.Layer{Name: "profile:" + p.Name, Values: v})
		}
	}

	for _, f := range extraFiles {
		v, err := values.LoadFile(f)
		if err != nil {
			return composed{}, err
		}
		if v == nil {
			return composed{}, fmt.Errorf("values file %s does not exist", f)
		}
		layers = append(layers, values.Layer{Name: "-f " + f, Values: v})
	}

	merged, prov := values.Merge(layers)
	return composed{Merged: merged, Provenance: prov}, nil
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
