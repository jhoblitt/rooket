package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// selectNothing leaves a command nothing that names a cluster — no
// $ROOKET_NAME or $ROOK_DIR, and a working directory in no rook clone; the
// caller leaves the command's --name and --dir unset — and gives it a HOME of
// its own. It returns that HOME's state root.
func selectNothing(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ROOKET_NAME", "")
	t.Setenv("ROOK_DIR", "")
	t.Setenv("KUBECONFIG", "untouched")
	t.Chdir(t.TempDir())
	return filepath.Join(home, ".local", "share", "rooket")
}

// stateFiles lists every path under the state root. Every cluster's lock and
// records live there, so a command refused before it starts leaves the list
// as it found it.
func stateFiles(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return treeFiles(t, root)
}

// assertRefused fails the test unless err refuses to guess a cluster,
// offering $ROOKET_NAME, a rook clone, and each of accepts — the command's own
// ways among --name, --dir, and $ROOK_DIR — and none of those it lacks.
func assertRefused(t *testing.T, err error, accepts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want the command refused for naming no cluster")
	}
	msg := err.Error()
	ways := []string{"$ROOKET_NAME", "rook clone"}
	for _, way := range []string{"--name", "--dir", "$ROOK_DIR"} {
		if slices.Contains(accepts, way) {
			ways = append(ways, way)
		} else if strings.Contains(msg, way) {
			t.Errorf("refusal %q offers %s, which the command does not take", msg, way)
		}
	}
	if slices.Contains(accepts, "--name") {
		ways = append(ways, "--name rook")
	} else {
		ways = append(ways, "ROOKET_NAME=rook")
	}
	for _, way := range ways {
		if !strings.Contains(msg, way) {
			t.Errorf("refusal %q does not offer %s", msg, way)
		}
	}
}

// A cluster named "rook" is up here, so a command that fell back to that name
// would reach it: kubectl and helm would run.
func TestPassthroughCommandsRefuseAnUnnamedCluster(t *testing.T) {
	for _, tc := range []struct {
		bin string
		run func() error
	}{
		{"kubectl", func() error { return kubectlCmd.RunE(kubectlCmd, []string{"get", "nodes"}) }},
		{"helm", func() error { return helmCmd.RunE(helmCmd, []string{"list"}) }},
	} {
		t.Run(tc.bin, func(t *testing.T) {
			root := selectNothing(t)
			writeKubeconfig(t, "rook")
			// The stand-in is all that is on PATH, so a regression cannot
			// reach the real tool; it only records that it ran.
			bin, ran := t.TempDir(), filepath.Join(t.TempDir(), "ran")
			if err := os.WriteFile(filepath.Join(bin, tc.bin), []byte("#!/bin/sh\n: > '"+ran+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			before := stateFiles(t, root)

			assertRefused(t, tc.run())
			if _, err := os.Stat(ran); err == nil {
				t.Errorf("%s ran", tc.bin)
			}
			if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
				t.Errorf("KUBECONFIG = %q, want it untouched", kc)
			}
			if after := stateFiles(t, root); !slices.Equal(after, before) {
				t.Errorf("state root holds %v, want it as it was: %v", after, before)
			}
		})
	}
}

// down is where a guessed name costs most: it would delete someone else's
// cluster, and with --delete-disks its disks.
func TestDownRefusesAnUnnamedCluster(t *testing.T) {
	root := selectNothing(t)
	// Were down to go ahead, it would find no kind, engine, or iSCSI tool.
	t.Setenv("PATH", t.TempDir())
	prev := downName
	t.Cleanup(func() { downName = prev })
	downName = ""

	assertRefused(t, downCmd.RunE(downCmd, nil), "--name")
	if files := stateFiles(t, root); len(files) != 0 {
		t.Errorf("state root holds %v, want nothing: no lock taken, no state written", files)
	}
}

func TestWaitRefusesAnUnnamedCluster(t *testing.T) {
	selectNothing(t)
	setWaitFlags(t, "")
	writeKubeconfig(t, "rook")
	h := newWaitHarness(readyCluster())
	stubWaitKubectl(t, h)

	assertRefused(t, waitCmd.RunE(waitCmd, nil), "--name")
	if len(h.calls) != 0 {
		t.Errorf("ran %q", h.calls)
	}
	if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
		t.Errorf("KUBECONFIG = %q, want it untouched", kc)
	}
}

func TestCephConfigRefusesAnUnnamedCluster(t *testing.T) {
	selectNothing(t)
	out := t.TempDir()
	setCephConfigFlags(t, "", out)
	writeKubeconfig(t, "rook")
	calls := stubKubectl(t, hostNetworked(1))

	assertRefused(t, cephConfigCmd.RunE(cephConfigCmd, nil), "--name")
	if len(*calls) != 0 {
		t.Errorf("ran %q", *calls)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("--out holds %d entries, want none written", len(entries))
	}
	if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
		t.Errorf("KUBECONFIG = %q, want it untouched", kc)
	}
}

