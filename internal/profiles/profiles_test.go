package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, dir, name, desc, valuesChart, valuesBody string) {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(root, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profile.yaml"),
		[]byte("description: "+desc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if valuesChart != "" {
		if err := os.WriteFile(filepath.Join(root, "values", valuesChart+".yaml"),
			[]byte(valuesBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "templates", "10-thing.yaml"),
		[]byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUserProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "custom", "my thing", "rook-ceph-cluster", "toolbox:\n  enabled: false\n")

	p, err := Load(dir, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "my thing" {
		t.Errorf("description = %q", p.Description)
	}
	if p.BuiltIn {
		t.Error("BuiltIn should be false")
	}
	tb := p.Values["rook-ceph-cluster"]["toolbox"].(map[string]any)
	if tb["enabled"] != false {
		t.Errorf("values = %#v", p.Values)
	}
	if string(p.Templates["10-thing.yaml"]) != "kind: ConfigMap\n" {
		t.Errorf("templates = %#v", p.Templates)
	}
}

func TestUserProfileShadowsBuiltIn(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "rbd", "shadowed", "", "")

	p, err := Load(dir, "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if p.BuiltIn || p.Description != "shadowed" {
		t.Errorf("built-in was not shadowed: %+v", p)
	}
}

func TestLoadRejectsReservedName(t *testing.T) {
	_, err := Load(t.TempDir(), "local")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("err = %v, want a reserved-name error", err)
	}
}

func TestLoadUnknownNameLists(t *testing.T) {
	_, err := Load(t.TempDir(), "nope")
	if err == nil || !strings.Contains(err.Error(), "rbd") {
		t.Errorf("err = %v, want it to name the available profiles", err)
	}
}

func TestListIncludesBuiltInsAndUsers(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "mine", "user one", "", "")

	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.Name] = true
	}
	for _, want := range []string{"rbd", "mine"} {
		if !seen[want] {
			t.Errorf("List missing %q: %#v", want, seen)
		}
	}
}

func TestFork(t *testing.T) {
	dir := t.TempDir()
	out, err := Fork(dir, "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if out != filepath.Join(dir, "rbd") {
		t.Errorf("dir = %q", out)
	}
	if _, err := os.Stat(filepath.Join(out, "profile.yaml")); err != nil {
		t.Errorf("forked profile.yaml missing: %v", err)
	}

	if _, err := Fork(dir, "rbd"); err == nil {
		t.Error("forking over an existing profile should fail")
	}
}
