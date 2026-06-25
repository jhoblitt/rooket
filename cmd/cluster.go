package cmd

import "github.com/spf13/cobra"

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage kind clusters for Rook development",
}

func init() {
	rootCmd.AddCommand(clusterCmd)
}
