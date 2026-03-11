package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "rooket",
	Short: "Spin up a Rook development cluster using kind and a local OCI registry",
	Long: `rooket bootstraps a Kubernetes-in-Docker (kind) cluster pre-configured for
Rook development and testing. It creates:

  • A podman-backed local OCI registry (default port 5001) that every cluster
    node is configured to pull from, so you can push locally-built Rook images
    with:  podman push localhost:5001/rook/ceph:dev

  • A multi-node kind cluster whose containerd is wired to the local registry.

  • (Optional) Sparse disk images attached as loop devices and passed through
    into each worker node so Rook/Ceph can consume them as raw block OSDs.
`,
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
