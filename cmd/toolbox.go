package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
)

// kubectlOutput runs kubectl for ceph-config. Indirected so tests can answer
// with canned output instead of a cluster.
var kubectlOutput = runKubectl

// runKubectl runs kubectl against the cluster $KUBECONFIG names (see
// useCluster) within ctx, tracing the command line to stdout, and returns its
// trimmed stdout. A failure carries kubectl's stderr, without which it would
// say only "exit status 1" — not, for one, that the toolbox is missing.
func runKubectl(ctx context.Context, args ...string) (string, error) {
	return runKubectlContextTo(ctx, os.Stdout, args...)
}

// runKubectlContextTo is runKubectl with the command's trace line written to w
// rather than stdout. A kubectl ctx's deadline killed fails with an error that
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

// toolboxCeph returns the kubectl arguments that run a ceph command in the
// toolbox, bounded so that ceph gives up by itself. Killing kubectl at the end
// of a budget leaves the ceph it started running, and unbounded, ceph waits
// five minutes to connect and as long as it takes for a mon to answer. Each
// option is one argument, so the ceph CLI cannot take its value for a word
// of the command.
func toolboxCeph(command ...string) []string {
	return toolboxArgs(append([]string{"ceph", "--connect-timeout=20", "--rados-mon-op-timeout=20"}, command...)...)
}

// budgetSpent names the budget that ran out, spent, for a kubectl its
// context's deadline killed, which would otherwise say only "signal: killed".
func budgetSpent(err error, spent string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out: %s ran out", spent)
	}
	return err
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
