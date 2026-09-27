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
)

// keep restores *p to its current value when the test ends.
func keep[T any](t *testing.T, p *T) {
	t.Helper()
	v := *p
	t.Cleanup(func() { *p = v })
}

// downHost is what the stubbed host reports to a down run.
type downHost struct {
	live       []string // the kind clusters 'kind get clusters' lists
	containers []string // the container names the engine's 'ps -a' lists
	kindFails  bool     // 'kind get clusters' fails, as it does with the engine down
	psFails    bool     // the engine's 'ps -a' fails
}

// stubDownHost puts stubs for every command a down run can reach on PATH, and
// nothing else, so no real kind, container engine, or iSCSI tool can run — as
// root or through sudo. Each stub appends its invocation to the returned log.
// kind stops listing a cluster once it has been asked to delete it. targetcli
// fails every delete, as the real one does for an object that does not exist.
func stubDownHost(t *testing.T, h downHost) string {
	t.Helper()
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
	// than an edit of a list.
	deleted := filepath.Join(dir, "deleted-")
	kindList := fmt.Sprintf(`for n in %s; do [ -e %q"$n" ] || printf '%%s\n' "$n"; done`,
		strings.Join(h.live, " "), deleted)
	if h.kindFails {
		kindList = "exit 1"
	}
	psList := printLines(h.containers)
	if h.psFails {
		psList = "exit 1"
	}
	stubs := map[string]string{
		"kind": fmt.Sprintf("case \"$*\" in\n\"get clusters\") %s ;;\n\"delete cluster --name \"*) : > %q\"$4\" ;;\nesac",
			kindList, deleted),
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
	for name, body := range stubs {
		script := "#!/bin/sh\n" + logCall + "\n" + body + "\nexit 0\n"
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

// runDown runs down with args as its command line and returns what it
// printed. Everything a run writes — down's flags, and the variables of the
// delete and block teardown steps it hands work to — is restored afterwards.
func runDown(t *testing.T, args ...string) (string, error) {
	t.Helper()
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
	containerEngine = engine.Podman
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
