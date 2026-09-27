package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterName(t *testing.T) {
	mkClone := func(t *testing.T) string {
		t.Helper()
		clone := filepath.Join(t.TempDir(), "myrook")
		if err := os.MkdirAll(filepath.Join(clone, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeGoMod(t, clone, rookModulePath)
		return clone
	}
	// Run inside one clone, pointed at a second with --dir and at a third with
	// $ROOK_DIR, so each case below shows its source outranking the later ones.
	clone, dirClone, envClone := mkClone(t), mkClone(t), mkClone(t)
	t.Chdir(clone)
	t.Setenv("ROOK_DIR", envClone)
	every := map[string]func() (string, error){
		"clusterName":          func() (string, error) { return clusterName("") },
		"clusterNameOrDir":     func() (string, error) { return clusterNameOrDir("", dirClone) },
		"clusterNameOrRookDir": func() (string, error) { return clusterNameOrRookDir("", dirClone, false) },
		"envClusterName":       envClusterName,
		"rookDirClusterName":   func() (string, error) { return rookDirClusterName(dirClone, false) },
		"clusterNameOrRookDir, released": func() (string, error) {
			return clusterNameOrRookDir("", dirClone, true)
		},
		"rookDirClusterName, released": func() (string, error) { return rookDirClusterName(dirClone, true) },
	}

	t.Run("flag takes precedence", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "fromenv")
		for resolver, got := range map[string]func() (string, error){
			"clusterName":          func() (string, error) { return clusterName("fromflag") },
			"clusterNameOrDir":     func() (string, error) { return clusterNameOrDir("fromflag", dirClone) },
			"clusterNameOrRookDir": func() (string, error) { return clusterNameOrRookDir("fromflag", dirClone, false) },
			"clusterNameOrRookDir, released": func() (string, error) {
				return clusterNameOrRookDir("fromflag", dirClone, true)
			},
		} {
			if name, err := got(); err != nil || name != "fromflag" {
				t.Errorf("%s = (%q, %v), want fromflag", resolver, name, err)
			}
		}
	})

	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "fromenv")
		for resolver, got := range every {
			if name, err := got(); err != nil || name != "fromenv" {
				t.Errorf("%s = (%q, %v), want fromenv", resolver, name, err)
			}
		}
	})

	// The working directory's clone outranks the ones --dir and $ROOK_DIR
	// name, so no name a clone already gave a cluster changes.
	t.Run("the enclosing clone when neither", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Chdir(filepath.Join(clone, "pkg"))
		want := encodePath(clone)
		for resolver, got := range every {
			if name, err := got(); err != nil || name != want {
				t.Errorf("%s = (%q, %v), want %q", resolver, name, err, want)
			}
		}
	})

	// Found the way the working directory's is: a path inside the clone
	// names its root, and a relative one is taken from the working directory.
	// --dir outranks $ROOK_DIR, as it does for resolveRookDir.
	t.Run("the clone --dir names outside any clone", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Chdir(filepath.Dir(dirClone))
		want := encodePath(dirClone)
		for _, dir := range []string{dirClone, filepath.Join(dirClone, "pkg"), "myrook"} {
			for resolver, got := range map[string]func() (string, error){
				"clusterNameOrDir":     func() (string, error) { return clusterNameOrDir("", dir) },
				"clusterNameOrRookDir": func() (string, error) { return clusterNameOrRookDir("", dir, false) },
				"rookDirClusterName":   func() (string, error) { return rookDirClusterName(dir, false) },
			} {
				if name, err := got(); err != nil || name != want {
					t.Errorf("%s(--dir %s) = (%q, %v), want %q", resolver, dir, name, err, want)
				}
			}
		}
	})

	// Only a command that finds its tree with resolveRookDir takes $ROOK_DIR.
	t.Run("the clone $ROOK_DIR names outside any clone", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Chdir(t.TempDir())
		want := encodePath(envClone)
		for resolver, got := range map[string]func() (string, error){
			"clusterNameOrRookDir": func() (string, error) { return clusterNameOrRookDir("", "", false) },
			"rookDirClusterName":   func() (string, error) { return rookDirClusterName("", false) },
		} {
			if name, err := got(); err != nil || name != want {
				t.Errorf("%s = (%q, %v), want %q", resolver, name, err, want)
			}
		}
		_, err := clusterNameOrDir("", "")
		assertRefused(t, err, "--name", "--dir")
	})

	// Given --rook-version, a command reads released charts rather than a tree,
	// and --dir only locates its configuration home, so neither --dir nor
	// $ROOK_DIR names the cluster, and the refusal offers neither.
	t.Run("released, neither --dir nor $ROOK_DIR names", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Chdir(t.TempDir())
		for _, tc := range []struct {
			resolver string
			resolve  func() (string, error)
			accepts  []string
		}{
			{"clusterNameOrRookDir by $ROOK_DIR", func() (string, error) { return clusterNameOrRookDir("", "", true) }, []string{"--name"}},
			{"clusterNameOrRookDir by --dir", func() (string, error) { return clusterNameOrRookDir("", dirClone, true) }, []string{"--name"}},
			{"rookDirClusterName by $ROOK_DIR", func() (string, error) { return rookDirClusterName("", true) }, nil},
			{"rookDirClusterName by --dir", func() (string, error) { return rookDirClusterName(dirClone, true) }, nil},
		} {
			t.Run(tc.resolver, func(t *testing.T) {
				got, err := tc.resolve()
				if got != "" {
					t.Errorf("%s = %q, want no name", tc.resolver, got)
				}
				assertRefused(t, err, tc.accepts...)
			})
		}
	})

	t.Run("distinguishes same-basename clones in different dirs", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		mk := func() string {
			t.Chdir(mkClone(t))
			name, err := clusterName("")
			if err != nil {
				t.Fatalf("clusterName in a clone: %v", err)
			}
			return name
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

	// Outside any clone with nothing else set, the name "rook" is not
	// assumed: that cluster may be someone else's.
	t.Run("refuses with nothing to go on", func(t *testing.T) {
		t.Setenv("ROOKET_NAME", "")
		t.Setenv("ROOK_DIR", "")
		t.Chdir(t.TempDir())
		notAClone := t.TempDir()

		for _, tc := range []struct {
			resolver string
			resolve  func() (string, error)
			accepts  []string
		}{
			{"clusterName", func() (string, error) { return clusterName("") }, []string{"--name"}},
			{"clusterNameOrDir", func() (string, error) { return clusterNameOrDir("", "") }, []string{"--name", "--dir"}},
			{"clusterNameOrDir at no clone", func() (string, error) { return clusterNameOrDir("", notAClone) }, []string{"--name", "--dir"}},
			{"clusterNameOrRookDir", func() (string, error) { return clusterNameOrRookDir("", "", false) }, []string{"--name", "--dir", "$ROOK_DIR"}},
			{"clusterNameOrRookDir at no clone", func() (string, error) { return clusterNameOrRookDir("", notAClone, false) }, []string{"--name", "--dir", "$ROOK_DIR"}},
			{"envClusterName", envClusterName, nil},
			{"rookDirClusterName", func() (string, error) { return rookDirClusterName("", false) }, []string{"--dir", "$ROOK_DIR"}},
			{"rookDirClusterName at no clone", func() (string, error) { return rookDirClusterName(notAClone, false) }, []string{"--dir", "$ROOK_DIR"}},
		} {
			t.Run(tc.resolver, func(t *testing.T) {
				got, err := tc.resolve()
				if got != "" {
					t.Errorf("%s = %q, want no name", tc.resolver, got)
				}
				assertRefused(t, err, tc.accepts...)
			})
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

// The e2e suite matches this error's "is it up?", so its text is pinned whole.
func TestRequireKubeconfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	kc, err := kubeconfigPath("alpha")
	if err != nil {
		t.Fatal(err)
	}

	_, err = requireKubeconfig("alpha")
	want := fmt.Sprintf("no kubeconfig for cluster %q at %s (is it up?)", "alpha", kc)
	if err == nil || err.Error() != want {
		t.Fatalf("requireKubeconfig with no kubeconfig = %v, want %q", err, want)
	}

	if err := os.MkdirAll(filepath.Dir(kc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := requireKubeconfig("alpha"); err != nil || got != kc {
		t.Errorf("requireKubeconfig = (%q, %v), want (%q, nil)", got, err, kc)
	}

	if _, err := requireKubeconfig("../escape"); err == nil || strings.Contains(err.Error(), "is it up?") {
		t.Errorf("requireKubeconfig(\"../escape\") = %v, want the invalid-name error", err)
	}
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
