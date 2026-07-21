package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"plain":   "'plain'",
		"a b":     "'a b'",
		"semi;rm": "'semi;rm'",
		"$(hi)":   "'$(hi)'",
		"it's":    `'it'\''s'`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// A shell metacharacter in an operand must be confined to a single-quoted
// literal, so it cannot start a new command in the root-run script. None of
// these payloads contains a single quote, so each must appear verbatim wrapped
// in single quotes.
func TestBuildISCSIStepsQuoteOperands(t *testing.T) {
	evil := iscsiDisk{
		imgPath:       "/tmp/x; touch /tmp/pwned",
		backstoreName: "clus-$(id)",
		targetIQN:     "iqn.2003-01.local.rooket:clus;reboot",
	}
	initIQN := "iqn.2003-01.local.rooket:initiator"

	mustQuote := func(t *testing.T, label, script string, operands ...string) {
		for _, op := range operands {
			if !strings.Contains(script, "'"+op+"'") {
				t.Errorf("%s: operand %q not single-quoted in script:\n%s", label, op, script)
			}
		}
	}
	mustQuote(t, "setup", renderScript(buildISCSISteps(initIQN, []iscsiDisk{evil}, 10, true)),
		evil.imgPath, evil.backstoreName, evil.targetIQN)
	mustQuote(t, "teardown", renderScript(buildISCSITeardownSteps([]iscsiDisk{evil})),
		evil.backstoreName, evil.targetIQN)
}

// Every step the builders emit must be covered by the sudoers vocabulary; an
// uncovered one would silently fall back to a pkexec prompt on a host that
// installed the rule.
func TestBuiltStepsAreGranted(t *testing.T) {
	disks := []iscsiDisk{{
		imgPath:       "/home/u/.local/share/rooket/c/worker0-disk0.img",
		backstoreName: "c-worker0-disk0",
		targetIQN:     "iqn.2003-01.local.rooket:c-worker0-disk0",
	}}
	initIQN := "iqn.2003-01.local.rooket:initiator"

	for _, write := range []bool{true, false} {
		if err := validateSteps(buildISCSISteps(initIQN, disks, 10, write)); err != nil {
			t.Errorf("setup steps (writeInitiator=%v): %v", write, err)
		}
	}
	if err := validateSteps(buildISCSITeardownSteps(disks)); err != nil {
		t.Errorf("teardown steps: %v", err)
	}
}

func TestValidateIQNDate(t *testing.T) {
	for _, ok := range []string{"2003-01", "2026-12", "0000-00"} {
		if err := validateIQNDate(ok); err != nil {
			t.Errorf("validateIQNDate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "2003", "2003-1", "2003-01-01", "x; rm", "2003_01"} {
		if err := validateIQNDate(bad); err == nil {
			t.Errorf("validateIQNDate(%q) = nil, want error", bad)
		}
	}
}

func TestResolveDeviceLink(t *testing.T) {
	dir := t.TempDir()

	t.Run("absolute target", func(t *testing.T) {
		link := filepath.Join(dir, "abs")
		if err := os.Symlink("/dev/sdz", link); err != nil {
			t.Fatal(err)
		}
		if got := resolveDeviceLink(link); got != "/dev/sdz" {
			t.Errorf("resolveDeviceLink = %q, want /dev/sdz", got)
		}
	})

	t.Run("relative target resolved against the link's directory", func(t *testing.T) {
		link := filepath.Join(dir, "rel")
		if err := os.Symlink("../../dev/sdy", link); err != nil {
			t.Fatal(err)
		}
		want := filepath.Clean(filepath.Join(dir, "../../dev/sdy"))
		if got := resolveDeviceLink(link); got != want {
			t.Errorf("resolveDeviceLink = %q, want %q", got, want)
		}
	})

	t.Run("missing link", func(t *testing.T) {
		if got := resolveDeviceLink(filepath.Join(dir, "nope")); got != "" {
			t.Errorf("resolveDeviceLink on a missing link = %q, want empty", got)
		}
	})
}

func TestInitiatorNameCurrent(t *testing.T) {
	const want = "iqn.2003-01.local.rooket:initiator"
	cases := []struct {
		name    string
		content string
		write   bool // false => do not create the file at all
		current bool
	}{
		{name: "exact match", content: "InitiatorName=" + want + "\n", write: true, current: true},
		{name: "trailing whitespace", content: "InitiatorName=" + want + "  \n", write: true, current: true},
		{name: "with comments", content: "# comment\n\nInitiatorName=" + want + "\n", write: true, current: true},
		{name: "different iqn", content: "InitiatorName=iqn.2003-01.local.other:initiator\n", write: true},
		{name: "second assignment mismatched", content: "InitiatorName=" + want + "\nInitiatorName=iqn.x\n", write: true},
		// The rule is "declare the wanted name and nothing else": a second
		// assignment must be rejected even when it repeats the same IQN, not
		// only when it disagrees.
		{name: "second assignment repeats the wanted iqn", content: "InitiatorName=" + want + "\nInitiatorName=" + want + "\n", write: true},
		{name: "no assignment", content: "# nothing here\n", write: true},
		{name: "empty file", content: "", write: true},
		{name: "absent file", write: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "initiatorname.iscsi")
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := initiatorNameCurrent(path, want); got != tc.current {
				t.Errorf("initiatorNameCurrent = %v, want %v", got, tc.current)
			}
		})
	}
}

// The restart exists only to make iscsid pick up a changed initiator name, and
// iscsid is shared by every other cluster's live sessions — so it must be
// emitted if and only if the write is.
func TestBuildISCSIScriptInitiatorWriteGatesRestart(t *testing.T) {
	disks := []iscsiDisk{{
		imgPath:       "/tmp/worker0-disk0.img",
		backstoreName: "c-worker0-disk0",
		targetIQN:     "iqn.2003-01.local.rooket:c-worker0-disk0",
	}}
	initIQN := "iqn.2003-01.local.rooket:initiator"

	has := func(steps []privStep, argv ...string) bool {
		for _, s := range steps {
			if slices.Equal(s.argv, argv) {
				return true
			}
		}
		return false
	}

	with := buildISCSISteps(initIQN, disks, 10, true)
	if !has(with, "tee", "/etc/iscsi/initiatorname.iscsi") {
		t.Error("writeInitiator=true omitted the tee step")
	}
	if !has(with, "systemctl", "restart", "iscsid") {
		t.Error("writeInitiator=true omitted the iscsid restart")
	}

	without := buildISCSISteps(initIQN, disks, 10, false)
	if has(without, "tee", "/etc/iscsi/initiatorname.iscsi") {
		t.Error("writeInitiator=false emitted the tee step")
	}
	if has(without, "systemctl", "restart", "iscsid") {
		t.Error("writeInitiator=false emitted the iscsid restart")
	}
	if !has(without, "systemctl", "start", "iscsid") {
		t.Error("writeInitiator=false dropped the unconditional iscsid start")
	}

	if err := validateSteps(without); err != nil {
		t.Errorf("steps without the initiator write are not granted: %v", err)
	}
}
