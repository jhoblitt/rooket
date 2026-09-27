package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

// parseWorkersFlags parses args into c's flags, and resets --workers and --dir,
// value and Changed state both, when the test ends.
func parseWorkersFlags(t *testing.T, c *cobra.Command, args ...string) {
	t.Helper()
	prevDir := valuesDir
	t.Cleanup(func() {
		for _, name := range []string{"workers", "dir"} {
			f := c.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		// Last, since resetting "dir" above writes valuesDir through its bound
		// flag.Value; this restore must win.
		valuesDir = prevDir
	})
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
}

// previewEnv isolates a values command from the developer's own clusters and
// configuration, naming the cluster it renders for "preview".
func previewEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "preview")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	t.Setenv("ROOK_DIR", "")
}

// showClusterValues runs 'values show cluster' against the rook clone dir with
// args and returns the values it prints.
func showClusterValues(t *testing.T, dir string, args ...string) map[string]any {
	t.Helper()
	parseWorkersFlags(t, valuesShowCmd, append([]string{"--dir", dir}, args...)...)
	var runErr error
	out := captureStdout(t, func() { runErr = valuesShowCmd.RunE(valuesShowCmd, []string{"cluster"}) })
	if runErr != nil {
		t.Fatalf("values show cluster %v: %v", args, runErr)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("values show printed unparseable YAML: %v\n%s", err, out)
	}
	return got
}

// A harness previews what a cluster it has not brought up yet will get, which
// has no recorded shape to read the worker count from.
func TestValuesShowWorkers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded int // 0 records no shape
		args     []string
		wantFit  bool
	}{
		{name: "--workers 1 fits a cluster with no recorded shape", args: []string{"--workers", "1"}, wantFit: true},
		{name: "unset, no recorded shape leaves the chart's sizing"},
		{name: "unset follows a recorded single worker", recorded: 1, wantFit: true},
		{name: "--workers 1 overrides a recorded three", recorded: 3, args: []string{"--workers", "1"}, wantFit: true},
		{name: "--workers 3 overrides a recorded one", recorded: 1, args: []string{"--workers", "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previewEnv(t)
			if tc.recorded > 0 {
				if err := writeShape("preview", clusterShape{Workers: tc.recorded, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
					t.Fatal(err)
				}
			}

			got := showClusterValues(t, rookCloneWithBlockPool(t), tc.args...)
			spec, _ := got["cephClusterSpec"].(map[string]any)
			mon, hasMon := spec["mon"].(map[string]any)
			if !tc.wantFit {
				if hasMon {
					t.Errorf("cephClusterSpec.mon = %#v, want the chart's own mons", mon)
				}
				if pools, ok := got["cephBlockPools"]; ok {
					t.Errorf("cephBlockPools = %#v, want the chart's own pools", pools)
				}
				return
			}
			if !hasMon || mon["count"] != 1 {
				t.Errorf("cephClusterSpec.mon = %#v, want count 1 for one worker", spec["mon"])
			}
			if size := blockPoolSize(t, got); size != 1 {
				t.Errorf("block pool size = %#v, want 1 for one worker", size)
			}
		})
	}
}

// values edit seeds a new overrides file with the base a deploy would use, so
// --workers fits that seed as it does show's output.
func TestValuesEditSeedsTheWorkersFitting(t *testing.T) {
	previewEnv(t)
	// The editor prints the seed and leaves it unedited, which edit takes as
	// an empty layer and saves nothing.
	t.Setenv("VISUAL", "cat")
	parseWorkersFlags(t, valuesEditCmd, "--dir", rookCloneWithBlockPool(t), "--workers", "1")

	var runErr error
	out := captureStdout(t, func() { runErr = valuesEditCmd.RunE(valuesEditCmd, []string{"cluster"}) })
	if runErr != nil {
		t.Fatalf("values edit cluster --workers 1: %v", runErr)
	}
	if !strings.Contains(out, "osd_pool_default_size = 1") {
		t.Errorf("seed does not carry the one-worker fitting's osd_pool_default_size = 1:\n%s", out)
	}
}

func TestValuesWorkersRefusesFewerThanOne(t *testing.T) {
	for _, c := range []*cobra.Command{valuesShowCmd, valuesEditCmd} {
		for _, n := range []string{"0", "-1"} {
			t.Run(c.Name()+" --workers "+n, func(t *testing.T) {
				previewEnv(t)
				// An edit that got past the refusal fails here rather than
				// opening the developer's own editor.
				t.Setenv("VISUAL", "false")
				parseWorkersFlags(t, c, "--dir", rookCloneWithBlockPool(t), "--workers="+n)

				err := c.RunE(c, []string{"cluster"})
				if want := "--workers must be more than 0, not " + n; err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("values %s --workers %s = %v, want %q", c.Name(), n, err, want)
				}
			})
		}
	}
}
