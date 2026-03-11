package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	loadName         string
	loadRegistryPort int
)

var loadCmd = &cobra.Command{
	Use:   "load <image>",
	Short: "Tag and push a local image to the cluster's OCI registry",
	Long: `load makes a locally-available podman image available inside the kind cluster
by pushing it to the local registry.

The image is re-tagged as localhost:<registry-port>/<basename> and pushed.
For example:

  rooket load rook/ceph:latest
  # pushes as localhost:5001/ceph:latest

  rooket load localhost/rook/ceph:dev
  # pushes as localhost:5001/ceph:dev

After loading, reference the image in your Rook manifests as:
  localhost:<registry-port>/<basename>
`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		src := args[0]

		// Derive the destination tag: strip any host prefix, keep name:tag.
		destBase := imageBasename(src)
		dest := fmt.Sprintf("localhost:%d/%s", loadRegistryPort, destBase)

		fmt.Printf("==> tagging %s → %s\n", src, dest)
		if err := run.Cmd("podman", "tag", src, dest); err != nil {
			return fmt.Errorf("tag image: %w", err)
		}

		fmt.Printf("==> pushing %s\n", dest)
		if err := run.Cmd("podman", "push", "--tls-verify=false", dest); err != nil {
			return fmt.Errorf("push image: %w", err)
		}

		fmt.Printf("\nImage available inside the cluster as:\n  %s\n", dest)
		return nil
	},
}

// imageBasename strips a registry host prefix from an image reference so that
// "quay.io/rook/ceph:latest" becomes "rook/ceph:latest" and
// "localhost/foo:bar" becomes "foo:bar".
func imageBasename(ref string) string {
	// If there is a slash, check whether the first segment looks like a host
	// (contains a dot, colon, or is "localhost").
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 {
		first := parts[0]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			return parts[1]
		}
	}
	return ref
}

func init() {
	rootCmd.AddCommand(loadCmd)

	loadCmd.Flags().StringVar(&loadName, "name", "rook", "kind cluster name (used for context only)")
	loadCmd.Flags().IntVar(&loadRegistryPort, "registry-port", 5001, "host port of the local OCI registry")
}
