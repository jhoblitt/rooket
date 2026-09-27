package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneDir(t *testing.T) {
	t.Run("recorded clone path wins over the build stamp", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, clonePathFile), "/home/u/rookA\n")
		writeFile(t, filepath.Join(dir, "build-stamp.json"), `{"dir":"/home/u/rookB"}`)
		if got := cloneDir(dir); got != "/home/u/rookA" {
			t.Errorf("cloneDir = %q, want /home/u/rookA", got)
		}
	})

	t.Run("falls back to the build stamp", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "build-stamp.json"), `{"dir":"/home/u/rookB"}`)
		if got := cloneDir(dir); got != "/home/u/rookB" {
			t.Errorf("cloneDir = %q, want /home/u/rookB", got)
		}
	})

	t.Run("empty when neither records a clone", func(t *testing.T) {
		if got := cloneDir(t.TempDir()); got != "" {
			t.Errorf("cloneDir = %q, want empty", got)
		}
	})

	t.Run("empty for an unreadable or malformed stamp", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "build-stamp.json"), "{not json")
		if got := cloneDir(dir); got != "" {
			t.Errorf("cloneDir = %q, want empty", got)
		}
	})
}

func TestCloneGone(t *testing.T) {
	t.Run("false while the clone still exists", func(t *testing.T) {
		clone := t.TempDir()
		state := t.TempDir()
		writeFile(t, filepath.Join(state, clonePathFile), clone+"\n")
		if cloneGone(state) {
			t.Error("cloneGone = true for an existing clone")
		}
	})

	t.Run("true once the clone is removed", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "gone")
		state := t.TempDir()
		writeFile(t, filepath.Join(state, clonePathFile), clone+"\n")
		if !cloneGone(state) {
			t.Error("cloneGone = false for a removed clone")
		}
	})

	// Unknown provenance keeps prune's pre-existing behavior: a state dir
	// that names no clone (created before rooket recorded one, or by a
	// --name run outside any clone) must stay sweepable, or every legacy
	// directory on the host becomes unprunable.
	t.Run("true when no clone is recorded at all", func(t *testing.T) {
		if !cloneGone(t.TempDir()) {
			t.Error("cloneGone = false for a state dir with no recorded clone")
		}
	})
}

func TestRecordClonePath(t *testing.T) {
	t.Run("records the clone the cluster was created from", func(t *testing.T) {
		clone := t.TempDir()
		state := t.TempDir()
		recordClonePath(state, clone)
		if got := cloneDir(state); got != clone {
			t.Errorf("cloneDir = %q, want %q", got, clone)
		}
	})

	// The first clone to create a cluster owns it: a later command run from
	// a different tree (e.g. --name reused elsewhere) must not silently
	// repoint the record and make prune judge the wrong directory.
	t.Run("does not overwrite an existing record", func(t *testing.T) {
		state := t.TempDir()
		recordClonePath(state, "/home/u/first")
		recordClonePath(state, "/home/u/second")
		if got := cloneDir(state); got != "/home/u/first" {
			t.Errorf("cloneDir = %q, want /home/u/first", got)
		}
	})

	t.Run("records nothing when there is no clone", func(t *testing.T) {
		state := t.TempDir()
		recordClonePath(state, "")
		if _, err := os.Stat(filepath.Join(state, clonePathFile)); !os.IsNotExist(err) {
			t.Errorf("stat %s = %v, want not-exist", clonePathFile, err)
		}
	})
}

