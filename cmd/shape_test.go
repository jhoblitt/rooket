package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestShapeRecordRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := clusterShape{Workers: 1, DiskCount: 2, IQNDate: "2024-06"}
	if err := writeShape("c1", want); err != nil {
		t.Fatalf("writeShape: %v", err)
	}
	got, ok := readShape("c1")
	if !ok || got != want {
		t.Errorf("readShape = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestReadShapeWithoutUsableRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got, ok := readShape("never-created"); ok {
		t.Errorf("readShape of a cluster with no record = (%+v, true), want ok=false", got)
	}

	dir, err := ensureStateDir("torn")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, shapeFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readShape("torn"); ok {
		t.Errorf("readShape of an unparseable record = (%+v, true), want ok=false", got)
	}
}

func TestResolveRecorded(t *testing.T) {
	cases := []struct {
		name       string
		use        shapeUse
		val        int
		explicit   bool
		recorded   int
		haveRecord bool
		want       int
	}{
		{"record fills an unset flag", matchShape, 3, false, 1, true, 1},
		{"default stands with no record", matchShape, 3, false, 0, false, 3},
		{"explicit flag with no record", matchShape, 5, true, 0, false, 5},
		{"explicit flag agreeing with the record", matchShape, 1, true, 1, true, 1},
		{"reshape: record fills an unset flag", reshape, 3, false, 1, true, 1},
		{"reshape: explicit flag replaces the record", reshape, 3, true, 1, true, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveRecorded(c.use, "workers", c.val, c.explicit, c.recorded, c.haveRecord)
			if err != nil || got != c.want {
				t.Errorf("resolveRecorded = (%d, %v), want (%d, nil)", got, err, c.want)
			}
		})
	}
}

// deploy, down, and block teardown act on the cluster as it was built, so a
// flag contradicting the record can only be a mistake — one that, left to run,
// waits on iSCSI disks that were never created.
func TestResolveRecordedRejectsAContradictingFlag(t *testing.T) {
	_, err := resolveRecorded(matchShape, "workers", 3, true, 1, true)
	if err == nil {
		t.Fatal("resolveRecorded = nil error, want the conflict reported")
	}
	if !strings.Contains(err.Error(), "--workers 1") {
		t.Errorf("error %q does not name the recorded --workers 1", err)
	}
}

func TestUseRecordedShapeFillsUnsetFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := writeShape("single", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
		t.Fatal(err)
	}

	workers, disks, iqn := 3, 1, "2003-01"
	noneChanged := func(string) bool { return false }
	if err := useRecordedShape("single", noneChanged, matchShape, &workers, &disks, &iqn); err != nil {
		t.Fatalf("useRecordedShape: %v", err)
	}
	if workers != 1 {
		t.Errorf("workers = %d, want the recorded 1 rather than the flag default", workers)
	}
}

func TestUseRecordedShapeNamesTheClusterInAConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := writeShape("single", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
		t.Fatal(err)
	}

	workers, disks, iqn := 3, 1, "2003-01"
	onlyWorkers := func(flag string) bool { return flag == "workers" }
	err := useRecordedShape("single", onlyWorkers, matchShape, &workers, &disks, &iqn)
	if err == nil || !strings.Contains(err.Error(), `"single"`) {
		t.Errorf("useRecordedShape = %v, want a conflict error naming cluster \"single\"", err)
	}
}

func TestUseRecordedShapeLeavesUnrecordedClustersAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	workers, disks, iqn := 3, 1, "2003-01"
	noneChanged := func(string) bool { return false }
	if err := useRecordedShape("predates-records", noneChanged, matchShape, &workers, &disks, &iqn); err != nil {
		t.Fatalf("useRecordedShape: %v", err)
	}
	if workers != 3 || disks != 1 || iqn != "2003-01" {
		t.Errorf("shape = (%d, %d, %q), want the flag defaults (3, 1, \"2003-01\")", workers, disks, iqn)
	}
}

// useRecordedShape asks each command whether a flag was changed by name, and
// pflag answers false for a name the command does not define — a typo there
// would make the record silently override what the user passed.
func TestShapeConsumersDefineTheShapeFlags(t *testing.T) {
	for _, c := range []*cobra.Command{upCmd, createCmd, blockSetupCmd, deployCmd, downCmd, blockTeardownCmd} {
		for _, flag := range []string{"workers", "disk-count", "iqn-date"} {
			if c.Flags().Lookup(flag) == nil && c.PersistentFlags().Lookup(flag) == nil {
				t.Errorf("%s defines no --%s flag", c.CommandPath(), flag)
			}
		}
	}
}

