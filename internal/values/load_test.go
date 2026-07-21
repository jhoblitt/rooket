package values

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is an empty layer", func(t *testing.T) {
		m, err := LoadFile(filepath.Join(dir, "absent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			t.Errorf("got %#v, want nil", m)
		}
	})

	t.Run("parses a mapping", func(t *testing.T) {
		p := filepath.Join(dir, "ok.yaml")
		if err := os.WriteFile(p, []byte("a:\n  b: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := LoadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sub, ok := m["a"].(map[string]any)
		if !ok || sub["b"] != 1 {
			t.Errorf("got %#v", m)
		}
	})

	t.Run("comment-only file is an empty layer", func(t *testing.T) {
		p := filepath.Join(dir, "comments.yaml")
		if err := os.WriteFile(p, []byte("# nothing here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := LoadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 0 {
			t.Errorf("got %#v, want empty", m)
		}
	})

	t.Run("parse error names the file", func(t *testing.T) {
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte("a: [1,\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadFile(p)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "bad.yaml") {
			t.Errorf("error %q does not name the file", err)
		}
	})
}

func TestEncodeRoundTrips(t *testing.T) {
	in := map[string]any{"a": map[string]any{"b": 1}}
	data, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].(map[string]any)["b"] != 1 {
		t.Errorf("got %#v", got)
	}
}
