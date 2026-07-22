package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// sudoersPath is the file rooket generates and reads back to detect drift.
const sudoersPath = "/etc/sudoers.d/rooket"

// readInstalledSudoersFunc is a package-level variable so tests can inject the
// installed rule's content (and its "is it live" state) without root or a
// real file on disk; restore it via t.Cleanup after overriding.
var readInstalledSudoersFunc = readInstalledSudoers

// readInstalledSudoers returns the installed rule's exact bytes. It goes
// through the pinned `cat` the rule grants, because /etc/sudoers.d is mode
// 0750 and the file is therefore unreadable directly.
func readInstalledSudoers() (string, bool) {
	out, err := exec.Command("sudo", "-n", resolvedCommandPath("cat"), sudoersPath).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

const sudoersHeader = `# Managed by rooket. Do not edit; regenerate with ` + "`rooket sudoers install`" + `.
#
# Grants passwordless root for the iSCSI target operations performed by
# ` + "`rooket block setup`" + `, ` + "`rooket block teardown`" + `, and ` + "`rooket down --all`" + `.
#
# SECURITY: this grant is root-equivalent. targetcli can expose any file as a
# fileio backstore and rooket's disk images are user-writable, so anyone
# holding this grant can obtain root. It is a convenience for a single-user
# development workstation, not a privilege boundary.
`

// sudoersSafeRE allows only characters known safe within a sudoers command
// path or argument. Denylisting fnmatch metacharacters (*?[]!) and whitespace
// is not enough: "," ends an entry, "#" starts a comment that swallows the
// rest of a continued alias line, and '"' / '\' have their own escaping rules
// this file does not implement. Allowlisting means a gap like that fails
// closed instead of needing to be remembered.
var sudoersSafeRE = regexp.MustCompile(`^[A-Za-z0-9._/=-]+$`)

func validateSudoersWord(kind, name, word string) error {
	if !sudoersSafeRE.MatchString(word) {
		return fmt.Errorf("command %q has %s %q containing a character outside the sudoers-safe set [A-Za-z0-9._/=-]; refusing to render a rule that could match more than the validator permits", name, kind, word)
	}
	return nil
}

func validateExactArg(name, arg string) error {
	return validateSudoersWord("exactArgs element", name, arg)
}

// validateRenderInputs checks everything renderSudoers is about to interpolate
// into the file: the grant user, every exactArgs literal, and that paths has a
// complete, absolute entry for each vocabulary command. An incomplete paths
// map would otherwise render a valid-looking rule for the wrong command.
func validateRenderInputs(user string, paths map[string]string) error {
	if err := validGrantUser(user); err != nil {
		return err
	}
	for _, c := range privilegedCommands {
		if c.anyArgs && len(c.exactArgs) > 0 {
			return fmt.Errorf("command %q sets both anyArgs and exactArgs; the rendered rule would grant only the exact-args form while the validator accepts any arguments", c.name)
		}
		if c.denyBare && !c.anyArgs {
			return fmt.Errorf("command %q sets denyBare without anyArgs; the alias never grants the bare form this would deny", c.name)
		}
		p, ok := paths[c.name]
		if !ok {
			return fmt.Errorf("no resolved path for command %q; refusing to render an incomplete sudoers rule", c.name)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path %q for command %q is not absolute; refusing to render a sudoers rule with an ambiguous command path", p, c.name)
		}
		if err := validateSudoersWord("path", c.name, p); err != nil {
			return err
		}
		for _, a := range c.exactArgs {
			if err := validateExactArg(c.name, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// renderSudoers builds the rule from the same vocabulary the step validator
// uses, so a command rooket runs and a command the rule grants cannot diverge.
func renderSudoers(user string, paths map[string]string) (string, error) {
	if err := validateRenderInputs(user, paths); err != nil {
		return "", err
	}

	var entries, denials []string
	for _, c := range privilegedCommands {
		spec := paths[c.name]
		switch {
		case len(c.exactArgs) > 0:
			spec += " " + strings.Join(c.exactArgs, " ")
		case !c.anyArgs:
			// sudoers permits ANY arguments unless "" is spelled out explicitly;
			// privCommand.matches treats this shape (no anyArgs, no exactArgs)
			// as zero arguments only, so the rule must say so too or it would
			// grant a broader command line than the validator permits.
			spec += ` ""`
		}
		entries = append(entries, spec)
		if c.denyBare {
			denials = append(denials, fmt.Sprintf(`!%s ""`, paths[c.name]))
		}
	}

	var sb strings.Builder
	sb.WriteString(sudoersHeader)
	sb.WriteString("\nCmnd_Alias ROOKET_ISCSI = ")
	sb.WriteString(strings.Join(entries, ", \\\n                          "))
	sb.WriteString("\n\n")
	sb.WriteString(user + " ALL=(root) NOPASSWD: ROOKET_ISCSI")
	for _, d := range denials {
		sb.WriteString(", " + d)
	}
	sb.WriteString("\n")
	return sb.String(), nil
}

// grantUserRE matches a portable POSIX user name. The name is interpolated
// into a sudoers file, so anything outside this set is refused rather than
// escaped.
var grantUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

func validGrantUser(name string) error {
	if !grantUserRE.MatchString(name) {
		return fmt.Errorf("invalid user name %q: want a POSIX user name matching %s", name, grantUserRE)
	}
	return nil
}

// defaultGrantUser is the invoking user, resolved through SUDO_USER first so
// that `sudo rooket sudoers install` grants the real user rather than root.
// The name is returned only alongside a nil error: a caller that drops the
// error must not be able to carry an unvalidated name forward.
func defaultGrantUser() (string, error) {
	if u := os.Getenv("SUDO_USER"); u != "" {
		if err := validGrantUser(u); err != nil {
			return "", err
		}
		return u, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if err := validGrantUser(u.Username); err != nil {
		return "", err
	}
	return u.Username, nil
}

// checkOwnershipAndMode is the trust predicate shared by the binary and every
// ancestor directory leading to it: owned by uid 0, and not group- or
// world-writable. The sticky bit is deliberately not treated as an exemption
// for a writable directory — it blocks unrelated users from deleting each
// other's files, but does nothing to stop planting a new one, which is enough
// to replace a "trusted" binary.
func checkOwnershipAndMode(path string, uid uint32, mode os.FileMode) error {
	if uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not root; refusing to grant passwordless sudo on it", path, uid)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group- or world-writable (mode %04o); refusing to grant passwordless sudo on it", path, mode.Perm())
	}
	return nil
}

func checkTrustedOwnership(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: cannot determine ownership on this platform", path)
	}
	return checkOwnershipAndMode(path, st.Uid, fi.Mode())
}

// checkAncestorDirs walks every directory from path's parent up to / and
// rejects any that is not root-owned and free of group/world write bits. A
// writable directory anywhere in the chain lets an unprivileged user replace
// (or rename in) a file at the leaf, regardless of the leaf's own ownership.
func checkAncestorDirs(path string) error {
	dir := filepath.Dir(path)
	for {
		if err := checkTrustedOwnership(dir); err != nil {
			return err
		}
		if dir == "/" {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// checkTrustedBinary refuses to grant sudo on anything a non-root account
// could replace. $PATH is caller-controlled and os.Stat follows symlinks, so
// without resolving first, a user-owned symlink pointing at a root-owned
// binary would report the target's ownership and pass. The resolved path is
// what gets validated and returned — and what sudo should exec — so a
// symlink's own (possibly writable) location never matters.
func checkTrustedBinary(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path; checkAncestorDirs' walk to \"/\" never terminates on a relative one", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	fi, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", resolved, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file; refusing to grant passwordless sudo on it", resolved)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("stat %s: cannot determine ownership on this platform", resolved)
	}
	if err := checkOwnershipAndMode(resolved, st.Uid, fi.Mode()); err != nil {
		return "", err
	}
	if err := checkAncestorDirs(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveCommandPaths() (map[string]string, error) {
	paths := map[string]string{}
	for _, c := range privilegedCommands {
		if _, done := paths[c.name]; done {
			continue
		}
		p, err := exec.LookPath(c.name)
		if err != nil {
			return nil, fmt.Errorf("locate %s: %w", c.name, err)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		resolved, err := checkTrustedBinary(abs)
		if err != nil {
			return nil, err
		}
		paths[c.name] = resolved
	}
	return paths, nil
}
