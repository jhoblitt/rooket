package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List rooket clusters (live and state-only)",
	Long: `list shows every cluster rooket knows about: kind clusters live under any
installed engine, plus state directories under ~/.local/share/rooket. A row
with no state dir is a kind cluster rooket did not create (or whose state was
pruned); a row that is not live is teardown debris — 'rooket prune' removes it.
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		live, consulted, failed := liveClusters()
		for _, eng := range failed {
			fmt.Fprintf(os.Stderr, "warning: %s is installed but could not be queried; its clusters may be missing below\n", eng)
		}
		liveUnknown := len(consulted) == 0
		if liveUnknown {
			fmt.Fprintln(os.Stderr, "warning: no queryable container engine; live status unknown")
		}

		hasState := map[string]bool{}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root := filepath.Join(home, ".local", "share", "rooket")
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					hasState[e.Name()] = true
				}
			}
		}
		for n := range live {
			if _, ok := hasState[n]; !ok {
				hasState[n] = false
			}
		}
		if len(hasState) == 0 {
			fmt.Println("no clusters")
			return nil
		}

		names := make([]string, 0, len(hasState))
		for n := range hasState {
			names = append(names, n)
		}
		sort.Strings(names)

		w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tLIVE\tREGISTRY PORT\tSTATE DIR")
		for _, n := range names {
			liveCol := "-"
			if liveUnknown {
				liveCol = "?"
			} else if engs := live[n]; len(engs) > 0 {
				ss := make([]string, len(engs))
				for i, e := range engs {
					ss[i] = e.String()
				}
				liveCol = strings.Join(ss, ",")
			}
			portCol := "-"
			if p := readRegistryPort(n); p != 0 {
				portCol = strconv.Itoa(p)
			}
			dirCol := "-"
			if hasState[n] {
				dirCol = filepath.Join(root, n)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, liveCol, portCol, dirCol)
		}
		return w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
