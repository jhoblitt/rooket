package cmd

import (
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
