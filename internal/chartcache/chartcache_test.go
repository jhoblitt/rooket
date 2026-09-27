package chartcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePull unpacks a stand-in chart the way helm pull --untar does, counting
// calls per chart.
func fakePull(calls map[string]int) Puller {
	return func(dir, chart, version string) error {
		calls[chart]++
		c := filepath.Join(dir, chart)
		if err := os.MkdirAll(c, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c, "Chart.yaml"), []byte("name: "+chart+"\nversion: "+version+"\n"), 0o644)
	}
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"v1.20.7", "v1.21.0-beta.0"} {
		if err := ValidVersion(v); err != nil {
			t.Errorf("ValidVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"", "1.20.7", "v1.20", "v1.20.x", "~1.20", "v1.20.7 "} {
		if err := ValidVersion(v); err == nil {
			t.Errorf("ValidVersion(%q) = nil, want it rejected", v)
		}
	}
}

func TestEnsurePullsEachChartOnce(t *testing.T) {
	root := t.TempDir()
	calls := map[string]int{}

	entry, err := Ensure(root, "v1.20.7", fakePull(calls))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != filepath.Join(root, "v1.20.7") {
		t.Errorf("entry = %q, want %q", entry, filepath.Join(root, "v1.20.7"))
	}
	for _, c := range Charts {
		if _, err := os.Stat(filepath.Join(entry, "deploy", "charts", c, "Chart.yaml")); err != nil {
			t.Errorf("chart %s not laid out like a clone's: %v", c, err)
		}
	}
	if _, err := Ensure(root, "v1.20.7", fakePull(calls)); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	for _, c := range Charts {
		if calls[c] != 1 {
			t.Errorf("%s pulled %d times, want once: a present entry is reused", c, calls[c])
		}
	}
}

// A pull that fails part way must leave nothing a later run would trust.
func TestEnsureLeavesNoEntryAfterAFailedPull(t *testing.T) {
	root := t.TempDir()
	boom := errors.New("network down")
	pull := func(dir, chart, version string) error {
		if chart == "rook-ceph-cluster" {
			return boom
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	if _, err := Ensure(root, "v1.20.7", pull); !errors.Is(err, boom) {
		t.Fatalf("Ensure = %v, want the pull's error", err)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("root holds %d entries after a failed pull, want none", len(left))
	}
}

// Two runs pulling the same version at once: the one that loses the rename
// uses the winner's entry instead of failing.
func TestEnsureYieldsToAConcurrentWinner(t *testing.T) {
	root := t.TempDir()
	winner := filepath.Join(root, "v1.20.7")
	pull := func(dir, chart, version string) error {
		if chart == Charts[len(Charts)-1] {
			for _, c := range Charts {
				cd := filepath.Join(winner, "deploy", "charts", c)
				if err := os.MkdirAll(cd, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cd, "Chart.yaml"), []byte("name: "+c+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	entry, err := Ensure(root, "v1.20.7", pull)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != winner {
		t.Errorf("entry = %q, want the winner's %q", entry, winner)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("root holds %d entries, want only the winner's: the loser's copy is discarded", len(left))
	}
}

func TestEnsureRejectsAnIncompleteEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v1.20.7", "deploy", "charts", "rook-ceph"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(root, "v1.20.7", fakePull(map[string]int{}))
	if err == nil {
		t.Fatal("Ensure = nil error, want the incomplete entry reported")
	}
	if want := filepath.Join(root, "v1.20.7"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the entry %s to delete", err, want)
	}
}
