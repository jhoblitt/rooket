package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/engine"
)

func TestResolveColor(t *testing.T) {
	// A regular file is not a terminal (unlike /dev/null, which is a char
	// device); use it to exercise the auto path's negative case.
	reg, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if orig, ok := os.LookupEnv("NO_COLOR"); ok {
		defer os.Setenv("NO_COLOR", orig)
	} else {
		defer os.Unsetenv("NO_COLOR")
	}
	os.Unsetenv("NO_COLOR")

	for _, c := range []struct {
		mode string
		want bool
	}{
		{"always", true},
		{"never", false},
		{"auto", false}, // regular file → not a terminal
		{"", false},
	} {
		got, err := resolveColor(c.mode, reg)
		if err != nil || got != c.want {
			t.Errorf("resolveColor(%q) = (%v, %v), want (%v, nil)", c.mode, got, err, c.want)
		}
	}

	os.Setenv("NO_COLOR", "")
	if got, _ := resolveColor("auto", reg); got {
		t.Error("NO_COLOR present must disable auto")
	}
	if got, _ := resolveColor("always", reg); !got {
		t.Error("explicit always must override NO_COLOR")
	}
	os.Unsetenv("NO_COLOR")

	if _, err := resolveColor("bogus", reg); err == nil {
		t.Error("invalid --color value should error")
	}
}

func TestEnvTruthy(t *testing.T) {
	for val, want := range map[string]bool{
		"":        false,
		"0":       false,
		"false":   false,
		"FALSE":   false,
		" false ": false,
		"no":      false,
		"off":     false,
		"1":       true,
		"true":    true,
		"yes":     true,
	} {
		t.Setenv("ROOKET_TEST_TRUTHY", val)
		if got := envTruthy("ROOKET_TEST_TRUTHY"); got != want {
			t.Errorf("envTruthy(%q) = %v, want %v", val, got, want)
		}
	}
}

// runRooket runs args through the command tree and execute, as main does, and
// returns everything both printed.
func runRooket(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	err := execute(&out)
	return out.String(), err
}

// resetWaitCmd undoes what running `rooket wait` through the command tree
// leaves behind: its parsed --timeout, and the SilenceUsage set on it.
func resetWaitCmd(t *testing.T) {
	t.Helper()
	silenced := waitCmd.SilenceUsage
	t.Cleanup(func() {
		f := waitCmd.Flags().Lookup("timeout")
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
		waitCmd.SilenceUsage = silenced
	})
}

// fakeRootfulPodman puts a podman first on PATH that reports itself rootful,
// so the root command's engine probe passes without a container engine, and
// restores what that probe sets.
func fakeRootfulPodman(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\necho false\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(engine.EnvVar, "")
	t.Setenv("KIND_EXPERIMENTAL_PROVIDER", "")
	t.Setenv("DOCKERCMD", "")
	prevEngine, prevFlag, prevColor := containerEngine, engineFlag, colorFlag
	engineFlag, colorFlag = engine.Podman.String(), "never"
	t.Cleanup(func() { containerEngine, engineFlag, colorFlag = prevEngine, prevFlag, prevColor })
}

// A command that fails once its command line is accepted has made no usage
// mistake: its error is printed once, and the flag listing not at all.
func TestAFailedCommandPrintsItsErrorOnceWithoutUsage(t *testing.T) {
	fakeRootfulPodman(t)
	resetWaitCmd(t)

	out, err := runRooket(t, "wait", "--timeout", "0s")
	if err == nil || !strings.Contains(err.Error(), "--timeout must be more than 0") {
		t.Fatalf("rooket wait --timeout 0s = %v, want wait's own refusal", err)
	}
	if n := strings.Count(out, err.Error()); n != 1 {
		t.Errorf("printed the error %d times, want once:\n%s", n, out)
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("printed the usage after a failure that is not a usage mistake:\n%s", out)
	}
	if strings.Contains(out, helpHint) {
		t.Errorf("pointed at --help after a failure that is not a usage mistake:\n%s", out)
	}
}

// helpHint is the pointer to --help a command line naming no command gets.
const helpHint = "Run 'rooket --help' for usage."

