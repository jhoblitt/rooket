package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	if err := checkProfileSelection(rookDir, true); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("err = %v, want the misnamed values file in %s rejected", err, bad)
	}

	upWithOnly = []string{"rbd"}
	if err := checkProfileSelection(rookDir, true); err != nil {
		t.Errorf("a valid selection failed: %v", err)
	}

	upWith, upWithOnly = []string{"./does-not-exist"}, nil
	if err := checkProfileSelection(rookDir, false); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
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
