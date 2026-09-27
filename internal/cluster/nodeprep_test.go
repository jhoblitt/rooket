package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/jhoblitt/rooket/internal/engine"
)

func TestNodePrepScript(t *testing.T) {
	script := nodePrepScript([]string{"/dev/sdb", "/dev/sde"})

	for _, want := range []string{
		"mount -o remount,rw /sys",
		`systemctl show -p DefaultTasksMax --value`,
		`DefaultTasksMax=infinity\n' > /etc/systemd/system.conf.d/90-rooket-tasksmax.conf`,
		"systemctl daemon-reload",
		`ROOKET_FAIL:raise systemd DefaultTasksMax`,
		"command -v vgs",
		"command -v cryptsetup",
		"apt-get install -y lvm2 cryptsetup",
		// Device mask: enumerate every block AND char node on the /dev tmpfs
		// (find -xdev spares the devpts/shm submounts) and remove all but the
		// allowlist plus this node's own OSD disks.
		`for dev in $(find /dev -xdev \( -type b -o -type c \) 2>/dev/null); do`,
		`case " ` + allowedDevs + ` /dev/sdb /dev/sde " in *" $dev "*) continue ;; esac`,
		`rm -f "$dev" || { echo "ROOKET_FAIL:mask host device $dev"; rc=1; }`,
		"echo ROOKET_DONE",
		"exit $rc",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// The allowlist must keep the essentials and never a host-access device.
	for _, keep := range []string{"/dev/null", "/dev/kmsg", "/dev/net/tun", "/dev/mapper/control"} {
		if !strings.Contains(allowedDevs, keep) {
			t.Errorf("allowedDevs missing essential %q: %s", keep, allowedDevs)
		}
	}
	for _, banned := range []string{"/dev/mem", "/dev/kvm", "/dev/sd", "/dev/nvme", "/dev/sg", "/dev/dm-", "/dev/snapshot"} {
		if strings.Contains(allowedDevs, banned) {
			t.Errorf("allowedDevs must not allow host device %q: %s", banned, allowedDevs)
		}
	}

	// The obsolete dmsetup mknodes workaround must be gone: materialising the
	// host's dm nodes is exactly what exposed the host root LVM to the node.
	if strings.Contains(script, "dmsetup mknodes") {
		t.Errorf("script must not run dmsetup mknodes:\n%s", script)
	}

	// A node with no OSD disks (the control-plane) still runs the mask loop and
	// keeps the allowlist, but names no disk to keep.
	noKeep := nodePrepScript(nil)
	if !strings.Contains(noKeep, `for dev in $(find /dev -xdev \( -type b -o -type c \) 2>/dev/null); do`) {
		t.Errorf("no-keep script should still mask devices:\n%s", noKeep)
	}
	if !strings.Contains(noKeep, "/dev/null /dev/zero") {
		t.Errorf("no-keep script should keep the allowlist:\n%s", noKeep)
	}
	if strings.Contains(noKeep, "/dev/sd") {
		t.Errorf("no-keep script should name no disk to keep:\n%s", noKeep)
	}
}

func TestNodePrepScriptRBD(t *testing.T) {
	script := nodePrepScript([]string{"/dev/sdb"})

	for _, want := range []string{
		"modprobe rbd 2>/dev/null || true",
		`rbd_major=$(awk '$2 == "rbd" { print $1 }' /proc/devices)`,
		`minor=$((i << 4))`,
		`mknod "/dev/rbd$i" b "$rbd_major" "$minor"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// The rbd nodes must be in the prune's keep-set (allowedDevs), spared by
	// the same case statement that spares the essentials and this node's own
	// OSD disk.
	caseLine := fmt.Sprintf(`case " %s /dev/sdb " in *" $dev "*) continue ;; esac`, allowedDevs)
	if !strings.Contains(script, caseLine) {
		t.Errorf("prune keep-set does not match allowedDevs+keepDevs:\n%s", script)
	}
	for i := 0; i < rbdMaxDevices; i++ {
		dev := fmt.Sprintf("/dev/rbd%d", i)
		if !strings.Contains(allowedDevs, dev) {
			t.Errorf("allowedDevs missing %q: %s", dev, allowedDevs)
		}
	}

	// The rbd step is best-effort: unlike every other operation in this
	// script, it must never contribute a ROOKET_FAIL marker or touch rc.
	rbdSection := script[strings.Index(script, "modprobe rbd"):strings.Index(script, "echo ROOKET_DONE")]
	if strings.Contains(rbdSection, "ROOKET_FAIL") {
		t.Errorf("rbd step must not emit ROOKET_FAIL:\n%s", rbdSection)
	}
	if strings.Contains(rbdSection, "rc=1") {
		t.Errorf("rbd step must not set rc=1:\n%s", rbdSection)
	}
}

func TestRegistryScript(t *testing.T) {
	script := containerdScript("rook-registry", 5001, "", nil)
	for _, want := range []string{
		"mkdir -p '/etc/containerd/certs.d/localhost:5001'",
		`server = "http://rook-registry:5000"`,
		`capabilities = ["pull", "resolve", "push"]`,
		"ROOKET_EOF",
		"echo ROOKET_DONE",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestCacheScript(t *testing.T) {
	script := containerdScript("rook-registry", 5001, "rooket-cache:5000", []string{"quay.io", "registry.k8s.io"})

	for _, want := range []string{
		"mkdir -p '/etc/containerd/certs.d/quay.io'",
		"mkdir -p '/etc/containerd/certs.d/registry.k8s.io'",
		`[host."http://rooket-cache:5000/v2/quay.io"]`,
		`[host."http://rooket-cache:5000/v2/registry.k8s.io"]`,
		`capabilities = ["pull", "resolve"]`,
		"echo ROOKET_DONE",
		"exit $rc",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// override_path is what stops containerd appending its own /v2 to a host
	// URL that already carries the upstream's prefix inside the cache.
	if strings.Count(script, "override_path = true") != 2 {
		t.Errorf("every cache mirror needs override_path = true:\n%s", script)
	}

	// The cache blocks must not set 'server': omitting it leaves the upstream
	// namespace as the fallback, which is what keeps the cache a soft
	// dependency. Exactly one 'server' line may appear — the registry's, whose
	// mirror is authoritative rather than an optimisation.
	if got := strings.Count(script, "server ="); got != 1 {
		t.Errorf("want exactly 1 'server' line (the registry's), got %d; a cache mirror with one loses upstream fallback:\n%s", got, script)
	}

	// Likewise push: the per-cluster registry is pushed to, the cache never is.
	if got := strings.Count(script, `"push"`); got != 1 {
		t.Errorf("want exactly 1 push capability (the registry's), got %d:\n%s", got, script)
	}

	// With the cache down, the registry must still be wired and no cache mirror
	// written — nodes then pull upstream exactly as before the cache existed.
	noCache := containerdScript("rook-registry", 5001, "rooket-cache:5000", nil)
	for _, want := range []string{"/etc/containerd/certs.d/localhost:5001", "echo ROOKET_DONE"} {
		if !strings.Contains(noCache, want) {
			t.Errorf("registry wiring must survive a missing cache, want %q:\n%s", want, noCache)
		}
	}
	if strings.Contains(noCache, "rooket-cache:5000") {
		t.Errorf("no cache mirror may be written when no upstreams are proxied:\n%s", noCache)
	}
}

// TestRunNodeScriptSurvivesStdinReaders proves runNodeScript's transport is
// immune to script commands that read stdin: with the script fed directly as
// sh's stdin, a child like apt-get/dpkg that reads fd 0 would consume the
// remaining script lines — silently skipping the safety-critical masking. The
// engine is a stub that drops "exec -i <node>" and runs the rest of the argv
// on this host, so the real transport runs against the local sh.
func TestRunNodeScriptSurvivesStdinReaders(t *testing.T) {
	// The padding puts the sentinel past the first block a block-reading
	// shell such as dash pulls in before it runs cat.
	script := "echo first\ncat >/dev/null\n#" + strings.Repeat("x", 32<<10) + "\necho ROOKET_DONE\n"

	// A pass proves nothing unless this sh, fed the script on stdin, loses the
	// sentinel to cat.
	direct := exec.Command("sh")
	direct.Stdin = strings.NewReader(script)
	out, err := direct.CombinedOutput()
	if err != nil {
		t.Fatalf("direct run failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "ROOKET_DONE") {
		t.Skip("this sh buffered the script past its sentinel before starting cat, so the stdin-consumption hazard cannot reproduce here")
	}

	stub := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// runNodeScript sleeps between retries of a run that loses the sentinel;
	// the bubble's fake clock spares a regression from sitting through them.
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		if errs := runNodeScript(engine.Engine(stub), &buf, "node", script); len(errs) != 0 {
			t.Fatalf("script did not run to completion: %v\n%s", errors.Join(errs...), buf.String())
		}
	})
}

func TestNodeScriptErrors(t *testing.T) {
	t.Run("markers become per-operation errors", func(t *testing.T) {
		out := "junk\nROOKET_FAIL:remount /sys read-write\nmore\nROOKET_FAIL:mask host device /dev/nvme0n1\nROOKET_DONE\n"
		errs := nodeScriptErrors("worker2", out, errors.New("exit status 1"))
		if len(errs) != 2 {
			t.Fatalf("got %d errors, want 2: %v", len(errs), errs)
		}
		if got := errs[0].Error(); got != "remount /sys read-write on node worker2" {
			t.Errorf("errs[0] = %q", got)
		}
		if got := errs[1].Error(); got != "mask host device /dev/nvme0n1 on node worker2" {
			t.Errorf("errs[1] = %q", got)
		}
	})

	t.Run("failed run without markers yields one generic error", func(t *testing.T) {
		errs := nodeScriptErrors("worker", "no markers here", errors.New("exit status 1"))
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "node script on node worker") {
			t.Fatalf("got %v", errs)
		}
	})

	t.Run("clean run yields nothing", func(t *testing.T) {
		if errs := nodeScriptErrors("worker", "all good\nROOKET_DONE", nil); len(errs) != 0 {
			t.Fatalf("got %v", errs)
		}
	})
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/dev/sdb"); got != "'/dev/sdb'" {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("got %q", got)
	}
}
