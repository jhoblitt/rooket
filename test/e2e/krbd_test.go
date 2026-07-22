//go:build e2e

package e2e

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("rooket krbd", Ordered, func() {
	It("serves I/O on an RBD PVC through CSI, mounted via krbd", func() {
		// Exercises the full krbd data path: provision, kernel map, node mount,
		// pod I/O, unmap, reclaim. This needs the /dev/rbdN nodes PrepareNodes
		// pre-creates in every node's per-node tmpfs /dev (see
		// internal/cluster/cluster.go: rbdNodeScript) — without them the map
		// succeeds but the node can't see the device and mount fails with
		// "rbd: mapping succeeded but /dev/rbd0 is not accessible, is host /dev
		// mounted?". The block PVC spec in updown_test.go deliberately stops
		// short of mounting for that same historical reason; this spec is the
		// mount-and-read-back proof, modeled on the CephFS spec there.
		const manifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rooket-e2e-krbd-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ceph-block
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: rooket-e2e-krbd-io
spec:
  restartPolicy: Never
  containers:
    - name: io
      image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
      command: ["sh", "-c", "echo rooket-e2e-krbd > /data/f && sync && grep -q rooket-e2e-krbd /data/f"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: rooket-e2e-krbd-pvc
`
		out, err := kubectlApply(manifest)
		Expect(err).NotTo(HaveOccurred(), "apply PVC+pod:\n%s", out)
		DeferCleanup(func() {
			_, _ = kubectlNS("delete", "pod", "rooket-e2e-krbd-io", "--ignore-not-found", "--timeout=120s")
			_, _ = kubectlNS("delete", "pvc", "rooket-e2e-krbd-pvc", "--ignore-not-found", "--timeout=120s")
		})

		By("binding via the rbd provisioner")
		Eventually(func(g Gomega) {
			out, _ := kubectlNS("get", "pvc", "rooket-e2e-krbd-pvc", "-o", "jsonpath={.status.phase}")
			g.Expect(out).To(Equal("Bound"), "PVC phase")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("the pod krbd-mapping, writing, and reading back through the mount")
		Eventually(func(g Gomega) {
			out, _ := kubectlNS("get", "pod", "rooket-e2e-krbd-io", "-o", "jsonpath={.status.phase}")
			g.Expect(out).To(Equal("Succeeded"), "pod phase")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("detaching, unmapping, and deleting cleanly")
		out, err = kubectlNS("delete", "pod", "rooket-e2e-krbd-io", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "delete pod:\n%s", out)
		out, err = kubectlNS("delete", "pvc", "rooket-e2e-krbd-pvc", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "delete pvc:\n%s", out)
		Eventually(func() string {
			out, _ := kubectlNS("get", "pvc", "rooket-e2e-krbd-pvc", "--ignore-not-found")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 10*time.Second).Should(BeEmpty(), "PVC not gone after delete")
	})
})
