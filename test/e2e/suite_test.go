//go:build e2e

// Package e2e contains rooket's end-to-end integration tests: they drive the
// real `rooket` binary to bring a Rook/Ceph cluster up on kind and tear it down,
// asserting the cluster provisions correctly and settles.
//
// Prerequisites (the suite Skips if ROOK_DIR is unset):
//   - ROOK_DIR points at a Rook source tree (its charts are deployed).
//   - The iSCSI OSD block devices already exist ('rooket block setup', which
//     needs root); the suite runs up/down with --skip-block by default.
//   - The container engine (podman by default, or docker via $ROOKET_ENGINE),
//     kind, kubectl, helm and a Go toolchain are on PATH.
//
// Run with:  go test -tags e2e ./test/e2e/ -timeout 60m
package e2e

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	rookDir     = os.Getenv("ROOK_DIR")
	clusterName = envOr("ROOKET_NAME", "rook")
	workers     = envOr("ROOKET_WORKERS", "3")
	skipBlock   = envOr("ROOKET_SKIP_BLOCK", "true") == "true"
	eng         = envOr("ROOKET_ENGINE", "podman") // engine for the suite's own container/kind calls

	rooketBin string // built in BeforeSuite
	kubeCtx   string // kind-<name>
	stateDir  string // ~/.local/share/rooket/<name>
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func numWorkers() int { n, _ := strconv.Atoi(workers); return n }

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "rooket e2e: up/down")
}

var _ = BeforeSuite(func() {
	if rookDir == "" {
		Skip("ROOK_DIR not set; skipping rooket e2e (needs a Rook source tree + iSCSI block devices)")
	}
	kubeCtx = "kind-" + clusterName

	// rooket writes each cluster's kubeconfig under its own state dir, not
	// ~/.kube/config; point the suite's kubectl/helm calls at it.
	home, herr := os.UserHomeDir()
	Expect(herr).NotTo(HaveOccurred())
	stateDir = filepath.Join(home, ".local", "share", "rooket", clusterName)
	Expect(os.Setenv("KUBECONFIG", filepath.Join(stateDir, "kubeconfig"))).To(Succeed())

	// Use a prebuilt binary if given (CI builds rooket up front and passes
	// ROOKET_BIN); otherwise build it from the repo.
	if b := os.Getenv("ROOKET_BIN"); b != "" {
		abs, err := filepath.Abs(b)
		Expect(err).NotTo(HaveOccurred())
		rooketBin = abs
	} else {
		repoRoot, err := filepath.Abs("../..")
		Expect(err).NotTo(HaveOccurred())
		bin := filepath.Join(GinkgoT().TempDir(), "rooket")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build rooket:\n%s", out)
		rooketBin = bin
	}
	GinkgoWriter.Printf("rooket: %s (cluster=%s workers=%s skipBlock=%v rook=%s)\n",
		rooketBin, clusterName, workers, skipBlock, rookDir)
})

// AfterSuite tears the cluster down best-effort, so a failed spec doesn't leave
// a cluster behind to block the next run.
var _ = AfterSuite(func() {
	if rooketBin == "" {
		return
	}
	args := []string{"down", "--workers", workers, "--name", clusterName}
	if skipBlock {
		args = append(args, "--skip-block")
	}
	_, _ = rooketRun(10*time.Minute, args...)
})

