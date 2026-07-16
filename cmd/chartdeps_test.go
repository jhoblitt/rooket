package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestChartDeps(t *testing.T) {
	t.Run("master style: alias, condition, mixed quoting", func(t *testing.T) {
		p := writeChartYAML(t, `apiVersion: v2
name: rook-ceph
version: 0.0.1
appVersion: 0.0.1
sources:
  - https://github.com/rook/rook
dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
  - name: ceph-csi-operator
    version: 1.0.4
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.installCsiOperator
`)
		deps, err := chartDeps(p)
		if err != nil {
			t.Fatal(err)
		}
		want := []chartDep{
			{name: "library", version: "0.0.1"},
			{name: "ceph-csi-operator", version: "1.0.4", condition: "csi.installCsiOperator"},
		}
		if !reflect.DeepEqual(deps, want) {
			t.Fatalf("got %+v, want %+v", deps, want)
		}
	})

	t.Run("unindented list items (rook-ceph-cluster style)", func(t *testing.T) {
		p := writeChartYAML(t, `apiVersion: v2
name: rook-ceph-cluster
version: 0.0.1
dependencies:
- name: library
  version: "0.0.1"
  repository: "file://../library"
`)
		deps, err := chartDeps(p)
		if err != nil {
			t.Fatal(err)
		}
		want := []chartDep{{name: "library", version: "0.0.1"}}
		if !reflect.DeepEqual(deps, want) {
			t.Fatalf("got %+v, want %+v", deps, want)
		}
	})

	t.Run("no dependencies", func(t *testing.T) {
		p := writeChartYAML(t, `apiVersion: v2
name: library
version: 0.0.1
type: library
`)
		deps, err := chartDeps(p)
		if err != nil || len(deps) != 0 {
			t.Fatalf("got (%+v, %v), want no deps and nil error", deps, err)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := chartDeps(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("got nil error for missing file")
		}
	})
}

// writeChartTree builds <root>/deploy/charts/<chart>/ with an optional
// Chart.yaml and the given entries under charts/, returning the charts/ dir.
func writeChartTree(t *testing.T, root, chart, chartYAML string, archives ...string) string {
	t.Helper()
	dir := filepath.Join(root, "deploy", "charts", chart)
	depDir := filepath.Join(dir, "charts")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if chartYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range archives {
		if err := os.WriteFile(filepath.Join(depDir, a), []byte("archive"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return depDir
}

func TestPruneStaleChartDeps(t *testing.T) {
	rookCephYAML := `dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
  - name: ceph-csi-operator
    version: 1.0.4
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.installCsiOperator
`

	t.Run("stale and unknown archives removed, pinned ones kept", func(t *testing.T) {
		root := t.TempDir()
		depDir := writeChartTree(t, root, "rook-ceph", rookCephYAML,
			"ceph-csi-operator-1.0.1.tgz", "ceph-csi-operator-1.0.4.tgz",
			"library-0.0.1.tgz", "unrelated-2.0.0.tgz")
		if err := os.WriteFile(filepath.Join(depDir, "notes.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../library", filepath.Join(depDir, "library")); err != nil {
			t.Fatal(err)
		}

		if err := pruneStaleChartDeps(root); err != nil {
			t.Fatal(err)
		}

		for _, gone := range []string{"ceph-csi-operator-1.0.1.tgz", "unrelated-2.0.0.tgz"} {
			if _, err := os.Lstat(filepath.Join(depDir, gone)); err == nil {
				t.Errorf("%s still present, want removed", gone)
			}
		}
		for _, kept := range []string{"ceph-csi-operator-1.0.4.tgz", "library-0.0.1.tgz", "notes.txt", "library"} {
			if _, err := os.Lstat(filepath.Join(depDir, kept)); err != nil {
				t.Errorf("%s missing, want kept: %v", kept, err)
			}
		}
	})

	t.Run("archives without a Chart.yaml are left alone", func(t *testing.T) {
		root := t.TempDir()
		depDir := writeChartTree(t, root, "mystery", "", "mystery-dep-1.2.3.tgz")
		if err := pruneStaleChartDeps(root); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(depDir, "mystery-dep-1.2.3.tgz")); err != nil {
			t.Errorf("archive missing, want kept: %v", err)
		}
	})

	t.Run("chart without a charts dir is skipped", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "deploy", "charts", "library")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("name: library\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := pruneStaleChartDeps(root); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing deploy/charts is a no-op", func(t *testing.T) {
		if err := pruneStaleChartDeps(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
}
