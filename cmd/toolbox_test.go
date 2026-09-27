package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKubectl puts a kubectl first on PATH that exits 1 with its arguments on
// stderr when the first is "fail", and otherwise prints them on stdout.
func fakeKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = fail ]; then echo \"stderr: $*\" >&2; exit 1; fi\n" +
		"echo \"stdout: $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// useRealKubectl points kubectlOutput at its default for the test.
func useRealKubectl(t *testing.T) {
	t.Helper()
	prev := kubectlOutput
	kubectlOutput = runKubectl
	t.Cleanup(func() { kubectlOutput = prev })
}

func TestKubectlOutputReturnsStdout(t *testing.T) {
	fakeKubectl(t)
	useRealKubectl(t)

	out, err := kubectlOutput(context.Background(), "get", "cephcluster")
	if err != nil {
		t.Fatalf("kubectlOutput: %v", err)
	}
	if out != "stdout: get cephcluster" {
		t.Errorf("kubectlOutput() = %q, want kubectl's trimmed stdout", out)
	}
}

// Without kubectl's stderr the error would read only "exit status 1".
func TestKubectlOutputFailureCarriesStderr(t *testing.T) {
	fakeKubectl(t)
	useRealKubectl(t)

	_, err := kubectlOutput(context.Background(), "fail", "exec", "deploy/rook-ceph-tools")
	if err == nil {
		t.Fatal("kubectlOutput succeeded for a kubectl that exits 1")
	}
	if !strings.Contains(err.Error(), "stderr: fail exec deploy/rook-ceph-tools") {
		t.Errorf("kubectlOutput() error = %q, want kubectl's stderr in it", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("kubectlOutput() error = %v, want it to still wrap the *exec.ExitError", err)
	}
}

// A deadline that has passed stops ceph-config's runner before it starts
// kubectl, and the error says so, which is what ceph-config's budget rests on.
func TestKubectlOutputHonorsItsDeadline(t *testing.T) {
	fakeKubectl(t)
	useRealKubectl(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if out, err := kubectlOutput(ctx, "get", "cephcluster"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("kubectlOutput() past its deadline = %q, %v; want context.DeadlineExceeded", out, err)
	}
}

// writeKubeconfig puts a kubeconfig where rooket keeps name's, so the cluster
// reads as up.
func writeKubeconfig(t *testing.T, name string) string {
	t.Helper()
	kc, err := kubeconfigPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(kc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}
