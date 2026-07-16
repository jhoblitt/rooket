package cmd

import (
	"runtime/debug"
	"strings"
	"testing"
)

// version must work on a host with no usable container engine, so it has to
// bypass the root command's engine-probing PersistentPreRunE.
func TestVersionSkipsEngineResolution(t *testing.T) {
	oldFlag := engineFlag
	engineFlag = "bogus-engine"
	defer func() {
		engineFlag = oldFlag
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	}()

	var out strings.Builder
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rooket version failed: %v", err)
	}
	if !strings.HasPrefix(out.String(), "rooket ") {
		t.Errorf("output = %q, want prefix %q", out.String(), "rooket ")
	}
}

func TestVersionString(t *testing.T) {
	t.Run("nil build info", func(t *testing.T) {
		if got, want := versionString(nil), "rooket (unknown)\n"; got != want {
			t.Errorf("versionString(nil) = %q, want %q", got, want)
		}
	})

	t.Run("devel build with dirty vcs stamps", func(t *testing.T) {
		info := &debug.BuildInfo{
			GoVersion: "go1.26.0",
			Main:      debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
				{Key: "vcs.time", Value: "2026-07-16T01:02:03Z"},
				{Key: "vcs.modified", Value: "true"},
			},
		}
		want := `rooket (devel)
  commit: 0123456789ab (dirty)
  built:  2026-07-16T01:02:03Z
  go:     go1.26.0
`
		if got := versionString(info); got != want {
			t.Errorf("versionString = %q, want %q", got, want)
		}
	})

	t.Run("clean vcs stamps omit the dirty marker", func(t *testing.T) {
		info := &debug.BuildInfo{
			GoVersion: "go1.26.0",
			Main:      debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
				{Key: "vcs.modified", Value: "false"},
			},
		}
		want := `rooket (devel)
  commit: 0123456789ab
  go:     go1.26.0
`
		if got := versionString(info); got != want {
			t.Errorf("versionString = %q, want %q", got, want)
		}
	})

	t.Run("tagged module version without vcs stamps", func(t *testing.T) {
		info := &debug.BuildInfo{
			GoVersion: "go1.26.0",
			Main:      debug.Module{Version: "v1.2.3"},
		}
		want := `rooket v1.2.3
  go:     go1.26.0
`
		if got := versionString(info); got != want {
			t.Errorf("versionString = %q, want %q", got, want)
		}
	})

	t.Run("empty module version reads as devel", func(t *testing.T) {
		info := &debug.BuildInfo{GoVersion: "go1.26.0"}
		want := `rooket (devel)
  go:     go1.26.0
`
		if got := versionString(info); got != want {
			t.Errorf("versionString = %q, want %q", got, want)
		}
	})
}
