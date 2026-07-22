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

// This suite and updown_test.go's "rooket up/down" are independent top-level
// Describe blocks sharing one cluster name (clusterName); Ginkgo randomizes
// top-level order by default, so neither suite may assume it runs before or
// after the other, or rely on cluster state the other leaves behind.
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
	})

	It("installs every built-in profile", func() {
		args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName,
			"--with-only", "rbd", "--with-only", "rgw", "--with-only", "nfs"}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(40*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket up failed:\n%s", tail(out, 40))

		// rooket up returns once helm has installed the charts, well before OSD
		// prepare jobs finish and Ceph has pools/daemons for the specs below to
		// bind against.
		waitClusterSettled()

		By("binding the rbd PVC")
		// No pod mounts this PVC: krbd maps the image fine, but the device
		// node lands in the host's /dev, not the per-container tmpfs /dev
		// rooket gives a kind node, so the mount never becomes visible there.
		// That isolation is the price of rooket's per-node OSD device
		// masking; see updown_test.go's CSI note for the CI-observed error.
		Eventually(func() (string, error) { return pvcPhase("rooket-rbd-pvc") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Bound"))

		By("binding the OBC and running the s3 pod")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "obc", "rooket-rgw-bucket",
				"-o", "jsonpath={.status.phase}")
			return strings.TrimSpace(out)
		}, 10*time.Minute, 10*time.Second).Should(Equal("Bound"))
		Eventually(func() (string, error) { return podPhase("rooket-rgw-smoke") }, 10*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("binding the nfs PVC and running its pod")
		Eventually(func() (string, error) { return pvcPhase("rooket-nfs-pvc") }, 15*time.Minute, 15*time.Second).
			Should(Equal("Bound"))
		Eventually(func() (string, error) { return podPhase("rooket-nfs-smoke") }, 15*time.Minute, 15*time.Second).
			Should(Equal("Running"))

		By("installing the clone's own template")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch",
				"-o", "jsonpath={.data.from}")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 5*time.Second).Should(Equal("clone-templates"))
	})

	It("prunes the profiles that are switched off, keeping clone templates", func() {
		out, err := rooketRun(15*time.Minute, "deploy", "cluster",
			"--dir", rookDir, "--name", clusterName, "--with-only", "rbd")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		Eventually(func() (string, error) { return podPhase("rooket-rgw-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rgw pod was not pruned")
		Eventually(func() (string, error) { return podPhase("rooket-nfs-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "nfs pod was not pruned")

		rbdPhase, err := pvcPhase("rooket-rbd-pvc")
		Expect(err).NotTo(HaveOccurred())
		Expect(rbdPhase).To(Equal("Bound"), "rbd PVC should survive")

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	It("shows exactly the values helm received", func() {
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
			shownRaw, err := rooketRun(2*time.Minute, "values", "show", c.chart,
				"--dir", rookDir, "--with-only", "rbd")
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

	AfterAll(func() {
		Expect(os.Remove(scratch)).To(Succeed())
		// The clone-templates ConfigMap outlives its source template once that
		// file is gone; delete it too so this suite leaves no cluster-side
		// residue for the next run (or the sibling up/down suite) to trip over.
		_, err := kubectl("-n", "rook-ceph", "delete", "cm", "rooket-scratch", "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred(), "failed to delete rooket-scratch ConfigMap")
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
