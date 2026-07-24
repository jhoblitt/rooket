package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/lio"
)

func fileio(name, path string) lio.StorageObject {
	return lio.StorageObject{Name: name, Plugin: "fileio", Path: path}
}

func target(iqn string, exports ...string) lio.Target {
	return lio.Target{IQN: iqn, StorageObjects: exports}
}

// needsCreate names a disk no fixture below already has a storage object for,
// so the run it stands in for is one that must create a backstore — the only
// kind an orphan can block.
var needsCreate = []iscsiDisk{{
	backstoreName: "c-worker7-disk0",
	targetIQN:     "iqn.2003-01.local.rooket:c-worker7-disk0",
}}

// Only a storage object whose recorded backing file is definitively gone is
// an orphan. One with a live file is a working disk, and one with no path at
// all (a ramdisk) has no file to lose.
func TestFindLIOOrphans(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "worker0-disk0.img")
	if err := os.WriteFile(live, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st := lio.State{
		StorageObjects: []lio.StorageObject{
			fileio("c-worker0-disk0", live),
			fileio("c-worker3-disk0", filepath.Join(dir, "worker3-disk0.img")),
			{Name: "ram0", Plugin: "rd_mcp"},
		},
		Targets: []lio.Target{
			target("iqn.2003-01.local.rooket:c-worker3-disk0", "c-worker3-disk0"),
		},
	}
	orphans := findLIOOrphans(st)
	if len(orphans) != 1 || orphans[0].object.Name != "c-worker3-disk0" {
		t.Fatalf("orphans = %+v, want only c-worker3-disk0", orphans)
	}
	if got := orphans[0].iqns; len(got) != 1 || got[0] != "iqn.2003-01.local.rooket:c-worker3-disk0" {
		t.Errorf("orphan iqns = %v, want the target exporting it", got)
	}
}

// Either mark of rooket's own making — an IQN in its namespace, or a backing
// file inside its state root — is enough to claim an orphan. An orphan
// carrying neither belongs to something else on the host.
func TestLIOOrphanOwnership(t *testing.T) {
	const stateRoot = "/home/u/.local/share/rooket"
	cases := []struct {
		name   string
		orphan lioOrphan
		want   bool
	}{
		{"rooket iqn", lioOrphan{
			object: fileio("c-worker0-disk0", "/elsewhere/worker0-disk0.img"),
			iqns:   []string{"iqn.2003-01.local.rooket:c-worker0-disk0"},
		}, true},
		{"path in state root", lioOrphan{
			object: fileio("c-worker0-disk0", stateRoot+"/c/worker0-disk0.img"),
		}, true},
		{"neither", lioOrphan{
			object: fileio("someone-else", "/srv/vm/disk.img"),
			iqns:   []string{"iqn.2005-03.com.example:someone-else"},
		}, false},
		{"state root as a prefix but not a parent", lioOrphan{
			object: fileio("x", stateRoot+"-other/disk.img"),
		}, false},
	}
	for _, c := range cases {
		if got := c.orphan.rooketOwned(stateRoot); got != c.want {
			t.Errorf("%s: rooketOwned = %v, want %v", c.name, got, c.want)
		}
	}
}

// An orphan rooket cannot claim stops the run before anything privileged
// happens: removing a stranger's storage object is not rooket's call, and
// continuing would only fail later with a symptom that names nothing.
func TestLIORepairStepsRefusesForeignOrphan(t *testing.T) {
	st := lio.State{StorageObjects: []lio.StorageObject{fileio("someone-else", "/srv/vm/gone.img")}}
	steps, err := lioRepairSteps(io.Discard, st, "/home/u/.local/share/rooket", needsCreate)
	if err == nil {
		t.Fatalf("steps = %+v, want an error naming the foreign orphan", steps)
	}
	if !strings.Contains(err.Error(), "someone-else") || !strings.Contains(err.Error(), "targetcli /backstores/fileio delete someone-else") {
		t.Errorf("error does not name the object and how to remove it:\n%v", err)
	}
	if steps != nil {
		t.Errorf("steps = %+v, want none alongside the error", steps)
	}
}

