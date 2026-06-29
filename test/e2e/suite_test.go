//go:build e2e

// Package e2e contains rooket's end-to-end integration tests: they drive the
// real `rooket` binary to bring a Rook/Ceph cluster up on kind and tear it down,
// asserting the cluster provisions correctly and settles.
//
// Prerequisites (the suite Skips if ROOK_DIR is unset):
//   - ROOK_DIR points at a Rook source tree (its charts are deployed).
//   - The iSCSI OSD block devices already exist ('rooket block setup', which
//     needs root); the suite runs up/down with --skip-block by default.
//   - podman, kind, kubectl, helm and a Go toolchain are on PATH.
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

	rooketBin string // built in BeforeSuite
	kubeCtx   string // kind-<name>
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

	repoRoot, err := filepath.Abs("../..")
	Expect(err).NotTo(HaveOccurred())
	bin := filepath.Join(GinkgoT().TempDir(), "rooket")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build rooket:\n%s", out)
	rooketBin = bin
	GinkgoWriter.Printf("built rooket: %s (cluster=%s workers=%s skipBlock=%v rook=%s)\n",
		bin, clusterName, workers, skipBlock, rookDir)
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

// --- command helpers ---

// rooketRun runs the rooket binary, streaming output to the Ginkgo log while
// capturing it; returns the captured output and the exit error.
func rooketRun(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, rooketBin, args...)
	mw := io.MultiWriter(GinkgoWriter, &buf)
	cmd.Stdout, cmd.Stderr = mw, mw
	err := cmd.Run()
	return buf.String(), err
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
	out, _ := runOut(30*time.Second, "podman", "images", "--format", "{{.ID}} {{.Repository}}")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "kindest/node") {
			if f := strings.Fields(line); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

func podmanPrivileged(script string) string {
	img := kindNodeImageID()
	if img == "" {
		return ""
	}
	out, _ := runOut(2*time.Minute, "podman", "run", "--rm", "--privileged",
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
