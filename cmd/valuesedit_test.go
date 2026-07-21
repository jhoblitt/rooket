package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditValuesSeedsWhenAbsent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	seed := []byte("# rooket base\n# toolbox:\n#   enabled: true\n")

	var sawContent string
	err := editValues(p, seed, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sawContent = string(data)
		return os.WriteFile(path, []byte("toolbox:\n  enabled: false\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawContent, "# toolbox:") {
		t.Errorf("editor did not see the seed: %q", sawContent)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "enabled: false") {
		t.Errorf("saved file = %q", got)
	}
}

func TestEditValuesRemovesEmptyResult(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := editValues(p, nil, func(path string) error {
		return os.WriteFile(path, []byte("# everything commented out\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty result should remove the layer file")
	}
}

func TestEditValuesReopensOnParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	calls := 0
	err := editValues(p, nil, func(path string) error {
		calls++
		if calls == 1 {
			return os.WriteFile(path, []byte("a: [1,\n"), 0o644)
		}
		return os.WriteFile(path, []byte("a: 1\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("editor called %d times, want 2", calls)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Errorf("saved %q", data)
	}
}
