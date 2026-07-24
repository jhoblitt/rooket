package cmd

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestChartRepos(t *testing.T) {
	rookDir := t.TempDir()
	writeChartTree(t, rookDir, "rook-ceph", `apiVersion: v2
name: rook-ceph
dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
  - name: ceph-csi-operator
    version: 1.0.4
    repository: https://ceph.github.io/ceph-csi-operator
`)
	writeChartTree(t, rookDir, "rook-ceph-cluster", `apiVersion: v2
name: rook-ceph-cluster
`)
	got := chartRepos(rookDir)
	want := map[string]string{"ceph-csi-operator": "https://ceph.github.io/ceph-csi-operator"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chartRepos = %v, want %v (file:// dependencies need no repository)", got, want)
	}

	t.Run("no charts directory", func(t *testing.T) {
		if got := chartRepos(t.TempDir()); len(got) != 0 {
			t.Errorf("chartRepos = %v, want none", got)
		}
	})
}

func TestRegisteredRepoURLs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "repositories.yaml")
	if err := os.WriteFile(p, []byte(`apiVersion: ""
generated: "0001-01-01T00:00:00Z"
repositories:
  - name: ceph-csi-operator
    url: https://ceph.github.io/ceph-csi-operator/
  - name: rook-release
    url: https://charts.rook.io/release
`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HELM_CONFIG_HOME=/x", "HELM_REPOSITORY_CONFIG=" + p}
	got := registeredRepoURLs(env)
	// The trailing slash helm records must not make an already-registered
	// repository look missing.
	if !got["https://ceph.github.io/ceph-csi-operator"] || !got["https://charts.rook.io/release"] {
		t.Errorf("registeredRepoURLs = %v, want both repositories", got)
	}

	t.Run("a configuration helm has not written yet", func(t *testing.T) {
		got := registeredRepoURLs([]string{"HELM_REPOSITORY_CONFIG=" + filepath.Join(t.TempDir(), "repositories.yaml")})
		if len(got) != 0 {
			t.Errorf("registeredRepoURLs = %v, want none", got)
		}
	})
}

func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "HELM_REPOSITORY_CONFIG=/tmp/repositories.yaml", "B=2"}
	if got := envValue(env, "HELM_REPOSITORY_CONFIG"); got != "/tmp/repositories.yaml" {
		t.Errorf("envValue = %q, want the config path", got)
	}
	if got := envValue(env, "MISSING"); got != "" {
		t.Errorf("envValue = %q, want empty", got)
	}
}

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
			{name: "library", version: "0.0.1", repository: "file://../library"},
			{name: "ceph-csi-operator", version: "1.0.4",
				repository: "https://ceph.github.io/ceph-csi-operator", condition: "csi.installCsiOperator"},
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
		want := []chartDep{{name: "library", version: "0.0.1", repository: "file://../library"}}
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

		if err := pruneStaleChartDeps(io.Discard, root); err != nil {
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
		if err := pruneStaleChartDeps(io.Discard, root); err != nil {
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
		if err := pruneStaleChartDeps(io.Discard, root); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing deploy/charts is a no-op", func(t *testing.T) {
		if err := pruneStaleChartDeps(io.Discard, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
}
