package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterName(t *testing.T) {
	t.Run("flag takes precedence", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "fromenv")
		if got := clusterName("fromflag"); got != "fromflag" {
			t.Errorf("clusterName = %q, want fromflag", got)
		}
	})

	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "fromenv")
		if got := clusterName(""); got != "fromenv" {
			t.Errorf("clusterName = %q, want fromenv", got)
		}
	})

	t.Run("distinguishes same-basename clones in different dirs", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		mk := func() string {
			clone := filepath.Join(t.TempDir(), "myrook")
			if err := os.MkdirAll(clone, 0o755); err != nil {
				t.Fatal(err)
			}
			writeGoMod(t, clone, rookModulePath)
			t.Chdir(clone)
			return clusterName("")
		}
		a, b := mk(), mk()
		if a == b {
			t.Errorf("same-basename clones in different dirs both got %q; want distinct names", a)
		}
		for _, n := range []string{a, b} {
			if strings.HasPrefix(n, "-") || n != strings.ToLower(n) {
				t.Errorf("clusterName = %q, want lowercase with no leading dash", n)
			}
		}
	})

	t.Run("falls back to rook", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Chdir(t.TempDir())
		if got := clusterName(""); got != "rook" {
			t.Errorf("clusterName = %q, want rook", got)
		}
	})
}

func TestResolveRegistryPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Run("explicit flag when nothing persisted", func(t *testing.T) {
		got, err := resolveRegistryPort("c1", 5050, true)
		if err != nil || got != 5050 {
			t.Fatalf("resolveRegistryPort = (%d, %v), want (5050, nil)", got, err)
		}
	})

	t.Run("persisted port is reused when no flag", func(t *testing.T) {
		if err := writeRegistryPort("c2", 5123); err != nil {
			t.Fatal(err)
		}
		got, err := resolveRegistryPort("c2", 9999, false)
		if err != nil || got != 5123 {
			t.Fatalf("resolveRegistryPort = (%d, %v), want (5123, nil)", got, err)
		}
	})

	t.Run("flag conflicting with the persisted port errors", func(t *testing.T) {
		if err := writeRegistryPort("c2b", 5123); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveRegistryPort("c2b", 9999, true); err == nil {
			t.Fatal("resolveRegistryPort = nil error, want a conflict error")
		}
		got, err := resolveRegistryPort("c2b", 5123, true)
		if err != nil || got != 5123 {
			t.Fatalf("resolveRegistryPort with matching flag = (%d, %v), want (5123, nil)", got, err)
		}
	})

	t.Run("free port when no flag and nothing persisted", func(t *testing.T) {
		got, err := resolveRegistryPort("c3", 5001, false)
		if err != nil || got < 5001 {
			t.Fatalf("resolveRegistryPort = (%d, %v), want a free port >= 5001", got, err)
		}
	})
}

func TestEncodePath(t *testing.T) {
	cases := map[string]string{
		"/home/jhoblitt/github/rook3": "home-jhoblitt-github-rook3",
		"/home/jhoblitt/github/rook":  "home-jhoblitt-github-rook",
		"/Home/A.B/Rook":              "home-a-b-rook",
		"/a//b/":                      "a-b",
	}
	for in, want := range cases {
		if got := encodePath(in); got != want {
			t.Errorf("encodePath(%q) = %q, want %q", in, got, want)
		}
	}

	// A very long path is truncated to a bounded length but stays unique.
	long := "/home/jhoblitt/" + strings.Repeat("verydeep/", 12) + "rook"
	got := encodePath(long)
	if len(got) > 45 {
		t.Errorf("encodePath(long) = %q (len %d), want <= 45", got, len(got))
	}
	if got == encodePath(long+"x") {
		t.Errorf("encodePath did not distinguish two long paths")
	}
}
