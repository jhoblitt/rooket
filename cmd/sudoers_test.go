package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testPaths() map[string]string {
	return map[string]string{
		"targetcli": "/usr/bin/targetcli",
		"iscsiadm":  "/usr/sbin/iscsiadm",
		"systemctl": "/usr/bin/systemctl",
		"tee":       "/usr/bin/tee",
		"cat":       "/usr/bin/cat",
	}
}

func TestRenderSudoers(t *testing.T) {
	got, err := renderSudoers("tester", testPaths())
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}

	want := sudoersHeader +
		"\nCmnd_Alias ROOKET_ISCSI = " +
		strings.Join([]string{
			"/usr/bin/targetcli",
			"/usr/sbin/iscsiadm",
			"/usr/bin/systemctl start iscsid",
			"/usr/bin/systemctl restart iscsid",
			"/usr/bin/tee /etc/iscsi/initiatorname.iscsi",
			"/usr/bin/cat /etc/sudoers.d/rooket",
		}, ", \\\n                          ") +
		"\n\n" +
		`tester ALL=(root) NOPASSWD: ROOKET_ISCSI, !/usr/bin/targetcli ""` +
		"\n"

	if got != want {
		t.Errorf("rendered sudoers file mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderSudoersRejectsUnvalidatedUser(t *testing.T) {
	if _, err := renderSudoers(`evil ALL=(ALL) NOPASSWD: ALL #`, testPaths()); err == nil {
		t.Fatal("renderSudoers accepted an invalid grant user, want error")
	}
}

func TestRenderSudoersRejectsIncompletePaths(t *testing.T) {
	paths := testPaths()
	delete(paths, "tee")
	if _, err := renderSudoers("tester", paths); err == nil {
		t.Fatal("renderSudoers accepted a paths map missing an entry, want error")
	}
}

func TestRenderSudoersRejectsRelativePath(t *testing.T) {
	paths := testPaths()
	paths["cat"] = "cat"
	if _, err := renderSudoers("tester", paths); err == nil {
		t.Fatal("renderSudoers accepted a non-absolute path, want error")
	}
}

func TestRenderSudoersRejectsUnsafePath(t *testing.T) {
	for _, bad := range []string{"/usr/bin/cat*", "/usr/bin/ca t", "/usr/bin/cat,evil", "/usr/bin/cat#evil"} {
		paths := testPaths()
		paths["cat"] = bad
		if _, err := renderSudoers("tester", paths); err == nil {
			t.Errorf("renderSudoers accepted path %q containing a sudoers-unsafe character, want error", bad)
		}
	}
}

func TestRenderSudoersIncludesSecurityWarning(t *testing.T) {
	got, err := renderSudoers("tester", testPaths())
	if err != nil {
		t.Fatalf("renderSudoers: %v", err)
	}
	const want = "# SECURITY: this grant is root-equivalent."
	if !strings.Contains(got, want) {
		t.Errorf("rendered sudoers file does not contain the mandated security warning %q:\n%s", want, got)
	}
}

func TestRenderSudoersRejectsContradictoryVocabularyShapes(t *testing.T) {
	restore := privilegedCommands
	defer func() { privilegedCommands = restore }()

	t.Run("anyArgs with exactArgs", func(t *testing.T) {
		privilegedCommands = []privCommand{{name: "targetcli", anyArgs: true, exactArgs: []string{"foo"}}}
		if _, err := renderSudoers("tester", testPaths()); err == nil {
			t.Fatal("renderSudoers accepted a command with both anyArgs and exactArgs, want error")
		}
	})

	t.Run("denyBare without anyArgs", func(t *testing.T) {
		privilegedCommands = []privCommand{{name: "targetcli", exactArgs: []string{"foo"}, denyBare: true}}
		if _, err := renderSudoers("tester", testPaths()); err == nil {
			t.Fatal("renderSudoers accepted denyBare on a command without anyArgs, want error")
		}
	})
}

func TestValidateExactArg(t *testing.T) {
	for _, ok := range []string{
		"start", "restart", "iscsid", "/etc/iscsi/initiatorname.iscsi", "/etc/sudoers.d/rooket",
		"a-b_c.d", "tpg_lun_or_backstore=lun0", "mapped_lun=0",
	} {
		if err := validateExactArg("cmd", ok); err != nil {
			t.Errorf("validateExactArg(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a b", "*", "?", "[abc]", "!x", "a\tb", "a\nb", "a,b", "a#b", `a"b`, `a\b`} {
		if err := validateExactArg("cmd", bad); err == nil {
			t.Errorf("validateExactArg(%q) = nil, want error", bad)
		}
	}
}

func TestVocabularyExactArgsAreSudoersSafe(t *testing.T) {
	for _, c := range privilegedCommands {
		for _, a := range c.exactArgs {
			if err := validateExactArg(c.name, a); err != nil {
				t.Errorf("privilegedCommands entry %q: %v", c.name, err)
			}
		}
	}
}

func TestCheckTrustedBinary(t *testing.T) {
	t.Run("rejects a non-root-owned binary", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "targetcli")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := checkTrustedBinary(p)
		if err == nil {
			t.Fatal("accepted a binary owned by the test user, want error")
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("error %q does not explain the ownership requirement", err)
		}
	})

	t.Run("rejects a symlink to a non-root-owned binary", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real-targetcli")
		if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "targetcli")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, err := checkTrustedBinary(link)
		if err == nil {
			t.Fatal("accepted a symlink to a user-owned binary, want error")
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("error %q does not explain the ownership requirement", err)
		}
		if strings.Contains(err.Error(), link) {
			t.Errorf("error %q names the symlink %q rather than its resolved target; os.Stat-style following would have hidden this distinction", err, link)
		}
	})

	t.Run("accepts a root-owned system binary and returns its resolved path", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; ownership checks are not meaningful here")
		}
		want, err := filepath.EvalSymlinks("/bin/sh")
		if err != nil {
			t.Fatal(err)
		}
		got, err := checkTrustedBinary("/bin/sh")
		if err != nil {
			t.Fatalf("checkTrustedBinary(/bin/sh) = %v, want nil", err)
		}
		if got != want {
			t.Errorf("checkTrustedBinary(/bin/sh) resolved to %q, want %q", got, want)
		}
	})

	t.Run("accepts a symlink, in a world-writable directory, to a trusted root binary", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; ownership checks are not meaningful here")
		}
		want, err := filepath.EvalSymlinks("/bin/sh")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "sh")
		if err := os.Symlink("/bin/sh", link); err != nil {
			t.Fatal(err)
		}
		// The attacker fully controls this symlink's own directory, but the
		// resolved path is the real trusted binary elsewhere on disk, so the
		// symlink's location is irrelevant to what sudo will actually exec.
		got, err := checkTrustedBinary(link)
		if err != nil {
			t.Fatalf("checkTrustedBinary(%s) = %v, want nil", link, err)
		}
		if got != want {
			t.Errorf("checkTrustedBinary(%s) resolved to %q, want %q", link, got, want)
		}
	})

	t.Run("rejects a missing binary", func(t *testing.T) {
		if _, err := checkTrustedBinary(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("accepted a missing path, want error")
		}
	})

	t.Run("rejects a relative path before walking ancestors", func(t *testing.T) {
		// filepath.Dir(".") == ".", so checkAncestorDirs' walk to "/" never
		// terminates on a relative input; this must be rejected before that
		// walk starts rather than relied on to fail some other way.
		if _, err := checkTrustedBinary("relative/path"); err == nil {
			t.Error("accepted a relative path, want error")
		}
	})
}

