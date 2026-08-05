package registry

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhoblitt/rooket/internal/zot"
)

func TestGenerateConfig(t *testing.T) {
	raw, err := GenerateConfig()
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg zot.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not valid JSON: %v\n%s", err, raw)
	}

	// docker engines push Docker schema-2 manifests; without docker2s2 zot
	// rejects them with HTTP 415 and 'rooket build push' breaks under
	// --engine docker.
	if !strings.Contains(strings.Join(cfg.HTTP.Compat, ","), "docker2s2") {
		t.Errorf("http.compat must include docker2s2, got %v", cfg.HTTP.Compat)
	}
	// Cluster nodes reach the registry over the kind network; loopback-only
	// exposure is the host port mapping's job, not the listener's.
	if cfg.HTTP.Address != "0.0.0.0" {
		t.Errorf("http.address = %q, want 0.0.0.0 (nodes connect over the container network)", cfg.HTTP.Address)
	}
	if cfg.HTTP.Port != "5000" {
		t.Errorf("http.port = %q, want 5000 (the port InClusterAddr and the nodes' hosts.toml use)", cfg.HTTP.Port)
	}
	// Every re-push of the same tag orphans the previous manifest; without
	// gc those blobs accumulate for the cluster's whole life.
	if !cfg.Storage.GC {
		t.Error("storage.gc must be on")
	}
	if cfg.Storage.RootDirectory != zot.StoragePath {
		t.Errorf("rootDirectory = %q, want %q", cfg.Storage.RootDirectory, zot.StoragePath)
	}
	// The per-cluster registry is a pure push target. A sync section here
	// would turn it into a second mirror, shadowing the shared cache and
	// re-fetching upstreams once per cluster.
	if cfg.Extensions != nil {
		t.Errorf("config must carry no extensions, got %+v", cfg.Extensions)
	}
}

func TestRunArgs(t *testing.T) {
	cfg := Config{
		Name:           "krbd-registry",
		HostPort:       5001,
		Network:        "kind",
		HostConfigPath: "/home/u/.local/share/rooket/krbd/registry-config.json",
	}
	args := runArgs(cfg, "abc123")
	joined := strings.Join(args, " ")

	// The config is generated under the user's home, whose SELinux type a
	// confined container cannot read. Without a relabel option zot exits at
	// startup on any enforcing host.
	if !slices.Contains(args, cfg.HostConfigPath+":"+zot.ConfigPath+":ro,z") {
		t.Errorf("config mount must carry the SELinux relabel option:\n%s", joined)
	}
	// The container records which config it was built from, so the next run
	// can tell a current registry from one that needs recreating without
	// relying on state held in the process that wrote the file.
	if !slices.Contains(args, "--label") || !slices.Contains(args, ConfigLabel+"=abc123") {
		t.Errorf("container must be labelled with its config sum:\n%s", joined)
	}

	// One zot pin serves both the cache and every registry; a second image
	// here would reintroduce a separate bootstrap pull to track and bump.
	if !slices.Contains(args, zot.Image) {
		t.Errorf("args must run the shared zot image %q:\n%s", zot.Image, joined)
	}
	if !strings.HasSuffix(joined, zot.Image+" serve "+zot.ConfigPath) {
		t.Errorf("zot needs its config path as the serve argument:\n%s", joined)
	}
	// The registry serves this host only: 127.0.0.1 for the host side,
	// the kind network for nodes — never other machines.
	if !slices.Contains(args, "127.0.0.1:5001:5000") {
		t.Errorf("host port must bind loopback only:\n%s", joined)
	}
	if !slices.Contains(args, "--restart=always") {
		t.Errorf("registry must restart with the engine:\n%s", joined)
	}
	if !slices.Contains(args, "--network=kind") {
		t.Errorf("registry must join the cluster network:\n%s", joined)
	}

	cfg.Network = ""
	if joined := strings.Join(runArgs(cfg, "abc123"), " "); strings.Contains(joined, "--network") {
		t.Errorf("empty Network must add no --network flag:\n%s", joined)
	}
}

// TestWaitReadyAcceptsAServingRegistry covers the reason the wait exists:
// 'run -d' returns as soon as the engine accepts the container, so without
// this a zot that exits at startup — an unreadable config, an image that
// cannot run on this architecture — is reported as a registry that came up,
// and the failure resurfaces much later as a confusing push error.
func TestWaitReadyAcceptsAServingRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatalf("parse httptest port from %q: %v", srv.URL, err)
	}
	if err := WaitReady(io.Discard, port, 5*time.Second); err != nil {
		t.Errorf("WaitReady against a serving registry: %v", err)
	}
}

func TestWaitReadyFailsWhenNothingServes(t *testing.T) {
	// Bind and immediately release a port: nothing is listening on it, and
	// it is one the OS just confirmed is not in use by something else.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	err = WaitReady(io.Discard, port, 300*time.Millisecond)
	if err == nil {
		t.Fatal("WaitReady reported a dead registry as ready")
	}
	// The message is the whole point: it is what turns a silent registry
	// failure into something the user can act on.
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("error should name the port that never answered, got: %v", err)
	}
}
