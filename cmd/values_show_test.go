package cmd

import (
	"strings"
	"testing"
)

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
