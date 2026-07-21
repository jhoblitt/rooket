//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func kubectl(args ...string) (string, error) {
	return rooketRun(2*time.Minute, append([]string{"k"}, args...)...)
}

func podPhase(name string) string {
	out, err := kubectl("-n", "rook-ceph", "get", "pod", name, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func pvcPhase(name string) string {
	out, err := kubectl("-n", "rook-ceph", "get", "pvc", name, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

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

		By("binding the rbd PVC and running its pod")
		Eventually(func() string { return pvcPhase("rooket-rbd-pvc") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-rbd-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("binding the OBC and running the s3 pod")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "obc", "rooket-rgw-bucket",
				"-o", "jsonpath={.status.phase}")
			return strings.TrimSpace(out)
		}, 5*time.Minute, 10*time.Second).Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-rgw-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("binding the nfs PVC and running its pod")
		Eventually(func() string { return pvcPhase("rooket-nfs-pvc") }, 10*time.Minute, 15*time.Second).
			Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-nfs-smoke") }, 10*time.Minute, 15*time.Second).
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

		Eventually(func() string { return podPhase("rooket-rgw-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rgw pod was not pruned")
		Eventually(func() string { return podPhase("rooket-nfs-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "nfs pod was not pruned")

		Expect(podPhase("rooket-rbd-smoke")).To(Equal("Running"), "rbd pod should survive")

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	It("shows exactly the values helm received", func() {
		for _, c := range []struct{ chart, release string }{
			{"cluster", "rook-ceph-cluster"},
			{"operator", "rook-ceph"},
		} {
			shown, err := rooketRun(2*time.Minute, "values", "show", c.chart,
				"--dir", rookDir, "--with-only", "rbd")
			Expect(err).NotTo(HaveOccurred())

			supplied, err := rooketRun(2*time.Minute, "helm", "-n", "rook-ceph",
				"get", "values", c.release, "-o", "yaml")
			Expect(err).NotTo(HaveOccurred())

			for _, key := range []string{"toolbox", "cephClusterSpec", "image"} {
				if strings.Contains(supplied, key+":") {
					Expect(shown).To(ContainSubstring(key+":"),
						"%s: preview is missing %q that helm received", c.chart, key)
				}
			}
		}
	})

	AfterAll(func() {
		Expect(os.Remove(scratch)).To(Succeed())
	})
})