// AfterEach dumps cluster diagnostics to the Ginkgo log when a spec fails, so CI
// (where AfterSuite then tears the cluster down) still captures the cause.
var _ = AfterEach(func() {
	if !CurrentSpecReport().Failed() {
		return
	}
	GinkgoWriter.Println("\n=================== FAILURE DIAGNOSTICS ===================")
	dumps := []struct {
		label string
		args  []string
	}{
		{"pods -o wide", []string{"get", "pods", "-o", "wide"}},
		{"cephcluster status", []string{"get", "cephcluster", "-o", "jsonpath={.items[0].status.phase} {.items[0].status.message}"}},
		{"profile resources (object store, filesystem, nfs, bucket claim)",
			[]string{"get", "cephobjectstores,cephfilesystems,cephnfses,objectbucketclaims"}},
		{"pvc + storageclasses", []string{"get", "pvc,sc"}},
		{"csi driver CRs", []string{"get", "drivers.csi.ceph.io"}},
		{"events (recent)", []string{"get", "events", "--sort-by=.lastTimestamp"}},
		{"ceph -s", []string{"exec", "deploy/rook-ceph-tools", "--", "ceph", "-s"}},
		{"osd-prepare logs", []string{"logs", "--prefix", "-l", "app=rook-ceph-osd-prepare", "--tail=120"}},
		{"osd-prepare describe", []string{"describe", "pods", "-l", "app=rook-ceph-osd-prepare"}},
		{"operator log tail", []string{"logs", "-l", "app=rook-ceph-operator", "--tail=120"}},
	}
	for _, d := range dumps {
		out, _ := kubectlNS(d.args...)
		GinkgoWriter.Printf("----- %s -----\n%s\n", d.label, tail(out, 60))
	}
	GinkgoWriter.Printf("----- node block devices + loops -----\n%s\n", nodeDevDump())
})

func nodeDevDump() string {
	cmd := exec.Command("kind", "get", "nodes", "--name", clusterName)
	cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER="+eng)
	out, _ := cmd.Output()
	var b strings.Builder
	for _, node := range strings.Fields(string(out)) {
		o, _ := runOut(30*time.Second, eng, "exec", node, "sh", "-c",
			"echo sd: $(ls -d /dev/sd* 2>/dev/null); echo loops: $(losetup -a 2>/dev/null | wc -l)")
		b.WriteString(node + ": " + strings.TrimSpace(o) + "\n")
	}
	return b.String()
}

// --- command helpers ---

// rooketRun runs the rooket binary, streaming output to the Ginkgo log while
// capturing it; returns the captured output and the exit error.
func rooketRun(timeout time.Duration, args ...string) (string, error) {
	return rooketRunEnv(timeout, nil, args...)
}

// rooketRunEnv is rooketRun with extra environment variables appended — used
// for commands like 'rooket kubectl' that resolve the cluster name from
// $ROOKET_NAME rather than a --name flag.
func rooketRunEnv(timeout time.Duration, extraEnv []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, rooketBin, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	mw := io.MultiWriter(GinkgoWriter, &buf)
	cmd.Stdout, cmd.Stderr = mw, mw
	err := cmd.Run()
	return buf.String(), err
}

// kubectlApply pipes a manifest into kubectl apply in the rook-ceph namespace.
func kubectlApply(manifest string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context", kubeCtx, "-n", "rook-ceph", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// kindRun runs kind with the suite's engine provider and an explicit
// kubeconfig, so foreign-cluster fixtures never touch the rooket cluster's
// kubeconfig in $KUBECONFIG.
func kindRun(timeout time.Duration, kubeconfig string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Env = append(os.Environ(),
		"KIND_EXPERIMENTAL_PROVIDER="+eng, "KUBECONFIG="+kubeconfig)
	cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
	return cmd.Run()
}

func runOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func kubectlNS(args ...string) (string, error) {
	return runOut(2*time.Minute, "kubectl", append([]string{"--context", kubeCtx, "-n", "rook-ceph"}, args...)...)
}

func cephTool(g Gomega, args ...string) string {
	out, err := kubectlNS(append([]string{"exec", "deploy/rook-ceph-tools", "--", "ceph"}, args...)...)
	g.Expect(err).NotTo(HaveOccurred(), "ceph %s:\n%s", strings.Join(args, " "), out)
	return out
}

// kindNodeImageID returns a locally-present kindest/node image ID for the
// throwaway privileged container used to inspect host /dev.
func kindNodeImageID() string {
	out, _ := runOut(30*time.Second, eng, "images", "--format", "{{.ID}} {{.Repository}}")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "kindest/node") {
			if f := strings.Fields(line); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

// enginePrivileged runs a throwaway privileged container (with the configured
// engine) that shares the host /dev, used to inspect loop devices / disk state.
func enginePrivileged(script string) string {
	img := kindNodeImageID()
	if img == "" {
		return ""
	}
	out, _ := runOut(2*time.Minute, eng, "run", "--rm", "--privileged",
		"-v", "/dev:/dev", "--entrypoint", "sh", img, "-c", script)
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
