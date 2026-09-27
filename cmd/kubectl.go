package cmd

import (
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
	RunE: func(cmd *cobra.Command, args []string) error {
		kc, err := requireKubeconfig(clusterName(""))
		if err != nil {
			return err
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
