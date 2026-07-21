package profileschart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{"clusterName: my-cluster", "workers: 3"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("values.yaml missing %q:\n%s", want, data)
		}
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