// A command line that names no command is answered with the error, once, and
// then the pointer to --help; one whose flags rooket cannot parse gets the
// usage instead. The cases run in this order, in one test, because pflag never
// clears a flag set's parsed state: once `rooket --bogus` has run, rooket's
// own flags read as parsed for the rest of the process.
func TestACommandLineNamingNoCommand(t *testing.T) {
	t.Run("an unknown command", func(t *testing.T) {
		if rootCmd.Flags().Parsed() {
			t.Skip("rooket's own flags were parsed earlier in this process, which only a new process undoes")
		}
		out, err := runRooket(t, "bogus")
		if err == nil || !strings.Contains(err.Error(), `unknown command "bogus"`) {
			t.Fatalf("rooket bogus = %v, want an unknown-command error", err)
		}
		if n := strings.Count(out, err.Error()); n != 1 {
			t.Errorf("printed the error %d times, want once:\n%s", n, out)
		}
		if at, hint := strings.Index(out, err.Error()), strings.Index(out, helpHint); hint < at {
			t.Errorf("output =\n%s\nwant %q after the error", out, helpHint)
		}
	})
	t.Run("an unknown flag", func(t *testing.T) {
		out, err := runRooket(t, "--bogus")
		if err == nil || !strings.Contains(err.Error(), "unknown flag: --bogus") {
			t.Fatalf("rooket --bogus = %v, want an unknown-flag error", err)
		}
		if n := strings.Count(out, err.Error()); n != 1 {
			t.Errorf("printed the error %d times, want once:\n%s", n, out)
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("printed no usage for an unknown flag:\n%s", out)
		}
		if strings.Contains(out, helpHint) {
			t.Errorf("pointed at --help after already printing the usage:\n%s", out)
		}
	})
}

// values replaces the root's PersistentPreRunE with its own, and its failures
// print as everyone else's do.
func TestAFailedValuesCommandPrintsItsErrorOnceWithoutUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("ROOKET_CONFIG_DIR", "")
	prevValuesDir, silenced := valuesDir, valuesShowCmd.SilenceUsage
	t.Cleanup(func() {
		valuesShowCmd.SilenceUsage = silenced
		dirFlag := valuesCmd.PersistentFlags().Lookup("dir")
		_ = dirFlag.Value.Set(dirFlag.DefValue)
		dirFlag.Changed = false
		valuesDir = prevValuesDir
	})

	out, err := runRooket(t, "values", "show", "bogus", "--dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `unknown chart "bogus"`) {
		t.Fatalf("rooket values show bogus = %v, want the unknown chart refused", err)
	}
	if n := strings.Count(out, err.Error()); n != 1 {
		t.Errorf("printed the error %d times, want once:\n%s", n, out)
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("printed the usage after a failure that is not a usage mistake:\n%s", out)
	}
}

// A command line cobra cannot parse is a usage mistake, and still shows the
// usage, with the error printed once.
func TestAnUnknownFlagStillPrintsUsage(t *testing.T) {
	resetWaitCmd(t)

	out, err := runRooket(t, "wait", "--bogus")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --bogus") {
		t.Fatalf("rooket wait --bogus = %v, want an unknown-flag error", err)
	}
	if n := strings.Count(out, "unknown flag: --bogus"); n != 1 {
		t.Errorf("printed the error %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("printed no usage for an unknown flag:\n%s", out)
	}
}

// Cobra checks required flags and flag groups only after the root's
// PersistentPreRunE, yet a command line that fails them is as much a usage
// mistake as an unknown flag, and shows the usage the same way.
func TestAFlagRuleBrokenStillPrintsUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
		cmd  *cobra.Command
		set  []string
	}{
		{name: "a required flag missing", args: []string{"ceph-config"}, want: `required flag(s) "out" not set`},
		{
			name: "exclusive flags together",
			args: []string{"up", "--skip-build", "--force-build"},
			want: "none of the others can be",
			cmd:  upCmd,
			set:  []string{"skip-build", "force-build"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cmd != nil {
				t.Cleanup(func() {
					for _, name := range tc.set {
						f := tc.cmd.Flags().Lookup(name)
						_ = f.Value.Set(f.DefValue)
						f.Changed = false
					}
				})
			}

			out, err := runRooket(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rooket %s = %v, want an error containing %q", strings.Join(tc.args, " "), err, tc.want)
			}
			if n := strings.Count(out, err.Error()); n != 1 {
				t.Errorf("printed the error %d times, want once:\n%s", n, out)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("printed no usage for a usage mistake:\n%s", out)
			}
		})
	}
}

// Cobra runs only the nearest PersistentPreRunE, so a command with its own
// runs it in place of the root's, and would bring back the usage listing
// after its failures unless its hook accepts the command line too.
func TestEveryHookInPlaceOfTheRootsAcceptsTheCommandLine(t *testing.T) {
	withOnlySet := deployWithOnlySet
	t.Cleanup(func() { deployWithOnlySet = withOnlySet })
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == rootCmd || c.PersistentPreRunE == nil {
			return
		}
		silenced := c.SilenceUsage
		t.Cleanup(func() { c.SilenceUsage = silenced })
		c.SilenceUsage = false
		if err := c.PersistentPreRunE(c, nil); err != nil {
			t.Errorf("rooket %s's PersistentPreRunE: %v", c.Name(), err)
			return
		}
		if !c.SilenceUsage {
			t.Errorf("rooket %s's PersistentPreRunE leaves the usage listing on for its failures; "+
				"have it call acceptCommandLine", c.Name())
		}
	}
	walk(rootCmd)
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1234 * time.Millisecond:                "1.2s",
		24140 * time.Millisecond:               "24.1s",
		192*time.Second + 440*time.Millisecond: "3m12.4s",
		time.Hour + time.Minute + time.Second:  "1h1m1s",
	} {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}
