package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
)

var (
	loadName         string
	loadRegistryPort int
)

var loadCmd = &cobra.Command{
	Use:   "load <image>",
	Short: "Tag and push a local image to the cluster's OCI registry",
	Long: `load makes a locally-available image available inside the kind cluster
by pushing it to the local registry.

The image is re-tagged as localhost:<registry-port>/<path>, where <path> is
the image reference without its registry host, and pushed. For example:

  rooket load rook/ceph:latest
  # pushes as localhost:5001/rook/ceph:latest

  rooket load localhost/rook/ceph:dev
  # pushes as localhost:5001/rook/ceph:dev

load refuses a cluster whose registry is not running; --registry-port skips
that check.

After loading, reference the image in your Rook manifests as:
  localhost:<registry-port>/<path>
`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := clusterName(loadName)
		if err != nil {
			return err
		}
		release, err := LockCluster(name)
		if err != nil {
			return err
		}
		defer release()

		// Without --registry-port only the recorded port names a registry.
		// resolveRegistryPort would fall back to the first free port, which is
		// free because no registry listens there. And a recorded port outlives
		// the registry behind it: a plain 'rooket down' removes the container
		// and keeps the record, and a reboot leaves the container stopped.
		if !cmd.Flags().Changed("registry-port") {
			if readRegistryPort(name) == 0 {
				return fmt.Errorf("no registry for cluster %q (is it up?)", name)
			}
			up, answered, err := registryRunning(name)
			switch {
			case up:
			case len(answered) == 0:
				return fmt.Errorf("could not ask %w whether the registry of cluster %q is running", err, name)
			default:
				names := make([]string, len(answered))
				for i, eng := range answered {
					names[i] = eng.String()
				}
				notRunning := fmt.Errorf("no running registry for cluster %q under %s (is it up?)", name, strings.Join(names, " or "))
				if err != nil {
					return fmt.Errorf("%w; could not ask %w", notRunning, err)
				}
				return notRunning
			}
		}
		port, err := resolveRegistryPort(name, loadRegistryPort, cmd.Flags().Changed("registry-port"))
		if err != nil {
			return err
		}
		loadRegistryPort = port

		src := args[0]

		// Derive the destination tag: strip any host prefix, keep name:tag.
		destBase := imageBasename(src)
		dest := fmt.Sprintf("localhost:%d/%s", loadRegistryPort, destBase)

		run.Printf("==> tagging %s → %s\n", src, dest)
		if err := run.Cmd(containerEngine.String(), "tag", src, dest); err != nil {
			return fmt.Errorf("tag image: %w", err)
		}

		run.Printf("==> pushing %s\n", dest)
		if err := run.Cmd(containerEngine.String(), containerEngine.PushArgs(dest)...); err != nil {
			return fmt.Errorf("push image: %w", err)
		}

		run.Printf("\nImage available inside the cluster as:\n  %s\n", dest)
		return nil
	},
}

// registryRunning reports whether a container engine runs the registry of
// cluster name. It asks every installed engine, as liveClusters does, and this
// run's in any case: load pushes to the registry's published host port, which
// reaches the registry whichever engine runs it, so the cluster need not have
// been made under this run's engine. When none runs it, answered lists the
// engines that said so, and err the ones that could not say, any of which may
// be the one running it. err reads "podman (<failure>) or docker (<failure>)",
// on one line, for a message to name the engines by.
func registryRunning(name string) (up bool, answered []engine.Engine, err error) {
	for _, eng := range []engine.Engine{engine.Podman, engine.Docker} {
		if _, lookErr := exec.LookPath(eng.String()); lookErr != nil && eng != containerEngine {
			continue
		}
		running, askErr := registry.Running(os.Stdout, eng, registry.ContainerName(name))
		if askErr != nil {
			askErr = fmt.Errorf("%s (%w)", eng, askErr)
			if err == nil {
				err = askErr
			} else {
				err = fmt.Errorf("%w or %w", err, askErr)
			}
			continue
		}
		if running {
			return true, nil, nil
		}
		answered = append(answered, eng)
	}
	return false, answered, err
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

	loadCmd.Flags().StringVar(&loadName, "name", "", "cluster name (selects the registry port)")
	loadCmd.Flags().IntVar(&loadRegistryPort, "registry-port", 5001, "host port of the local OCI registry")
}
