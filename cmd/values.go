package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/values"
)

var (
	valuesDir        string
	valuesShowLayers bool
)

var valuesCmd = &cobra.Command{
	Use:   "values",
	Short: "Inspect and edit the Helm values rooket deploys",
	Long: `values manages the layered chart values rooket supplies to the rook charts.

Layers, lowest first: rooket's generated base, the clone's .rooket/values/,
each active profile in selection order, then any -f files.
`,
}

var valuesShowCmd = &cobra.Command{
	Use:   "show [chart]",
	Short: "Print the merged values rooket would deploy",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		charts := []string{chartOperator, chartCluster, chartCSI}
		if len(args) == 1 {
			c, err := chartName(args[0])
			if err != nil {
				return err
			}
			charts = []string{c}
		}

		cloneDir := clone.Open(dir)
		names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		active, err := loadProfiles(names)
		if err != nil {
			return err
		}

		for i, chart := range charts {
			c, err := composeChart(chart, showBase(chart), cloneDir, active, deployValueFiles)
			if err != nil {
				return err
			}
			out, err := renderShow(c, valuesShowLayers)
			if err != nil {
				return err
			}
			if i > 0 {
				fmt.Println("---")
			}
			fmt.Printf("# %s\n%s", chart, out)
		}
		return nil
	},
}

// showBase reproduces the generated layer without contacting the registry or
// an iSCSI session: show runs against a cluster that may not exist, so the
// image digest and resolved device paths are deliberately absent.
func showBase(chart string) map[string]any {
	switch chart {
	case chartOperator:
		return values.OperatorBase(values.OperatorInput{
			ImageRepo: fmt.Sprintf("localhost:%d/%s/%s", deployRegistryPort, deployNamespace, deployImageName),
			ImageTag:  "<git ref>",
		})
	case chartCSI:
		return values.CSIBase()
	default:
		return values.ClusterBase(values.ClusterInput{OperatorNamespace: "rook-ceph"})
	}
}

func renderShow(c composed, withLayers bool) (string, error) {
	data, err := values.Encode(c.Merged)
	if err != nil {
		return "", err
	}
	if !withLayers {
		return string(data), nil
	}
	paths := make([]string, 0, len(c.Provenance))
	for p := range c.Provenance {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.Write(data)
	b.WriteString("\n# layers\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "#   %-60s %s\n", p, c.Provenance[p])
	}
	return b.String(), nil
}

func init() {
	rootCmd.AddCommand(valuesCmd)
	valuesCmd.AddCommand(valuesShowCmd)

	// cmd.Flags() on a subcommand includes inherited persistent flags, so this
	// sees --with-only wherever it was given under `values`.
	valuesCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		deployWithOnlySet = cmd.Flags().Changed("with-only")
		return nil
	}

	pf := valuesCmd.PersistentFlags()
	pf.StringVar(&valuesDir, "dir", "", "path to the rook source directory (default: current directory)")
	// Bound to deploy's variables so the profile selection a user previews here
	// is the same one composeChart resolves during a deploy.
	pf.StringArrayVar(&deployWith, "with", nil, "profile to enable, in addition to the clone's sticky list (repeatable)")
	pf.StringArrayVar(&deployWithOnly, "with-only", nil, "profile to enable, replacing the clone's sticky list (repeatable)")
	pf.StringArrayVarP(&deployValueFiles, "values", "f", nil, "additional values file, applied above profiles (repeatable)")

	valuesShowCmd.Flags().BoolVar(&valuesShowLayers, "layers", false, "annotate each key with the layer that set it")
}
