package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/chartcache"
)

func writeChartYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Chart.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCephCsiOperatorDep(t *testing.T) {
	t.Run("v1.20+/master style: installCsiOperator condition", func(t *testing.T) {
		p := writeChartYAML(t, `apiVersion: v2
name: rook-ceph
version: 0.0.1
dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
  - name: ceph-csi-operator
    version: 1.0.1
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.installCsiOperator
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "1.0.1" || c != "csi.installCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (1.0.1, csi.installCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("v1.18/v1.19 style: rookUseCsiOperator condition", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: ceph-csi-operator
    version: "0.6.0"
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.rookUseCsiOperator
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "0.6.0" || c != "csi.rookUseCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (0.6.0, csi.rookUseCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("no ceph-csi-operator dependency", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "" || c != "" {
			t.Fatalf("got (%q, %q, %v), want empty fields and nil error", v, c, err)
		}
	})

	t.Run("other dependency's fields not picked up", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: ceph-csi-operator
    version: 1.2.3
    condition: csi.installCsiOperator
  - name: something-else
    version: "9.9.9"
    condition: other.flag
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "1.2.3" || c != "csi.installCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (1.2.3, csi.installCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, _, err := cephCsiOperatorDep(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("got nil error for missing file")
		}
	})
}

// A released chart's dependencies come unpacked, so the archive check would
// count them missing and run helm inside the chart cache entry that every
// cluster deploying that version shares.
func TestRestoreChartDepsLeavesAReleasedChartAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Were the restore to run, it would find no helm here rather than fetch.
	t.Setenv("PATH", t.TempDir())
	prev := deployName
	t.Cleanup(func() { deployName = prev })
	deployName = "released"

	entry := t.TempDir()
	chartDir := filepath.Join(entry, "deploy", "charts", chartOperator)
	if err := os.MkdirAll(filepath.Join(chartDir, "charts", "ceph-csi-operator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(`apiVersion: v2
name: rook-ceph
version: v1.20.7
dependencies:
  - name: ceph-csi-operator
    version: 1.0.1
    repository: https://ceph.github.io/ceph-csi-operator
`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeFiles(t, chartDir)

	if err := restoreChartDeps(rookSource{charts: entry, released: "v1.20.7"}, chartOperator); err != nil {
		t.Fatalf("restoreChartDeps = %v, want nil: a released chart has nothing to restore", err)
	}
	if after := treeFiles(t, chartDir); !slices.Equal(after, before) {
		t.Errorf("chart directory holds %v after the restore, want it untouched: %v", after, before)
	}
}

// treeFiles lists every path under dir, relative to it.
func treeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClusterStorageNodesFromDisks(t *testing.T) {
	got, err := clusterStorageNodes("c", 2, 1, func(iqn string) (string, error) {
		return "/dev/disk/by-path/" + iqn, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Name != "c-worker" || got[1].Name != "c-worker2" {
		t.Errorf("node names = %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[0].Devices) != 1 {
		t.Errorf("devices = %#v", got[0].Devices)
	}
}

func TestClusterStorageNodesUnresolvedDeviceErrors(t *testing.T) {
	resolveErr := errors.New("boom")
	got, err := clusterStorageNodes("c", 2, 1, func(iqn string) (string, error) {
		return "", resolveErr
	})
	if err == nil {
		t.Fatal("got nil error, want an error from the unresolved device")
	}
	if !errors.Is(err, resolveErr) {
		t.Errorf("error %v does not wrap %v", err, resolveErr)
	}
	if got != nil {
		t.Errorf("got %#v nodes, want nil", got)
	}
}

func TestApplyWithOnlyGuardPreservesUpForwardedValue(t *testing.T) {
	t.Cleanup(func() { deployWithOnlySet = false })

	// Simulate 'rooket up' having already forwarded --with-only before
	// calling deployCmd.RunE, on a path where deployCmd's own flag is unset.
	deployWithOnlySet = true
	applyWithOnlyGuard(false)
	if !deployWithOnlySet {
		t.Error("deployWithOnlySet was cleared though deploy's own --with-only flag was not changed")
	}
}

// isolateDeploySetup gives deploySetup a throwaway home, rook clone, and
// cluster recorded with shape, and restores the package state it writes.
func isolateDeploySetup(t *testing.T, cluster string, shape clusterShape) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	name, dir, ctx, iqn := deployName, deployDir, deployKubeContext, deployIQNDate
	port, workers, disks, env := deployRegistryPort, deployWorkers, deployDiskCount, deployHelmEnv
	version, config, with := deployRookVersion, deployConfigDir, deployWith
	t.Cleanup(func() {
		deployName, deployDir, deployKubeContext, deployIQNDate = name, dir, ctx, iqn
		deployRegistryPort, deployWorkers, deployDiskCount, deployHelmEnv = port, workers, disks, env
		deployRookVersion, deployConfigDir, deployWith = version, config, with
	})

	if err := writeShape(cluster, shape); err != nil {
		t.Fatal(err)
	}
	if err := writeRegistryPort(cluster, 5001); err != nil {
		t.Fatal(err)
	}
	deployName, deployDir, deployKubeContext = cluster, t.TempDir(), ""
	deployWorkers, deployDiskCount, deployIQNDate = 3, 1, "2003-01"
	deployRookVersion, deployConfigDir, deployWith = "", "", nil
}

// After 'rooket up --workers 1', a plain 'rooket deploy' pinned OSDs for three
// workers and waited on iSCSI disks that were never created. 'up' reaches
// deploySetup the same way this does: deployCmd with none of its flags set.
func TestDeploySetupUsesTheRecordedShape(t *testing.T) {
	isolateDeploySetup(t, "single", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})

	_, _, release, err := deploySetup(deployCmd)
	if err != nil {
		t.Fatalf("deploySetup: %v", err)
	}
	defer release()
	if deployWorkers != 1 {
		t.Errorf("deployWorkers = %d, want the recorded 1 rather than the flag default", deployWorkers)
	}
}

func TestDeploySetupRejectsAContradictingWorkersFlag(t *testing.T) {
	isolateDeploySetup(t, "single", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	parseDeployFlags(t, "--workers=3")

	if _, _, _, err := deploySetup(deployCmd); err == nil {
		t.Fatal("deploySetup = nil error, want --workers 3 refused for a cluster created with 1")
	}
}

// parseDeployFlags parses args into deployCmd's flags as a command line would,
// and resets every flag these tests set when the test ends, so none leaks
// into the next.
func parseDeployFlags(t *testing.T, args ...string) {
	t.Helper()
	if err := deployCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"rook-version", "config-dir", "workers"} {
			f := deployCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
}

func TestDeploySetupReleasedUsesTheChartCacheAndRecordsIt(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	parseDeployFlags(t, "--rook-version=v1.20.7")

	src, _, release, err := deploySetup(deployCmd)
	if err != nil {
		t.Fatalf("deploySetup: %v", err)
	}
	defer release()
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want v1.20.7", src.released)
	}
	if _, err := os.Stat(filepath.Join(src.charts, "deploy", "charts", chartCluster, "Chart.yaml")); err != nil {
		t.Errorf("charts %s is not a pulled cache entry: %v", src.charts, err)
	}
	if rec, ok := readSource("released"); !ok || rec.RookVersion != "v1.20.7" {
		t.Errorf("record = (%+v, %v), want v1.20.7 recorded for later commands", rec, ok)
	}
}

// Recorded before the pull, a version that was never published would be
// retried by every later deploy that does not name another.
func TestDeploySetupRecordsNoVersionItCouldNotPull(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	pullErr := errors.New("chart not found")
	prev := chartPuller
	chartPuller = func([]string) chartcache.Puller {
		return func(dir, chart, version string) error { return pullErr }
	}
	t.Cleanup(func() { chartPuller = prev })
	parseDeployFlags(t, "--rook-version=v9.9.9")

	if _, _, _, err := deploySetup(deployCmd); !errors.Is(err, pullErr) {
		t.Fatalf("deploySetup = %v, want the pull's error", err)
	}
	if rec, ok := readSource("released"); ok {
		t.Errorf("record = %+v, want none for a version that was never pulled", rec)
	}
}

// A deploy that stops at its profile selection deployed nothing, so the
// record must not claim the cluster runs the version it named.
func TestDeploySetupRecordsNothingWhenAProfileFailsToLoad(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	parseDeployFlags(t, "--rook-version=v1.20.7")
	deployWith = []string{"typo"}

	if _, _, _, err := deploySetup(deployCmd); err == nil {
		t.Fatal("deploySetup = nil error, want the unknown profile refused")
	}
	if rec, ok := readSource("released"); ok {
		t.Errorf("record = %+v, want none for a deploy refused on its profiles", rec)
	}
}

func TestDeploySetupTakesTheReleasedVersionFromTheRecord(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}

	src, _, release, err := deploySetup(deployCmd)
	if err != nil {
		t.Fatalf("deploySetup: %v", err)
	}
	defer release()
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want the recorded v1.20.7", src.released)
	}
}

func TestDeploySetupReleasedRefusesTheFallbackName(t *testing.T) {
	isolateDeploySetup(t, "unused", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	t.Setenv("ROOKET_NAME", "")
	t.Chdir(t.TempDir())
	deployName = ""
	parseDeployFlags(t, "--rook-version=v1.20.7")

	if _, _, _, err := deploySetup(deployCmd); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("deploySetup = %v, want the fallback name refused, pointing at --name", err)
	}
}

// Traced failure: while T1 holds cluster "released"'s lock (recorded at
// v1.20.7), T2's deploySetup for the same cluster with a differing
// --rook-version must be refused before it ever writes — or T1's own deploy
// step, which reads the record with no --rook-version of its own, would pick
// up a version it never asked for.
func TestDeploySetupWritesNoRecordWhenTheClusterIsLocked(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}
	parseDeployFlags(t, "--rook-version=v1.20.8")
	lockClusterExternally(t, "released")

	if _, _, _, err := deploySetup(deployCmd); err == nil || !strings.Contains(err.Error(), "locked by another rooket") {
		t.Fatalf("deploySetup = %v, want the locked cluster refused", err)
	}
	if rec, ok := readSource("released"); !ok || rec.RookVersion != "v1.20.7" {
		t.Errorf("record = (%+v, %v), want the original v1.20.7 unchanged", rec, ok)
	}
}

// Unchecked, clone.Open(dir).Ensure() would MkdirAll a typo'd --dir into a
// fresh, empty .rooket tree and deploy with no sticky configuration, silently.
func TestDeploySetupReleasedRefusesAMissingDir(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	parseDeployFlags(t, "--rook-version=v1.20.7")
	missing := filepath.Join(t.TempDir(), "typo")
	deployDir = missing

	if _, _, _, err := deploySetup(deployCmd); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("deploySetup = %v, want the missing --dir refused, naming %s", err, missing)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a typo'd --dir got created rather than refused")
	}
}
