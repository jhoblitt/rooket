package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
)

// chartDep is one entry of a helm Chart.yaml dependencies block.
type chartDep struct {
	name       string
	version    string
	repository string
	condition  string
}

// chartDeps parses the dependency entries of a helm Chart.yaml: entries
// begin at "- name:" lines and carry the version/repository/condition
// fields that follow, until the next "- name:" line.
func chartDeps(chartYAML string) ([]chartDep, error) {
	data, err := os.ReadFile(chartYAML)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", chartYAML, err)
	}
	var deps []chartDep
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if n, ok := strings.CutPrefix(t, "- name:"); ok {
			deps = append(deps, chartDep{name: strings.TrimSpace(n)})
			continue
		}
		if len(deps) == 0 {
			continue
		}
		cur := &deps[len(deps)-1]
		if v, ok := strings.CutPrefix(t, "version:"); ok {
			cur.version = strings.Trim(strings.TrimSpace(v), `"'`)
		}
		if r, ok := strings.CutPrefix(t, "repository:"); ok {
			cur.repository = strings.Trim(strings.TrimSpace(r), `"'`)
		}
		if c, ok := strings.CutPrefix(t, "condition:"); ok {
			cur.condition = strings.TrimSpace(c)
		}
	}
	return deps, nil
}

// ensureChartDeps restores a chart's fetchable dependency archives before a
// deploy from the source tree. The archives are gitignored build INPUTS of
// the deploy (helm refuses to install a chart dir with missing deps): rook's
// make normally leaves them behind, but 'make clean' deletes them while the
// build-skip stamp and registry stay valid, so deploy must not depend on
// make having run. file:// dependencies live in the tree and need nothing.
func ensureChartDeps(rookDir, chart string) error {
	chartDir := filepath.Join(rookDir, "deploy", "charts", chart)
	deps, err := chartDeps(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return err
	}
	missing := false
	for _, d := range deps {
		if d.version == "" || !strings.HasPrefix(d.repository, "http") {
			continue
		}
		if _, err := os.Stat(filepath.Join(chartDir, "charts", d.name+"-"+d.version+".tgz")); err != nil {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	env, err := helmEnv(deployName, "make")
	if err != nil {
		return err
	}
	run.Printf("==> restoring helm chart dependencies for %s\n", chart)
	if err := run.CmdWithEnv(env, "helm", "dependency", "build", chartDir); err != nil {
		// A Chart.lock out of sync with Chart.yaml fails 'build'; resolve
		// afresh instead.
		return run.CmdWithEnv(env, "helm", "dependency", "update", chartDir)
	}
	return nil
}

// pruneStaleChartDeps deletes archives under deploy/charts/*/charts/ whose
// name-version doesn't match a dependency pinned in that chart's Chart.yaml.
// rook's make freezes a `find` of each chart dir as the package rule's
// prerequisites before its `helm dependency update` recipe deletes outdated
// archives, so an archive left by a build at another rook ref (older pin, or
// another branch) kills make mid-run with "No rule to make target". Removing
// it before make parses keeps the frozen prerequisite list consistent.
func pruneStaleChartDeps(out io.Writer, rookDir string) error {
	chartsDir := filepath.Join(rookDir, "deploy", "charts")
	charts, err := os.ReadDir(chartsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", chartsDir, err)
	}
	for _, chart := range charts {
		if !chart.IsDir() {
			continue
		}
		archives, err := filepath.Glob(filepath.Join(chartsDir, chart.Name(), "charts", "*.tgz"))
		if err != nil || len(archives) == 0 {
			continue
		}
		deps, err := chartDeps(filepath.Join(chartsDir, chart.Name(), "Chart.yaml"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		expected := make(map[string]bool)
		for _, d := range deps {
			if d.name != "" && d.version != "" {
				expected[d.name+"-"+d.version+".tgz"] = true
			}
		}
		for _, a := range archives {
			if expected[filepath.Base(a)] {
				continue
			}
			if fi, err := os.Lstat(a); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			run.Fprintf(out, "pruning stale helm dependency archive %s\n", a)
			if err := os.Remove(a); err != nil {
				return fmt.Errorf("remove %s: %w", a, err)
			}
		}
	}
	return nil
}
