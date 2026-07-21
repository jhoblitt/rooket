package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/profileschart"
	"github.com/jhoblitt/rooket/internal/run"
)

const profilesRelease = "rooket-profiles"

func profileSources(cloneDir clone.Dir, active []profiles.Profile) ([]profileschart.Source, error) {
	var out []profileschart.Source

	local, err := cloneDir.Templates()
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		out = append(out, profileschart.Source{Prefix: profiles.Reserved, Files: local})
	}
	for _, p := range active {
		if len(p.Templates) > 0 {
			out = append(out, profileschart.Source{Prefix: p.Name, Files: p.Templates})
		}
	}
	return out, nil
}

// installProfilesChart installs the resources contributed by the clone and the
// active profiles as their own release, so disabling a profile prunes what it
// owned on the next deploy.
func installProfilesChart(rookDir string) error {
	cloneDir := clone.Open(rookDir)
	names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
	if err != nil {
		return err
	}
	active, err := loadProfiles(names)
	if err != nil {
		return err
	}
	sources, err := profileSources(cloneDir, active)
	if err != nil {
		return err
	}

	state, err := stateDirPath(deployName)
	if err != nil {
		return err
	}
	chartDir := filepath.Join(state, "profiles-chart")

	any, err := profileschart.Render(chartDir, profileschart.Context{
		ClusterName:       deployName,
		Namespace:         "rook-ceph",
		OperatorNamespace: "rook-ceph",
		Workers:           deployWorkers,
	}, sources)
	if err != nil {
		return err
	}

	if !any {
		if err := run.CmdWithEnv(deployHelmEnv, "helm",
			"--kube-context", deployKubeContext, "-n", "rook-ceph",
			"uninstall", profilesRelease, "--ignore-not-found"); err != nil {
			return fmt.Errorf("uninstall %s: %w", profilesRelease, err)
		}
		return nil
	}

	run.Printf("==> deploying %s (%d source(s))\n", profilesRelease, len(sources))
	return run.CmdWithEnv(deployHelmEnv, "helm",
		"--kube-context", deployKubeContext, "-n", "rook-ceph",
		"upgrade", "--install", profilesRelease, chartDir)
}
