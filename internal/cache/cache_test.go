package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateConfig(t *testing.T) {
	raw, err := GenerateConfig([]string{"quay.io", "docker.io"})
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg zotConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, raw)
	}

	if !cfg.Extensions.Sync.Enable {
		t.Error("sync extension must be enabled; without it nothing is proxied")
	}
	if len(cfg.Extensions.Sync.Registries) != 2 {
		t.Fatalf("want one registry entry per upstream, got %d", len(cfg.Extensions.Sync.Registries))
	}

	// zot resolves an upstream from the repository prefix, so each upstream's
	// content must land under a destination equal to its namespace — that is
	// the path the nodes' hosts.toml asks for.
	quay := cfg.Extensions.Sync.Registries[0]
	if got := quay.URLs[0]; got != "https://quay.io" {
		t.Errorf("quay.io url = %q, want https://quay.io", got)
	}
	if got := quay.Content[0].Destination; got != "/quay.io" {
		t.Errorf("quay.io destination = %q, want /quay.io", got)
	}
	if got := quay.Content[0].Prefix; got != "**" {
		t.Errorf("prefix = %q, want ** (all repositories within the registry)", got)
	}
	if !quay.OnDemand {
		t.Error("onDemand must be set; a poll-only mirror would never populate")
	}
	if !quay.TLSVerify {
		t.Error("tlsVerify must stay on for upstream fetches")
	}

	// docker.io is the one namespace whose registry API lives elsewhere.
	if got := cfg.Extensions.Sync.Registries[1].URLs[0]; got != "https://index.docker.io" {
		t.Errorf("docker.io url = %q, want https://index.docker.io", got)
	}
	if got := cfg.Extensions.Sync.Registries[1].Content[0].Destination; got != "/docker.io" {
		t.Errorf("docker.io destination = %q, want /docker.io", got)
	}

	// zot is OCI-native; without docker2s2 it cannot serve the Docker schema-2
	// manifests much of the ecosystem still publishes.
	if !strings.Contains(strings.Join(cfg.HTTP.Compat, ","), "docker2s2") {
		t.Errorf("http.compat must include docker2s2, got %v", cfg.HTTP.Compat)
	}
	if !cfg.Storage.GC {
		t.Error("gc must be on; the cache is shared by every cluster and grows unbounded otherwise")
	}
	if cfg.Storage.RootDirectory != StoragePath {
		t.Errorf("rootDirectory = %q, want %q (the named volume mountpoint)", cfg.Storage.RootDirectory, StoragePath)
	}
}

func TestGenerateConfigDefaultsToUpstreams(t *testing.T) {
	raw, err := GenerateConfig(nil)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	var cfg zotConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(cfg.Extensions.Sync.Registries) != len(Upstreams) {
		t.Errorf("nil upstreams should fall back to the default list (%d), got %d",
			len(Upstreams), len(cfg.Extensions.Sync.Registries))
	}
}

// TestDefaultUpstreamsCoverRook guards the registries a rook deployment
// actually pulls from. One missing here is not fatal — an unproxied registry
// pulls straight from the internet, exactly as before the cache existed — but
// it silently forfeits the cache for those images.
func TestDefaultUpstreamsCoverRook(t *testing.T) {
	for _, want := range []string{"quay.io", "registry.k8s.io", "docker.io"} {
		found := false
		for _, ns := range Upstreams {
			if ns == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default upstreams missing %q, which rook pulls from: %v", want, Upstreams)
		}
	}
}

// TestCacheIsNotClusterScoped locks in the property that makes the cache
// shared: rooket's teardown paths delete "<cluster>-registry" by exact name, so
// a cache name that ever embedded a cluster name would be swept away with it.
func TestCacheIsNotClusterScoped(t *testing.T) {
	if strings.Contains(ContainerName, "registry") {
		t.Errorf("ContainerName %q must not collide with the per-cluster registry naming", ContainerName)
	}
	if InClusterAddr() != ContainerName+":5000" {
		t.Errorf("InClusterAddr = %q, want %s:5000", InClusterAddr(), ContainerName)
	}
}
