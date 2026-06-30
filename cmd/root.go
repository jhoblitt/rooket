package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/engine"
)

// engineFlag is the raw --engine value; containerEngine is the validated engine
// resolved once in PersistentPreRunE and read by every subcommand.
var (
	engineFlag      string
	containerEngine engine.Engine
)

var rootCmd = &cobra.Command{
	Use:   "rooket",
	Short: "Spin up a Rook development cluster using kind and a local OCI registry",
	Long: `rooket bootstraps a Kubernetes-in-Docker (kind) cluster pre-configured for
Rook development and testing. It drives podman or docker (select with --engine
or $ROOKET_ENGINE) to create:

  • A local OCI registry (default port 5001) that every cluster node is
    configured to pull from, so you can push locally-built Rook images with:
    <engine> push localhost:5001/rook/ceph:dev

  • A multi-node kind cluster whose containerd is wired to the local registry.

  • (Optional) iSCSI-backed block devices passed through into each worker node
    so Rook/Ceph can consume them as raw block OSDs.
`,
	PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
		eng, err := engine.Parse(engineFlag)
		if err != nil {
			return err
		}
		containerEngine = eng
		// rooket drives a single engine per invocation. Export it so every child
		// process selects the matching backend: kind via KIND_EXPERIMENTAL_PROVIDER
		// (covers `kind get/create/delete`), and rook's `make` via DOCKERCMD so the
		// image lands in the same store rooket then tags and pushes from.
		_ = os.Setenv("KIND_EXPERIMENTAL_PROVIDER", eng.String())
		_ = os.Setenv("DOCKERCMD", eng.String())
		return nil
	},
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// The default honors $ROOKET_ENGINE so it can be set once; an explicit
	// --engine still overrides it.
	defaultEngine := os.Getenv(engine.EnvVar)
	if defaultEngine == "" {
		defaultEngine = engine.Default.String()
	}
	rootCmd.PersistentFlags().StringVar(&engineFlag, "engine", defaultEngine,
		"container engine to drive: podman or docker (also via $ROOKET_ENGINE)")
}
