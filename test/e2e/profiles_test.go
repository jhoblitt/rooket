//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

func kubectl(args ...string) (string, error) {
	return rooketRun(2*time.Minute, append([]string{"k"}, args...)...)
}

// podPhase and pvcPhase return ("", nil) only when the resource is genuinely
// absent (--ignore-not-found exits 0 with empty output), and a non-nil error
// on any other kubectl failure (bad context, unreachable API server, ...).
// Collapsing both cases to "" would let a lookup failure masquerade as
// "pruned" in a BeEmpty() assertion.
func podPhase(name string) (string, error) {
	out, err := kubectl("-n", "rook-ceph", "get", "pod", name, "--ignore-not-found",
		"-o", "jsonpath={.status.phase}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func pvcPhase(name string) (string, error) {
	out, err := kubectl("-n", "rook-ceph", "get", "pvc", name, "--ignore-not-found",
		"-o", "jsonpath={.status.phase}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// withOnlyArgs expands a profile selection into repeated --with-only flags.
func withOnlyArgs(profiles []string) []string {
	args := make([]string, 0, 2*len(profiles))
	for _, p := range profiles {
		args = append(args, "--with-only", p)
	}
	return args
}

// This suite and updown_test.go's "rooket up/down" are independent top-level
// Describe blocks sharing one cluster name (clusterName); Ginkgo randomizes
// top-level order by default, so neither suite may assume it runs before or
// after the other, or rely on cluster state the other leaves behind.
//
// Only the rbd profile is exercised here. It is the lightest built-in profile —
// a PVC and a pod, with no CSI driver DaemonSet or extra server daemon — and it
// tears down cleanly (kernel krbd unmap, no server for a client mount to hang
// on). That keeps this suite within the 4-core CI runner's CPU budget and clear
// of the teardown-mount wedge the nfs profile's NFS client causes. rbd still
// exercises the whole feature pipeline end to end: layered value composition,
// the generated rooket-profiles chart, its install, and prune-on-disable. The
// rgw and nfs profiles' values overlays and templates are covered by unit
// tests; running all three built-ins on one cluster exhausts CPU on the
// smallest supported ref.
var _ = Describe("rooket profiles", Ordered, func() {
	scratch := filepath.Join(rookDir, ".rooket", "templates", "scratch-cm.yaml")

	BeforeAll(func() {
		Expect(os.MkdirAll(filepath.Dir(scratch), 0o755)).To(Succeed())
		Expect(os.WriteFile(scratch, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: rooket-scratch
data:
  from: clone-templates
`), 0o644)).To(Succeed())

		args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName,
			"--with-only", "rbd"}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(40*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket up failed:\n%s", tail(out, 40))

		// rooket up returns once helm has installed the charts, well before OSD
		// prepare jobs finish and Ceph has pools/daemons for the specs below to
		// bind against.
		waitClusterSettled()
	})

	It("brings up the rbd profile", func() {
		By("binding the rbd PVC and running its pod")
		Eventually(func() (string, error) { return pvcPhase("rooket-rbd-pvc") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Bound"))
		// The pod krbd-maps the PVC's image. This depends on the pre-created
		// /dev/rbdN nodes node prep adds to every kind node's per-container
		// tmpfs /dev; a regression there will surface here first.
		Eventually(func() (string, error) { return podPhase("rooket-rbd-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("installing the clone's own template")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch",
				"-o", "jsonpath={.data.from}")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 5*time.Second).Should(Equal("clone-templates"))
	})

	It("shows exactly the values helm received", func() {
		// Compare rooket's composed values (base + clone + the active profile,
		// merged into one file) against what helm actually received. rbd carries
		// no values/ overlay, so this pins the base+clone composition path — the
		// merge, encode, and -f handoff — rather than profile-overlay
		// composition, which the internal/values unit tests cover directly.
		// Re-deploy the exact selection first so the spec is self-contained and
		// not dependent on the state a prior spec left.
		previewWithOnly := []string{"rbd"}
		deployArgs := append([]string{"deploy", "--dir", rookDir, "--name", clusterName}, withOnlyArgs(previewWithOnly)...)
		deployOut, err := rooketRun(15*time.Minute, deployArgs...)
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(deployOut, 40))

		// 'values show' fakes the parts of the base layer that need a live
		// cluster to resolve for real (see showBase in cmd/values.go): the
		// operator's image repo/tag/digest, and the cluster's per-node OSD
		// device list. Those paths are excluded below so the comparison covers
		// everything rooket actually composes identically in both places —
		// not just top-level key names, which would pass even if the bodies
		// diverged completely.
		for _, c := range []struct {
			chart, release string
			ignore         [][]string
		}{
			{"cluster", "rook-ceph-cluster", [][]string{{"cephClusterSpec", "storage"}}},
			{"operator", "rook-ceph", [][]string{{"image"}, {"annotations"}}},
		} {
			showArgs := append([]string{"values", "show", c.chart, "--dir", rookDir}, withOnlyArgs(previewWithOnly)...)
			shownRaw, err := rooketRun(2*time.Minute, showArgs...)
			Expect(err).NotTo(HaveOccurred())

			suppliedRaw, err := rooketRun(2*time.Minute, "helm", "-n", "rook-ceph",
				"get", "values", c.release, "-o", "yaml")
			Expect(err).NotTo(HaveOccurred())

			shown, err := decodeValues(shownRaw)
			Expect(err).NotTo(HaveOccurred(), "%s: parse preview:\n%s", c.chart, shownRaw)
			supplied, err := decodeValues(suppliedRaw)
			Expect(err).NotTo(HaveOccurred(), "%s: parse helm values:\n%s", c.chart, suppliedRaw)

			for _, path := range c.ignore {
				deletePath(shown, path)
				deletePath(supplied, path)
			}
			Expect(shown).To(Equal(supplied), "%s: preview does not match what helm received", c.chart)
		}
	})

	It("prunes the profile on disable, keeping clone templates", func() {
		// --with-only "" selects zero profiles, so this deploy empties the
		// rooket-profiles release down to the clone's own always-active
		// template — verifying prune-on-disable and that a clone template
		// survives a profile change.
		out, err := rooketRun(15*time.Minute, "deploy",
			"--dir", rookDir, "--name", clusterName, "--with-only", "")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		By("pruning the rbd pod and PVC")
		Eventually(func() (string, error) { return podPhase("rooket-rbd-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rbd pod was not pruned")
		Eventually(func() (string, error) { return pvcPhase("rooket-rbd-pvc") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rbd PVC was not pruned")

		By("keeping the clone's own template, which is not tied to any profile")
		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	AfterAll(func() {
		// Remove the clone template's source file first, then prune it from the
		// cluster with a zero-profile deploy — leaving the rooket-profiles
		// release empty so this suite hands the shared cluster back clean for
		// the sibling up/down suite or teardown. Best-effort: after a mid-suite
		// failure the cluster may still have rbd active, so this both cleans up
		// and disables. A slow or failed restore is logged, not fatal.
		Expect(os.Remove(scratch)).To(Succeed())
		out, err := rooketRun(20*time.Minute, "deploy", "--dir", rookDir, "--name", clusterName, "--with-only", "")
		if err != nil {
			GinkgoWriter.Printf("AfterAll: restoring cluster to zero profiles failed (non-fatal):\n%s\n", tail(out, 40))
		}
	})
})

// decodeValues parses a Helm values YAML document, tolerating the
// "USER-SUPPLIED VALUES:" header some Helm versions prepend to 'get values'
// output (rendered rooket previews never carry it, but stripping it
// unconditionally when present keeps both sides going through one path).
func decodeValues(raw string) (map[string]any, error) {
	if i := strings.IndexByte(raw, '\n'); i >= 0 && strings.Contains(strings.ToUpper(raw[:i]), "USER-SUPPLIED VALUES") {
		raw = raw[i+1:]
	}
	var v map[string]any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// deletePath removes the nested key at path from m, doing nothing if any
// segment is absent or not itself a map.
func deletePath(m map[string]any, path []string) {
	for i, k := range path {
		if i == len(path)-1 {
			delete(m, k)
			return
		}
		next, ok := m[k].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
}
