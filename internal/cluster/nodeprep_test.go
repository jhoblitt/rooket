package cluster

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestNodePrepScript(t *testing.T) {
	script := nodePrepScript([]string{"/dev/sdb", "/dev/sde"})

	for _, want := range []string{
		"mount -o remount,rw /sys",
		"command -v vgs",
		"command -v cryptsetup",
		"apt-get install -y lvm2 cryptsetup",
		"dmsetup mknodes",
		"rm -f '/dev/sdb' || { echo 'ROOKET_FAIL:mask foreign OSD device /dev/sdb'; rc=1; }",
		"rm -f '/dev/sde' || { echo 'ROOKET_FAIL:mask foreign OSD device /dev/sde'; rc=1; }",
		"echo ROOKET_DONE",
		"exit $rc",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	if noMask := nodePrepScript(nil); strings.Contains(noMask, "rm -f") {
		t.Errorf("script with no foreign devices should not mask anything:\n%s", noMask)
	}
}

func TestRegistryScript(t *testing.T) {
	script := registryScript("rook-registry", 5001)
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

// TestEvalWrapperSurvivesStdinReaders proves the `sh -c 'eval "$(cat)"'`
// transport is immune to script commands that read stdin: with the script
// fed directly as sh's stdin, a child like apt-get/dpkg that reads fd 0
// would consume the remaining script lines — silently skipping the
// safety-critical masking. The wrapper slurps the whole script first.
func TestEvalWrapperSurvivesStdinReaders(t *testing.T) {
	script := "echo first\nhead -c 100000 >/dev/null\necho ROOKET_DONE\n"
	cmd := exec.Command("sh", "-c", `eval "$(cat)"`)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper run failed: %v\n%s", err, out)
	}
	for _, want := range []string{"first", "ROOKET_DONE"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output missing %q (stdin reader consumed the script?):\n%s", want, out)
		}
	}

	// Sanity-check the failure mode being defended against: the same script
	// fed as sh's stdin loses everything after the stdin-reading command.
	direct := exec.Command("sh")
	direct.Stdin = strings.NewReader(script)
	out, err = direct.CombinedOutput()
	if err != nil {
		t.Fatalf("direct run failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "ROOKET_DONE") {
		t.Skip("this sh does not exhibit the stdin-consumption hazard; wrapper remains harmless")
	}
}

func TestNodeScriptErrors(t *testing.T) {
	t.Run("markers become per-operation errors", func(t *testing.T) {
		out := "junk\nROOKET_FAIL:dmsetup mknodes\nmore\nROOKET_FAIL:mask foreign OSD device /dev/sdb\nROOKET_DONE\n"
		errs := nodeScriptErrors("worker2", out, errors.New("exit status 1"))
		if len(errs) != 2 {
			t.Fatalf("got %d errors, want 2: %v", len(errs), errs)
		}
		if got := errs[0].Error(); got != "dmsetup mknodes on node worker2" {
			t.Errorf("errs[0] = %q", got)
		}
		if got := errs[1].Error(); got != "mask foreign OSD device /dev/sdb on node worker2" {
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
