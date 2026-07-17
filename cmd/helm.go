package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var helmCmd = &cobra.Command{
	Use:   "helm [args...]",
	Short: "Run helm against the cluster with its isolated helm config",
	Long: `helm runs the real helm with KUBECONFIG pointed at the cluster's own
kubeconfig and HELM_CONFIG_HOME/HELM_CACHE_HOME/HELM_DATA_HOME pointed at the
cluster's isolated helm state — the same one rooket's own installs use — so
repositories added here belong to this cluster only. The host's helm
plugins, repositories, and credentials are not visible; use plain helm for
those. The cluster is selected the same way as the rest of rooket:
$ROOKET_NAME, or the name derived from the enclosing rook clone's path.

  rooket helm list -n rook-ceph
  rooket helm repo add example https://charts.example.com
`,
	// Forward all flags (e.g. -n, -o) straight to helm rather than parsing
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
		// Fail with an actionable error instead of letting helm chase a
		// missing kubeconfig into "connection refused" noise.
		if _, err := os.Stat(kc); err != nil {
			return fmt.Errorf("no kubeconfig for cluster %q at %s (is it up?)", name, kc)
		}
		if err := os.Setenv("KUBECONFIG", kc); err != nil {
			return err
		}
		env, err := helmEnv(name, "rooket")
		if err != nil {
			return err
		}
		c := exec.Command("helm", args...)
		c.Env = append(os.Environ(), env...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	},
}

func init() {
	rootCmd.AddCommand(helmCmd)
}
