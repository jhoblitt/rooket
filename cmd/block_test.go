package cmd

import (
	"os"
	"path/filepath"
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
func TestBuildISCSIScriptsQuoteOperands(t *testing.T) {
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
	mustQuote(t, "setup", buildISCSIScript(initIQN, []iscsiDisk{evil}, 10),
		evil.imgPath, evil.backstoreName, evil.targetIQN)
	mustQuote(t, "teardown", buildISCSITeardownScript([]iscsiDisk{evil}),
		evil.backstoreName, evil.targetIQN)
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
