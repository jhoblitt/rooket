package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/chartcache"
	"github.com/jhoblitt/rooket/internal/clone"
)

func TestUpForwardsValueFlags(t *testing.T) {
	t.Cleanup(func() {
		upWith, upWithOnly = nil, nil
		deployWith, deployWithOnly = nil, nil
		deployWithOnlySet = false
	})

	upWith = []string{"rgw"}
	upWithOnly = []string{"rbd"}

	applyUpValueFlags(true)

	if len(deployWith) != 1 || deployWith[0] != "rgw" {
		t.Errorf("deployWith = %#v", deployWith)
	}
	if len(deployWithOnly) != 1 || deployWithOnly[0] != "rbd" {
		t.Errorf("deployWithOnly = %#v", deployWithOnly)
	}
	if !deployWithOnlySet {
		t.Error("deployWithOnlySet not propagated")
	}
}

func TestCheckProfileSelection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { upWith, upWithOnly = nil, nil })
	rookDir := t.TempDir()
	bad := writePathProfile(t, t.TempDir(), "mytest", map[string]string{"cluster.yaml": "a: 1\n"})

	upWith, upWithOnly = nil, []string{bad}
	if err := checkProfileSelection(clone.Open(rookDir), true); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("err = %v, want the misnamed values file in %s rejected", err, bad)
	}

	upWithOnly = []string{"rbd"}
	if err := checkProfileSelection(clone.Open(rookDir), true); err != nil {
		t.Errorf("a valid selection failed: %v", err)
	}

	upWith, upWithOnly = []string{"./does-not-exist"}, nil
	if err := checkProfileSelection(clone.Open(rookDir), false); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("err = %v, want the missing --with path rejected", err)
	}
}

// TestChartAgnosticValueFlagsRemoved pins the removal of -f/--values and
// --set: both reached every chart, so a key meant for one chart landed in
// all of them. Per-chart values come from a profile directory instead.
func TestChartAgnosticValueFlagsRemoved(t *testing.T) {
	for name, c := range map[string]*cobra.Command{"up": upCmd, "deploy": deployCmd, "values": valuesCmd} {
		// Command.Flag checks local and persistent flags, climbing to parents.
		for _, flag := range []string{"values", "set"} {
			if c.Flag(flag) != nil {
				t.Errorf("%s still defines --%s", name, flag)
			}
		}
		if c.Flags().ShorthandLookup("f") != nil || c.PersistentFlags().ShorthandLookup("f") != nil {
			t.Errorf("%s still defines -f", name)
		}
	}
}

// A released version is deployed from its published images, so there is no
// tree to build — --force-build asks for something that cannot happen.
func TestReleasedBuildConflict(t *testing.T) {
	if err := releasedBuildConflict(true, true); err == nil {
		t.Error("releasedBuildConflict(released, force) = nil, want --force-build refused")
	}
	for _, c := range []struct{ released, force bool }{{true, false}, {false, true}, {false, false}} {
		if err := releasedBuildConflict(c.released, c.force); err != nil {
			t.Errorf("releasedBuildConflict(%v, %v) = %v, want nil", c.released, c.force, err)
		}
	}
}

// isolateUpSource gives upSource a home, chart cache, and working directory
// of its own, with no rook clone anywhere, and restores the up flags these
// tests set.
func isolateUpSource(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("ROOK_DIR", "")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	t.Chdir(t.TempDir())
	stubChartPuller(t)
	dir, force, skipBuild, skipDeploy := upRookDir, upForceBuild, upSkipBuild, upSkipDeploy
	with, withOnly := upWith, upWithOnly
	wait, waitFor := upWait, upWaitTimeout
	t.Cleanup(func() {
		upRookDir, upForceBuild, upSkipBuild, upSkipDeploy = dir, force, skipBuild, skipDeploy
		upWith, upWithOnly = with, withOnly
		upWait, upWaitTimeout = wait, waitFor
	})
	upRookDir, upForceBuild, upSkipBuild, upSkipDeploy = "", false, false, false
	upWith, upWithOnly = nil, nil
	upWait, upWaitTimeout = false, defaultWaitTimeout
}

// parseUpFlags parses args into upCmd's flags as a command line would, and
// resets every flag these tests set when the test ends.
func parseUpFlags(t *testing.T, args ...string) {
	t.Helper()
	if err := upCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"rook-version", "config-dir", "wait", "wait-timeout"} {
			f := upCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
}

