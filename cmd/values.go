package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/values"
)

var (
	valuesDir         string
	valuesRookVersion string
	valuesConfigDir   string
	valuesShowLayers  bool
)

var valuesCmd = &cobra.Command{
	Use:   "values",
	Short: "Inspect and edit the Helm values rooket deploys",
	Long: `values manages the layered chart values rooket supplies to the rook charts.

Layers, lowest first: rooket's generated base, the configuration home's
values/ (a named configuration directory, else the rook clone's .rooket),
then each active profile in selection order.
`,
}

var valuesShowCmd = &cobra.Command{
	Use:   "show [chart]",
	Short: "Print the merged values rooket would deploy",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		src, err := valuesSource(cmd)
		if err != nil {
			return err
		}
		charts := allCharts
		if len(args) == 1 {
			c, err := chartName(args[0])
			if err != nil {
				return err
			}
			charts = []string{c}
		}

		names, err := activeProfileNames(src.config, deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		active, err := loadProfiles(names)
		if err != nil {
			return err
		}

		for i, chart := range charts {
			base, err := showBase(chart, src)
			if err != nil {
				return err
			}
			c, err := composeChart(chart, base, src.config, active)
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

// valuesSource resolves the charts and configuration home a values command
// renders for, as a deploy of the cluster in scope would. It writes no record:
// only a deploy changes what a cluster runs.
func valuesSource(cmd *cobra.Command) (rookSource, error) {
	// values has no --name flag of its own; $ROOKET_NAME or an enclosing clone
	// must name the cluster, the same refusal deploy and up apply.
	if cmd.Flags().Changed("rook-version") {
		if err := releasedName(""); err != nil {
			return rookSource{}, err
		}
	}
	rec, _, err := resolveSource(clusterName(""), valuesRookVersion, cmd.Flags().Changed("rook-version"),
		valuesConfigDir, cmd.Flags().Changed("config-dir"))
	if err != nil {
		return rookSource{}, err
	}
	if rec.RookVersion == "" {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return rookSource{}, err
		}
		return rookSource{charts: dir, config: configHome(rec, dir)}, nil
	}
	charts, err := releasedCharts(rec.RookVersion)
	if err != nil {
		return rookSource{}, err
	}
	rookDir := valuesDir
	if rookDir == "" {
		if wd, err := os.Getwd(); err == nil {
			rookDir = findRookRoot(wd)
		}
	} else if rec.ConfigDir == "" {
		// Otherwise rookDir never reaches configHome below (rec.ConfigDir
		// wins), and refusing it would refuse a --dir a --config-dir made
		// irrelevant.
		if err := checkConfigDir(rookDir); err != nil {
			return rookSource{}, err
		}
	}
	return rookSource{charts: charts, config: configHome(rec, rookDir), released: rec.RookVersion}, nil
}

// showBase reproduces the generated layer without contacting the registry or
// an iSCSI session: show runs against a cluster that may not exist, so the
// image digest and resolved device paths are deliberately absent. The worker
// count is the cluster's recorded one, which is what a deploy would use.
func showBase(chart string, src rookSource) (map[string]any, error) {
	switch chart {
	case chartOperator:
		if src.released != "" {
			return values.OperatorBase(values.OperatorInput{}), nil
		}
		return values.OperatorBase(values.OperatorInput{
			ImageRepo: fmt.Sprintf("localhost:%d/%s/%s", deployRegistryPort, deployNamespace, deployImageName),
			ImageTag:  "<git ref>",
		}), nil
	case chartCSI:
		return values.CSIBase(), nil
	default:
		shape, _ := readShape(clusterName(""))
		return clusterBase(src.charts, shape.Workers, nil)
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

	// Cobra runs this instead of the root's PersistentPreRunE under `values`,
	// so it accepts the command line itself. cmd.Flags() on a subcommand
	// includes inherited persistent flags, so this sees --with-only wherever it
	// was given under `values`.
	valuesCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := acceptCommandLine(cmd); err != nil {
			return err
		}
		deployWithOnlySet = cmd.Flags().Changed("with-only")
		return nil
	}

	pf := valuesCmd.PersistentFlags()
	pf.StringVar(&valuesDir, "dir", "", "path to the rook source directory (default: $ROOK_DIR, else the rook clone found by walking up from the current directory); for a released Rook version it only locates the configuration home, its .rooket (default: the rook clone enclosing the current directory, if any)")
	pf.StringVar(&valuesRookVersion, "rook-version", "", "render for this released Rook version (default: the cluster's recorded version, else the rook clone)")
	pf.StringVar(&valuesConfigDir, "config-dir", "", "configuration directory laid out like .rooket/ (default: $ROOKET_CONFIG_DIR, else the cluster's recorded one, else the rook clone's .rooket)")
	// Bound to deploy's variables so the profile selection a user previews here
	// is the same one composeChart resolves during a deploy.
	pf.StringArrayVar(&deployWith, "with", nil, "profile to enable, by name or by directory path (./dir), in addition to the configuration home's sticky list (repeatable)")
	pf.StringArrayVar(&deployWithOnly, "with-only", nil, "profile to enable, by name or by directory path (./dir), replacing the configuration home's sticky list (repeatable)")

	valuesShowCmd.Flags().BoolVar(&valuesShowLayers, "layers", false, "annotate each key with the layer that set it")
}
