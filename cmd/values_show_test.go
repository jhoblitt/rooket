package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValuesShowInheritsWithOnlyFlag exercises the real command tree so a
// regression in valuesCmd.PersistentPreRunE — e.g. the "with-only" literal
// getting out of sync with the flag name, or a future subcommand shadowing
// this hook (cobra runs only the nearest ancestor's PersistentPreRunE) — fails
// a test instead of silently making --with-only a no-op.
func TestValuesShowInheritsWithOnlyFlag(t *testing.T) {
	t.Cleanup(func() {
		deployWith, deployWithOnly, deployValueFiles, deploySets = nil, nil, nil, nil
		deployWithOnlySet = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	})

	dir := t.TempDir()

	// "without" runs first: pflag's Changed latches true permanently once set
	// and is never reset between Execute() calls on a reused command tree, so
	// this order is the only one where the "unset" case isn't contaminated by
	// the earlier --with-only invocation.
	t.Run("without with-only leaves the flag unset", func(t *testing.T) {
		var out strings.Builder
		rootCmd.SetOut(&out)
		rootCmd.SetArgs([]string{"values", "show", "cluster", "--dir", dir})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rooket values show failed: %v", err)
		}
		if deployWithOnlySet {
			t.Error("deployWithOnlySet = true, want false without --with-only")
		}
	})

	t.Run("with-only sets the flag and value", func(t *testing.T) {
		var out strings.Builder
		rootCmd.SetOut(&out)
		rootCmd.SetArgs([]string{"values", "show", "cluster", "--with-only", "rbd", "--dir", dir})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rooket values show failed: %v", err)
		}
		if !deployWithOnlySet {
			t.Error("deployWithOnlySet = false, want true after --with-only")
		}
		if len(deployWithOnly) != 1 || deployWithOnly[0] != "rbd" {
			t.Errorf("deployWithOnly = %#v, want [\"rbd\"]", deployWithOnly)
		}
	})
}

func TestPrintSetsNote(t *testing.T) {
	t.Run("present when --set is supplied", func(t *testing.T) {
		var out strings.Builder
		printSetsNote(&out, []string{"a=b"})
		if !strings.Contains(out.String(), "--set") {
			t.Errorf("got %q, want a note mentioning --set", out.String())
		}
	})

	t.Run("absent when --set is not supplied", func(t *testing.T) {
		var out strings.Builder
		printSetsNote(&out, nil)
		if out.String() != "" {
			t.Errorf("got %q, want no output without --set", out.String())
		}
	})
}

func TestRenderShow(t *testing.T) {
	c := composed{
		Merged:     map[string]any{"a": 1, "m": map[string]any{"b": 2}},
		Provenance: map[string]string{"a": "rooket base", "m.b": "profile:rgw"},
	}

	t.Run("plain yaml", func(t *testing.T) {
		got, err := renderShow(c, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "a: 1") {
			t.Errorf("got %q", got)
		}
		if strings.Contains(got, "profile:rgw") {
			t.Errorf("provenance leaked into plain output: %q", got)
		}
	})

	t.Run("with layers", func(t *testing.T) {
		got, err := renderShow(c, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"a", "rooket base", "m.b", "profile:rgw"} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})
}

// rookCloneWithBlockPool plants a rook clone whose rook-ceph-cluster chart
// defaults to one three-replica block pool, and returns its path.
func rookCloneWithBlockPool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	chart := filepath.Join(dir, "deploy", "charts", chartCluster)
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	defaults := "cephBlockPools:\n  - name: ceph-blockpool\n    spec:\n      replicated:\n        size: 3\n"
	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), []byte(defaults), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func blockPoolSize(t *testing.T, base map[string]any) any {
	t.Helper()
	pools, ok := base["cephBlockPools"].([]any)
	if !ok || len(pools) == 0 {
		t.Fatalf("cephBlockPools = %#v, want the chart's pool fitted to the cluster", base["cephBlockPools"])
	}
	return pools[0].(map[string]any)["spec"].(map[string]any)["replicated"].(map[string]any)["size"]
}

func TestClusterBaseReadsTheChartsPools(t *testing.T) {
	base, err := clusterBase(rookCloneWithBlockPool(t), 1, nil)
	if err != nil {
		t.Fatalf("clusterBase: %v", err)
	}
	if size := blockPoolSize(t, base); size != 1 {
		t.Errorf("block pool size = %#v, want 1 for a single-worker cluster", size)
	}
}

// 'values show' and 'values edit' render what a deploy would, so they size the
// pools for the cluster's recorded worker count too.
func TestShowBaseUsesTheRecordedShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "single")
	if err := writeShape("single", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
		t.Fatal(err)
	}

	base, err := showBase(chartCluster, rookCloneWithBlockPool(t))
	if err != nil {
		t.Fatalf("showBase: %v", err)
	}
	if size := blockPoolSize(t, base); size != 1 {
		t.Errorf("block pool size = %#v, want 1 for the recorded single worker", size)
	}
}
