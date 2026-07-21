// Package run provides helpers for executing external commands with consistent
// output handling.
package run

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ANSI styles that distinguish rooket's own output from the commands it runs:
// status lines are cyan, the "+ cmd" command echoes are dim, and the commands'
// actual output is left in the terminal's default color.
const (
	colorStatus = "\x1b[36m" // cyan
	colorTrace  = "\x1b[2m"  // dim
	colorReset  = "\x1b[0m"
)

var (
	timestamps bool
	colorized  bool
	startTime  = time.Now()
)

// SetTimestamps enables the elapsed-time prefix on rooket output. It must be
// called before commands run (the root command's PersistentPreRunE) and never
// concurrently with them.
func SetTimestamps(on bool) { timestamps = on }

// SetColor enables ANSI coloring of rooket's own output. Same lifecycle rules
// as SetTimestamps.
func SetColor(on bool) { colorized = on }

// Printf prints a rooket status line to stdout.
func Printf(format string, a ...any) {
	Fprintf(os.Stdout, format, a...)
}

// Fprintf is Printf writing to an explicit writer — used by callers that
// buffer a concurrent task's output for later, ordered flushing.
func Fprintf(w io.Writer, format string, a ...any) {
	emit(w, colorStatus, fmt.Sprintf(format, a...))
}

// Tracef emits a dim "+ name args" command echo to w, for callers that run a
// command outside the Cmd* helpers (e.g. the streamed make) but still want the
// echo styled like every other command trace.
func Tracef(w io.Writer, name string, args ...string) {
	tracef(w, name, args)
}

// tracef emits a "+ cmd args" command echo, styled dim to set it apart from
// both status lines and the command's own output.
func tracef(w io.Writer, name string, args []string) {
	line := "+ " + name
	if len(args) > 0 {
		line += " " + strings.Join(args, " ")
	}
	emit(w, colorTrace, line+"\n")
}

// emit writes msg, applying the --timestamps prefix and the given color to
// each non-empty line. Both wrap the whole line (prefix included), and the
// message is written in one call so concurrent printers cannot interleave a
// prefix or color escape with another caller's text. Child-process output is
// never routed through here and streams unstyled.
func emit(w io.Writer, color, msg string) {
	var prefix string
	if timestamps {
		prefix = fmt.Sprintf("[%6.1fs] ", time.Since(startTime).Seconds())
	}
	if prefix == "" && !colorized {
		fmt.Fprint(w, msg)
		return
	}
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		if l == "" {
			continue
		}
		l = prefix + l
		if colorized {
			l = color + l + colorReset
		}
		lines[i] = l
	}
	fmt.Fprint(w, strings.Join(lines, "\n"))
}

// Cmd runs a command, streaming stdout/stderr to the terminal.
// stdin is connected to the process's controlling terminal so that programs
// like sudo can prompt for a password interactively.
func Cmd(name string, args ...string) error {
	return CmdWithEnvTo(os.Stdout, nil, name, args...)
}

// CmdTo is Cmd writing the trace line and BOTH child output streams to w —
// for commands run concurrently whose output is buffered and flushed in
// order.
func CmdTo(w io.Writer, name string, args ...string) error {
	return CmdWithEnvTo(w, nil, name, args...)
}

// CmdWithEnv runs a command with additional environment variables appended to
// the current environment.
func CmdWithEnv(extraEnv []string, name string, args ...string) error {
	return CmdWithEnvTo(os.Stdout, extraEnv, name, args...)
}

// CmdWithEnvTo is CmdWithEnv with the trace line and both child streams
// routed to w.
func CmdWithEnvTo(w io.Writer, extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// Use /dev/tty so interactive programs (e.g. sudo) can prompt even when
	// os.Stdin is /dev/null inside a systemd scope.
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		cmd.Stdin = tty
		defer tty.Close()
	} else {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	tracef(w, name, args)
	return cmd.Run()
}

// Output runs a command and returns its stdout output as a string.
// stdin is /dev/null; use OutputInteractive when the command may prompt.
func Output(name string, args ...string) (string, error) {
	return OutputWithEnvTo(os.Stdout, nil, name, args...)
}

// OutputTo is Output with the trace line routed to w. The RETURNED stdout
// stays clean for machine parsing, and child stderr stays captured (surfaced
// via *exec.ExitError), exactly like Output.
func OutputTo(w io.Writer, name string, args ...string) (string, error) {
	return OutputWithEnvTo(w, nil, name, args...)
}

// OutputWithEnv runs a command with additional environment variables appended
// to the current environment (later entries override earlier ones) and returns
// its stdout output as a string.
func OutputWithEnv(extraEnv []string, name string, args ...string) (string, error) {
	return OutputWithEnvTo(os.Stdout, extraEnv, name, args...)
}

// OutputWithEnvTo is OutputWithEnv with the trace line routed to w.
func OutputWithEnvTo(w io.Writer, extraEnv []string, name string, args ...string) (string, error) {
	tracef(w, name, args)
	cmd := exec.Command(name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// OutputInteractive runs a command with stdin connected to /dev/tty (the
// controlling terminal) so that programs like sudo can prompt for a password
// even when the process's os.Stdin is /dev/null (e.g. inside a systemd scope).
// Returns the stdout output as a string.
func OutputInteractive(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)

	// Open /dev/tty directly so sudo can prompt regardless of how stdin is wired.
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		cmd.Stdin = tty
		defer tty.Close()
	} else {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	tracef(os.Stdout, name, args)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// CmdWithStdin runs a command with stdin piped from the provided reader.
func CmdWithStdin(stdin io.Reader, name string, args ...string) error {
	return CmdWithStdinEnv(stdin, nil, name, args...)
}

// CmdWithStdinEnv runs a command with stdin piped from the provided reader
// and additional environment variables appended to the current environment.
func CmdWithStdinEnv(stdin io.Reader, extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	tracef(os.Stdout, name, args)
	return cmd.Run()
}

// CmdWithStdinTo runs a command with stdin piped from the provided reader,
// writing the trace line and BOTH child output streams to w. Used for
// commands run concurrently, whose output is buffered and flushed in order.
func CmdWithStdinTo(w io.Writer, stdin io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = w
	cmd.Stderr = w
	tracef(w, name, args)
	return cmd.Run()
}
