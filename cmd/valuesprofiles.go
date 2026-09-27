package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/run"
)

var valuesProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List the available profiles, marking the ones this clone enables",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		userDir, err := userProfileDir()
		if err != nil {
			return err
		}
		active, err := activeProfileNames(clone.Open(dir), deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		all, err := listedProfiles(userDir, active)
		if err != nil {
			return err
		}
		fmt.Print(renderProfileList(all, active))
		return nil
	},
}

// listedProfiles returns every discoverable profile plus each active path
// profile, which nothing discovers and so is listed only when selected.
func listedProfiles(userDir string, active []string) ([]profiles.Profile, error) {
	all, err := profiles.List(userDir)
	if err != nil {
		return nil, err
	}
	for _, n := range active {
		if !isProfilePath(n) {
			continue
		}
		p, err := profiles.LoadDir(n)
		if err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	return all, nil
}

var valuesProfilesForkCmd = &cobra.Command{
	Use:   "fork <profile>",
	Short: "Copy a built-in profile into ~/.config/rooket/profiles to edit",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		userDir, err := userProfileDir()
		if err != nil {
			return err
		}
		dst, err := profiles.Fork(userDir, args[0])
		if err != nil {
			return err
		}
		run.Printf("==> forked %s to %s\n", args[0], dst)
		return nil
	},
}

func renderProfileList(all []profiles.Profile, active []string) string {
	on := make(map[string]bool, len(active))
	for _, n := range active {
		on[n] = true
	}
	var b strings.Builder
	for _, p := range all {
		mark := " "
		if on[p.Label()] {
			mark = "*"
		}
		origin := "user"
		switch {
		case p.Path != "":
			origin = "path"
		case p.BuiltIn:
			origin = "built-in"
		}
		fmt.Fprintf(&b, " %s %-12s (%-8s) %s\n", mark, p.Label(), origin, p.Description)
	}
	return b.String()
}

func init() {
	valuesCmd.AddCommand(valuesProfilesCmd)
	valuesProfilesCmd.AddCommand(valuesProfilesForkCmd)
}
