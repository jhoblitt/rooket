package cmd

import (
	"os"
	"path/filepath"
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

func TestActiveProfileNames(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.SetProfiles([]string{"sticky"}); err != nil {
		t.Fatal(err)
	}

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
}

func TestComposeChartLayerOrder(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ValuesPath(chartCluster),
		[]byte("a: from-clone\nb: from-clone\nc: from-clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "extra.yaml")
	if err := os.WriteFile(extra, []byte("c: from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := composeChart(chartCluster,
		map[string]any{"a": "from-base", "b": "from-base", "c": "from-base", "d": "from-base"},
		d,
		[]profiles.Profile{{
			Name:   "p",
			Values: map[string]map[string]any{chartCluster: {"b": "from-profile", "c": "from-profile"}},
		}},
		[]string{extra},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a": "from-clone",
		"b": "from-profile",
		"c": "from-file",
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
