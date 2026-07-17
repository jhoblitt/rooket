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

var (
	timestamps bool
	startTime  = time.Now()
)

// SetTimestamps enables the elapsed-time prefix on Printf output. It must be
// called before commands run (the root command's PersistentPreRunE) and never
// concurrently with them.
func SetTimestamps(on bool) { timestamps = on }

// Printf prints rooket-emitted output, applying the --timestamps elapsed-time
// prefix to each non-empty line when enabled. The message is assembled and
// written in one call so concurrent printers cannot interleave a prefix with
// another caller's text. Child-process output is never routed through here
// and streams unprefixed.
func Printf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if timestamps {
		prefix := fmt.Sprintf("[%6.1fs] ", time.Since(startTime).Seconds())
		lines := strings.Split(msg, "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = prefix + l
			}
		}
		msg = strings.Join(lines, "\n")
	}
	fmt.Print(msg)
}

func trace(name string, args []string) {
	Printf("+ %s %s\n", name, strings.Join(args, " "))
}

// Cmd runs a command, streaming stdout/stderr to the terminal.
// stdin is connected to the process's controlling terminal so that programs
// like sudo can prompt for a password interactively.
func Cmd(name string, args ...string) error {
	return CmdWithEnv(nil, name, args...)
}

// CmdWithEnv runs a command with additional environment variables appended to
// the current environment.
func CmdWithEnv(extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// Use /dev/tty so interactive programs (e.g. sudo) can prompt even when
	// os.Stdin is /dev/null inside a systemd scope.
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		cmd.Stdin = tty
		defer tty.Close()
	} else {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	trace(name, args)
	return cmd.Run()
}

// Output runs a command and returns its stdout output as a string.
// stdin is /dev/null; use OutputInteractive when the command may prompt.
func Output(name string, args ...string) (string, error) {
	return OutputWithEnv(nil, name, args...)
}

// OutputWithEnv runs a command with additional environment variables appended
// to the current environment (later entries override earlier ones) and returns
// its stdout output as a string.
func OutputWithEnv(extraEnv []string, name string, args ...string) (string, error) {
	trace(name, args)
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
	trace(name, args)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// CmdWithStdin runs a command with stdin piped from the provided reader.
func CmdWithStdin(stdin io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	trace(name, args)
	return cmd.Run()
}