// recordSource writes s as a state dir's source record, for tests that need
// one already in place.
func recordSource(t *testing.T, s clusterSource) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sourceFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOwnerGone(t *testing.T) {
	t.Run("released, its configuration directory still there", func(t *testing.T) {
		if ownerGone(recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: t.TempDir()})) {
			t.Error("ownerGone = true, want parked while its configuration directory exists")
		}
	})
	t.Run("released, its configuration directory removed", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "removed")
		if !ownerGone(recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: gone})) {
			t.Error("ownerGone = false, want abandoned once its only owner is gone")
		}
	})
	// Nothing on disk will ever disappear to say such a cluster was abandoned,
	// and its record proves it is not a leftover from before provenance.
	t.Run("released with no owner at all", func(t *testing.T) {
		if ownerGone(recordSource(t, clusterSource{RookVersion: "v1.20.7"})) {
			t.Error("ownerGone = true, want parked")
		}
	})
	// A released cluster is owned by both its clone and its configuration
	// directory when it has both, not by whichever the code happens to check
	// first: it must stay parked as long as either is still there.
	t.Run("released, its recorded clone exists though its configuration directory is gone", func(t *testing.T) {
		clone := t.TempDir()
		dir := recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: filepath.Join(t.TempDir(), "removed")})
		writeFile(t, filepath.Join(dir, clonePathFile), clone+"\n")
		if ownerGone(dir) {
			t.Error("ownerGone = true, want parked: its clone still exists even though its configuration directory is gone")
		}
	})
	t.Run("released, both its recorded clone and its configuration directory are gone", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "removed-clone")
		dir := recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: filepath.Join(t.TempDir(), "removed-config")})
		writeFile(t, filepath.Join(dir, clonePathFile), clone+"\n")
		if !ownerGone(dir) {
			t.Error("ownerGone = false, want abandoned once every recorded owner is gone")
		}
	})
	// A clone-built record (empty RookVersion) is judged by its clone alone;
	// a ConfigDir it happens to name too must not save it, or every
	// clone-built cluster that ever passed --config-dir would become
	// unprunable once its clone was removed.
	t.Run("clone-built record naming a configuration directory is still judged by its clone alone", func(t *testing.T) {
		clone := filepath.Join(t.TempDir(), "removed-clone")
		dir := recordSource(t, clusterSource{ConfigDir: t.TempDir()})
		writeFile(t, filepath.Join(dir, clonePathFile), clone+"\n")
		if !ownerGone(dir) {
			t.Error("ownerGone = false, want abandoned: RookVersion is empty, so its configuration directory must not save it")
		}
	})
	t.Run("no source record at all falls back to cloneGone, abandoned as before", func(t *testing.T) {
		if !ownerGone(t.TempDir()) {
			t.Error("ownerGone of an unrecorded state dir = false, want abandoned, as cloneGone says")
		}
	})
}

func TestParkedBecause(t *testing.T) {
	t.Run("clone-built: names its clone", func(t *testing.T) {
		clone := t.TempDir()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, clonePathFile), clone+"\n")
		want := fmt.Sprintf("its clone %s still exists", clone)
		if got := parkedBecause(dir); got != want {
			t.Errorf("parkedBecause = %q, want %q", got, want)
		}
	})
	t.Run("released, its clone still exists: names its clone", func(t *testing.T) {
		clone := t.TempDir()
		dir := recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: filepath.Join(t.TempDir(), "removed")})
		writeFile(t, filepath.Join(dir, clonePathFile), clone+"\n")
		want := fmt.Sprintf("its clone %s still exists", clone)
		if got := parkedBecause(dir); got != want {
			t.Errorf("parkedBecause = %q, want %q", got, want)
		}
	})
	t.Run("released, its configuration directory still exists: names it", func(t *testing.T) {
		configDir := t.TempDir()
		dir := recordSource(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: configDir})
		want := fmt.Sprintf("its configuration directory %s still exists", configDir)
		if got := parkedBecause(dir); got != want {
			t.Errorf("parkedBecause = %q, want %q", got, want)
		}
	})
	t.Run("released, names no owner at all", func(t *testing.T) {
		dir := recordSource(t, clusterSource{RookVersion: "v1.20.7"})
		want := "it deploys released Rook v1.20.7 and names no clone or configuration directory"
		if got := parkedBecause(dir); got != want {
			t.Errorf("parkedBecause = %q, want %q", got, want)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
