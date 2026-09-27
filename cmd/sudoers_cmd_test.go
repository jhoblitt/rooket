package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSudoersCommandsAreRegistered(t *testing.T) {
	var sudoers *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "sudoers" {
			sudoers = c
			break
		}
	}
	if sudoers == nil {
		t.Fatal("rooket sudoers is not registered on rootCmd")
	}
	want := map[string]bool{"print": false, "status": false, "install": false, "uninstall": false}
	for _, c := range sudoers.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("rooket sudoers %s is missing", name)
		}
	}
}

// An invalid --user must be refused before install re-runs itself under sudo.
// The stub sudo succeeds, so a re-exec would neither prompt nor fail.
func TestSudoersInstallRejectsBadUser(t *testing.T) {
	dir, logPath := writeStubSudo(t, 0, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := sudoersInstall("../../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), `invalid user name "../../etc/passwd"`) {
		t.Fatalf("sudoersInstall = %v, want the invalid-user rejection", err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sudo was invoked for an invalid user (stat %s: %v)", logPath, err)
	}
}

// The rule can be installed on a host with neither podman nor docker — the CI
// policy job's container is exactly that — so sudoers must bypass the root
// command's engine-probing PersistentPreRunE, as version does.
//
// This drives a runnable leaf ("status") rather than "--help": in cobra
// v1.10.2, "--help" returns flag.ErrHelp before PersistentPreRunE ever runs,
// and sudoersCmd itself is non-Runnable, so a "--help" invocation would pass
// even with the PersistentPreRunE override deleted outright. An explicit
// --user and an empty $PATH make "status" fail resolving the vocabulary
// before it consults the host's user database or reads the installed rule
// back through sudo, so this tolerates any error EXCEPT the specific one
// engine.Parse produces — that's the one signal that the override was
// bypassed and the root engine probe ran.
func TestSudoersSkipsEngineResolution(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	oldFlag, oldUser, silenced := engineFlag, sudoersUser, sudoersStatusCmd.SilenceUsage
	engineFlag = "bogus-engine"
	defer func() {
		engineFlag, sudoersUser, sudoersStatusCmd.SilenceUsage = oldFlag, oldUser, silenced
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	}()

	var out strings.Builder
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"sudoers", "status", "--user", "tester"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "unsupported container engine") {
		t.Fatalf("rooket sudoers status with an unusable engine hit the root engine probe: %v", err)
	}
}

// sudoers replaces the root's PersistentPreRunE with its own, yet its usage
// mistakes show the usage and its other failures do not, as every other
// command's do.
func TestSudoersFailuresPrintLikeEveryCommands(t *testing.T) {
	prevUser, silenced := sudoersUser, sudoersPrintCmd.SilenceUsage
	t.Cleanup(func() {
		sudoersUser, sudoersPrintCmd.SilenceUsage = prevUser, silenced
		sudoersPrintCmd.Flags().Lookup("user").Changed = false
	})

	t.Run("an unknown flag", func(t *testing.T) {
		out, err := runRooket(t, "sudoers", "print", "--bogus")
		if err == nil || !strings.Contains(err.Error(), "unknown flag: --bogus") {
			t.Fatalf("rooket sudoers print --bogus = %v, want an unknown-flag error", err)
		}
		if n := strings.Count(out, err.Error()); n != 1 {
			t.Errorf("printed the error %d times, want once:\n%s", n, out)
		}
		if !strings.Contains(out, "Usage:") {
			t.Errorf("printed no usage for an unknown flag:\n%s", out)
		}
	})
	t.Run("a refused user", func(t *testing.T) {
		out, err := runRooket(t, "sudoers", "print", "--user", "Not A User")
		if err == nil || !strings.Contains(err.Error(), "invalid user name") {
			t.Fatalf("rooket sudoers print --user 'Not A User' = %v, want the user refused", err)
		}
		if n := strings.Count(out, err.Error()); n != 1 {
			t.Errorf("printed the error %d times, want once:\n%s", n, out)
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("printed the usage after a failure that is not a usage mistake:\n%s", out)
		}
	})
}

// stubGrantedCommands replaces $PATH with a temp dir holding every vocabulary
// command as a symlink to the host's `true`, so grantTarget resolves on a host
// without targetcli or iscsiadm. checkTrustedBinary vets a symlink's target,
// not the link, which is what lets the links live in a user-owned dir. The
// dir is returned so a test can add a sudo stub beside them.
func stubGrantedCommands(t *testing.T) string {
	t.Helper()
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no true on $PATH to stand in for the granted commands: %v", err)
	}
	if _, err := checkTrustedBinary(trueBin); err != nil {
		t.Skipf("%s cannot stand in for the granted commands: %v", trueBin, err)
	}
	dir := t.TempDir()
	for _, c := range privilegedCommands {
		if err := os.Symlink(trueBin, filepath.Join(dir, c.name)); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// sudoersState's three branches are drift detection, this feature's
// headline claim. A stub sudo answers only the exact read-back of the
// installed rule through the cat the rule grants, so each branch is reached
// through readInstalledSudoers without root or a rule on disk.
func TestSudoersState(t *testing.T) {
	dir := stubGrantedCommands(t)
	_, paths, err := grantTarget("tester")
	if err != nil {
		t.Fatalf("grantTarget: %v", err)
	}
	rendered, err := renderSudoers("tester", paths)
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}

	cases := []struct {
		name      string
		installed string
		exit      int // sudo's exit for the read-back; a missing rule denies it
		wantMsg   string
		wantOK    bool
	}{
		{name: "not installed", exit: 1, wantMsg: "not installed"},
		{name: "stale", installed: rendered + "# drift\n", wantMsg: "stale"},
		{name: "up to date", installed: rendered, wantMsg: "up to date", wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := fmt.Sprintf(`#!/bin/sh
if [ "$#" = 3 ] && [ "$1" = -n ] && [ "$2" = %s ] && [ "$3" = %s ]; then
  printf '%%s' %s
  exit %d
fi
exit 1
`, shQuote(paths["cat"]), shQuote(sudoersPath), shQuote(tc.installed), tc.exit)
			if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			msg, ok, err := sudoersState("tester")
			if err != nil {
				t.Fatalf("sudoersState: %v", err)
			}
			if msg != tc.wantMsg || ok != tc.wantOK {
				t.Errorf("sudoersState = %q, %v; want %q, %v", msg, ok, tc.wantMsg, tc.wantOK)
			}
		})
	}
}

// print must write through cmd.OutOrStdout() rather than fmt.Print, so its
// output is testable via cobra the same way version's is.
func TestSudoersPrintWritesThroughCobraWriter(t *testing.T) {
	stubGrantedCommands(t)
	oldUser := sudoersUser
	silenced := sudoersPrintCmd.SilenceUsage
	defer func() {
		sudoersUser, sudoersPrintCmd.SilenceUsage = oldUser, silenced
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	}()

	_, paths, err := grantTarget("tester")
	if err != nil {
		t.Fatalf("grantTarget: %v", err)
	}
	want, err := renderSudoers("tester", paths)
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}

	var out strings.Builder
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"sudoers", "print", "--user", "tester"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rooket sudoers print: %v", err)
	}
	if got := out.String(); got != want {
		t.Errorf("rooket sudoers print output = %q, want %q", got, want)
	}
}