// shapeFlagCase is a value a test passes one shape flag, and the refusal it
// wants, or "" for a value the command accepts.
type shapeFlagCase struct {
	flag, value, want string
}

// rooket can neither build nor tear down a cluster of no workers, and a
// negative disk count or a disk of no size describes no cluster at all.
// Nothing here names a cluster, so a value a command lets through is refused
// for that instead: the check runs before the command names, locks, or records
// anything.
func TestCommandsRefuseAShapeNoClusterCanHave(t *testing.T) {
	workers := []shapeFlagCase{
		{"workers", "0", "--workers must be more than 0, not 0"},
		{"workers", "-1", "--workers must be more than 0, not -1"},
		{"workers", "1", ""},
	}
	// down's and block teardown's --workers 0 names no worker, which for a
	// cluster with no record leaves the disks they can find.
	teardownWorkers := []shapeFlagCase{
		{"workers", "-1", "--workers must be 0 or more, not -1"},
		{"workers", "0", ""},
		{"workers", "1", ""},
	}
	diskCount := []shapeFlagCase{
		{"disk-count", "-1", "--disk-count must be 0 or more, not -1"},
		{"disk-count", "0", ""},
	}
	diskSize := []shapeFlagCase{
		{"disk-size", "0", "--disk-size must be more than 0, not 0"},
		{"disk-size", "-1", "--disk-size must be more than 0, not -1"},
		{"disk-size", "1", ""},
	}
	for _, tc := range []struct {
		cmd   *cobra.Command
		names []*string // the command's --name and --dir, cleared so nothing names a cluster
		cases []shapeFlagCase
	}{
		{upCmd, []*string{&upName, &upRookDir}, slices.Concat(workers, diskCount, diskSize)},
		{createCmd, []*string{&createName}, slices.Concat(workers, diskCount)},
		{blockSetupCmd, []*string{&blockSetupName}, slices.Concat(workers, diskCount, diskSize)},
		{deployCmd, []*string{&deployName, &deployDir}, slices.Concat(workers, diskCount, diskSize)},
		{configCmd, []*string{&configName}, slices.Concat(workers, diskCount)},
		{downCmd, []*string{&downName}, slices.Concat(teardownWorkers, diskCount)},
		{blockTeardownCmd, []*string{&blockTeardownName}, slices.Concat(teardownWorkers, diskCount)},
	} {
		for _, c := range tc.cases {
			t.Run(tc.cmd.CommandPath()+" --"+c.flag+"="+c.value, func(t *testing.T) {
				root := selectNothing(t)
				// Were the command to go ahead, it would find no kind, engine,
				// or iSCSI tool.
				t.Setenv("PATH", t.TempDir())
				for _, p := range tc.names {
					keep(t, p)
					*p = ""
				}
				setFlag(t, tc.cmd, c.flag, c.value)

				err := tc.cmd.RunE(tc.cmd, nil)
				want := c.want
				if want == "" {
					want = "no cluster selected"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s --%s=%s = %v, want %q", tc.cmd.CommandPath(), c.flag, c.value, err, want)
				}
				if kc := os.Getenv("KUBECONFIG"); kc != "untouched" {
					t.Errorf("KUBECONFIG = %q, want it untouched", kc)
				}
				if files := stateFiles(t, root); len(files) != 0 {
					t.Errorf("state root holds %v, want nothing: no lock taken, no state written", files)
				}
			})
		}
	}
}

// setFlag sets c's flag name to value as a command line would, and restores
// the flag's value and Changed state when the test ends. It is for scalar
// flags only: a slice flag's Set appends to what it holds rather than
// replacing it, so the restore would not undo the parse.
func setFlag(t *testing.T, c *cobra.Command, name, value string) {
	t.Helper()
	f := c.Flag(name)
	if f == nil {
		t.Fatalf("%s defines no --%s flag", c.CommandPath(), name)
	}
	prev, changed := f.Value.String(), f.Changed
	t.Cleanup(func() {
		_ = f.Value.Set(prev)
		f.Changed = changed
	})
	if err := c.ParseFlags([]string{"--" + name + "=" + value}); err != nil {
		t.Fatal(err)
	}
}
