package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var kubectlCmd = &cobra.Command{
	Use:     "kubectl [args...]",
	Aliases: []string{"k"},
	Short:   "Run kubectl against the cluster (KUBECONFIG set automatically)",
	Long: `kubectl runs the real kubectl with KUBECONFIG pointed at the cluster's own
kubeconfig, forwarding every argument. The cluster is selected the same way as
the rest of rooket: $ROOKET_NAME, or the name derived from the enclosing rook
clone's path.

  rooket kubectl get pods -n rook-ceph
  rooket k get nodes
`,
	// Forward all flags (e.g. -n, -o) straight to kubectl rather than parsing
	// them as rooket flags.
	DisableFlagParsing: true,
	SilenceUsage:       true,
	SilenceErrors:      true,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := clusterName("")
		kc, err := kubeconfigPath(name)
		if err != nil {
			return err
		}
		// Fail with an actionable error instead of letting kubectl chase a
		// missing kubeconfig into "connection refused" noise.
		if _, err := os.Stat(kc); err != nil {
			return fmt.Errorf("no kubeconfig for cluster %q at %s (is it up?)", name, kc)
		}
		if err := os.Setenv("KUBECONFIG", kc); err != nil {
			return err
		}
		c := exec.Command("kubectl", args...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	},
}

func init() {
	rootCmd.AddCommand(kubectlCmd)
}
