package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
)

// kubectlOutput runs kubectl for ceph-config. Indirected so tests can answer
// with canned output instead of a cluster.
var kubectlOutput = runKubectl

// runKubectl runs kubectl against the cluster $KUBECONFIG names (see
// useCluster) and returns its trimmed stdout. A failure carries kubectl's
// stderr, without which it would say only "exit status 1" — not, for one,
// that the toolbox is missing.
func runKubectl(args ...string) (string, error) {
	out, err := run.Output("kubectl", args...)
	return out, withKubectlStderr(err)
}

// runKubectlContextTo is runKubectl bounded by ctx, with the command's trace
// line written to w. A kubectl the deadline killed fails with an error that
// wraps context.DeadlineExceeded.
func runKubectlContextTo(ctx context.Context, w io.Writer, args ...string) (string, error) {
	out, err := run.OutputContextTo(ctx, w, "kubectl", args...)
	return out, withKubectlStderr(err)
}

// withKubectlStderr appends what a failed kubectl wrote to stderr to its
// error.
func withKubectlStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// toolboxArgs returns the kubectl arguments that run command in the Ceph
// toolbox the cluster chart deploys.
func toolboxArgs(command ...string) []string {
	return append([]string{"-n", "rook-ceph", "exec", "deploy/rook-ceph-tools", "--"}, command...)
}

// cephClusterList is the part of `kubectl get cephcluster -o json` that
// ceph-config and wait read.
type cephClusterList struct {
	Items []struct {
		Spec struct {
			Network struct {
				Provider    string `json:"provider"`
				HostNetwork bool   `json:"hostNetwork"`
			} `json:"network"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}
