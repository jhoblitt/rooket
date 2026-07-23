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

// podGoneOrTerminating reports whether a pod is either fully deleted or has
// been marked for deletion (non-empty deletionTimestamp). It fetches the
// pod's name alongside its deletionTimestamp in one call so "gone" (no
// output; --ignore-not-found exits 0) can be told apart from "present but
// not yet terminating" (name present, deletionTimestamp empty) — collapsing
// the latter into "gone" would let the assertion pass on a pod that was
// simply never touched by the prune.
func podGoneOrTerminating(name string) (bool, error) {
	out, err := kubectl("-n", "rook-ceph", "get", "pod", name, "--ignore-not-found",
		"-o", `jsonpath={.metadata.name}{"\t"}{.metadata.deletionTimestamp}`)
	if err != nil {
		return false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return true, nil
	}
	parts := strings.SplitN(out, "\t", 2)
	return len(parts) == 2 && strings.TrimSpace(parts[1]) != "", nil
}

// This suite and updown_test.go's "rooket up/down" are independent top-level
// Describe blocks sharing one cluster name (clusterName); Ginkgo randomizes
// top-level order by default, so neither suite may assume it runs before or
// after the other, or rely on cluster state the other leaves behind.
//
// The three built-in profiles run one at a time, never together. rbd+rgw+nfs
// simultaneously exhausts CPU on the smallest supported ref's 4-core kind
// cluster: the RGW gateway and rook's detect-version jobs sit Pending on
// Insufficient cpu, since nfs alone adds a CSI nodeplugin DaemonSet (one pod
// per node) plus a provisioner and a CephNFS server on top of rbd+rgw's load.
// Exercising one profile at a time bounds peak load to whichever single
// profile is heaviest, and doubles as a direct test of prune-on-switch: each
// switch below first asserts the previous profile's resources are gone, then
// that the new one is up.
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

	It("switches to rgw, pruning rbd", func() {
		out, err := rooketRun(15*time.Minute, "deploy",
			"--dir", rookDir, "--name", clusterName, "--with-only", "rgw")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		By("binding the OBC and running the s3 pod")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "obc", "rooket-rgw-bucket",
				"-o", "jsonpath={.status.phase}")
			return strings.TrimSpace(out)
		}, 10*time.Minute, 10*time.Second).Should(Equal("Bound"))
		Eventually(func() (string, error) { return podPhase("rooket-rgw-smoke") }, 10*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("pruning rbd's pod and PVC")
		Eventually(func() (bool, error) { return podGoneOrTerminating("rooket-rbd-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeTrue(), "rbd pod was neither pruned nor marked for deletion")
		Eventually(func() (string, error) { return pvcPhase("rooket-rbd-pvc") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rbd PVC was not pruned")

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	It("switches to nfs, pruning rgw", func() {
		out, err := rooketRun(15*time.Minute, "deploy",
			"--dir", rookDir, "--name", clusterName, "--with-only", "nfs")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		By("binding the nfs PVC and running its pod")
		Eventually(func() (string, error) { return pvcPhase("rooket-nfs-pvc") }, 15*time.Minute, 15*time.Second).
			Should(Equal("Bound"))
		Eventually(func() (string, error) { return podPhase("rooket-nfs-smoke") }, 15*time.Minute, 15*time.Second).
			Should(Equal("Running"))

		By("pruning rgw's pod")
		Eventually(func() (string, error) { return podPhase("rooket-rgw-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rgw pod was not pruned")

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	It("shows exactly the values helm received", func() {
		// nfs is the only built-in profile with a values/ overlay onto either
		// chart compared below — it sets the operator chart's csi.nfs.enabled.
		// rbd and rgw carry no such overlay, so previewing either of them
		// would pass even if profile-driven overlay composition were broken.
		// Deploy nfs's exact selection again (a no-op reconcile, since the
		// previous spec already left it active) so this spec is
		// self-contained and not dependent on exactly what state the prior
		// spec left, then compare against that known state.
		previewWithOnly := []string{"nfs"}
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

	It("switches back to rbd, pruning nfs", func() {
		// Delete the client pod while its CephNFS server is still up so the
		// kernel NFS client can unmount cleanly; otherwise the prune below
		// removes the server and the pod in the same operation, the unmount
		// hangs reaching the now-gone server, and the wedged mount blocks
		// `docker rm` on that node at cluster teardown.
		_, err := kubectl("-n", "rook-ceph", "delete", "pod", "rooket-nfs-smoke",
			"--ignore-not-found", "--wait=true", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "failed to pre-delete nfs pod before pruning its CephNFS server")

		out, err := rooketRun(15*time.Minute, "deploy",
			"--dir", rookDir, "--name", clusterName, "--with-only", "rbd")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		By("pruning the CephNFS server and its StorageClass")
		Eventually(func() (string, error) {
			out, err := kubectl("-n", "rook-ceph", "get", "cephnfs", "rooket-nfs", "--ignore-not-found")
			return strings.TrimSpace(out), err
		}, 3*time.Minute, 5*time.Second).Should(BeEmpty(), "CephNFS rooket-nfs was not pruned")
		Eventually(func() (string, error) {
			out, err := kubectl("get", "storageclass", "rooket-nfs", "--ignore-not-found")
			return strings.TrimSpace(out), err
		}, 3*time.Minute, 5*time.Second).Should(BeEmpty(), "StorageClass rooket-nfs was not pruned")

		By("pruning the nfs pod (already deleted above while its server was reachable)")
		Eventually(func() (string, error) { return podPhase("rooket-nfs-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "nfs pod was not pruned")

		By("rebinding the rbd PVC and running its pod")
		Eventually(func() (string, error) { return pvcPhase("rooket-rbd-pvc") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Bound"))
		Eventually(func() (string, error) { return podPhase("rooket-rbd-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	AfterAll(func() {
		Expect(os.Remove(scratch)).To(Succeed())
		// The clone-templates ConfigMap outlives its source template once that
		// file is gone; delete it too so this suite leaves no cluster-side
		// residue for the next run (or the sibling up/down suite) to trip over.
		_, err := kubectl("-n", "rook-ceph", "delete", "cm", "rooket-scratch", "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred(), "failed to delete rooket-scratch ConfigMap")

		// Delete the nfs pod (a no-op if the "switches back to rbd" spec above
		// already pruned it, or if nfs was never brought up) while its CephNFS
		// server may still be running, before the restore below can otherwise
		// prune both at once and wedge `docker rm` on that node at teardown.
		podOut, podErr := kubectl("-n", "rook-ceph", "delete", "pod", "rooket-nfs-smoke",
			"--ignore-not-found", "--wait=true", "--timeout=120s")
		if podErr != nil {
			GinkgoWriter.Printf("AfterAll: pre-delete of nfs pod failed (non-fatal):\n%s\n", tail(podOut, 40))
		}

		// This suite shares one cluster with two other top-level Describe
		// containers in Ginkgo's randomised run order, and it's the only one
		// that stands up an RGW gateway and a CephNFS server. With the clone
		// template gone (above), --with-only "" selects zero profiles, so this
		// deploy prunes the whole rooket-profiles release, shedding the
		// RGW/NFS/CephFS load before the next container or teardown runs.
		// Best-effort: a slow or failed restore is logged, not fatal, since
		// the suite has already passed by this point.
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
