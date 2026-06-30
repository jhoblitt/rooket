package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGoMod(t *testing.T, dir, module string) {
	t.Helper()
	content := "module " + module + "\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// eval resolves symlinks so comparisons hold on platforms where TempDir returns
// a symlinked path (e.g. macOS /var -> /private/var).
func eval(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindRookRoot(t *testing.T) {
	root := t.TempDir()
	writeGoMod(t, root, rookModulePath)
	sub := filepath.Join(root, "pkg", "operator")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	want := eval(t, root)
	for _, start := range []string{root, sub} {
		if got := findRookRoot(start); eval(t, got) != want {
			t.Errorf("findRookRoot(%q) = %q, want %q", start, got, want)
		}
	}
}

func TestFindRookRootNonRook(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, "example.com/other")
	if got := findRookRoot(dir); got != "" {
		t.Errorf("findRookRoot on a non-rook module = %q, want %q", got, "")
	}
}

func TestResolveRookDir(t *testing.T) {
	t.Run("flag takes precedence over env", func(t *testing.T) {
		t.Setenv("ROOK_DIR", "/from/env")
		got, err := resolveRookDir("/from/flag")
		if err != nil || got != "/from/flag" {
			t.Fatalf("resolveRookDir = (%q, %v), want (/from/flag, nil)", got, err)
		}
	})

	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv("ROOK_DIR", "/from/env")
		got, err := resolveRookDir("")
		if err != nil || got != "/from/env" {
			t.Fatalf("resolveRookDir = (%q, %v), want (/from/env, nil)", got, err)
		}
	})

	t.Run("auto-detect from a subdirectory", func(t *testing.T) {
		t.Setenv("ROOK_DIR", "")
		root := t.TempDir()
		writeGoMod(t, root, rookModulePath)
		sub := filepath.Join(root, "cmd", "rook")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)

		got, err := resolveRookDir("")
		if err != nil {
			t.Fatal(err)
		}
		if eval(t, got) != eval(t, root) {
			t.Errorf("resolveRookDir auto-detect = %q, want %q", got, root)
		}
	})

	t.Run("error when not in a rook clone", func(t *testing.T) {
		t.Setenv("ROOK_DIR", "")
		t.Chdir(t.TempDir())
		if _, err := resolveRookDir(""); err == nil {
			t.Error("resolveRookDir = nil error, want an error when no rook clone is found")
		}
	})
}
