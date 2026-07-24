package cmd

import (
	"strings"
	"testing"
)

// TestValuesShowInheritsWithOnlyFlag exercises the real command tree so a
// regression in valuesCmd.PersistentPreRunE — e.g. the "with-only" literal
// getting out of sync with the flag name, or a future subcommand shadowing
// this hook (cobra runs only the nearest ancestor's PersistentPreRunE) — fails
// a test instead of silently making --with-only a no-op.
func TestValuesShowInheritsWithOnlyFlag(t *testing.T) {
	t.Cleanup(func() {
		deployWith, deployWithOnly, deployValueFiles, deploySets = nil, nil, nil, nil
		deployWithOnlySet = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	})

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

func TestPrintSetsNote(t *testing.T) {
	t.Run("present when --set is supplied", func(t *testing.T) {
		var out strings.Builder
		printSetsNote(&out, []string{"a=b"})
		if !strings.Contains(out.String(), "--set") {
			t.Errorf("got %q, want a note mentioning --set", out.String())
		}
	})

	t.Run("absent when --set is not supplied", func(t *testing.T) {
		var out strings.Builder
		printSetsNote(&out, nil)
		if out.String() != "" {
			t.Errorf("got %q, want no output without --set", out.String())
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
