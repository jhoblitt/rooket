package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/run"
)

// privStep is one command run with root privileges. The same step list is
// executed itemized through sudo and rendered to a shell script for the pkexec
// fallback, so anything that changes the command's shape belongs here rather
// than in either executor.
type privStep struct {
	argv          []string
	stdinLine     string // written to the command's stdin, newline appended
	ignoreErr     bool
	warnOnFailure bool // like ignoreErr, but prints a warning naming the step instead of swallowing the failure silently
	quietStdout   bool // discard the command's stdout, e.g. tee's echo of what it wrote
	quietStderr   bool // discard the command's stderr, e.g. a re-run's "already exists" noise
	settle        time.Duration
}

// privCommand is one entry in the vocabulary of commands rooket may run as
// root. It is the single source for both the generated sudoers rule and the
// validation that no step escapes that rule.
type privCommand struct {
	name      string
	anyArgs   bool
	exactArgs []string
	denyBare  bool // the no-argument form is targetcli's interactive root shell
}

var privilegedCommands = []privCommand{
	{name: "targetcli", anyArgs: true, denyBare: true},
	{name: "iscsiadm", anyArgs: true},
	{name: "systemctl", exactArgs: []string{"start", "iscsid"}},
	{name: "systemctl", exactArgs: []string{"restart", "iscsid"}},
	{name: "tee", exactArgs: []string{initiatorNamePath}},
	{name: "cat", exactArgs: []string{sudoersPath}},
}

func (c privCommand) matches(argv []string) bool {
	if len(argv) == 0 || argv[0] != c.name {
		return false
	}
	args := argv[1:]
	if c.anyArgs {
		return !(c.denyBare && len(args) == 0)
	}
	return slices.Equal(args, c.exactArgs)
}

// validateSteps fails when a step names a command the generated sudoers rule
// does not grant. Such a step would silently fall back to a pkexec prompt on a
// host that installed the rule, so it is caught by unit test instead.
func validateSteps(steps []privStep) error {
	for _, s := range steps {
		granted := false
		for _, c := range privilegedCommands {
			if c.matches(s.argv) {
				granted = true
				break
			}
		}
		if !granted {
			return fmt.Errorf("step %q is not covered by rooket's privileged command vocabulary", strings.Join(s.argv, " "))
		}
	}
	return nil
}

// renderScript renders steps as a /bin/sh script for the pkexec fallback. Every
// argument is single-quoted: the script runs as root, so no operand may be able
// to start a new command.
func renderScript(steps []privStep) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	for _, s := range steps {
		sb.WriteString(renderStepLine(s))
		sb.WriteString("\n")
	}
	return sb.String()
}

// resolvedCommandPath returns the absolute, symlink-resolved path of name --
// the same path resolveCommandPaths records in the generated sudoers rule.
//
// sudo matches a rule against the path it resolves the command to through
// secure_path, and will NOT match a symlink against a rule naming that
// symlink's target: on Fedora `iscsiadm` resolves to /usr/sbin/iscsiadm, which
// sudo refuses to match against the rule's /usr/bin/iscsiadm, so a bare-name
// invocation is denied. Naming the resolved path here keeps what rooket runs
// and what the rule grants identical by construction.
//
// Resolution failures degrade to the bare name rather than erroring: this is
// about agreeing with the rule, not about trusting the binary, which is an
// install-time concern handled by checkTrustedBinary.
func resolvedCommandPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return name
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return resolved
}

// runSteps runs each step with prefix prepended to its argv. quietStdout and
// quietStderr each discard their own stream independently, matching what the
// rendered script does with "> /dev/null" and "2>/dev/null".
func runSteps(w io.Writer, prefix []string, steps []privStep) error {
	for _, s := range steps {
		argv := append(append([]string{}, prefix...), s.argv...)
		argv[len(prefix)] = resolvedCommandPath(s.argv[0])
		outW, errW := w, w
		if s.quietStdout {
			outW = io.Discard
		}
		if s.quietStderr {
			errW = io.Discard
		}
		var err error
		if s.stdinLine != "" {
			err = run.CmdWithStdinSplitTo(outW, errW, strings.NewReader(s.stdinLine+"\n"), argv[0], argv[1:]...)
		} else {
			err = run.CmdSplitTo(outW, errW, argv[0], argv[1:]...)
		}
		if err != nil {
			switch {
			case s.warnOnFailure:
				fmt.Fprintf(w, "warning: %s failed, continuing: %v\n", strings.Join(s.argv, " "), err)
			case s.ignoreErr:
				// swallowed silently: this is the expected re-run case (e.g. --login
				// on an already-established session), not a diagnostic-worthy failure.
			default:
				return fmt.Errorf("%s: %w", strings.Join(s.argv, " "), err)
			}
		}
		if s.settle > 0 {
			time.Sleep(s.settle)
		}
	}
	return nil
}

