package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestChartName(t *testing.T) {
	for in, want := range map[string]string{
		"operator":          chartOperator,
		"rook-ceph":         chartOperator,
		"cluster":           chartCluster,
		"rook-ceph-cluster": chartCluster,
		"csi":               chartCSI,
		"ceph-csi-drivers":  chartCSI,
	} {
		got, err := chartName(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("chartName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := chartName("nope"); err == nil {
		t.Error("want an error for an unknown chart")
	}
}

// cloneWithConfig returns a clone whose .rooket/config.yaml holds config,
// written by hand as the README tells users to.
func cloneWithConfig(t *testing.T, config string) clone.Dir {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".rooket"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".rooket", "config.yaml"), config)
	return clone.Open(root)
}

func TestActiveProfileNames(t *testing.T) {
	d := cloneWithConfig(t, "profiles: [sticky]\n")

	t.Run("with appends to the sticky list", func(t *testing.T) {
		got, err := activeProfileNames(d, []string{"extra"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "sticky" || got[1] != "extra" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("with-only replaces it", func(t *testing.T) {
		got, err := activeProfileNames(d, nil, []string{"just-this"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "just-this" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("empty with-only clears", func(t *testing.T) {
		got, err := activeProfileNames(d, nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v", got)
		}
	})

	// pflag's StringArray can't express an empty list: `--with-only ""`
	// arrives here as []string{""}, not nil.
	t.Run("--with-only \"\" clears, as the flag actually delivers it", func(t *testing.T) {
		got, err := activeProfileNames(d, nil, []string{""}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("with also drops empty entries", func(t *testing.T) {
		got, err := activeProfileNames(d, []string{"", "extra"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "sticky" || got[1] != "extra" {
			t.Errorf("got %#v", got)
		}
	})
}

func TestComposeChartLayerOrder(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ValuesPath(chartCluster),
		[]byte("a: from-clone\nb: from-clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := composeChart(chartCluster,
		map[string]any{"a": "from-base", "b": "from-base", "d": "from-base"},
		d,
		[]profiles.Profile{{
			Name:   "p",
			Values: map[string]map[string]any{chartCluster: {"b": "from-profile"}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a": "from-clone",
		"b": "from-profile",
		"d": "from-base",
	}
	for k, v := range want {
		if got.Merged[k] != v {
			t.Errorf("%s = %v, want %v", k, got.Merged[k], v)
		}
	}
	if got.Provenance["b"] != "profile:p" {
		t.Errorf("provenance[b] = %q", got.Provenance["b"])
	}
}

func TestComposeChartProfileOrder(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}

	got, err := composeChart(chartCluster,
		map[string]any{"c": "from-base"},
		d,
		[]profiles.Profile{
			{
				Name:   "first",
				Values: map[string]map[string]any{chartCluster: {"c": "from-first"}},
			},
			{
				Name:   "second",
				Values: map[string]map[string]any{chartCluster: {"c": "from-second"}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got.Merged["c"] != "from-second" {
		t.Errorf("c = %v, want from-second (later profile should win)", got.Merged["c"])
	}
	if got.Provenance["c"] != "profile:second" {
		t.Errorf("provenance[c] = %q, want profile:second", got.Provenance["c"])
	}
}

func TestComposedWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "values.yaml")
	c := composed{Merged: map[string]any{"a": 1}}
	if err := c.write(p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Errorf("got %q", data)
	}
}

// writePathProfile creates a profile directory called name under root, with
// one values file per entry of vals (file name → body), and returns its path.
func writePathProfile(t *testing.T, root, name string, vals map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte("description: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for file, body := range vals {
		if err := os.WriteFile(filepath.Join(dir, "values", file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestIsProfilePath(t *testing.T) {
	for in, want := range map[string]bool{
		"rbd":           false,
		"my.profile":    false,
		"./mytest":      true,
		"/srv/t/mytest": true,
		"tests/mytest":  true,
		".":             true,
		"..":            true,
	} {
		if got := isProfilePath(in); got != want {
			t.Errorf("isProfilePath(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPathProfileReachesOnlyItsCharts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := writePathProfile(t, t.TempDir(), "mytest", map[string]string{
		"rook-ceph.yaml":         "marker: operator\n",
		"rook-ceph-cluster.yaml": "marker: cluster\n",
	})
	active, err := loadProfiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	d := clone.Open(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}

	for chart, want := range map[string]string{chartOperator: "operator", chartCluster: "cluster", chartCSI: ""} {
		c, err := composeChart(chart, map[string]any{}, d, active)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := c.Merged["marker"].(string)
		if got != want {
			t.Errorf("%s: marker = %q, want %q", chart, got, want)
		}
		if want != "" && c.Provenance["marker"] != "profile:"+path {
			t.Errorf("%s: provenance = %q, want profile:%s", chart, c.Provenance["marker"], path)
		}
	}
}

func TestLoadProfilesRejectsMisnamedValuesFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := writePathProfile(t, t.TempDir(), "mytest", map[string]string{"cluster.yaml": "a: 1\n"})

	_, err := loadProfiles([]string{path})
	if err == nil || !strings.Contains(err.Error(), "values/cluster.*") || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name values/cluster.* and %s", err, path)
	}
}

func TestBuiltInProfilesPassValueChartCheck(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	all, err := profiles.List(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		t.Fatal("no built-in profiles found")
	}
	if _, err := loadProfiles(names); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProfilesDuplicateNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	shadow := writePathProfile(t, root, "rbd", nil)
	a := writePathProfile(t, filepath.Join(root, "a"), "mytest", nil)
	b := writePathProfile(t, filepath.Join(root, "b"), "mytest", nil)

	t.Run("a path profile alongside the built-in of the same name", func(t *testing.T) {
		_, err := loadProfiles([]string{"rbd", shadow})
		if err == nil || !strings.Contains(err.Error(), shadow) {
			t.Errorf("err = %v, want it to name %s", err, shadow)
		}
	})

	t.Run("two directories with one basename", func(t *testing.T) {
		_, err := loadProfiles([]string{a, b})
		if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
			t.Errorf("err = %v, want it to name %s and %s", err, a, b)
		}
	})

	t.Run("the same directory twice is one source", func(t *testing.T) {
		t.Chdir(filepath.Join(root, "a"))
		if _, err := loadProfiles([]string{a, "./mytest"}); err != nil {
			t.Error(err)
		}
	})

	t.Run("a path profile may reuse a built-in's name on its own", func(t *testing.T) {
		got, err := loadProfiles([]string{shadow})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Name != "rbd" || got[0].Path != shadow || got[0].BuiltIn {
			t.Errorf("got %+v", got[0])
		}
	})
}

// The sticky layer is labeled by where it actually came from: a clone's own
// .rooket keeps the familiar name, but a named --config-dir must not be told
// it came from a .rooket the user does not have.
func TestComposeChartLabelsTheStickyLayerByItsSource(t *testing.T) {
	t.Run("a clone's own .rooket", func(t *testing.T) {
		d := clone.Open(t.TempDir())
		if err := d.Ensure(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.ValuesPath(chartCluster), []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := composeChart(chartCluster, map[string]any{}, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provenance["a"] != ".rooket/values" {
			t.Errorf("provenance[a] = %q, want .rooket/values", got.Provenance["a"])
		}
	})

	t.Run("a named --config-dir", func(t *testing.T) {
		d := clone.At(t.TempDir())
		if err := os.MkdirAll(filepath.Join(d.Path(), "values"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.ValuesPath(chartCluster), []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := composeChart(chartCluster, map[string]any{}, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Provenance["a"] != "--config-dir values" {
			t.Errorf("provenance[a] = %q, want \"--config-dir values\"", got.Provenance["a"])
		}
	})
}

func TestActiveProfileNamesRejectsStickyPath(t *testing.T) {
	d := cloneWithConfig(t, "profiles: [rbd, ./mytest]\n")

	_, err := activeProfileNames(d, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "./mytest") || !strings.Contains(err.Error(), "--with-only") {
		t.Errorf("err = %v, want it to name ./mytest and point at --with/--with-only", err)
	}

	// --with-only never reads the sticky list, so a path there is fine.
	got, err := activeProfileNames(d, nil, []string{"./mytest"}, true)
	if err != nil || len(got) != 1 || got[0] != "./mytest" {
		t.Errorf("got %#v, %v", got, err)
	}
}
