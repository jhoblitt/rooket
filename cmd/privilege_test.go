package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivCommandMatches(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"targetcli with args", []string{"targetcli", "/iscsi", "create", "iqn.x"}, true},
		{"targetcli bare is the root REPL", []string{"targetcli"}, false},
		{"iscsiadm with args", []string{"iscsiadm", "-m", "node", "--login"}, true},
		{"systemctl start iscsid", []string{"systemctl", "start", "iscsid"}, true},
		{"systemctl restart iscsid", []string{"systemctl", "restart", "iscsid"}, true},
		{"systemctl restart sshd", []string{"systemctl", "restart", "sshd"}, false},
		{"systemctl link", []string{"systemctl", "link", "/tmp/evil.service"}, false},
		{"tee initiatorname", []string{"tee", "/etc/iscsi/initiatorname.iscsi"}, true},
		{"tee passwd", []string{"tee", "/etc/passwd"}, false},
		{"cat rooket sudoers", []string{"cat", "/etc/sudoers.d/rooket"}, true},
		{"cat shadow", []string{"cat", "/etc/shadow"}, false},
		{"sh", []string{"sh", "-c", "id"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := false
			for _, c := range privilegedCommands {
				if c.matches(tc.argv) {
					got = true
					break
				}
			}
			if got != tc.want {
				t.Errorf("vocabulary match for %v = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestValidateStepsRejectsUngrantedCommand(t *testing.T) {
	err := validateSteps([]privStep{{argv: []string{"rm", "-rf", "/"}}})
	if err == nil {
		t.Fatal("validateSteps accepted an ungranted command, want error")
	}
	if !strings.Contains(err.Error(), "rm") {
		t.Errorf("error %q does not name the offending command", err)
	}
}

func TestValidateStepsAcceptsGrantedCommands(t *testing.T) {
	steps := []privStep{
		{argv: []string{"systemctl", "start", "iscsid"}},
		{argv: []string{"targetcli", "saveconfig"}},
	}
	if err := validateSteps(steps); err != nil {
		t.Fatalf("validateSteps rejected granted commands: %v", err)
	}
}

func TestRenderScript(t *testing.T) {
	steps := []privStep{
		{argv: []string{"systemctl", "start", "iscsid"}},
		{argv: []string{"tee", "/etc/iscsi/initiatorname.iscsi"}, stdinLine: "InitiatorName=iqn.x", quietStdout: true},
		{argv: []string{"targetcli", "/iscsi", "create", "iqn.y"}, quietStderr: true, ignoreErr: true},
		{argv: []string{"systemctl", "restart", "iscsid"}, settle: time.Second},
		{argv: []string{"iscsiadm", "-m", "node", "--login"}, ignoreErr: true},
	}
	want := strings.Join([]string{
		"set -e",
		"systemctl 'start' 'iscsid'",
		"printf '%s\\n' 'InitiatorName=iqn.x' | tee '/etc/iscsi/initiatorname.iscsi' > /dev/null",
		"targetcli '/iscsi' 'create' 'iqn.y' 2>/dev/null || true",
		"systemctl 'restart' 'iscsid' && sleep 1",
		"iscsiadm '-m' 'node' '--login' || true",
		"",
	}, "\n")
	if got := renderScript(steps); got != want {
		t.Errorf("renderScript =\n%s\nwant\n%s", got, want)
	}
}

// quietStdout and quietStderr must discard their own stream independently —
// this is what makes a failing tee's real diagnostic survive on stderr while
// its stdout echo of the written content stays hidden.
func TestRenderStepLineSplitsStdoutAndStderr(t *testing.T) {
	cases := []struct {
		name string
		step privStep
		want string
	}{
		{"quiet stdout only", privStep{argv: []string{"cmd", "a"}, quietStdout: true}, "cmd 'a' > /dev/null"},
		{"quiet stderr only", privStep{argv: []string{"cmd", "a"}, quietStderr: true}, "cmd 'a' 2>/dev/null"},
		{"quiet both", privStep{argv: []string{"cmd", "a"}, quietStdout: true, quietStderr: true}, "cmd 'a' > /dev/null 2>/dev/null"},
		{"neither quiet", privStep{argv: []string{"cmd", "a"}}, "cmd 'a'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStepLine(tc.step); got != tc.want {
				t.Errorf("renderStepLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// A settle below one second must not render "sleep 0" — int(Seconds())
// truncates, which would make the pkexec fallback skip the settle entirely.
func TestRenderStepLineClampsSubSecondSettle(t *testing.T) {
	step := privStep{argv: []string{"systemctl", "restart", "iscsid"}, settle: 200 * time.Millisecond}
	want := "systemctl 'restart' 'iscsid' && sleep 1"
	if got := renderStepLine(step); got != want {
		t.Errorf("renderStepLine = %q, want %q", got, want)
	}
}

// The rendered script runs the whole list under "set -e", so a warnOnFailure
// step must still not abort it — but unlike ignoreErr's "|| true", it must
// leave a visible trace when it fails, since the whole point is that a
// swallowed failure here was once invisible.
func TestRenderStepLineWarnOnFailure(t *testing.T) {
	step := privStep{argv: []string{"targetcli", "create", "iqn.y"}, warnOnFailure: true}
	want := `targetcli 'create' 'iqn.y' || printf 'warning: %s failed, continuing\n' 'targetcli create iqn.y' >&2`
	if got := renderStepLine(step); got != want {
		t.Errorf("renderStepLine = %q, want %q", got, want)
	}
}

// writeStubSudo puts fake sudo and pkexec on PATH, each recording its argv to
// a shared log and also echoing it to its own stdout, so a test can assert on
// either the log (argv actually issued) or the stdout a caller's writer
// received. Exit codes are set independently so a test can make the sudo
// probe fail while letting the pkexec fallback succeed.
func writeStubSudo(t *testing.T, sudoExit, pkexecExit int) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	for name, code := range map[string]int{"sudo": sudoExit, "pkexec": pkexecExit} {
		stub := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf '%%s\\n' \"$*\"\nexit %d\n", logPath, code)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, logPath
}

// writeStubSudoDenyingRooketRule puts a fake sudo on PATH that denies exactly
// rooket's own `cat` probe (as if rooket's rule were never installed) but
// authorizes everything else, modeling a host with a blanket passwordless
// grant (e.g. "%wheel ALL=(ALL) NOPASSWD: ALL") and no rooket-specific rule —
// the CRITICAL regression this stub exists to catch.
func writeStubSudoDenyingRooketRule(t *testing.T) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	stub := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
printf '%%s\n' "$*"
case "$*" in
  "-n %s %s") exit 1 ;;
  *) exit 0 ;;
esac
`, logPath, resolvedCommandPath("cat"), sudoersPath)
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, logPath
}

func TestRunStepsIssuesItemizedCommands(t *testing.T) {
	dir, logPath := writeStubSudo(t, 0, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{
		{argv: []string{"systemctl", "start", "iscsid"}},
		{argv: []string{"targetcli", "saveconfig"}},
	}
	if err := runSteps(io.Discard, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("-n %s start iscsid\n-n %s saveconfig\n",
		resolvedCommandPath("systemctl"), resolvedCommandPath("targetcli"))
	if string(got) != want {
		t.Errorf("stub sudo recorded\n%q\nwant\n%q", got, want)
	}
}

func TestRunStepsHonoursIgnoreErr(t *testing.T) {
	dir, _ := writeStubSudo(t, 3, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := runSteps(io.Discard, []string{"sudo", "-n"}, []privStep{
		{argv: []string{"targetcli", "saveconfig"}, ignoreErr: true},
	}); err != nil {
		t.Errorf("runSteps returned %v for an ignoreErr step, want nil", err)
	}
	if err := runSteps(io.Discard, []string{"sudo", "-n"}, []privStep{
		{argv: []string{"targetcli", "saveconfig"}},
	}); err == nil {
		t.Error("runSteps returned nil for a failing step, want error")
	}
}

// This is the regression test for the targetcli-create-swallowing bug: a
// warnOnFailure step's failure must not abort the run, must still let the
// next step execute, and must be reported (both the command's own stderr and
// an explicit warning naming the failed command) rather than discarded, the
// way ignoreErr+quietStderr used to discard it.
func TestRunStepsWarnOnFailureContinuesAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	stub := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "-n %s fail-step") echo boom-diagnostic >&2; exit 1 ;;
  *) exit 0 ;;
esac
`, resolvedCommandPath("targetcli"))
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	steps := []privStep{
		{argv: []string{"targetcli", "fail-step"}, warnOnFailure: true},
		{argv: []string{"systemctl", "next-step"}},
	}
	if err := runSteps(&buf, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps returned %v for a warnOnFailure step, want nil", err)
	}
	got := buf.String()
	if !strings.Contains(got, "boom-diagnostic") {
		t.Errorf("failed step's own stderr was not visible: %q", got)
	}
	if !strings.Contains(got, "warning") || !strings.Contains(got, "fail-step") {
		t.Errorf("no warning naming the failed command: %q", got)
	}
	if !strings.Contains(got, "next-step") {
		t.Errorf("subsequent step did not run: %q", got)
	}
}

func TestRunStepsSuppressesQuietStepOutput(t *testing.T) {
	dir, _ := writeStubSudo(t, 0, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	steps := []privStep{
		{argv: []string{"targetcli", "quiet-step"}, quietStdout: true},
		{argv: []string{"systemctl", "loud-step"}},
	}
	if err := runSteps(&buf, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "quiet-step") {
		t.Errorf("quiet step's output was not suppressed: %q", got)
	}
	if !strings.Contains(got, "loud-step") {
		t.Errorf("non-quiet step's output is missing: %q", got)
	}
}

// This is the regression test for the tee diagnostics bug: quietStdout must
// hide only the command's own stdout, never its stderr, or a real failure
// (e.g. /etc/iscsi missing) reports no cause at all.
func TestRunStepsQuietStdoutStillSurfacesStderr(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\necho stdout-noise\necho stderr-diagnostic >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	steps := []privStep{{argv: []string{"tee", initiatorNamePath}, stdinLine: "x", quietStdout: true}}
	if err := runSteps(&buf, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "stdout-noise") {
		t.Errorf("quietStdout step's stdout was not suppressed: %q", got)
	}
	if !strings.Contains(got, "stderr-diagnostic") {
		t.Errorf("quietStdout step's stderr was wrongly suppressed too: %q", got)
	}
}

// The mirror image: quietStderr (used to hide targetcli's "already exists"
// noise on a re-run) must not also hide the command's real stdout.
func TestRunStepsQuietStderrKeepsStdoutVisible(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\necho stdout-info\necho stderr-noise >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	steps := []privStep{{argv: []string{"targetcli", "create"}, quietStderr: true}}
	if err := runSteps(&buf, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "stdout-info") {
		t.Errorf("quietStderr step's stdout was wrongly suppressed: %q", got)
	}
	if strings.Contains(got, "stderr-noise") {
		t.Errorf("quietStderr step's stderr was not suppressed: %q", got)
	}
}

// Dropping the stdinLine branch (e.g. always calling the no-stdin executor)
// would make tee inherit no input at all — this proves the reader actually
// reaches the child process.
func TestRunStepsRoutesStdinToCommand(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "stdin.out")
	stub := fmt.Sprintf("#!/bin/sh\ncat > %q\n", outPath)
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{{argv: []string{"tee", initiatorNamePath}, stdinLine: "InitiatorName=iqn.x"}}
	if err := runSteps(io.Discard, []string{"sudo", "-n"}, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "InitiatorName=iqn.x\n"; string(got) != want {
		t.Errorf("stdin received by command = %q, want %q", got, want)
	}
}

func TestRunPrivilegedFallsBackToPkexec(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runPrivileged takes the already-root branch and would run a real targetcli command")
	}
	dir, logPath := writeStubSudo(t, 1, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{{argv: []string{"targetcli", "saveconfig"}}}
	if err := runPrivileged(steps); err != nil {
		t.Fatalf("runPrivileged: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Both probes run first and fail (the stub denies everything), so no
	// itemized step is attempted and the whole script goes to pkexec instead.
	if !strings.Contains(string(got), "-n "+resolvedCommandPath("cat")+" "+sudoersPath) {
		t.Errorf("rooket-rule probe not attempted; log:\n%s", got)
	}
	if strings.Contains(string(got), "-n "+resolvedCommandPath("targetcli")+" saveconfig") {
		t.Errorf("itemized step attempted despite a dead grant; log:\n%s", got)
	}
	if !strings.Contains(string(got), "sh /") {
		t.Errorf("pkexec fallback not invoked; log:\n%s", got)
	}
}

// Inverting the sudoersGrantLive()||sudoNoPasswordAvailable() branch in
// runPrivileged breaks the feature and would still pass `go test` without
// this: it asserts the itemized path, not pkexec, is taken when a
// passwordless probe succeeds.
func TestRunPrivilegedTakesItemizedPathWhenProbeSucceeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runPrivileged takes the already-root branch and would run a real targetcli command")
	}
	dir, logPath := writeStubSudo(t, 0, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{{argv: []string{"targetcli", "saveconfig"}}}
	if err := runPrivileged(steps); err != nil {
		t.Fatalf("runPrivileged: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-n "+resolvedCommandPath("targetcli")+" saveconfig") {
		t.Errorf("itemized invocation missing; log:\n%s", got)
	}
	if strings.Contains(string(got), "sh /") {
		t.Errorf("pkexec fallback invoked despite a live probe; log:\n%s", got)
	}
}

// CRITICAL regression test: on a host with a blanket passwordless sudo grant
// (e.g. the CI runner's, or a workstation's "%wheel ALL=(ALL) NOPASSWD: ALL")
// but no rooket-specific rule, runPrivileged must still take the itemized
// path rather than falling all the way through to a pkexec prompt.
func TestRunPrivilegedTakesItemizedPathOnBlanketSudoWithoutRooketRule(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runPrivileged takes the already-root branch and would run a real targetcli command")
	}
	dir, logPath := writeStubSudoDenyingRooketRule(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{{argv: []string{"targetcli", "saveconfig"}}}
	if err := runPrivileged(steps); err != nil {
		t.Fatalf("runPrivileged: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-n "+resolvedCommandPath("cat")+" "+sudoersPath) {
		t.Errorf("rooket-rule probe not attempted; log:\n%s", got)
	}
	if !strings.Contains(string(got), "-n true") {
		t.Errorf("blanket-sudo probe not attempted; log:\n%s", got)
	}
	if !strings.Contains(string(got), "-n "+resolvedCommandPath("targetcli")+" saveconfig") {
		t.Errorf("itemized step not attempted despite blanket sudo; log:\n%s", got)
	}
	if strings.Contains(string(got), "sh /") {
		t.Errorf("pkexec fallback invoked despite blanket sudo; log:\n%s", got)
	}
}

// A stale rooket rule (e.g. targetcli relocated by a distro upgrade) still
// passes both live probes but then denies the individual command; the failed
// itemized attempt must fall back to pkexec rather than surfacing the denial.
func TestRunPrivilegedFallsBackToPkexecWhenItemizedRunFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runPrivileged takes the already-root branch and would run a real targetcli command")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	sudoStub := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  "-n %s %s") exit 0 ;;
  "-n %s saveconfig") exit 1 ;;
  *) exit 0 ;;
esac
`, logPath, resolvedCommandPath("cat"), sudoersPath, resolvedCommandPath("targetcli"))
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudoStub), 0o755); err != nil {
		t.Fatal(err)
	}
	pkexecStub := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", logPath)
	if err := os.WriteFile(filepath.Join(dir, "pkexec"), []byte(pkexecStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	steps := []privStep{{argv: []string{"targetcli", "saveconfig"}}}
	if err := runPrivileged(steps); err != nil {
		t.Fatalf("runPrivileged: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-n "+resolvedCommandPath("targetcli")+" saveconfig") {
		t.Errorf("itemized attempt not made; log:\n%s", got)
	}
	if !strings.Contains(string(got), "sh /") {
		t.Errorf("pkexec fallback not invoked after a stale itemized grant; log:\n%s", got)
	}
}

func TestRunPrivilegedRejectsUngrantedStep(t *testing.T) {
	if err := runPrivileged([]privStep{{argv: []string{"rm", "-rf", "/"}}}); err == nil {
		t.Fatal("runPrivileged accepted an ungranted command, want error")
	}
}

// sudo matches a rule by the command path it resolves through secure_path, and
// will not match a symlink against a rule naming the symlink's target. rooket's
// generated rule records the resolved path, so an itemized invocation must name
// that same resolved path rather than the bare command name.
func TestResolvedCommandPathFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-tool")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "targetcli")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedCommandPath("targetcli"); got != want {
		t.Errorf("resolvedCommandPath(targetcli) = %q, want the symlink target %q", got, want)
	}
}

func TestResolvedCommandPathFallsBackToTheBareName(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	const missing = "rooket-no-such-command"
	if got := resolvedCommandPath(missing); got != missing {
		t.Errorf("resolvedCommandPath(%q) = %q, want the name unchanged", missing, got)
	}
}

func TestRunStepsInvokesTheResolvedPath(t *testing.T) {
	dir, logPath := writeStubSudo(t, 0, 0)
	real := filepath.Join(dir, "real-targetcli")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "targetcli")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := runSteps(io.Discard, []string{"sudo", "-n"}, []privStep{
		{argv: []string{"targetcli", "saveconfig"}},
	}); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-n "+want+" saveconfig") {
		t.Errorf("stub sudo recorded\n%s\nwant the resolved path %q", got, want)
	}
}
