package cache

import (
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/zot"
)

func TestRunArgs(t *testing.T) {
	cfg := Config{
		Network:        "kind",
		HostConfigPath: "/home/u/.config/rooket/cache-config.json",
	}
	args := runArgs(cfg)
	joined := strings.Join(args, " ")

	// The config is generated under the user's home, whose SELinux type a
	// confined container cannot read. Without a relabel option zot exits at
	// startup on any enforcing host, and every node silently falls back to
	// pulling from upstream.
	if !slices.Contains(args, cfg.HostConfigPath+":"+zot.ConfigPath+":ro,z") {
		t.Errorf("config mount must carry the SELinux relabel option:\n%s", joined)
	}
	// The blobs live in a *named* volume: an anonymous one would be discarded
	// on every recreation, which is exactly what an image or config change
	// now triggers.
	if !slices.Contains(args, VolumeName+":"+zot.StoragePath) {
		t.Errorf("cache storage must be the named volume %s:\n%s", VolumeName, joined)
	}
	if !slices.Contains(args, zot.Image) {
		t.Errorf("cache must run the shared zot pin %q:\n%s", zot.Image, joined)
	}
}

func TestGenerateConfig(t *testing.T) {
	raw, err := GenerateConfig(nil)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg zot.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, raw)
	}

	if cfg.Extensions == nil {
		t.Fatal("config carries no extensions section; nothing would be proxied")
	}
	if !cfg.Extensions.Sync.Enable {
		t.Error("sync extension must be enabled; without it nothing is proxied")
	}

	// zot resolves an upstream from the repository prefix, so each upstream's
	// content must land under a destination equal to its namespace — that is
	// the path the nodes' hosts.toml asks for. Nodes get a hosts.toml for every
	// element of Upstreams, so each needs its own entry.
	regs := cfg.Extensions.Sync.Registries
	if len(regs) != len(Upstreams) {
		t.Fatalf("want one registry entry per upstream (%d), got %d", len(Upstreams), len(regs))
	}
	byDest := make(map[string]zot.SyncRegistry, len(regs))
	for _, r := range regs {
		if len(r.URLs) == 0 || len(r.Content) == 0 {
			t.Fatalf("registry entry has no url or content: %+v", r)
		}
		if got := r.Content[0].Prefix; got != "**" {
			t.Errorf("%s prefix = %q, want ** (all repositories within the registry)", r.URLs[0], got)
		}
		if !r.OnDemand {
			t.Errorf("%s: onDemand must be set; a poll-only mirror would never populate", r.URLs[0])
		}
		if !r.TLSVerify {
			t.Errorf("%s: tlsVerify must stay on for upstream fetches", r.URLs[0])
		}
		byDest[r.Content[0].Destination] = r
	}
	for _, ns := range Upstreams {
		if _, ok := byDest["/"+ns]; !ok {
			t.Errorf("no registry entry at destination /%s; pulls through its hosts.toml would miss the cache", ns)
		}
	}

	// The registries a rook deployment pulls from. One missing is not fatal —
	// an unproxied registry pulls straight from the internet — but it silently
	// forfeits the cache for those images.
	for _, tc := range []struct{ ns, url string }{
		{"quay.io", "https://quay.io"},
		{"registry.k8s.io", "https://registry.k8s.io"},
		// docker.io is the one namespace whose registry API lives elsewhere.
		{"docker.io", "https://index.docker.io"},
	} {
		t.Run(tc.ns, func(t *testing.T) {
			r, ok := byDest["/"+tc.ns]
			if !ok {
				t.Fatalf("no registry entry at destination /%s, which rook pulls from", tc.ns)
			}
			if got := r.URLs[0]; got != tc.url {
				t.Errorf("url = %q, want %q", got, tc.url)
			}
		})
	}

	// Nodes reach the cache over the kind network at InClusterAddr, so it must
	// name the port zot listens on inside its container.
	if want := net.JoinHostPort(ContainerName, cfg.HTTP.Port); InClusterAddr() != want {
		t.Errorf("InClusterAddr = %q, want %q", InClusterAddr(), want)
	}

	// zot is OCI-native; without docker2s2 it cannot serve the Docker schema-2
	// manifests much of the ecosystem still publishes.
	if !strings.Contains(strings.Join(cfg.HTTP.Compat, ","), "docker2s2") {
		t.Errorf("http.compat must include docker2s2, got %v", cfg.HTTP.Compat)
	}
	if !cfg.Storage.GC {
		t.Error("gc must be on; the cache is shared by every cluster and grows unbounded otherwise")
	}
	if cfg.Storage.RootDirectory != zot.StoragePath {
		t.Errorf("rootDirectory = %q, want %q (the named volume mountpoint)", cfg.Storage.RootDirectory, zot.StoragePath)
	}
}

// TestCacheIsNotClusterScoped locks in the property that makes the cache
// shared: rooket's teardown paths delete "<cluster>-registry" by exact name, so
// a cache name that ever embedded a cluster name would be swept away with it.
func TestCacheIsNotClusterScoped(t *testing.T) {
	if strings.Contains(ContainerName, "registry") {
		t.Errorf("ContainerName %q must not collide with the per-cluster registry naming", ContainerName)
	}
}