func TestUpSourceReleasedNeedsNoClone(t *testing.T) {
	isolateUpSource(t)
	parseUpFlags(t, "--rook-version=v1.20.7")

	src, rookDir, err := upSource(upCmd, "released")
	if err != nil {
		t.Fatalf("upSource: %v", err)
	}
	if rookDir != "" {
		t.Errorf("rookDir = %q, want none: no clone encloses the working directory", rookDir)
	}
	root, _ := chartcache.DefaultRoot()
	if _, err := os.Stat(filepath.Join(root, "v1.20.7", "deploy", "charts", chartCluster, "Chart.yaml")); err != nil {
		t.Errorf("charts not pulled before any cluster work: %v", err)
	}
	// deploy, which up runs with none of its own flags set, takes the version
	// from this record.
	if rec, ok := readSource("released"); !ok || rec != src || rec.RookVersion != "v1.20.7" {
		t.Errorf("record = (%+v, %v), want v1.20.7 recorded", rec, ok)
	}
}

// Unchecked, clone.Open(dir).Ensure() would MkdirAll a typo'd --dir into a
// fresh, empty .rooket tree and up would run with no sticky configuration,
// silently.
func TestUpSourceReleasedRefusesAMissingDir(t *testing.T) {
	isolateUpSource(t)
	missing := filepath.Join(t.TempDir(), "typo")
	upRookDir = missing
	parseUpFlags(t, "--rook-version=v1.20.7")

	if _, _, err := upSource(upCmd, "released"); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("upSource = %v, want the missing --dir refused, naming %s", err, missing)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a typo'd --dir got created rather than refused")
	}
}

// --skip-deploy still pulls the charts before recording the version: recorded
// unpulled, a version that was never published would be retried by every
// later command that does not name another.
func TestUpSourceSkipDeployRecordsNoVersionItCouldNotPull(t *testing.T) {
	isolateUpSource(t)
	pullErr := errors.New("chart not found")
	prev := chartPuller
	chartPuller = func([]string) chartcache.Puller {
		return func(dir, chart, version string) error { return pullErr }
	}
	t.Cleanup(func() { chartPuller = prev })
	upSkipDeploy = true
	parseUpFlags(t, "--rook-version=v9.9.9")

	if _, _, err := upSource(upCmd, "released"); !errors.Is(err, pullErr) {
		t.Fatalf("upSource = %v, want the pull's error", err)
	}
	if rec, ok := readSource("released"); ok {
		t.Errorf("record = %+v, want none for a version that was never pulled", rec)
	}
}

// A recorded released version refuses --force-build as the flag does:
// neither leaves a tree to build.
func TestUpSourceRefusesForceBuildForARecordedRelease(t *testing.T) {
	isolateUpSource(t)
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}
	upForceBuild = true

	if _, _, err := upSource(upCmd, "released"); err == nil || !strings.Contains(err.Error(), "--force-build") {
		t.Fatalf("upSource = %v, want --force-build refused", err)
	}
}

// The sticky profile list is read from the configuration home, not a clone,
// and a bad one stops up before it records a version it never deployed.
func TestUpSourceChecksProfilesInTheConfigHomeBeforeRecording(t *testing.T) {
	isolateUpSource(t)
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "config.yaml"), []byte("profiles: [typo]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parseUpFlags(t, "--rook-version=v1.20.7", "--config-dir="+config)

	if _, _, err := upSource(upCmd, "released"); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("upSource = %v, want the configuration home's unknown profile refused", err)
	}
	if rec, ok := readSource("released"); ok {
		t.Errorf("record = %+v, want none for an up refused on its profiles", rec)
	}
}

// Released Rook inside a clone builds nothing from it, but takes its .rooket
// as the configuration home, as deploy does; --dir names that clone instead.
func TestUpSourceReleasedLocatesTheConfigurationClone(t *testing.T) {
	isolateUpSource(t)
	enclosing := t.TempDir()
	writeGoMod(t, enclosing, rookModulePath)
	t.Chdir(enclosing)
	parseUpFlags(t, "--rook-version=v1.20.7")

	if _, rookDir, err := upSource(upCmd, "released"); err != nil || eval(t, rookDir) != eval(t, enclosing) {
		t.Errorf("upSource = (%q, %v), want the enclosing clone %s", rookDir, err, enclosing)
	}
	named := t.TempDir()
	upRookDir = named
	if _, rookDir, err := upSource(upCmd, "released"); err != nil || rookDir != named {
		t.Errorf("with --dir, upSource = (%q, %v), want %s", rookDir, err, named)
	}
}

