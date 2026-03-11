// Package run provides helpers for executing external commands with consistent
// output handling.
package run

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd runs a command, streaming stdout/stderr to the terminal.
func Cmd(name string, args ...string) error {
	return CmdWithEnv(nil, name, args...)
}

// CmdWithEnv runs a command with additional environment variables appended to
// the current environment.
func CmdWithEnv(extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	return cmd.Run()
}

// Output runs a command and returns its combined stdout output as a string.
func Output(name string, args ...string) (string, error) {
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// CmdWithStdin runs a command with stdin piped from the provided reader.
func CmdWithStdin(stdin io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	return cmd.Run()
}
