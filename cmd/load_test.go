package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/registry"
)

// load's help shows what each example image is pushed as, which a reader
// copies into a manifest; it must be the name load pushes.
func TestLoadHelpExamplesAreWhatLoadPushes(t *testing.T) {
	lines := strings.Split(loadCmd.Long, "\n")
	examples := 0
	for i, line := range lines {
		src, ok := strings.CutPrefix(strings.TrimSpace(line), "rooket load ")
		if !ok {
			continue
		}
		var pushed string
		if i+1 < len(lines) {
			pushed, ok = strings.CutPrefix(strings.TrimSpace(lines[i+1]), "# pushes as ")
		}
		if !ok {
			t.Errorf("example %q is not followed by what it pushes as", src)
			continue
		}
		examples++
		if want := "localhost:5001/" + imageBasename(src); pushed != want {
			t.Errorf("help says %s pushes as %s; load pushes it as %s", src, pushed, want)
		}
	}
	if examples == 0 {
		t.Fatal("found no examples in load's help")
	}
}

// loadHost is what the stubbed engines report to a load run.
type loadHost struct {
	running     []string // the containers podman's 'ps' lists
	stopped     []string // the containers only podman's 'ps -a' lists
	podmanFails bool     // podman's 'ps' fails
	// docker puts a docker on PATH beside podman, a host where both engines
	// work, whose 'ps' lists dockerRunning, or fails when dockerFails.
	docker        bool
	dockerRunning []string
	dockerFails   bool
}

// runLoad runs load of image with args as its flags, with a podman that logs
// what it is asked to do, and lists h's containers, on PATH (and a docker that
// logs its calls prefixed with its name, if h has one), and returns that log.
// load runs under podman.
func runLoad(t *testing.T, h loadHost, image string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	list := func(names ...string) string {
		if len(names) == 0 {
			return ":"
		}
		return "printf '%s\\n' '" + strings.Join(names, "' '") + "'"
	}
	stub := func(name, logPrefix string, running, stopped []string, fails bool) {
		listAll, listRunning := list(slices.Concat(running, stopped)...), list(running...)
		if fails {
			listAll, listRunning = "exit 1", "exit 1"
		}
		script := fmt.Sprintf("#!/bin/sh\nprintf '%s%%s\\n' \"$*\" >> %q\ncase \"$*\" in\n\"ps -a \"*) %s ;;\n\"ps \"*) %s ;;\nesac\n",
			logPrefix, logPath, listAll, listRunning)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("podman", "", h.running, h.stopped, h.podmanFails)
	if h.docker {
		stub("docker", "docker ", h.dockerRunning, nil, h.dockerFails)
	}
	t.Setenv("PATH", dir)
	keep(t, &containerEngine)
	containerEngine = engine.Podman
	// Every port reads as free, so a run that falls back to picking one does
	// not depend on what this machine listens on.
	keep(t, &portProbe)
	portProbe = func(int) bool { return true }
	keep(t, &loadName)
	keep(t, &loadRegistryPort)
	t.Cleanup(func() {
		for _, name := range []string{"name", "registry-port"} {
			f := loadCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
	if err := loadCmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	var err error
	captureStdout(t, func() { err = loadCmd.RunE(loadCmd, []string{image}) })
	b, rerr := os.ReadFile(logPath)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	return string(b), err
}

// load pushes to the registry its cluster recorded, when that registry is
// running, or to the one --registry-port names. A cluster with no recorded
// registry, or whose registry is not running — removed by a plain 'rooket
// down', or stopped by a reboot — has nothing to push to, so load says so
// before it tags or pushes anything, rather than failing to connect.
func TestLoadPushesOnlyToARegistryItKnows(t *testing.T) {
	const name, image = "w5-load", "rook/ceph:dev"
	reg := registry.ContainerName(name)
	const notRunning = `no running registry for cluster "w5-load" under podman (is it up?)`
	for _, tc := range []struct {
		desc     string
		recorded int
		host     loadHost
		args     []string
		wantPush string // the ref load pushes, or "" for none
		wantErr  string
	}{
		{desc: "recorded port", recorded: 5123, host: loadHost{running: []string{reg}}, wantPush: "localhost:5123/rook/ceph:dev"},
		{desc: "explicit port", args: []string{"--registry-port", "5234"}, wantPush: "localhost:5234/rook/ceph:dev"},
		{desc: "neither", wantErr: `no registry for cluster "w5-load" (is it up?)`},
		{desc: "recorded port, registry stopped", recorded: 5123, host: loadHost{stopped: []string{reg}}, wantErr: notRunning},
		{desc: "recorded port, registry removed", recorded: 5123, wantErr: notRunning},
		// The push goes to the registry's published host port, which reaches it
		// whichever engine runs it.
		{
			desc: "recorded port, registry under the other engine", recorded: 5123,
			host: loadHost{docker: true, dockerRunning: []string{reg}}, wantPush: "localhost:5123/rook/ceph:dev",
		},
		{
			desc: "recorded port, registry under neither engine", recorded: 5123, host: loadHost{docker: true},
			wantErr: `no running registry for cluster "w5-load" under podman or docker (is it up?)`,
		},
		// An engine that cannot answer may be the one running the registry.
		{
			desc: "recorded port, registry not under podman, docker unable to answer", recorded: 5123,
			host:    loadHost{docker: true, dockerFails: true},
			wantErr: `no running registry for cluster "w5-load" under podman (is it up?); could not ask docker (exit status 1)`,
		},
		{
			desc: "recorded port, registry under podman, docker unable to answer", recorded: 5123,
			host: loadHost{running: []string{reg}, docker: true, dockerFails: true}, wantPush: "localhost:5123/rook/ceph:dev",
		},
		{
			desc: "recorded port, podman alone, unable to answer", recorded: 5123, host: loadHost{podmanFails: true},
			wantErr: `could not ask podman (exit status 1) whether the registry of cluster "w5-load" is running`,
		},
		{
			desc: "recorded port, neither engine able to answer", recorded: 5123,
			host:    loadHost{podmanFails: true, docker: true, dockerFails: true},
			wantErr: `could not ask podman (exit status 1) or docker (exit status 1) whether the registry of cluster "w5-load" is running`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tc.recorded != 0 {
				if err := writeRegistryPort(name, tc.recorded); err != nil {
					t.Fatal(err)
				}
			}

			calls, err := runLoad(t, tc.host, image, append([]string{"--name", name}, tc.args...)...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("load = %v, want %q", err, tc.wantErr)
				}
				if err != nil && strings.Contains(err.Error(), "\n") {
					t.Errorf("load's error runs over more than one line:\n%v", err)
				}
				for line := range strings.SplitSeq(calls, "\n") {
					if strings.HasPrefix(line, "tag ") || strings.HasPrefix(line, "push ") {
						t.Errorf("load of a cluster with no running registry ran podman %s", line)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("load = %v, want success", err)
			}
			if want := "push --tls-verify=false " + tc.wantPush; !strings.Contains(calls, want) {
				t.Errorf("podman ran\n%s\nwant %q", calls, want)
			}
		})
	}
}

// load's help says when it refuses to push, and how to push anyway.
func TestLoadHelpSaysWhenItRefuses(t *testing.T) {
	for _, want := range []string{"registry is not running", "--registry-port"} {
		if !strings.Contains(loadCmd.Long, want) {
			t.Errorf("load's help does not mention %q:\n%s", want, loadCmd.Long)
		}
	}
}
