package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestRegistryConfigPathIsPerCluster locks in the property that makes the
// recreate-on-change check correct. One shared file across clusters would let
// the first cluster to notice a change rewrite it, leaving every other
// cluster's registry running the old config with nothing left to detect.
func TestRegistryConfigPathIsPerCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	one, err := registryConfigPath("alpha")
	if err != nil {
		t.Fatalf("registryConfigPath(alpha): %v", err)
	}
	two, err := registryConfigPath("beta")
	if err != nil {
		t.Fatalf("registryConfigPath(beta): %v", err)
	}
	if one == two {
		t.Fatalf("both clusters share config path %q", one)
	}

	// The state dir is where a cluster's other per-cluster files live
	// (registry-port, build stamp), so teardown reclaims this one with them.
	dir, err := stateDirPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(one) != dir {
		t.Errorf("config path %q is not in the cluster state dir %q", one, dir)
	}
}

// TestWriteRegistryConfigReplacesRatherThanTruncates proves the install is
// atomic. The file is bind-mounted into the registry container, which is a
// hard dependency of every build and push: a truncating rewrite can be
// observed as a partial file, and a container created in that window mounts a
// config zot cannot parse.
func TestWriteRegistryConfigReplacesRatherThanTruncates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path, err := registryConfigPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []byte(`{"distSpecVersion":"1.1.0"}`)
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	// A running container holds the file by inode, not by path.
	mounted, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer mounted.Close()

	got, changed, err := writeRegistryConfig("alpha")
	if err != nil {
		t.Fatalf("writeRegistryConfig: %v", err)
	}
	if !changed {
		t.Error("changed = false after replacing a different config")
	}
	if got != path {
		t.Errorf("returned path %q, want %q", got, path)
	}

	held, err := io.ReadAll(mounted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(held, stale) {
		t.Errorf("the already-mounted inode changed under the container:\ngot:  %s\nwant: %s", held, stale)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(onDisk, stale) {
		t.Error("the path still resolves to the stale config")
	}

	// A leftover temp file would be picked up by nothing, but it would also
	// show up in the state dir the user is invited to inspect.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("write left %q behind in the state dir", e.Name())
		}
	}
}

// TestWriteRegistryConfigUnchangedIsNotAChange keeps the common path quiet:
// every 'rooket up' re-renders the config, and reporting a change would
// recreate a working registry — discarding its images — on every run.
func TestWriteRegistryConfigUnchangedIsNotAChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, _, err := writeRegistryConfig("alpha"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, changed, err := writeRegistryConfig("alpha")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Error("re-rendering an identical config reported a change")
	}
}
