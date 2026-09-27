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

func TestListShadowsBuiltInWithUserProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "rbd", "shadowed", "", "")

	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var matches []Profile
	for _, p := range got {
		if p.Name == "rbd" {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		t.Fatalf(`List returned %d entries named "rbd", want exactly 1: %#v`, len(matches), matches)
	}
	if matches[0].BuiltIn || matches[0].Description != "shadowed" {
		t.Errorf("List did not shadow the built-in: %+v", matches[0])
	}
}

func TestListWithoutUserDir(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.Name] = true
	}
	if !seen["rbd"] {
		t.Errorf("List missing built-in %q with no user dir present: %#v", "rbd", seen)
	}
}

// withBuiltinFS swaps the package's built-in filesystem for the duration of
// a test, restoring it on cleanup. Tests are not run in parallel in this
// package, so the swap is safe.
func withBuiltinFS(t *testing.T, root string) {
	t.Helper()
	orig := builtinFS
	builtinFS = os.DirFS(root)
	t.Cleanup(func() { builtinFS = orig })
}

func TestLoadAndListDoNotRecurseOnUnloadableBuiltIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "builtin", "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	withBuiltinFS(t, root)

	if _, err := List(t.TempDir()); err == nil {
		t.Error("List should error on an unloadable built-in profile, not recurse")
	}
	if _, err := Load(t.TempDir(), "does-not-exist"); err == nil {
		t.Error("Load should error on an unknown name, not recurse")
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

func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "mytest", "path profile", "rook-ceph-cluster", "toolbox:\n  enabled: false\n")
	path := filepath.Join(root, "mytest")

	p, err := LoadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mytest" || p.Path != path || p.Label() != path {
		t.Errorf("Name=%q Path=%q Label=%q, want mytest and %q twice", p.Name, p.Path, p.Label(), path)
	}
	if p.BuiltIn || p.Description != "path profile" {
		t.Errorf("got %+v", p)
	}
	tb := p.Values["rook-ceph-cluster"]["toolbox"].(map[string]any)
	if tb["enabled"] != false {
		t.Errorf("values = %#v", p.Values)
	}
	if string(p.Templates["10-thing.yaml"]) != "kind: ConfigMap\n" {
		t.Errorf("templates = %#v", p.Templates)
	}
}

func TestLoadDirRelativeSpellings(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "mytest", "d", "", "")
	t.Chdir(root)

	for _, given := range []string{"./mytest", "mytest/", "./mytest/"} {
		p, err := LoadDir(given)
		if err != nil {
			t.Fatalf("%s: %v", given, err)
		}
		if p.Name != "mytest" || p.Path != given {
			t.Errorf("%s: Name=%q Path=%q", given, p.Name, p.Path)
		}
	}

	t.Chdir(filepath.Join(root, "mytest"))
	p, err := LoadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mytest" {
		t.Errorf(`LoadDir(".") Name = %q, want mytest`, p.Name)
	}
}

func TestLoadDirErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.yaml")
	if err := os.WriteFile(file, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noMeta := filepath.Join(root, "nometa")
	if err := os.MkdirAll(filepath.Join(noMeta, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, root, Reserved, "reserved", "", "")
	writeProfile(t, root, "_hidden", "underscore", "", "")

	for name, tc := range map[string]struct{ path, want string }{
		"missing":            {filepath.Join(root, "nope"), "no such file"},
		"not a directory":    {file, "not a directory"},
		"no profile.yaml":    {noMeta, "profile.yaml"},
		"reserved name":      {filepath.Join(root, Reserved), "reserved"},
		"leading underscore": {filepath.Join(root, "_hidden"), "starts with"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadDir(tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.path) {
				t.Errorf("err = %v, want it to mention %q and the path", err, tc.want)
			}
		})
	}
}

func TestSameSource(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "rbd", "shadow", "", "")
	t.Chdir(root)

	rel, err := LoadDir("./rbd")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := LoadDir(filepath.Join(root, "rbd"))
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}

	if !rel.SameSource(abs) {
		t.Error("two spellings of one directory must be the same source")
	}
	if rel.SameSource(builtin) {
		t.Error("a path profile and the built-in of the same name must be different sources")
	}
	if !builtin.SameSource(again) {
		t.Error("the same name loaded twice must be the same source")
	}
}

func TestLoadRejectsTwoValuesFilesForOneChart(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "dup", "d", "rook-ceph", "a: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "dup", "values", "rook-ceph.yml"), []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir, "dup")
	if err == nil || !strings.Contains(err.Error(), "rook-ceph.yaml") || !strings.Contains(err.Error(), "rook-ceph.yml") {
		t.Errorf("err = %v, want it to name both files", err)
	}
}

func TestLoadAcceptsYmlValuesFile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "yml", "d", "", "")
	if err := os.WriteFile(filepath.Join(dir, "yml", "values", "rook-ceph-cluster.yml"),
		[]byte("toolbox:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(dir, "yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Values["rook-ceph-cluster"]; !ok {
		t.Errorf("values = %#v, want a rook-ceph-cluster entry from the .yml file", p.Values)
	}
}

func TestLoadRejectsNonYAMLValuesFile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "js", "d", "", "")
	if err := os.WriteFile(filepath.Join(dir, "js", "values", "rook-ceph-cluster.json"),
		[]byte(`{"toolbox":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir, "js")
	if err == nil || !strings.Contains(err.Error(), "rook-ceph-cluster.json") {
		t.Errorf("err = %v, want it to name values/rook-ceph-cluster.json", err)
	}
}

func TestLoadIgnoresHiddenValuesFile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "hidden", "d", "rook-ceph", "a: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "hidden", "values", ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(dir, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Values) != 1 {
		t.Errorf("values = %#v, want only rook-ceph", p.Values)
	}
}