func TestCheckOwnershipAndMode(t *testing.T) {
	cases := []struct {
		name    string
		uid     uint32
		mode    os.FileMode
		wantErr string
	}{
		{"root owned, safe mode", 0, 0o755, ""},
		{"root owned, exact 0700", 0, 0o700, ""},
		{"non-root owner", 1000, 0o755, "not root"},
		{"root owned, group writable", 0, 0o775, "writable"},
		{"root owned, world writable", 0, 0o757, "writable"},
		{"root owned, sticky world writable", 0, 0o1777, "writable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOwnershipAndMode("/some/path", tc.uid, tc.mode)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("checkOwnershipAndMode(uid=%d, mode=%o) = %v, want nil", tc.uid, tc.mode, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("checkOwnershipAndMode(uid=%d, mode=%o) = %v, want error containing %q", tc.uid, tc.mode, err, tc.wantErr)
			}
		})
	}
}

// TestCheckAncestorDirsRejectsWorldWritableAncestor uses the real /tmp: on
// every Linux host it is root-owned yet world-writable with the sticky bit
// set, which is exactly the "root-owned but still plantable" case the sticky
// bit must not exempt. The leaf name need not exist; checkAncestorDirs only
// stats the directories above it.
func TestCheckAncestorDirsRejectsWorldWritableAncestor(t *testing.T) {
	const tmp = "/tmp"
	fi, err := os.Lstat(tmp)
	if err != nil {
		t.Skipf("cannot stat %s: %v", tmp, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		t.Skipf("%s is not root-owned on this host; cannot exercise this case", tmp)
	}
	if fi.Mode().Perm()&0o002 == 0 {
		t.Skipf("%s is not world-writable on this host; cannot exercise this case", tmp)
	}

	err = checkAncestorDirs(filepath.Join(tmp, "rooket-ancestor-check-leaf-need-not-exist"))
	if err == nil {
		t.Fatal("accepted an ancestor chain containing a world-writable directory despite root ownership and the sticky bit, want error")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("error %q does not explain the writability problem", err)
	}
}

func TestCheckAncestorDirsAcceptsRealTrustedBinaries(t *testing.T) {
	for _, p := range []string{"/usr/bin/targetcli", "/usr/sbin/iscsiadm"} {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Skipf("%s not present on this host: %v", p, err)
		}
		if err := checkAncestorDirs(resolved); err != nil {
			t.Errorf("checkAncestorDirs(%s) = %v, want nil", resolved, err)
		}
	}
}

func TestValidGrantUser(t *testing.T) {
	for _, ok := range []string{"tester", "jhoblitt", "_svc", "user-1", "a"} {
		if err := validGrantUser(ok); err != nil {
			t.Errorf("validGrantUser(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "ALL", "root, evil", "a b", "u\nALL=(ALL) NOPASSWD: ALL", "-x", "1abc"} {
		if err := validGrantUser(bad); err == nil {
			t.Errorf("validGrantUser(%q) = nil, want error", bad)
		}
	}
}