// An orphan only blocks a run that has to create a backstore. One that has
// everything it needs already — reattaching a logged-out device — must not be
// stopped by a foreign orphan it never touches.
func TestLIORepairStepsToleratesForeignOrphanWithNothingToCreate(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "worker0-disk0.img")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st := lio.State{StorageObjects: []lio.StorageObject{
		fileio("someone-else", "/srv/vm/gone.img"),
		fileio("c-worker0-disk0", img),
	}}
	var out strings.Builder
	steps, err := lioRepairSteps(&out, st, dir, []iscsiDisk{{backstoreName: "c-worker0-disk0"}})
	if err != nil {
		t.Fatalf("lioRepairSteps: %v, want the run to proceed", err)
	}
	if steps != nil {
		t.Errorf("steps = %+v, want none: the foreign orphan is not rooket's to remove", steps)
	}
	if !strings.Contains(out.String(), "not rooket's to remove") {
		t.Errorf("no warning about the foreign orphan:\n%s", out.String())
	}
}

// A backstore kind rooket never creates is not rooket's to remove, even when
// its own IQN exports it.
func TestLIORepairStepsRefusesUnrepairablePlugin(t *testing.T) {
	st := lio.State{
		StorageObjects: []lio.StorageObject{{Name: "c-worker0-disk0", Plugin: "block", Path: "/dev/gone"}},
		Targets:        []lio.Target{target("iqn.2003-01.local.rooket:c-worker0-disk0", "c-worker0-disk0")},
	}
	if _, err := lioRepairSteps(io.Discard, st, "/home/u/.local/share/rooket", needsCreate); err == nil {
		t.Fatal("want an error for a block backstore, got none")
	}
}

func TestLIORepairStepsRemovesOwnedOrphan(t *testing.T) {
	st := lio.State{
		StorageObjects: []lio.StorageObject{fileio("c-worker3-disk0", "/home/u/.local/share/rooket/c/worker3-disk0.img")},
		Targets:        []lio.Target{target("iqn.2003-01.local.rooket:c-worker3-disk0", "c-worker3-disk0")},
	}
	steps, err := lioRepairSteps(io.Discard, st, "/home/u/.local/share/rooket", needsCreate)
	if err != nil {
		t.Fatalf("lioRepairSteps: %v", err)
	}
	want := [][]string{
		{"systemctl", "start", "iscsid"},
		{"iscsiadm", "-m", "node", "-T", "iqn.2003-01.local.rooket:c-worker3-disk0", "-u"},
		{"iscsiadm", "-m", "node", "-T", "iqn.2003-01.local.rooket:c-worker3-disk0", "-o", "delete"},
		{"targetcli", "/iscsi", "delete", "iqn.2003-01.local.rooket:c-worker3-disk0"},
		{"targetcli", "/backstores/fileio", "delete", "c-worker3-disk0"},
	}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	for i, w := range want {
		if strings.Join(steps[i].argv, " ") != strings.Join(w, " ") {
			t.Errorf("step %d = %v, want %v", i, steps[i].argv, w)
		}
	}
	// The object must be gone before any create runs, so the delete that
	// removes it is the one failure the run cannot continue past.
	last := steps[len(steps)-1]
	if last.ignoreErr || last.warnOnFailure {
		t.Errorf("backstore delete is best-effort (%+v); a surviving orphan fails every create in the run", last)
	}
	if err := validateSteps(steps); err != nil {
		t.Errorf("repair steps escape the sudoers vocabulary: %v", err)
	}
}

// The repair runs as root and its operands come from the kernel's
// configuration, which any user with root on this host can name: they must be
// quoted like every other operand in the rendered script.
func TestLIORepairStepsQuoteOperands(t *testing.T) {
	st := lio.State{
		StorageObjects: []lio.StorageObject{fileio("c-worker0-disk0; touch /tmp/pwned", "/home/u/.local/share/rooket/c/gone.img")},
	}
	steps, err := lioRepairSteps(io.Discard, st, "/home/u/.local/share/rooket", needsCreate)
	if err != nil {
		t.Fatalf("lioRepairSteps: %v", err)
	}
	script := renderScript(steps)
	if !strings.Contains(script, "'c-worker0-disk0; touch /tmp/pwned'") {
		t.Errorf("operand not single-quoted in script:\n%s", script)
	}
}

// A configuration with nothing missing must add no steps at all: the common
// case pays nothing for the check.
func TestLIORepairStepsCleanHost(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "worker0-disk0.img")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st := lio.State{
		StorageObjects: []lio.StorageObject{fileio("c-worker0-disk0", img)},
		Targets:        []lio.Target{target("iqn.2003-01.local.rooket:c-worker0-disk0", "c-worker0-disk0")},
	}
	steps, err := lioRepairSteps(io.Discard, st, dir, needsCreate)
	if err != nil || steps != nil {
		t.Errorf("steps, err = %+v, %v; want none, nil", steps, err)
	}
}
