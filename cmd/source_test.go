package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/chartcache"
)

func TestSourceRecordRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := clusterSource{RookVersion: "v1.20.7", ConfigDir: "/work/rgw-go/hack/rooket"}
	if err := writeSource("c1", want); err != nil {
		t.Fatal(err)
	}
	if got, ok := readSource("c1"); !ok || got != want {
		t.Errorf("readSource = (%+v, %v), want (%+v, true)", got, ok, want)
	}
	if _, ok := readSource("never-deployed"); ok {
		t.Error("readSource of a cluster with no record = ok, want none")
	}
}

func TestResolveSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_CONFIG_DIR", "")
	cfg := t.TempDir()

	t.Run("record fills unset flags", func(t *testing.T) {
		if err := writeSource("rec", clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
		got, changed, err := resolveSource("rec", "", false, "", false)
		if err != nil || changed || got != (clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}) {
			t.Errorf("resolveSource = (%+v, %v, %v), want the record unchanged", got, changed, err)
		}
	})

	// The change is reported so the caller records it for every later command,
	// and it leaves the rest of the record alone.
	t.Run("a version flag replaces the record", func(t *testing.T) {
		if err := writeSource("upgrade", clusterSource{RookVersion: "v1.20.6", ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
		got, changed, err := resolveSource("upgrade", "v1.20.7", true, "", false)
		if err != nil || !changed || got != (clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}) {
			t.Errorf("resolveSource = (%+v, %v, %v), want v1.20.7 with ConfigDir %s kept, and changed", got, changed, err, cfg)
		}
	})

	t.Run("the recorded version is no change", func(t *testing.T) {
		if err := writeSource("same", clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
		got, changed, err := resolveSource("same", "v1.20.7", true, "", false)
		if err != nil || changed || got.RookVersion != "v1.20.7" {
			t.Errorf("resolveSource = (%+v, %v, %v), want v1.20.7 and unchanged", got, changed, err)
		}
	})

	t.Run("a range is rejected", func(t *testing.T) {
		if _, _, err := resolveSource("x", "v1.20.x", true, "", false); err == nil {
			t.Error("resolveSource accepted v1.20.x, want it rejected")
		}
	})

	t.Run("the environment names a configuration directory", func(t *testing.T) {
		t.Setenv("ROOKET_CONFIG_DIR", cfg)
		got, _, err := resolveSource("env", "", false, "", false)
		if err != nil || got.ConfigDir != cfg {
			t.Errorf("resolveSource = (%+v, %v), want ConfigDir %s", got, err, cfg)
		}
	})

	t.Run("the environment beats the record", func(t *testing.T) {
		if err := writeSource("env-rec", clusterSource{ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
		other := t.TempDir()
		t.Setenv("ROOKET_CONFIG_DIR", other)
		got, changed, err := resolveSource("env-rec", "", false, "", false)
		if err != nil || !changed || got.ConfigDir != other {
			t.Errorf("resolveSource = (%+v, %v, %v), want the environment's %s and changed", got, changed, err, other)
		}
	})

	t.Run("a flag beats the environment", func(t *testing.T) {
		t.Setenv("ROOKET_CONFIG_DIR", t.TempDir())
		got, _, err := resolveSource("both", "", false, cfg, true)
		if err != nil || got.ConfigDir != cfg {
			t.Errorf("resolveSource = (%+v, %v), want the flag's %s", got, err, cfg)
		}
	})

	t.Run("a missing configuration directory is an error naming it", func(t *testing.T) {
		missing := filepath.Join(cfg, "typo")
		_, _, err := resolveSource("typo", "", false, missing, true)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("resolveSource = %v, want an error naming %s", err, missing)
		}
	})

	// Passed through unchecked, a directory deleted or moved since it was
	// recorded would leave every later command with an empty configuration.
	t.Run("a recorded directory that is gone is an error naming it", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone")
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeSource("gone", clusterSource{ConfigDir: gone}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		_, _, err := resolveSource("gone", "", false, "", false)
		if err == nil || !strings.Contains(err.Error(), gone) {
			t.Errorf("resolveSource = %v, want an error naming %s", err, gone)
		}
	})

	t.Run("a file is not a configuration directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := resolveSource("file", "", false, file, true)
		if err == nil || !strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("resolveSource = %v, want an error saying %s is not a directory", err, file)
		}
	})

	// Recorded relative, it would name a different directory from wherever the
	// next command runs.
	t.Run("a relative directory is recorded absolute", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Mkdir(filepath.Join(parent, "hack"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(parent)
		t.Setenv("ROOKET_CONFIG_DIR", "hack")
		got, _, err := resolveSource("rel", "", false, "", false)
		if err != nil || got.ConfigDir != filepath.Join(parent, "hack") {
			t.Errorf("resolveSource = (%+v, %v), want ConfigDir %s", got, err, filepath.Join(parent, "hack"))
		}
	})
}

func TestConfigHome(t *testing.T) {
	named := t.TempDir()
	clone := t.TempDir()
	if got := configHome(clusterSource{ConfigDir: named}, clone).ValuesPath("rook-ceph"); got != filepath.Join(named, "values", "rook-ceph.yaml") {
		t.Errorf("with a named directory, ValuesPath = %q, want it under %s", got, named)
	}
	if got := configHome(clusterSource{RookVersion: "v1.20.7"}, clone).ValuesPath("rook-ceph"); got != filepath.Join(clone, ".rooket", "values", "rook-ceph.yaml") {
		t.Errorf("released inside a clone, ValuesPath = %q, want the clone's .rooket", got)
	}
	if got := configHome(clusterSource{RookVersion: "v1.20.7"}, "").ValuesPath("rook-ceph"); got != "" {
		t.Errorf("with neither, ValuesPath = %q, want no configuration", got)
	}
}

// stubChartPuller makes releasedCharts unpack stand-in charts into a
// throwaway cache instead of running helm.
func stubChartPuller(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	prev := chartPuller
	chartPuller = func([]string) chartcache.Puller {
		return func(dir, chart, version string) error {
			c := filepath.Join(dir, chart)
			if err := os.MkdirAll(c, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(c, "Chart.yaml"), []byte("name: "+chart+"\n"), 0o644)
		}
	}
	t.Cleanup(func() { chartPuller = prev })
}

func TestReleasedChartsUsesTheHostWideCache(t *testing.T) {
	stubChartPuller(t)
	entry, err := releasedCharts("v1.20.7")
	if err != nil {
		t.Fatalf("releasedCharts: %v", err)
	}
	root, _ := chartcache.DefaultRoot()
	if entry != filepath.Join(root, "v1.20.7") {
		t.Errorf("entry = %q, want it under %s", entry, root)
	}
}
