package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/lio"
	"github.com/jhoblitt/rooket/internal/registry"
)

// keep restores *p to its current value when the test ends.
func keep[T any](t *testing.T, p *T) {
	t.Helper()
	v := *p
	t.Cleanup(func() { *p = v })
}

// downHost is what the stubbed host reports to a down run.
type downHost struct {
	live []string // the kind clusters 'kind get clusters' lists
	// upLater are kind clusters 'kind get clusters' lists only from its second
	// call on: ones that come up after the command under test first looked.
	upLater []string
	// movedLater are kind clusters listed under podman on its first listing
	// and under docker from docker's second on: ones brought back up under the
	// other engine after the command under test first looked. Setting it puts
	// a docker on PATH, so that both engines are asked; each engine counts its
	// own listings and forgets only the clusters deleted under it.
	movedLater []string
	// docker puts a docker on PATH beside podman, whose kind lists dockerLive.
	docker     bool
	dockerLive []string
	containers []string // the container names the engine's 'ps -a' lists
	kindFails  bool     // 'kind get clusters' fails, as it does with the engine down
	// kindFailsFrom has an engine's 'kind get clusters' fail from its nth
	// listing on, as it does once that engine stops answering after the
	// command under test first looked.
	kindFailsFrom map[engine.Engine]int
	// kindDeleteFails has kind's delete of a cluster fail and leave it listed.
	kindDeleteFails bool
	psFails         bool // the engine's 'ps -a' fails
	// The kernel's iSCSI configuration, in writeFakeLIO's terms: a backstore
	// name and its backing path, each exported by a target.
	lio map[string]string
	// byPath are the target IQNs with a session logged in, each of which has a
	// /dev/disk/by-path link for its LUN 0.
	byPath []string
}

