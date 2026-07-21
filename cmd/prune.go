package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhoblitt/rooket/internal/run"
	"github.com/spf13/cobra"
)

var (
	pruneForce  bool
	pruneDryRun bool
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove state directories of clusters that no longer exist",
	Long: `prune deletes ~/.local/share/rooket/<name> directories whose kind cluster is
no longer running — e.g. a clone removed without 'rooket down'. The backing
disk images in those directories are deleted with them; any iSCSI targets an
orphan still has configured are left behind ('rooket down --all --delete-disks'
is the everything-at-once teardown that removes those too).

  rooket prune --dry-run   # list what would be removed
  rooket prune             # prompt, then remove
  rooket prune --force     # remove without prompting
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, stateNames, err := stateDirNames()
		if err != nil {
			return err
		}

		// Query every installed engine, not just the session's resolved one: a
		// cluster living under the other engine must not be pruned as orphaned.
		live, consulted, failed := liveClusters()
		if len(consulted) == 0 {
			return fmt.Errorf("cannot determine live clusters (no queryable container engine); not pruning")
		}
		for _, eng := range failed {
			run.Printf("warning: %s is installed but could not be queried; "+
				"its clusters (if any) would be misread as orphaned — not pruning\n", eng)
		}
		if len(failed) > 0 {
			return fmt.Errorf("refusing to prune with an unqueryable engine present")
		}

		var orphans []string
		for _, n := range stateNames {
			if _, ok := live[n]; !ok {
				orphans = append(orphans, n)
			}
		}
		if len(orphans) == 0 {
			run.Printf("nothing to prune\n")
			return nil
		}

		engNames := make([]string, len(consulted))
		for i, eng := range consulted {
			engNames[i] = eng.String()
		}
		run.Printf("Orphaned cluster state directories (no live kind cluster under %s):\n",
			strings.Join(engNames, " or "))
		for _, o := range orphans {
			run.Printf("  %s\n", filepath.Join(root, o))
		}
		if pruneDryRun {
			return nil
		}
		if !pruneForce {
			run.Printf("Remove these %d directories? [y/N] ", len(orphans))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				run.Printf("aborted\n")
				return nil
			}
		}
		for _, o := range orphans {
			p := filepath.Join(root, o)
			if err := os.RemoveAll(p); err != nil {
				run.Printf("warning: remove %s: %v\n", p, err)
			} else {
				run.Printf("removed %s\n", p)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pruneCmd)
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "list orphaned directories without removing them")
	pruneCmd.Flags().BoolVar(&pruneForce, "force", false, "remove without prompting")
}
