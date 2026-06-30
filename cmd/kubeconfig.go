package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var (
	kubeconfigName     string
	kubeconfigPathOnly bool
)

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Print the cluster's kubeconfig (or its path with --path)",
	Long: `kubeconfig writes the cluster's kubeconfig to stdout so it can be piped or
saved. rooket keeps each cluster's kubeconfig in its own state directory
(~/.local/share/rooket/<name>/kubeconfig) rather than editing ~/.kube/config.

  rooket kubeconfig > /tmp/rook.kubeconfig
  export KUBECONFIG="$(rooket kubeconfig --path)"
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := clusterName(kubeconfigName)
		path, err := kubeconfigPath(name)
		if err != nil {
			return err
		}
		if kubeconfigPathOnly {
			fmt.Println(path)
			return nil
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("no kubeconfig for cluster %q at %s (is it up?)", name, path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	},
}

func init() {
	rootCmd.AddCommand(kubeconfigCmd)
	kubeconfigCmd.Flags().StringVar(&kubeconfigName, "name", "", "cluster name (default: rook clone basename, or rook)")
	kubeconfigCmd.Flags().BoolVar(&kubeconfigPathOnly, "path", false, "print the kubeconfig file path instead of its contents")
}