// values renders what a deploy of the cluster in scope would, from that
// cluster's record; with no cluster in scope it would read the "rook" one's.
func TestValuesRefuseAnUnnamedCluster(t *testing.T) {
	t.Run("--dir at no clone", func(t *testing.T) {
		selectNothing(t)
		prev := valuesDir
		t.Cleanup(func() { valuesDir = prev })
		valuesDir = t.TempDir()

		assertRefused(t, valuesShowCmd.RunE(valuesShowCmd, nil), "--dir", "$ROOK_DIR")
	})
	// Given --rook-version, values reads no rook tree and --dir only locates
	// its configuration home, so a clone either points at does not name the
	// cluster.
	for _, pointed := range []string{"nothing", "ROOK_DIR", "dir"} {
		t.Run("released, pointed at "+pointed, func(t *testing.T) {
			selectNothing(t)
			stubChartPuller(t)
			clone := t.TempDir()
			writeGoMod(t, clone, rookModulePath)
			prev := valuesDir
			t.Cleanup(func() { valuesDir = prev })
			valuesDir = ""
			switch pointed {
			case "ROOK_DIR":
				t.Setenv("ROOK_DIR", clone)
			case "dir":
				valuesDir = clone
			}
			parseValuesFlags(t, "--rook-version=v1.20.7")

			assertRefused(t, valuesShowCmd.RunE(valuesShowCmd, nil))
		})
	}
}

// Commands that need no cluster, or find it from the clone they run in or are
// pointed at with --dir or $ROOK_DIR, must not start requiring one to be named.
func TestCommandsNeedingNoNameRunWithNothingSet(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		selectNothing(t)
		t.Setenv("PATH", t.TempDir())
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Errorf("list = %v, want nil", err)
		}
	})
	t.Run("values in a clone", func(t *testing.T) {
		selectNothing(t)
		t.Setenv("ROOKET_CONFIG_DIR", "")
		clone := t.TempDir()
		writeGoMod(t, clone, rookModulePath)
		t.Chdir(clone)
		prev := valuesDir
		t.Cleanup(func() { valuesDir = prev })
		valuesDir = ""

		src, err := valuesSource(valuesShowCmd)
		if err != nil {
			t.Fatalf("valuesSource = %v, want the clone to name the cluster", err)
		}
		if src.charts != clone {
			t.Errorf("charts = %q, want the clone %s", src.charts, clone)
		}
	})
	// The configuration directory recorded for each clone's cluster shows
	// whose record values read.
	t.Run("values pointed at a clone", func(t *testing.T) {
		mkClone := func(t *testing.T) (string, string) {
			clone, cfg := t.TempDir(), t.TempDir()
			writeGoMod(t, clone, rookModulePath)
			if err := writeSource(encodePath(clone), clusterSource{ConfigDir: cfg}); err != nil {
				t.Fatal(err)
			}
			return clone, filepath.Join(cfg, "values", chartCluster+".yaml")
		}
		for _, tc := range []struct {
			name                     string
			byDir, byEnv, otherInEnv bool
		}{
			{name: "by --dir", byDir: true},
			{name: "by ROOK_DIR", byEnv: true},
			{name: "by --dir over ROOK_DIR", byDir: true, otherInEnv: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				selectNothing(t)
				t.Setenv("ROOKET_CONFIG_DIR", "")
				clone, want := mkClone(t)
				prev := valuesDir
				t.Cleanup(func() { valuesDir = prev })
				valuesDir = ""
				if tc.byDir {
					valuesDir = clone
				}
				if tc.byEnv {
					t.Setenv("ROOK_DIR", clone)
				}
				if tc.otherInEnv {
					other, _ := mkClone(t)
					t.Setenv("ROOK_DIR", other)
				}

				src, err := valuesSource(valuesShowCmd)
				if err != nil {
					t.Fatalf("valuesSource = %v, want the clone to name the cluster", err)
				}
				if got := src.config.ValuesPath(chartCluster); got != want {
					t.Errorf("config ValuesPath = %q, want %q from the record of cluster %q", got, want, encodePath(clone))
				}
			})
		}
	})
	t.Run("deploy given --dir at a clone", func(t *testing.T) {
		selectNothing(t)
		clone := t.TempDir()
		writeGoMod(t, clone, rookModulePath)
		want := encodePath(clone)
		isolateDeploySetup(t, want, clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
		deployName, deployDir = "", clone

		_, _, release, err := deploySetup(deployCmd)
		if err != nil {
			t.Fatalf("deploySetup = %v, want --dir's clone to name the cluster", err)
		}
		defer release()
		if deployName != want {
			t.Errorf("deployName = %q, want %q", deployName, want)
		}
		if deployWorkers != 1 {
			t.Errorf("deployWorkers = %d, want the 1 recorded for cluster %q", deployWorkers, want)
		}
	})
}
