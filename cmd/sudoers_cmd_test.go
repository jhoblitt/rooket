package cmd

import (
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

func TestSudoersInstallRejectsBadUser(t *testing.T) {
	if err := sudoersInstall("../../etc/passwd"); err == nil {
		t.Fatal("sudoersInstall accepted an invalid user name, want error")
	}
}

// The rule can be installed on a host with neither podman nor docker — the CI
// policy job's container is exactly that — so sudoers must bypass the root
// command's engine-probing PersistentPreRunE, as version does.
//
// This drives a runnable leaf ("status") rather than "--help": in cobra
// v1.10.2, "--help" returns flag.ErrHelp before PersistentPreRunE ever runs,
// and sudoersCmd itself is non-Runnable, so a "--help" invocation would pass
// even with the PersistentPreRunE override deleted outright. "status"
// legitimately exits non-zero when no rule is installed (or the vocabulary's
// binaries aren't on $PATH), so this tolerates any error EXCEPT the specific
// one engine.Parse produces — that's the one signal that the override was
// bypassed and the root engine probe ran.
func TestSudoersSkipsEngineResolution(t *testing.T) {
	oldFlag := engineFlag
	engineFlag = "bogus-engine"
	defer func() {
		engineFlag = oldFlag
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	}()

	var out strings.Builder
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"sudoers", "status"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "unsupported container engine") {
		t.Fatalf("rooket sudoers status with an unusable engine hit the root engine probe: %v", err)
	}
}

// sudoersState's three branches are drift detection, this feature's
// headline claim, so all three need direct coverage rather than relying on
// a live sudoers file. readInstalledSudoersFunc is overridden to control what
// sudoersState sees as "installed" without root or a real file on disk.
func TestSudoersState(t *testing.T) {
	user, paths, err := grantTarget("")
	if err != nil {
		t.Skipf("cannot resolve a grant target on this host: %v", err)
	}
	rendered, err := renderSudoers(user, paths)
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}

	restore := readInstalledSudoersFunc
	t.Cleanup(func() { readInstalledSudoersFunc = restore })

	t.Run("not installed", func(t *testing.T) {
		readInstalledSudoersFunc = func() (string, bool) { return "", false }
		msg, ok, err := sudoersState("")
		if err != nil {
			t.Fatalf("sudoersState: %v", err)
		}
		if ok {
			t.Error("ok = true, want false when nothing is installed")
		}
		if msg != "not installed" {
			t.Errorf("msg = %q, want %q", msg, "not installed")
		}
	})

	t.Run("stale", func(t *testing.T) {
		readInstalledSudoersFunc = func() (string, bool) { return rendered + "# drift\n", true }
		msg, ok, err := sudoersState("")
		if err != nil {
			t.Fatalf("sudoersState: %v", err)
		}
		if ok {
			t.Error("ok = true, want false for a rule that no longer matches a fresh render")
		}
		if msg != "stale" {
			t.Errorf("msg = %q, want %q", msg, "stale")
		}
	})

	t.Run("up to date", func(t *testing.T) {
		readInstalledSudoersFunc = func() (string, bool) { return rendered, true }
		msg, ok, err := sudoersState("")
		if err != nil {
			t.Fatalf("sudoersState: %v", err)
		}
		if !ok {
			t.Error("ok = false, want true when installed matches a fresh render byte-for-byte")
		}
		if msg != "up to date" {
			t.Errorf("msg = %q, want %q", msg, "up to date")
		}
	})
}

// print must write through cmd.OutOrStdout() rather than fmt.Print, so its
// output is testable via cobra the same way version's is.
func TestSudoersPrintWritesThroughCobraWriter(t *testing.T) {
	oldUser := sudoersUser
	sudoersUser = ""
	defer func() {
		sudoersUser = oldUser
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
	}()

	user, paths, err := grantTarget("")
	if err != nil {
		t.Skipf("cannot resolve a grant target on this host: %v", err)
	}
	want, err := renderSudoers(user, paths)
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}

	var out strings.Builder
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"sudoers", "print"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rooket sudoers print: %v", err)
	}
	if got := out.String(); got != want {
		t.Errorf("rooket sudoers print output = %q, want %q", got, want)
	}
}
