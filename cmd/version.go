package cmd

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the rooket version and build information",
	Long: `version prints the rooket module version plus the VCS revision, build time,
and Go toolchain embedded by the Go build. A binary installed with
'go install github.com/jhoblitt/rooket@vX.Y.Z' reports that tag; a from-source
build reports (devel) with the commit it was built from.
`,
	Args: cobra.NoArgs,
	// PersistentPreRunE on the root command probes for a usable container
	// engine, which version has no use for and must not fail on.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	Run: func(cmd *cobra.Command, _ []string) {
		info, _ := debug.ReadBuildInfo()
		fmt.Fprint(cmd.OutOrStdout(), versionString(info))
	},
}

func versionString(info *debug.BuildInfo) string {
	if info == nil {
		return "rooket (unknown)\n"
	}
	version := info.Main.Version
	if version == "" {
		version = "(devel)"
	}
	var revision, buildTime string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			buildTime = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "rooket %s\n", version)
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += " (dirty)"
		}
		fmt.Fprintf(&b, "  commit: %s\n", revision)
	}
	if buildTime != "" {
		fmt.Fprintf(&b, "  built:  %s\n", buildTime)
	}
	fmt.Fprintf(&b, "  go:     %s\n", info.GoVersion)
	return b.String()
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
