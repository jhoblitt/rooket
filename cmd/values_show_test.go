package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/chartcache"
)

// TestValuesShowInheritsWithOnlyFlag exercises the real command tree so a
// regression in valuesCmd.PersistentPreRunE — e.g. the "with-only" literal
// getting out of sync with the flag name, or a future subcommand shadowing
// this hook (cobra runs only the nearest ancestor's PersistentPreRunE) — fails
// a test instead of silently making --with-only a no-op.
func TestValuesShowInheritsWithOnlyFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prevValuesDir := valuesDir
	silenced := valuesShowCmd.SilenceUsage
	t.Cleanup(func() {
		valuesShowCmd.SilenceUsage = silenced
		deployWith, deployWithOnly = nil, nil
		deployWithOnlySet = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		dirFlag := valuesCmd.PersistentFlags().Lookup("dir")
		_ = dirFlag.Value.Set(dirFlag.DefValue)
		dirFlag.Changed = false
		// with-only's Value is already reset by the deployWithOnly assignment
		// above (the same *[]string the flag binds); routing its DefValue
		// "[]" through Set would instead append a stray "[]" element —
		// stringArrayValue.Set appends rather than replaces once its private
		// "already set" latch trips, which happened above when --with-only
		// rbd ran in the second subtest.
		valuesCmd.PersistentFlags().Lookup("with-only").Changed = false
		// Last, since resetting "dir" above writes valuesDir through its
		// bound flag.Value; this restore must win.
		valuesDir = prevValuesDir
	})
	// show follows the cluster's source record; the developer's own records
	// could otherwise point it at a released version or a missing directory.
	t.Setenv("ROOKET_CONFIG_DIR", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

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

	base, err := showBase(chartCluster, rookSource{charts: rookCloneWithBlockPool(t)}, 0)
	if err != nil {
		t.Fatalf("showBase: %v", err)
	}
	if size := blockPoolSize(t, base); size != 1 {
		t.Errorf("block pool size = %#v, want 1 for the recorded single worker", size)
	}
}

func TestShowBaseOfAReleasedOperatorKeepsTheChartsImage(t *testing.T) {
	base, err := showBase(chartOperator, rookSource{released: "v1.20.7"}, 0)
	if err != nil {
		t.Fatalf("showBase: %v", err)
	}
	if _, ok := base["image"]; ok {
		t.Errorf("image = %#v, want none: a released chart pins its own", base["image"])
	}
}

// values show renders what a deploy of the cluster in scope would: for a
// released cluster, the recorded version's cached charts.
func TestValuesSourceOfAReleasedCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "released")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	stubChartPuller(t)
	cfg := t.TempDir()
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}); err != nil {
		t.Fatal(err)
	}

	src, err := valuesSource(valuesShowCmd)
	if err != nil {
		t.Fatalf("valuesSource: %v", err)
	}
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want the recorded v1.20.7", src.released)
	}
	root, err := chartcache.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "v1.20.7"); src.charts != want {
		t.Errorf("charts = %q, want the chart cache entry %s", src.charts, want)
	}
	if got := src.config.ValuesPath(chartCluster); got != filepath.Join(cfg, "values", chartCluster+".yaml") {
		t.Errorf("config ValuesPath = %q, want the recorded directory's", got)
	}
}

// Unchecked, clone.Open(dir).Ensure() would MkdirAll a typo'd --dir into a
// fresh, empty .rooket tree and values would render with no sticky
// configuration, silently.
func TestValuesSourceReleasedRefusesAMissingDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "released")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	stubChartPuller(t)
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}
	prev := valuesDir
	t.Cleanup(func() { valuesDir = prev })
	missing := filepath.Join(t.TempDir(), "typo")
	valuesDir = missing

	if _, err := valuesSource(valuesShowCmd); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("valuesSource = %v, want the missing --dir refused, naming %s", err, missing)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a typo'd --dir got created rather than refused")
	}
}

// parseValuesFlags parses args into valuesShowCmd's flags (which inherit
// valuesCmd's persistent ones, cobra's own reuse of --dir/--rook-version/
// --config-dir across every values subcommand), and resets every flag these
// tests set when the test ends.
func parseValuesFlags(t *testing.T, args ...string) {
	t.Helper()
	if err := valuesShowCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"rook-version", "config-dir"} {
			f := valuesShowCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
}

// values has no --name flag of its own, so a released --rook-version outside
// a clone must still be refused the fallback name deploy and up refuse: two
// unrelated consumers on the host would otherwise share the "rook" cluster.
func TestValuesSourceReleasedRefusesTheFallbackName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "")
	t.Chdir(t.TempDir())
	stubChartPuller(t)
	parseValuesFlags(t, "--rook-version=v1.20.7")

	if _, err := valuesSource(valuesShowCmd); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("valuesSource = %v, want the fallback name refused, pointing at --name", err)
	}
}

// A mutation turning configHome(rec, dir) into an unconditional clone.Open(dir)
// would send a clone-mode values command back to the clone's own .rooket even
// when a configuration directory is named; nothing else exercises this branch
// with a real clone in scope.
func TestValuesSourceCloneModeHonorsNamedConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "clone-with-config")
	t.Setenv("ROOK_DIR", "")
	prev := valuesDir
	t.Cleanup(func() { valuesDir = prev })
	valuesDir = ""
	rookDir := t.TempDir()
	writeGoMod(t, rookDir, rookModulePath)
	t.Chdir(rookDir)
	config := t.TempDir()
	t.Setenv("ROOKET_CONFIG_DIR", config)

	src, err := valuesSource(valuesShowCmd)
	if err != nil {
		t.Fatalf("valuesSource: %v", err)
	}
	if got, want := src.config.ValuesPath(chartCluster), filepath.Join(config, "values", chartCluster+".yaml"); got != want {
		t.Errorf("config ValuesPath = %q, want %q under the named --config-dir, not the clone's own .rooket", got, want)
	}
}

// A released cluster run outside any clone, with no directory named, has
// nowhere to keep overrides.
func TestValuesEditRefusesWithoutAConfigurationHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "released")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	t.Setenv("VISUAL", "false")
	t.Chdir(t.TempDir())
	stubChartPuller(t)
	prev := valuesDir
	valuesDir = ""
	t.Cleanup(func() { valuesDir = prev })
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}

	err := valuesEditCmd.RunE(valuesEditCmd, []string{"operator"})
	if err == nil || !strings.Contains(err.Error(), "no configuration to edit") {
		t.Fatalf("values edit = %v, want a refusal naming the missing configuration", err)
	}
	for _, way := range []string{"--config-dir", "--dir", "$ROOKET_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), way) {
			t.Errorf("refusal %q does not offer %s as a way to supply a configuration home", err, way)
		}
	}
}
