package profileschart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRenderWritesPrefixedTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	ok, err := Render(dir, Context{ClusterName: "c", Namespace: "rook-ceph"}, []Source{
		{Prefix: "local", Files: map[string][]byte{"scratch.yaml": []byte("kind: PersistentVolumeClaim\n")}},
		{Prefix: "rgw", Files: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("Render reported nothing to install")
	}
	for name, want := range map[string]string{
		"local-scratch.yaml": "kind: PersistentVolumeClaim\n",
		"rgw-20-obc.yaml":    "kind: ObjectBucketClaim\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, "templates", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Errorf("Chart.yaml missing: %v", err)
	}
}

func TestRenderExposesContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	if _, err := Render(dir, Context{ClusterName: "my-cluster", Namespace: "rook-ceph",
		OperatorNamespace: "rook-ceph", Workers: 3}, []Source{
		{Prefix: "p", Files: map[string][]byte{"a.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var vals map[string]any
	if err := yaml.Unmarshal(data, &vals); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	rooket, ok := vals["rooket"]
	if !ok {
		t.Fatal("values.yaml missing top-level 'rooket' key")
	}
	rooketMap, ok := rooket.(map[string]any)
	if !ok {
		t.Fatalf("rooket is %T, not a map", rooket)
	}
	if rooketMap["clusterName"] != "my-cluster" {
		t.Errorf("clusterName = %v, want my-cluster", rooketMap["clusterName"])
	}
	if rooketMap["workers"] != 3 {
		t.Errorf("workers = %v, want 3", rooketMap["workers"])
	}
}

func TestRenderWithNoTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	ok, err := Render(dir, Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Render should report nothing to install")
	}
}

func TestRenderClearsStaleTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	if _, err := Render(dir, Context{}, []Source{
		{Prefix: "old", Files: map[string][]byte{"gone.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(dir, Context{}, []Source{
		{Prefix: "new", Files: map[string][]byte{"here.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "templates", "old-gone.yaml")); err == nil {
		t.Error("stale template survived a re-render")
	}
}

func TestRenderEmptyDirError(t *testing.T) {
	_, err := Render("", Context{}, []Source{
		{Prefix: "p", Files: map[string][]byte{"a.yaml": []byte("kind: ConfigMap\n")}},
	})
	if err == nil {
		t.Error("Render should return error for empty dir")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty dir: %v", err)
	}
}

func TestRenderDetectesPathCollision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	_, err := Render(dir, Context{}, []Source{
		{Prefix: "a", Files: map[string][]byte{"b-c.yaml": []byte("kind: ConfigMap\n")}},
		{Prefix: "a-b", Files: map[string][]byte{"c.yaml": []byte("kind: ConfigMap\n")}},
	})
	if err == nil {
		t.Error("Render should return error for path collision")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error should mention collision: %v", err)
	}
}

func TestRenderDeterministicFileOrdering(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	ok, err := Render(dir, Context{}, []Source{
		{Prefix: "p", Files: map[string][]byte{
			"30-c.yaml": []byte("kind: ConfigMap\n"),
			"10-a.yaml": []byte("kind: Deployment\n"),
			"20-b.yaml": []byte("kind: Service\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("Render reported nothing to install")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "templates"))
	if err != nil {
		t.Fatal(err)
	}

	wantNames := []string{"p-10-a.yaml", "p-20-b.yaml", "p-30-c.yaml"}
	if len(entries) != len(wantNames) {
		t.Fatalf("got %d files, want %d", len(entries), len(wantNames))
	}
	for i, entry := range entries {
		if entry.Name() != wantNames[i] {
			t.Errorf("file order position %d: got %q, want %q", i, entry.Name(), wantNames[i])
		}
	}
}