// stubDownHost puts stubs for every command a down run can reach on PATH, and
// nothing else, so no real kind, container engine, or iSCSI tool can run — as
// root or through sudo. Each stub appends its invocation to the returned log.
// kind stops listing a cluster once it has been asked to delete it, and starts
// listing h.upLater from its second listing on. targetcli fails every delete,
// as the real one does for an object that does not exist. The kernel's iSCSI
// configuration is read from h.lio, and the by-path links from h.byPath, each
// empty unless set, and never from the machine's own.
func stubDownHost(t *testing.T, h downHost) string {
	t.Helper()
	keep(t, &hostLIORoot)
	lioRoot := writeFakeLIO(t, h.lio)
	hostLIORoot = func() string { return lioRoot }
	keep(t, &hostByPathDir)
	byPath := t.TempDir()
	for _, iqn := range h.byPath {
		if err := os.Symlink("/dev/sdz", filepath.Join(byPath, iscsiByPathPrefix+iqn+iscsiByPathSuffix)); err != nil {
			t.Fatal(err)
		}
	}
	hostByPathDir = func() string { return byPath }
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	logCall := fmt.Sprintf(`printf '%%s %%s\n' "${0##*/}" "$*" >> %q`, logPath)
	printLines := func(lines []string) string {
		if len(lines) == 0 {
			return ":"
		}
		return "printf '%s\\n' '" + strings.Join(lines, "' '") + "'"
	}
	// Only shell builtins are on PATH, so a deletion is a marker file rather
	// than an edit of a list, and the listings are counted in a file. kind's
	// engine is its KIND_EXPERIMENTAL_PROVIDER, so the markers and counts are
	// per engine, and its log lines carry it.
	deleted := filepath.Join(dir, "deleted-")
	listings := filepath.Join(dir, "kind-listings-")
	kindList := fmt.Sprintf(`p=$KIND_EXPERIMENTAL_PROVIDER
case $p in
docker) always=%[6]q; first=; later=%[5]q; failFrom=%[7]d ;;
*) always=%[2]q; first=%[5]q; later=%[4]q; failFrom=%[8]d ;;
esac
n=0; [ -e %[1]q"$p" ] && read n < %[1]q"$p"; n=$((n+1)); printf '%%s\n' "$n" > %[1]q"$p"
if [ "$failFrom" -gt 0 ] && [ "$n" -ge "$failFrom" ]; then exit 1; fi
if [ "$n" -gt 1 ]; then set -- $always $later; else set -- $always $first; fi
for c in "$@"; do [ -e %[3]q"$p-$c" ] || printf '%%s\n' "$c"; done`,
		listings, strings.Join(h.live, " "), deleted, strings.Join(h.upLater, " "), strings.Join(h.movedLater, " "),
		strings.Join(h.dockerLive, " "), h.kindFailsFrom[engine.Docker], h.kindFailsFrom[engine.Podman])
	if h.kindFails {
		kindList = "exit 1"
	}
	kindDelete := fmt.Sprintf(`: > %q"$KIND_EXPERIMENTAL_PROVIDER-$4"`, deleted)
	if h.kindDeleteFails {
		kindDelete = "exit 1"
	}
	psList := printLines(h.containers)
	if h.psFails {
		psList = "exit 1"
	}
	stubs := map[string]string{
		"kind": fmt.Sprintf("case \"$*\" in\n\"get clusters\") %s ;;\n\"delete cluster --name \"*) %s ;;\nesac",
			kindList, kindDelete),
		"podman":    fmt.Sprintf("case \"$1\" in\nps) %s ;;\nesac", psList),
		"targetcli": "case \"$*\" in\n*\" delete \"*) echo 'No such path' >&2; exit 1 ;;\nesac",
		// Drops -n and runs what it was handed, when that is a path: itemized
		// steps name the resolved stub, while the grant probes name a bare
		// command and simply succeed.
		"sudo":      "shift\ncase \"$1\" in\n*/*) exec \"$@\" ;;\nesac",
		"iscsiadm":  ":",
		"systemctl": ":",
		"pkexec":    ":",
	}
	if h.docker || len(h.movedLater) > 0 {
		stubs["docker"] = stubs["podman"]
	}
	kindLogCall := fmt.Sprintf(`printf 'KIND_EXPERIMENTAL_PROVIDER=%%s kind %%s\n' "$KIND_EXPERIMENTAL_PROVIDER" "$*" >> %q`, logPath)
	for name, body := range stubs {
		logLine := logCall
		if name == "kind" {
			logLine = kindLogCall
		}
		script := "#!/bin/sh\n" + logLine + "\n" + body + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return logPath
}

// calls returns everything the stubs logged.
func calls(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// deletedTargets returns the target IQNs targetcli was asked to delete, sorted.
func deletedTargets(log string) []string {
	var iqns []string
	for line := range strings.SplitSeq(log, "\n") {
		if iqn, ok := strings.CutPrefix(line, "targetcli /iscsi delete "); ok {
			iqns = append(iqns, iqn)
		}
	}
	slices.Sort(iqns)
	return iqns
}

// workerTargets names the disk-0 target of each of a cluster's workers.
func workerTargets(name string, workers ...int) []string {
	var iqns []string
	for _, w := range workers {
		iqns = append(iqns, fmt.Sprintf("iqn.2003-01.local.rooket:%s-worker%d-disk0", name, w))
	}
	return iqns
}

// statusLines returns what a run printed other than its command traces.
func statusLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "+ ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// runDown is runDownUnder podman.
func runDown(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runDownUnder(t, engine.Podman, args...)
}

// runDownUnder runs down under eng with args as its command line and returns
// what it printed. Everything a run writes — down's flags, and the variables
// of the delete and block teardown steps it hands work to — is restored
// afterwards.
func runDownUnder(t *testing.T, eng engine.Engine, args ...string) (string, error) {
	t.Helper()
	if hostLIORoot() == lio.DefaultRoot {
		t.Fatal("runDown without stubDownHost would read this machine's iSCSI configuration")
	}
	for _, p := range []*string{&downName, &downIQNDate, &deleteName, &blockTeardownName, &blockTeardownIQNDate} {
		keep(t, p)
	}
	for _, p := range []*int{&downWorkers, &downDiskCount, &blockTeardownWorkers, &blockTeardownDiskCount} {
		keep(t, p)
	}
	for _, p := range []*bool{&downDeleteDisks, &downDeleteCache, &downSkipBlock, &downSkipCluster,
		&downAll, &downForce, &downDryRun, &downInclUnmanaged, &blockTeardownDeleteDisks} {
		keep(t, p)
	}
	keep(t, &containerEngine)
	containerEngine = eng
	// Deleting the cluster points $KUBECONFIG at it.
	t.Setenv("KUBECONFIG", "")
	t.Cleanup(func() {
		for _, name := range []string{"name", "workers", "disk-count", "iqn-date", "delete-disks", "delete-cache",
			"skip-block", "skip-cluster", "all", "force", "dry-run", "include-unmanaged"} {
			f := downCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
	if err := downCmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	var err error
	out := captureStdout(t, func() { err = downCmd.RunE(downCmd, nil) })
	return out, err
}

// A harness tears down idempotently: down of a cluster that was never brought
// up, or is already gone, succeeds and says so once, rather than guessing the
// flag's worker count and warning about targets that never existed.
func TestDownOfAnUnknownClusterSaysSoOnce(t *testing.T) {
	for _, args := range [][]string{
		{"--name", "w2-unknown"},
		{"--name", "w2-unknown", "--delete-disks"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			log := stubDownHost(t, downHost{})

			out, err := runDown(t, args...)
			if err != nil {
				t.Fatalf("down = %v, want success", err)
			}
			if lines := statusLines(out); len(lines) != 1 || !strings.Contains(lines[0], "nothing to tear down") {
				t.Errorf("down printed\n%s\nwant one line saying there is nothing to tear down", out)
			}
			got := calls(t, log)
			for _, cmd := range []string{"kind delete", "targetcli", "iscsiadm", "sudo", "pkexec"} {
				if strings.Contains(got, cmd+" ") {
					t.Errorf("down of an unknown cluster ran %s:\n%s", cmd, got)
				}
			}
			if _, err := os.Stat(clusterLockFile(t, "w2-unknown")); !os.IsNotExist(err) {
				t.Errorf("down of an unknown cluster left the lock file it took (stat: %v)", err)
			}
		})
	}
}

// clusterLockFile returns where a cluster's lock file lives.
func clusterLockFile(t *testing.T, name string) string {
	t.Helper()
	root, err := stateDirRoot()
	if err != nil {
		t.Fatal(err)
	}
	path, err := clusterLockPath(root, name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The full teardown leaves nothing of the cluster behind, its lock file
// included; a plain down keeps the cluster's state, and the lock file with it.
func TestDownRemovesTheLockFileOnlyWithTheState(t *testing.T) {
	for _, c := range []struct {
		args []string
		kept bool
	}{
		{args: nil, kept: true},
		{args: []string{"--delete-disks"}, kept: false},
	} {
		t.Run(fmt.Sprintf("%v", c.args), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			stubDownHost(t, downHost{})
			const name = "w2-lockfile"
			if err := writeShape(name, clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
				t.Fatal(err)
			}

			if _, err := runDown(t, append([]string{"--name", name}, c.args...)...); err != nil {
				t.Fatalf("down: %v", err)
			}
			_, err := os.Stat(clusterLockFile(t, name))
			if c.kept && err != nil {
				t.Errorf("down %v removed the lock file of a cluster whose state it kept: %v", c.args, err)
			}
			if !c.kept && !os.IsNotExist(err) {
				t.Errorf("down %v left the lock file of the cluster it removed (stat: %v)", c.args, err)
			}
		})
	}
}

// down --all --delete-disks removes the lock file of every cluster whose state
// it removes. A cluster another rooket holds keeps both, and the sweep says so.
func TestDownAllDeleteDisksRemovesTheLockFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubDownHost(t, downHost{})
	gone, busy := []string{"w2-all-a", "w2-all-b"}, "w2-all-busy"
	for _, n := range append(slices.Clone(gone), busy) {
		if _, err := ensureStateDir(n); err != nil {
			t.Fatal(err)
		}
		release, err := LockCluster(n)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	lockClusterExternally(t, busy)

	_, err := runDown(t, "--all", "--delete-disks", "--force")
	if err == nil || !strings.Contains(err.Error(), busy) {
		t.Errorf("down --all = %v, want the held cluster %q reported", err, busy)
	}
	for _, n := range gone {
		dir, _ := stateDirPath(n)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: state dir survived (stat: %v)", n, err)
		}
		if _, err := os.Stat(clusterLockFile(t, n)); !os.IsNotExist(err) {
			t.Errorf("%s: lock file survived its cluster (stat: %v)", n, err)
		}
	}
	dir, _ := stateDirPath(busy)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the state dir of a cluster another rooket holds was removed: %v", err)
	}
	if _, err := os.Stat(clusterLockFile(t, busy)); err != nil {
		t.Errorf("the lock file another rooket holds was removed: %v", err)
	}
}

// lockProbeEnv makes a re-exec of this test binary report whether the lock of
// the cluster owning the target IQN it names is held, appending the answer to
// the file lockProbeLogEnv names. The stub targetcli of probeLocksAtTargetDelete
// runs it, so the answer is the lock's state at the moment a target goes.
const (
	lockProbeEnv    = "ROOKET_TEST_LOCK_PROBE"
	lockProbeLogEnv = "ROOKET_TEST_LOCK_PROBE_LOG"
)

// probeLocksAtTargetDelete replaces the stub targetcli beside logPath with one
// that, before each target delete, re-runs the named test as a lock probe (see
// lockProbeEnv). The probe is a separate process because a flock is owned by
// an open file description: only another process sees the sweep's lock.
func probeLocksAtTargetDelete(t *testing.T, logPath, test string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s %%s\n' "${0##*/}" "$*" >> %[1]q
case "$*" in
"/iscsi delete "*) %[2]s="$3" %[3]s=%[1]q %[4]q -test.run='^%[5]s$' >/dev/null 2>&1 ;;
esac
case "$*" in
*" delete "*) echo 'No such path' >&2; exit 1 ;;
esac
exit 0
`, logPath, lockProbeEnv, lockProbeLogEnv, exe, test)
	if err := os.WriteFile(filepath.Join(filepath.Dir(logPath), "targetcli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// probeClusterLock is the lock probe's side: it records whether some process
// holds the lock of the cluster owning iqn. It only tries the flock and lets go
// at once, so it cannot itself keep anyone out.
func probeClusterLock(t *testing.T, iqn, logPath string) {
	_, name, ok := parseRooketIQN(iqn)
	if !ok {
		t.Fatalf("%q is not one of rooket's target IQNs", iqn)
	}
	state := "free"
	if f, err := os.Open(clusterLockFile(t, name)); err == nil {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
			state = "held"
		}
		f.Close()
	}
	out, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	fmt.Fprintf(out, "lock %s %s\n", name, state)
}

// down --all --delete-disks removes a cluster's iSCSI targets, disk images, and
// state only while it holds that cluster's lock. A cluster another rooket holds
// — an up that has not created its kind cluster yet, say — keeps all of them,
// and the sweep reports it and fails.
func TestDownAllTearsDownOnlyTheClustersItHolds(t *testing.T) {
	if iqn := os.Getenv(lockProbeEnv); iqn != "" {
		probeClusterLock(t, iqn, os.Getenv(lockProbeLogEnv))
		return
	}
	t.Setenv("HOME", t.TempDir())
	const live, parked, busy = "w2-held-live", "w2-held-parked", "w2-held-busy"
	log := stubDownHost(t, downHost{live: []string{live}})
	probeLocksAtTargetDelete(t, log, t.Name())
	images := map[string]string{}
	for _, n := range []string{live, parked, busy} {
		dir, err := ensureStateDir(n)
		if err != nil {
			t.Fatal(err)
		}
		images[n] = filepath.Join(dir, "worker0-disk0.img")
		if err := os.WriteFile(images[n], nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lockClusterExternally(t, busy)

	_, err := runDown(t, "--all", "--delete-disks", "--force")
	if err == nil || !strings.Contains(err.Error(), busy) || strings.Contains(err.Error(), live) || strings.Contains(err.Error(), parked) {
		t.Errorf("down --all = %v, want it to fail naming only the held cluster %q", err, busy)
	}
	got := calls(t, log)
	want := append(workerTargets(live, 0), workerTargets(parked, 0)...)
	slices.Sort(want)
	if deleted := deletedTargets(got); !slices.Equal(deleted, want) {
		t.Errorf("deleted targets %v, want only the unheld clusters' %v", deleted, want)
	}
	probes := 0
	for line := range strings.SplitSeq(got, "\n") {
		if rest, ok := strings.CutPrefix(line, "lock "); ok {
			probes++
			if !strings.HasSuffix(rest, " held") {
				t.Errorf("a target was deleted while its cluster's lock was not held: %s", line)
			}
		}
	}
	if probes != len(want) {
		t.Errorf("the lock was probed at %d target delete(s), want %d:\n%s", probes, len(want), got)
	}
	for _, n := range []string{live, parked} {
		dir, _ := stateDirPath(n)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: state dir survived (stat: %v)", n, err)
		}
	}
	if _, err := os.Stat(images[busy]); err != nil {
		t.Errorf("the disk image of a cluster another rooket holds was removed: %v", err)
	}
}

// A cluster with no kind cluster when down --all looked, whose kind cluster
// came up before the sweep locked it — an up that finished in between — is
// torn down as the live cluster it now is: its kind cluster is deleted, and
// confirmed gone, before the batch removes the targets its nodes use.
func TestDownAllDeletesAClusterThatCameUpAfterItLooked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const late = "w5-late"
	log := stubDownHost(t, downHost{upLater: []string{late}})
	dir, err := ensureStateDir(late)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker0-disk0.img"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--all", "--delete-disks", "--force"); err != nil {
		t.Fatalf("down --all: %v", err)
	}
	got := calls(t, log)
	deleted := strings.Index(got, "kind delete cluster --name "+late+"\n")
	target := strings.Index(got, "targetcli /iscsi delete "+workerTargets(late, 0)[0]+"\n")
	if deleted < 0 {
		t.Fatalf("down --all never deleted the kind cluster of %s, which came up after it looked:\n%s", late, got)
	}
	switch {
	case target < 0:
		t.Errorf("down --all never tore down the targets of %s:\n%s", late, got)
	case target < deleted:
		t.Errorf("down --all tore down the targets of %s before deleting its kind cluster:\n%s", late, got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state dir of %s survived (stat: %v)", late, err)
	}
}

// A cluster down --all saw live under one engine, but that was brought back up
// under the other before the sweep locked it, is deleted under the engine it
// runs under now, and confirmed gone there, before the sweep touches its disks:
// a plain sweep's zap of its images, or the batched teardown of its targets.
// Deleted and confirmed gone under the engine the sweep first saw, it would
// look gone while its nodes still used those disks.
func TestDownAllDeletesAClusterUnderTheEngineItNowRunsUnder(t *testing.T) {
	const moved = "y6-moved"
	for _, c := range []struct {
		args  []string
		disks string // the first call of the sweep's that reaches the cluster's disks
	}{
		{args: nil, disks: " images --format "},
		{args: []string{"--delete-disks"}, disks: "targetcli /iscsi delete " + workerTargets(moved, 0)[0] + "\n"},
	} {
		t.Run(fmt.Sprintf("%v", c.args), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			log := stubDownHost(t, downHost{movedLater: []string{moved}})
			dir, err := ensureStateDir(moved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "worker0-disk0.img"), nil, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := runDown(t, append([]string{"--all", "--force"}, c.args...)...); err != nil {
				t.Fatalf("down --all %v: %v", c.args, err)
			}
			got := calls(t, log)
			deleted := strings.Index(got, "KIND_EXPERIMENTAL_PROVIDER=docker kind delete cluster --name "+moved+"\n")
			if deleted < 0 {
				t.Fatalf("down --all %v never deleted %s under docker, where it runs now:\n%s", c.args, moved, got)
			}
			switch touched := strings.Index(got, c.disks); {
			case touched < 0:
				t.Errorf("down --all %v never reached the disks of %s (%q):\n%s", c.args, moved, c.disks, got)
			case touched < deleted:
				t.Errorf("down --all %v reached the disks of %s before deleting it under docker:\n%s", c.args, moved, got)
			}
		})
	}
}

// osdData is what writeOSDImage puts in an image, and what is gone from it
// once a zap has truncated it.
const osdData = "OSD-DATA"

// writeOSDImage gives cluster name a state dir holding one disk image with
// data in it, and returns both.
func writeOSDImage(t *testing.T, name string) (dir, img string) {
	t.Helper()
	dir, err := ensureStateDir(name)
	if err != nil {
		t.Fatal(err)
	}
	img = filepath.Join(dir, "worker0-disk0.img")
	if err := os.WriteFile(img, []byte(osdData), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, img
}

// An engine that answered down --all's scan but cannot answer the re-check
// under the locks may still run a cluster the sweep holds, so the sweep stops,
// naming the engine, before it deletes, zaps, or tears down anything of any
// cluster.
func TestDownAllStopsWhenAnEngineCannotAnswerTheRecheck(t *testing.T) {
	const name = "y7-recheck"
	for _, eng := range []engine.Engine{engine.Podman, engine.Docker} {
		for _, c := range []struct {
			desc string
			args []string
			live bool // the cluster is live under eng at the scan
		}{
			{desc: "live", live: true},
			{desc: "live, --delete-disks", args: []string{"--delete-disks"}, live: true},
			{desc: "state only, --delete-disks", args: []string{"--delete-disks"}},
		} {
			t.Run(eng.String()+", "+c.desc, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				h := downHost{docker: true, kindFailsFrom: map[engine.Engine]int{eng: 2}}
				if c.live {
					h.containers = []string{registry.ContainerName(name)}
					if eng == engine.Docker {
						h.dockerLive = []string{name}
					} else {
						h.live = []string{name}
					}
				}
				log := stubDownHost(t, h)
				dir, img := writeOSDImage(t, name)

				_, err := runDown(t, append([]string{"--all", "--force"}, c.args...)...)
				if want := "(" + eng.String() + " could not be queried); nothing was torn down"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("down --all %v = %v, want it to stop with %q", c.args, err, want)
				}
				got := calls(t, log)
				for _, cmd := range []string{"kind delete ", " rm ", " images ", "targetcli "} {
					if strings.Contains(got, cmd) {
						t.Errorf("down --all %v ran %q with %s unable to answer:\n%s", c.args, cmd, eng, got)
					}
				}
				if b, err := os.ReadFile(img); string(b) != osdData {
					t.Errorf("the disk image of %s holds %q (%v), want it untouched", name, b, err)
				}
				if _, err := os.Stat(dir); err != nil {
					t.Errorf("the state dir of %s went: %v", name, err)
				}
			})
		}
	}
}

// A cluster down --all saw under one engine, but that was brought back up under
// the other before the sweep locked it, is confirmed gone under the engine it
// runs under now. When its delete there leaves it running, the sweep leaves its
// disks and state alone and fails naming it, though the engine the scan saw it
// under no longer lists it.
func TestDownAllConfirmsAClusterGoneUnderTheEngineItNowRunsUnder(t *testing.T) {
	const moved = "y7-survivor"
	for _, args := range [][]string{nil, {"--delete-disks"}} {
		t.Run(fmt.Sprintf("%v", args), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			log := stubDownHost(t, downHost{movedLater: []string{moved}, kindDeleteFails: true})
			dir, img := writeOSDImage(t, moved)

			out, err := runDown(t, append([]string{"--all", "--force"}, args...)...)
			if err == nil || !strings.Contains(err.Error(), moved) {
				t.Errorf("down --all %v = %v, want it to fail naming %s", args, err, moved)
			}
			if want := fmt.Sprintf("cluster %q is still present after delete", moved); !strings.Contains(out, want) {
				t.Errorf("down --all %v printed\n%s\nwant %q", args, out, want)
			}
			got := calls(t, log)
			for _, cmd := range []string{" images ", "targetcli "} {
				if strings.Contains(got, cmd) {
					t.Errorf("down --all %v ran %q on a cluster still running under docker:\n%s", args, cmd, got)
				}
			}
			if b, err := os.ReadFile(img); string(b) != osdData {
				t.Errorf("the disk image of %s holds %q (%v), want it untouched", moved, b, err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("the state dir of %s went: %v", moved, err)
			}
		})
	}
}

// checkLeftAlone fails t unless a run left a cluster's disks and state as they
// were: its image's data, its kubeconfig kc, and its state dir, with no zap
// and no target teardown in log.
func checkLeftAlone(t *testing.T, log, dir, img, kc string) {
	t.Helper()
	for _, cmd := range []string{" images ", "targetcli "} {
		if strings.Contains(log, cmd) {
			t.Errorf("the run ran %q:\n%s", cmd, log)
		}
	}
	if b, err := os.ReadFile(img); string(b) != osdData {
		t.Errorf("the disk image holds %q (%v), want it untouched", b, err)
	}
	for _, p := range []string{dir, kc} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s went: %v", p, err)
		}
	}
}

// checkTornDown fails t unless a run went on to cluster name's disks: with
// --delete-disks it tore down the targets of its one worker and removed its
// state dir, and without it it zapped the image and removed the kubeconfig kc.
func checkTornDown(t *testing.T, log, name, dir, img, kc string, deleteDisks bool) {
	t.Helper()
	if deleteDisks {
		if got, want := deletedTargets(log), workerTargets(name, 0); !slices.Equal(got, want) {
			t.Errorf("deleted targets %v, want %v", got, want)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the state dir survived (stat: %v)", err)
		}
		return
	}
	if b, err := os.ReadFile(img); err != nil || string(b) == osdData {
		t.Errorf("the disk image holds %q (%v), want it zapped", b, err)
	}
	if _, err := os.Stat(kc); !os.IsNotExist(err) {
		t.Errorf("the kubeconfig survived (stat: %v)", err)
	}
}

// A delete that fails still lets down go on once a listing under the run's
// engine shows the cluster gone. A listing that fails shows nothing: the
// cluster may still be running on its disks, so down stops, naming the engine
// and what failed, before it removes the kubeconfig, zaps the images, or tears
// down the targets and the state dir.
func TestDownGoesOnOnlyOnceAListingShowsTheClusterGone(t *testing.T) {
	const name = "y7-unlisted"
	for _, eng := range []engine.Engine{engine.Podman, engine.Docker} {
		for _, listingFails := range []bool{true, false} {
			for _, args := range [][]string{nil, {"--delete-disks"}} {
				t.Run(fmt.Sprintf("%s, listing fails %v, %v", eng, listingFails, args), func(t *testing.T) {
					t.Setenv("HOME", t.TempDir())
					h := downHost{docker: true, kindDeleteFails: true}
					if listingFails {
						h.kindFailsFrom = map[engine.Engine]int{eng: 1}
					}
					log := stubDownHost(t, h)
					if err := writeShape(name, clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
						t.Fatal(err)
					}
					dir, img := writeOSDImage(t, name)
					kc := writeKubeconfig(t, name)

					_, err := runDownUnder(t, eng, append([]string{"--name", name}, args...)...)
					got := calls(t, log)
					if !listingFails {
						if err != nil {
							t.Fatalf("down %v under %s = %v, want success", args, eng, err)
						}
						checkTornDown(t, got, name, dir, img, kc, len(args) > 0)
						return
					}
					if want := fmt.Sprintf("kind get clusters under %s: exit status 1", eng); err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("down %v under %s = %v, want it to stop naming %q", args, eng, err, want)
					}
					checkLeftAlone(t, got, dir, img, kc)
				})
			}
		}
	}
}

// down --all deletes each cluster it holds and confirms it gone before it
// touches the cluster's disks, and a listing that answers without the cluster
// confirms it. A listing that fails confirms nothing: the cluster may still be
// running on its disks, so the sweep leaves its kubeconfig, images, targets,
// and state dir alone, says which engine failed and how, and fails naming the
// cluster.
func TestDownAllGoesOnOnlyOnceAListingShowsTheClusterGone(t *testing.T) {
	const name = "y7-unconfirmed"
	for _, eng := range []engine.Engine{engine.Podman, engine.Docker} {
		for _, listingFails := range []bool{true, false} {
			for _, args := range [][]string{nil, {"--delete-disks"}} {
				t.Run(fmt.Sprintf("%s, listing fails %v, %v", eng, listingFails, args), func(t *testing.T) {
					t.Setenv("HOME", t.TempDir())
					h := downHost{docker: true}
					if eng == engine.Docker {
						h.dockerLive = []string{name}
					} else {
						h.live = []string{name}
					}
					if listingFails {
						// The scan and the re-check under the locks are the
						// first two listings; the third is confirm-gone's.
						h.kindFailsFrom = map[engine.Engine]int{eng: 3}
					}
					log := stubDownHost(t, h)
					dir, img := writeOSDImage(t, name)
					kc := writeKubeconfig(t, name)

					out, err := runDown(t, append([]string{"--all", "--force"}, args...)...)
					got := calls(t, log)
					if want := fmt.Sprintf("KIND_EXPERIMENTAL_PROVIDER=%s kind delete cluster --name %s\n", eng, name); !strings.Contains(got, want) {
						t.Fatalf("down --all %v never deleted %s under %s:\n%s", args, name, eng, got)
					}
					if !listingFails {
						if err != nil {
							t.Fatalf("down --all %v = %v, want success", args, err)
						}
						checkTornDown(t, got, name, dir, img, kc, len(args) > 0)
						return
					}
					if err == nil || !strings.Contains(err.Error(), name) {
						t.Errorf("down --all %v = %v, want it to fail naming %s", args, err, name)
					}
					if want := fmt.Sprintf("kind get clusters under %s: exit status 1", eng); !strings.Contains(out, want) {
						t.Errorf("down --all %v printed\n%s\nwant it to say %q", args, out, want)
					}
					checkLeftAlone(t, got, dir, img, kc)
				})
			}
		}
	}
}

// down --all leaves no lock file behind a cluster it deleted that has no state
// dir — a live cluster rooket's only by its registry has none to remove — and
// keeps it beside a state dir the sweep leaves in place.
func TestDownAllRemovesTheLockFileOfADeletedClusterWithNoStateDir(t *testing.T) {
	for _, c := range []struct {
		args      []string
		stateKept bool
	}{
		{args: nil, stateKept: true},
		{args: []string{"--delete-disks"}, stateKept: false},
		{args: []string{"--delete-disks", "--skip-block"}, stateKept: true},
	} {
		t.Run(fmt.Sprintf("%v", c.args), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			const bare, withState = "w2-bare", "w2-with-state"
			stubDownHost(t, downHost{live: []string{bare, withState}, containers: []string{registry.ContainerName(bare)}})
			if _, err := ensureStateDir(withState); err != nil {
				t.Fatal(err)
			}

			if _, err := runDown(t, append([]string{"--all", "--force"}, c.args...)...); err != nil {
				t.Fatalf("down --all %v: %v", c.args, err)
			}
			if _, err := os.Stat(clusterLockFile(t, bare)); !os.IsNotExist(err) {
				t.Errorf("the lock file of %s, deleted with no state dir, survived (stat: %v)", bare, err)
			}
			_, err := os.Stat(clusterLockFile(t, withState))
			if c.stateKept && err != nil {
				t.Errorf("the lock file of %s went while its state dir was kept: %v", withState, err)
			}
			if !c.stateKept && !os.IsNotExist(err) {
				t.Errorf("the lock file of %s survived its state dir (stat: %v)", withState, err)
			}
		})
	}
}

// A state dir whose name cannot be a cluster's — made by hand, say — has no
// lock anyone could hold, so down --all --delete-disks removes it unlocked and
// the sweep succeeds.
func TestDownAllDeleteDisksRemovesAStateDirNoClusterCouldOwn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubDownHost(t, downHost{})
	root, err := stateDirRoot()
	if err != nil {
		t.Fatal(err)
	}
	const name = "Hand_Made"
	if validateClusterName(name) == nil {
		t.Fatalf("%q is a valid cluster name", name)
	}
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--all", "--delete-disks", "--force"); err != nil {
		t.Errorf("down --all = %v, want success", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("state root holds %v, want %s removed and nothing left", entries, name)
	}
}

// Anything up leaves behind keeps a cluster from being reported absent, and so
// does a probe that cannot answer.
func TestClusterLeftovers(t *testing.T) {
	const name = "w2-probe"
	cases := []struct {
		name     string
		host     downHost
		stateDir bool
		lio      map[string]string
		want     bool
	}{
		{name: "nothing anywhere", want: false},
		{name: "a state directory", stateDir: true, want: true},
		{name: "a backstore the kernel still holds", lio: map[string]string{name + "-worker4-disk0": "/gone.img"}, want: true},
		{name: "another cluster's backstore", lio: map[string]string{name + "-x-worker0-disk0": "/x.img"}, want: false},
		{name: "a kind cluster", host: downHost{live: []string{name}}, want: true},
		{name: "a registry container", host: downHost{containers: []string{name + "-registry"}}, want: true},
		{name: "kind unable to list clusters", host: downHost{kindFails: true}, want: true},
		{name: "the engine unable to list containers", host: downHost{psFails: true}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			stubDownHost(t, c.host)
			keep(t, &containerEngine)
			containerEngine = engine.Podman
			if c.stateDir {
				if _, err := ensureStateDir(name); err != nil {
					t.Fatal(err)
				}
			}
			lioRoot := writeFakeLIO(t, c.lio)

			var got bool
			captureStdout(t, func() { got = clusterLeftovers(name, lioRoot, "2003-01") })
			if got != c.want {
				t.Errorf("clusterLeftovers = %v, want %v", got, c.want)
			}
		})
	}
}

// down reads the kernel's iSCSI configuration from the stubbed host, not from
// the machine the test runs on: a cluster whose only remnant is a target the
// stub's configuration holds is found there and torn down.
func TestDownReadsTheStubbedHostsISCSIConfiguration(t *testing.T) {
	const name = "w2-kernel"
	disks := map[string]string{name + "-worker3-disk0": "/gone/worker3-disk0.img"}
	for _, c := range []struct {
		desc string
		host downHost
		args []string
	}{
		{
			desc: "down",
			host: downHost{lio: disks},
			args: []string{"--name", name, "--delete-disks"},
		},
		{
			desc: "down --all",
			host: downHost{live: []string{name}, containers: []string{registry.ContainerName(name)}, lio: disks},
			args: []string{"--all", "--delete-disks", "--force"},
		},
	} {
		t.Run(c.desc, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			log := stubDownHost(t, c.host)

			if _, err := runDown(t, c.args...); err != nil {
				t.Fatalf("%s: %v", c.desc, err)
			}
			if got, want := deletedTargets(calls(t, log)), workerTargets(name, 3); !slices.Equal(got, want) {
				t.Errorf("deleted targets %v, want the one the stubbed host holds, %v", got, want)
			}
		})
	}
}

// A cluster that recorded its shape is torn down to that shape.
func TestDownTearsDownTheRecordedWorkers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	log := stubDownHost(t, downHost{})
	const name = "w2-recorded"
	if err := writeShape(name, clusterShape{Workers: 2, DiskCount: 1, IQNDate: "2003-01"}); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--name", name, "--delete-disks"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got, want := deletedTargets(calls(t, log)), workerTargets(name, 0, 1); !slices.Equal(got, want) {
		t.Errorf("deleted targets %v, want the recorded workers' %v", got, want)
	}
	dir, err := stateDirPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state dir survived the full teardown (stat: %v)", err)
	}
}

// With no record there is no worker count to trust, so down tears down the
// disks it can find — here the one image the state dir holds.
func TestDownWithoutARecordTearsDownWhatItFinds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	log := stubDownHost(t, downHost{})
	const name = "w2-unrecorded"
	dir, err := stateDirPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker0-disk0.img"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--name", name, "--delete-disks"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got, want := deletedTargets(calls(t, log)), workerTargets(name, 0); !slices.Equal(got, want) {
		t.Errorf("deleted targets %v, want only the one found, %v", got, want)
	}
}

// A cluster with no record and no disks to find has nothing for a privileged
// run to do, and a privileged run is the one step that can prompt.
func TestDownWithNoDisksToFindRunsNothingPrivileged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	log := stubDownHost(t, downHost{})
	const name = "w2-diskless"
	dir, err := stateDirPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--name", name, "--delete-disks"); err != nil {
		t.Fatalf("down: %v", err)
	}
	got := calls(t, log)
	for _, cmd := range []string{"targetcli", "sudo", "pkexec"} {
		if strings.Contains(got, cmd+" ") {
			t.Errorf("down with no disks to tear down ran %s:\n%s", cmd, got)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state dir survived the full teardown (stat: %v)", err)
	}
}

// An explicit --workers still names the targets of a cluster with no record.
func TestDownWithoutARecordHonorsAnExplicitWorkers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	log := stubDownHost(t, downHost{})
	const name = "w2-explicit"
	dir, err := stateDirPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runDown(t, "--name", name, "--workers", "2", "--delete-disks"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got, want := deletedTargets(calls(t, log)), workerTargets(name, 0, 1); !slices.Equal(got, want) {
		t.Errorf("deleted targets %v, want the explicit --workers 2's %v", got, want)
	}
}
