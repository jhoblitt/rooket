package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/registry"
)

// portsHelperEnv makes a re-exec of this test binary act as a second rooket
// process reserving a port.
const portsHelperEnv = "ROOKET_TEST_PORTS_HELPER"

// recordPort plants a cluster's state dir with a recorded registry port, the
// way a previous 'rooket up' would have left it.
func recordPort(t *testing.T, cluster string, port int) {
	t.Helper()
	if err := writeRegistryPort(cluster, port); err != nil {
		t.Fatalf("writeRegistryPort(%s, %d): %v", cluster, port, err)
	}
}

// A stopped cluster holds no port, so a bind probe says its port is free. The
// allocator has to consult what other clusters recorded, or the next cluster up
// steals it and the owner loses its registry on the following run.
// allPortsBindable makes the probe say yes, so a test states what the kernel
// would report instead of inheriting the workstation's listeners.
func allPortsBindable(t *testing.T) {
	t.Helper()
	prev := portProbe
	portProbe = func(int) bool { return true }
	t.Cleanup(func() { portProbe = prev })
}

func TestFreePortForSkipsPortsOtherClustersRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	allPortsBindable(t)
	recordPort(t, "alpha", 5001)
	recordPort(t, "beta", 5002)

	got, err := freePortFor("gamma", 5001)
	if err != nil {
		t.Fatalf("freePortFor: %v", err)
	}
	if got == 5001 || got == 5002 {
		t.Errorf("handed out port %d, which another cluster has recorded", got)
	}

	// A cluster's own recording is not a conflict with itself: it is re-picking
	// precisely because that port went bad.
	if got, err := freePortFor("alpha", 5001); err != nil {
		t.Fatalf("freePortFor: %v", err)
	} else if got != 5001 {
		t.Errorf("alpha got %d, want its own recorded 5001 back", got)
	}
}

func TestRecordedPortsExcludesTheCaller(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	recordPort(t, "alpha", 5001)
	recordPort(t, "beta", 5002)

	taken := recordedPorts("alpha")
	if _, ok := taken[5001]; ok {
		t.Errorf("recordedPorts included the caller's own port")
	}
	if owner := taken[5002]; owner != "beta" {
		t.Errorf("taken[5002] = %q, want beta", owner)
	}
}

// A state dir with no recorded port must not be read as port 0, which would
// then be "taken" and skew nothing visible until something asked for port 0.
func TestRecordedPortsIgnoresClustersWithoutAPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := ensureStateDir("portless"); err != nil {
		t.Fatalf("ensureStateDir: %v", err)
	}
	recordPort(t, "beta", 5002)

	taken := recordedPorts("")
	if _, ok := taken[0]; ok {
		t.Errorf("a cluster with no recorded port was counted as holding port 0")
	}
	if len(taken) != 1 {
		t.Errorf("taken = %v, want only beta's port", taken)
	}
}

// The ports lock lives beside the cluster locks, so its name must be one no
// cluster can produce — a cluster called "ports" would otherwise share it and
// deadlock itself.
func TestPortsLockNameCannotCollideWithAClusterLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, err := LockPorts()
	if err != nil {
		t.Fatalf("LockPorts: %v", err)
	}
	defer release()

	root, err := stateDirRoot()
	if err != nil {
		t.Fatalf("stateDirRoot: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var portsLock string
	for _, e := range entries {
		if e.Name() != "ports" {
			portsLock = e.Name()
		}
	}
	if portsLock == "" || portsLock[0] != '.' {
		t.Fatalf("ports lock %q must start with a dot; a cluster name cannot", portsLock)
	}
	// Proof that the collision it avoids is real: "ports" is a valid name.
	if err := validateClusterName("ports"); err != nil {
		t.Errorf("expected \"ports\" to be a legal cluster name: %v", err)
	}
	clusterLock, err := clusterLockPath(root, "ports")
	if err != nil {
		t.Fatalf("clusterLockPath: %v", err)
	}
	if filepath.Base(clusterLock) == portsLock {
		t.Errorf("cluster %q and the ports allocator share lock file %q", "ports", portsLock)
	}
}

// TestConcurrentReservationsDoNotCollide is the property the allocation lock
// exists for, and it needs real processes: two clusters coming up together each
// hold their own cluster lock, so nothing else stops them choosing one port
// twice. Each child reserves for a distinct cluster against a shared state
// root and prints what it got.
func TestConcurrentReservationsDoNotCollide(t *testing.T) {
	if os.Getenv(portsHelperEnv) != "" {
		name := os.Getenv("ROOKET_TEST_CLUSTER")
		port, err := reserveRegistryPort(io.Discard, name, registry.ContainerName(name), 5001, false)
		if err != nil {
			os.Stdout.WriteString("ERR " + err.Error() + "\n")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "PORT %d\n", port)
		return
	}

	home := t.TempDir()
	const racers = 6

	var children []*exec.Cmd
	var pipes []io.Reader
	for i := range racers {
		c := exec.Command(os.Args[0], "-test.run=TestConcurrentReservationsDoNotCollide")
		c.Env = append(os.Environ(), portsHelperEnv+"=1", "HOME="+home,
			fmt.Sprintf("ROOKET_TEST_CLUSTER=racer-%d", i))
		out, err := c.StdoutPipe()
		if err != nil {
			t.Fatalf("StdoutPipe: %v", err)
		}
		children, pipes = append(children, c), append(pipes, out)
	}
	// Start them all before reading any, so they genuinely contend.
	for i, c := range children {
		if err := c.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
	}

	seen := map[int]int{}
	for i, p := range pipes {
		line, err := bufio.NewReader(p).ReadString('\n')
		if err != nil {
			t.Fatalf("child %d produced nothing: %v", i, err)
		}
		var port int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "PORT %d", &port); err != nil {
			t.Fatalf("child %d: %s", i, strings.TrimSpace(line))
		}
		if prev, dup := seen[port]; dup {
			t.Errorf("racer-%d and racer-%d were both given port %d", prev, i, port)
		}
		seen[port] = i
		_ = children[i].Wait()
	}
	if len(seen) != racers {
		t.Errorf("got %d distinct ports for %d clusters", len(seen), racers)
	}
}
