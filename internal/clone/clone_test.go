package clone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureWritesSelfIgnoringGitignore(t *testing.T) {
	root := t.TempDir()
	d := Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".rooket", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "*\n" {
		t.Errorf("gitignore = %q, want %q", data, "*\n")
	}

	if err := d.Ensure(); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
}

func TestProfiles(t *testing.T) {
	root := t.TempDir()
	d := Open(root)

	t.Run("absent config yields no profiles", func(t *testing.T) {
		got, err := d.Profiles()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v, want none", got)
		}
	})

	t.Run("round-trips order", func(t *testing.T) {
		if err := d.SetProfiles([]string{"rbd", "rgw"}); err != nil {
			t.Fatal(err)
		}
		got, err := d.Profiles()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{"rbd", "rgw"}) {
			t.Errorf("got %#v", got)
		}
	})
}

func TestEnsurePreservesUserGitignoreEdits(t *testing.T) {
	root := t.TempDir()
	d := Open(root)

	// Create .rooket directory and a .gitignore with user edits before Ensure()
	rooketDir := filepath.Join(root, ".rooket")
	if err := os.MkdirAll(rooketDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := "# user edits\n"
	gi := filepath.Join(rooketDir, ".gitignore")
	if err := os.WriteFile(gi, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Call Ensure() — should not overwrite the existing .gitignore
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}

	// Verify user edits are preserved
	data, err := os.ReadFile(gi)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != userContent {
		t.Errorf("gitignore = %q, want %q", data, userContent)
	}
}

func TestTemplates(t *testing.T) {
	root := t.TempDir()
	d := Open(root)

	t.Run("absent directory yields none", func(t *testing.T) {
		got, err := d.Templates()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("present but empty directory yields none", func(t *testing.T) {
		dir := filepath.Join(root, ".rooket", "templates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := d.Templates()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v, want empty map", got)
		}
	})

	t.Run("reads yaml files only", func(t *testing.T) {
		dir := filepath.Join(root, ".rooket", "templates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"pvc.yaml":  "kind: PersistentVolumeClaim\n",
			"pod.yml":   "kind: Pod\n",
			"notes.txt": "ignore me",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := d.Templates()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %#v, want 2 entries", got)
		}
		if string(got["pvc.yaml"]) != "kind: PersistentVolumeClaim\n" {
			t.Errorf("pvc.yaml = %q", got["pvc.yaml"])
		}
	})
}

func TestValuesPath(t *testing.T) {
	got := Open("/x").ValuesPath("rook-ceph-cluster")
	want := filepath.Join("/x", ".rooket", "values", "rook-ceph-cluster.yaml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
