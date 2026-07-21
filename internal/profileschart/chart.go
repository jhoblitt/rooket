package profileschart

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Source is one contributor of Kubernetes resources: the clone's own templates
// or one active profile's.
type Source struct {
	Prefix string
	Files  map[string][]byte
}

// Context is exposed to templates as .Values.rooket.
type Context struct {
	ClusterName       string `yaml:"clusterName"`
	Namespace         string `yaml:"namespace"`
	OperatorNamespace string `yaml:"operatorNamespace"`
	Workers           int    `yaml:"workers"`
}

const chartYAML = `apiVersion: v2
name: rooket-profiles
description: Resources contributed by rooket's active profiles and the clone's templates
type: application
version: 0.0.0
appVersion: "0.0.0"
`

// Render writes a chart holding every source's templates and reports whether
// any were written. Helm owns their lifecycle, so a resource whose source is
// gone is pruned on the next upgrade rather than leaking as kubectl apply would.
//
// On error, dir may already contain Chart.yaml, values.yaml, and any
// templates written before the failure: Render does not clean up after
// itself, so a caller must not assume a failed Render left dir untouched.
func Render(dir string, ctx Context, sources []Source) (bool, error) {
	if dir == "" {
		return false, fmt.Errorf("dir must not be empty")
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("clear %s: %w", dir, err)
	}
	tmplDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", tmplDir, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		return false, fmt.Errorf("write Chart.yaml: %w", err)
	}
	vals, err := yaml.Marshal(map[string]any{"rooket": ctx})
	if err != nil {
		return false, fmt.Errorf("encode chart values: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), vals, 0o644); err != nil {
		return false, fmt.Errorf("write values.yaml: %w", err)
	}

	count := 0
	written := make(map[string]string)
	for _, s := range sources {
		names := make([]string, 0, len(s.Files))
		for n := range s.Files {
			names = append(names, n)
		}
		// Sorted so a collision error deterministically names the same
		// already-written source across runs, not whichever ran first.
		sort.Strings(names)
		for _, n := range names {
			out := filepath.Join(tmplDir, s.Prefix+"-"+n)
			if prev, ok := written[out]; ok {
				return false, fmt.Errorf("template path collision: %q produced by both %q and %q", out, prev, s.Prefix)
			}
			if err := os.WriteFile(out, s.Files[n], 0o644); err != nil {
				return false, fmt.Errorf("write %s: %w", out, err)
			}
			written[out] = s.Prefix
			count++
		}
	}
	return count > 0, nil
}
