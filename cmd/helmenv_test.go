package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHelmEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env, err := helmEnv("testcluster", "rooket")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".local", "share", "rooket", "testcluster", "helm", "rooket")
	want := []string{
		"HELM_CONFIG_HOME=" + filepath.Join(base, "config"),
		"HELM_CACHE_HOME=" + filepath.Join(base, "cache"),
		"HELM_DATA_HOME=" + filepath.Join(base, "data"),
		"HELM_REPOSITORY_CONFIG=" + filepath.Join(base, "config", "repositories.yaml"),
		"HELM_REPOSITORY_CACHE=" + filepath.Join(base, "cache", "repository"),
		"HELM_REGISTRY_CONFIG=" + filepath.Join(base, "config", "registry", "config.json"),
		"HELM_PLUGINS=" + filepath.Join(base, "data", "plugins"),
	}
	if len(env) != len(want) {
		t.Fatalf("got %d env entries, want %d: %v", len(env), len(want), env)
	}
	for i, w := range want {
		if env[i] != w {
			t.Errorf("env[%d] = %q, want %q", i, env[i], w)
		}
	}
	for _, sub := range []string{"config", "cache", "data"} {
		dir := filepath.Join(base, sub)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("dir %s not created: %v", dir, err)
		}
	}

	if _, err := helmEnv("Bad_Name!", "rooket"); err == nil {
		t.Error("invalid cluster name: got nil error")
	}
}