func TestUpSourceCloneMode(t *testing.T) {
	isolateUpSource(t)
	if _, _, err := upSource(upCmd, "clone"); err == nil || !strings.Contains(err.Error(), "rook source tree") {
		t.Errorf("upSource with no clone = %v, want the missing clone refused", err)
	}

	rookDir := t.TempDir()
	upRookDir = rookDir
	src, got, err := upSource(upCmd, "clone")
	if err != nil {
		t.Fatalf("upSource: %v", err)
	}
	if got != rookDir || src != (clusterSource{}) {
		t.Errorf("upSource = (%+v, %q), want the clone %s and no released version", src, got, rookDir)
	}
	if rec, ok := readSource("clone"); ok {
		t.Errorf("record = %+v, want none: a clone-mode up naming nothing records nothing", rec)
	}
}

// A cluster up builds from a clone that --dir or $ROOK_DIR names belongs to
// that clone, wherever up ran: prune keeps it parked while that clone exists,
// and not after. Recording none from outside a clone, or the working
// directory's clone from inside another, had prune sweep a cluster that
// 'rooket down' parked, disk images and iSCSI targets included.
func TestUpSourceRecordsTheNamedClone(t *testing.T) {
	for _, c := range []struct {
		name  string
		point func(t *testing.T, built string)
	}{
		{"--dir from outside any clone", func(t *testing.T, built string) {
			upRookDir = built
		}},
		{"relative --dir", func(t *testing.T, built string) {
			t.Chdir(filepath.Dir(built))
			upRookDir = filepath.Base(built)
		}},
		{"$ROOK_DIR from inside another clone", func(t *testing.T, built string) {
			other := t.TempDir()
			writeGoMod(t, other, rookModulePath)
			t.Chdir(other)
			t.Setenv("ROOK_DIR", built)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateUpSource(t)
			built := filepath.Join(t.TempDir(), "rook")
			if err := os.Mkdir(built, 0o755); err != nil {
				t.Fatal(err)
			}
			writeGoMod(t, built, rookModulePath)
			c.point(t, built)

			if _, _, err := upSource(upCmd, "parked"); err != nil {
				t.Fatalf("upSource: %v", err)
			}
			// Cluster create writes the state dir next, from where up ran.
			stateDir, err := ensureStateDir("parked")
			if err != nil {
				t.Fatal(err)
			}
			// prune runs from anywhere, so a relative record would dangle.
			t.Chdir(t.TempDir())
			if ownerGone(stateDir) {
				t.Fatal("ownerGone = true while the clone up built from exists, want the cluster parked")
			}
			if err := os.RemoveAll(built); err != nil {
				t.Fatal(err)
			}
			if !ownerGone(stateDir) {
				t.Error("ownerGone = false once the clone up built from is gone, want the cluster abandoned")
			}
		})
	}
}

// up's refusal of a cluster it cannot name comes from useClusterOrRookDir, at
// the top of its RunE, ahead of any cluster work; this wires it into the
// command a user actually runs. Given --rook-version, up reads no rook tree,
// so a clone that --dir or $ROOK_DIR points at does not name its cluster.
//
// isolateUpSource keeps a regressed refusal off the developer's real
// ~/.local/share/rooket and stubs the chart puller; upForceBuild with
// --rook-version is a second, independent tripwire so a regressed refusal
// still stops — inside upSource, via releasedBuildConflict, before
// releasedCharts or writeSource run — ahead of block setup (pkexec) and kind
// create, which isolating HOME alone would not stop.
func TestUpRunERefusesAnUnnamedCluster(t *testing.T) {
	for _, pointed := range []string{"nothing", "ROOK_DIR", "dir"} {
		t.Run(pointed, func(t *testing.T) {
			isolateUpSource(t)
			t.Setenv("ROOKET_NAME", "")
			t.Setenv("KUBECONFIG", "") // useCluster sets this outside testing's tracking
			name := upName
			t.Cleanup(func() { upName = name })
			upName = ""
			clone := t.TempDir()
			writeGoMod(t, clone, rookModulePath)
			switch pointed {
			case "ROOK_DIR":
				t.Setenv("ROOK_DIR", clone)
			case "dir":
				upRookDir = clone
			}
			parseUpFlags(t, "--rook-version=v1.20.7")
			upForceBuild = true

			assertRefused(t, upCmd.RunE(upCmd, nil), "--name")
		})
	}
}
