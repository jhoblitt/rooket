package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestProfileSourcesOrdersCloneFirst(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rooket", "templates", "scratch.yaml"),
		[]byte("kind: PersistentVolumeClaim\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := profileSources(d, []profiles.Profile{
		{Name: "rgw", Templates: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Prefix != "local" {
		t.Errorf("first prefix = %q, want local", got[0].Prefix)
	}
	if got[1].Prefix != "rgw" {
		t.Errorf("second prefix = %q, want rgw", got[1].Prefix)
	}
}

func TestProfileSourcesSkipsEmptyClone(t *testing.T) {
	got, err := profileSources(clone.Open(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want none", got)
	}
}