// sudoersGrantLive reports whether rooket's sudoers rule is installed and
// active, by reading it back through the pinned `cat` the rule itself grants.
// The probe executes nothing with side effects.
func sudoersGrantLive() bool {
	_, ok := readInstalledSudoers()
	return ok
}

// sudoNoPasswordAvailable reports whether the invoking user holds SOME
// passwordless sudo grant, even one that predates or is broader than
// rooket's own rule (e.g. a workstation's "%wheel ALL=(ALL) NOPASSWD: ALL",
// or a CI runner's blanket grant). "true" is deliberately absent from
// privilegedCommands: that absence is what keeps this probe distinguishable
// from sudoersGrantLive's `cat` probe. If "true" were ever added to the
// vocabulary, a host holding only rooket's narrow rule would also pass this
// probe, collapsing the two cases runPrivileged needs to tell apart — so do
// not "tidy" it in.
func sudoNoPasswordAvailable() bool {
	return exec.Command("sudo", "-n", "true").Run() == nil
}

// runPrivileged executes steps with root privileges: directly when already
// root, itemized through sudo when the caller can run sudo without a prompt
// (either rooket's own rule or some broader passwordless grant), and
// otherwise as a single script through pkexec — the only path that can
// prompt.
//
// A stale rooket rule (e.g. a distro upgrade relocating targetcli) still
// passes the itemized path's probes but then denies the individual command,
// so an itemized run that errors falls back to pkexec rather than surfacing
// the denial. This is safe because every step is idempotent by construction:
// the targetcli create steps carry warnOnFailure rather than aborting, and
// re-running the whole step list re-applies the same state, so retrying it
// through pkexec costs one extra prompt rather than any incorrect or
// duplicated work.
func runPrivileged(out io.Writer, steps []privStep) error {
	if err := validateSteps(steps); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return runSteps(out, nil, steps)
	}
	if sudoersGrantLive() || sudoNoPasswordAvailable() {
		if err := runSteps(out, []string{"sudo", "-n"}, steps); err == nil {
			return nil
		}
	}
	return runPrivilegedViaPkexec(out, renderScript(steps))
}

func runPrivilegedViaPkexec(out io.Writer, script string) error {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return fmt.Errorf("no passwordless sudo rule (run `rooket sudoers install`) and pkexec is unavailable: %w", err)
	}
	f, err := os.CreateTemp("", "rooket-iscsi-*.sh")
	if err != nil {
		return fmt.Errorf("create temp script: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		return err
	}
	// CmdTo still wires the child's stdin to /dev/tty, so pkexec's polkit agent
	// can prompt even though its trace and output are routed to out.
	run.Fprintf(out, "==> requesting root via pkexec (you may be prompted to authenticate)\n")
	return run.CmdTo(out, "pkexec", "sh", f.Name())
}

func renderStepLine(s privStep) string {
	quoted := make([]string, 0, len(s.argv))
	for i, a := range s.argv {
		if i == 0 {
			quoted = append(quoted, a)
			continue
		}
		quoted = append(quoted, shQuote(a))
	}
	line := strings.Join(quoted, " ")
	if s.stdinLine != "" {
		// The pipe's own "> /dev/null" always hides tee's echo of what it
		// wrote, independent of quietStdout, so no separate redirect is added
		// here for that case.
		line = fmt.Sprintf("printf '%%s\\n' %s | %s > /dev/null", shQuote(s.stdinLine), line)
	} else if s.quietStdout {
		line += " > /dev/null"
	}
	if s.quietStderr {
		line += " 2>/dev/null"
	}
	switch {
	case s.ignoreErr:
		line += " || true"
	case s.warnOnFailure:
		// "|| true" would also satisfy set -e, but would leave the failure as
		// unreported here as ignoreErr does; a printf that always succeeds
		// keeps the script going while still naming the step that failed.
		// Writes to stdout, matching runSteps' warnOnFailure branch (which
		// writes to w, always stdout in production) — the two executors must
		// agree on which stream carries the warning.
		line += fmt.Sprintf(" || printf 'warning: %%s failed, continuing\\n' %s", shQuote(strings.Join(s.argv, " ")))
	}
	if s.settle > 0 {
		settle := s.settle
		if settle < time.Second {
			settle = time.Second // int(s.settle.Seconds()) would truncate a sub-second settle to a no-op "sleep 0"
		}
		line += fmt.Sprintf(" && sleep %d", int(settle.Seconds()))
	}
	return line
}
