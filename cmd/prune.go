package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
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
disk images in those directories are deleted with them.

  rooket prune --dry-run   # list what would be removed
  rooket prune             # prompt, then remove
  rooket prune --force     # remove without prompting
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root := filepath.Join(home, ".local", "share", "rooket")
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("nothing to prune")
				return nil
			}
			return err
		}

		names, err := cluster.List()
		if err != nil {
			return fmt.Errorf("list kind clusters: %w", err)
		}
		live := make(map[string]struct{}, len(names))
		for _, n := range names {
			live[n] = struct{}{}
		}

		var orphans []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := live[e.Name()]; !ok {
				orphans = append(orphans, e.Name())
			}
		}
		if len(orphans) == 0 {
			fmt.Println("nothing to prune")
			return nil
		}

		fmt.Println("Orphaned cluster state directories (no live kind cluster):")
		for _, o := range orphans {
			fmt.Printf("  %s\n", filepath.Join(root, o))
		}
		if pruneDryRun {
			return nil
		}
		if !pruneForce {
			fmt.Printf("Remove these %d directories? [y/N] ", len(orphans))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				fmt.Println("aborted")
				return nil
			}
		}
		for _, o := range orphans {
			p := filepath.Join(root, o)
			if err := os.RemoveAll(p); err != nil {
				fmt.Printf("warning: remove %s: %v\n", p, err)
			} else {
				fmt.Printf("removed %s\n", p)
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
