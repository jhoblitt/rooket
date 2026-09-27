//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// rePgsTotal/rePgsClean live in health.go (untagged) alongside the pure
// pgsSettledEnough helper they feed.
var reOsdUp = regexp.MustCompile(`osd:\s+(\d+)\s+osds:\s+(\d+)\s+up`)

var _ = Describe("rooket up/down", Ordered, func() {
	It("brings up a healthy rook-ceph cluster that settles", func() {
		args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(40*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket up failed:\n%s", tail(out, 40))

		By("running one OSD pod per worker")
		Eventually(runningOSDs, 10*time.Minute, 15*time.Second).Should(Equal(numWorkers()))
		Expect(osdNodes()).To(HaveLen(numWorkers()), "OSDs not spread one-per-node")

		By("using no loop devices")
		loops, err := hostLoopDevices()
		Expect(err).NotTo(HaveOccurred(), "probe the host's loop devices")
		Expect(loops).To(BeEmpty(), "loop devices attached on the host")

		By("masking the host's real devices from every node (allowlist prune)")
		nodes, err := kindNodeNames()
		Expect(err).NotTo(HaveOccurred(), "list the kind nodes")
		Expect(nodes).To(HaveLen(numWorkers()+1), "expected control-plane + workers")
		for _, node := range nodes {
			Expect(hostSensitiveDevsOnNode(node)).To(BeEmpty(),
				"sensitive host devices still reachable from %s", node)
		}

		waitClusterSettled()

		By("serving I/O: round-tripping an object through RADOS")
		Eventually(func(g Gomega) {
			out, err := kubectlNS("exec", "deploy/rook-ceph-tools", "--", "sh", "-c",
				`set -e; echo "rooket-e2e round-trip" >/tmp/in; `+
					`rados -p .mgr put rooket-e2e /tmp/in; `+
					`rados -p .mgr get rooket-e2e /tmp/out; `+
					`cmp /tmp/in /tmp/out; rados -p .mgr rm rooket-e2e`)
			g.Expect(err).NotTo(HaveOccurred(), "RADOS round-trip failed:\n%s", out)
		}, 2*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("serves I/O on a CephFS PVC through CSI", func() {
		// The full CSI data path — provision, attach, node mount, pod I/O,
		// detach, reclaim — on the chart's ceph-filesystem StorageClass.
		const manifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rooket-e2e-fs-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ceph-filesystem
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: rooket-e2e-fs-io
spec:
  restartPolicy: Never
  containers:
    - name: io
      image: registry.k8s.io/e2e-test-images/busybox:1.36.1-1
      command: ["sh", "-c", "echo rooket-e2e-csi > /data/f && sync && grep -q rooket-e2e-csi /data/f"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: rooket-e2e-fs-pvc
`
		out, err := kubectlApply(manifest)
		Expect(err).NotTo(HaveOccurred(), "apply PVC+pod:\n%s", out)
		DeferCleanup(func() {
			_, _ = kubectlNS("delete", "pod", "rooket-e2e-fs-io", "--ignore-not-found", "--timeout=120s")
			_, _ = kubectlNS("delete", "pvc", "rooket-e2e-fs-pvc", "--ignore-not-found", "--timeout=120s")
		})

		By("the pod writing and reading back through the CephFS mount")
		Eventually(func(g Gomega) {
			out, _ := kubectlNS("get", "pod", "rooket-e2e-fs-io", "-o", "jsonpath={.status.phase}")
			g.Expect(out).To(Equal("Succeeded"), "pod phase")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("detaching and deleting cleanly")
		out, err = kubectlNS("delete", "pod", "rooket-e2e-fs-io", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "delete pod:\n%s", out)
		out, err = kubectlNS("delete", "pvc", "rooket-e2e-fs-pvc", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "delete pvc:\n%s", out)
		Eventually(func() string {
			out, _ := kubectlNS("get", "pvc", "rooket-e2e-fs-pvc", "--ignore-not-found")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 10*time.Second).Should(BeEmpty(), "PVC not gone after delete")
	})

	It("exposes the cluster through list, kubectl, and kubeconfig", func() {
		By("recording the registry port in the state dir")
		portBytes, err := os.ReadFile(filepath.Join(stateDir, "registry-port"))
		Expect(err).NotTo(HaveOccurred(), "read registry-port marker")
		recordedPort = strings.TrimSpace(string(portBytes))
		port, err := strconv.Atoi(recordedPort)
		Expect(err).NotTo(HaveOccurred(), "registry-port %q not an int", recordedPort)
		Expect(port).To(BeNumerically(">=", 5001))

		By("showing the cluster live with its port in 'rooket list'")
		out, err := rooketRun(time.Minute, "list")
		Expect(err).NotTo(HaveOccurred(), "rooket list:\n%s", out)
		row := listRow(out, clusterName)
		Expect(row).NotTo(BeEmpty(), "no row for %q in list output:\n%s", clusterName, out)
		Expect(row).To(ContainSubstring(eng), "cluster not live under %s:\n%s", eng, row)
		Expect(row).To(ContainSubstring(recordedPort), "registry port missing:\n%s", row)
		Expect(row).To(ContainSubstring(stateDir), "state dir missing:\n%s", row)

		By("running kubectl through 'rooket k'")
		out, err = rooketRunEnv(time.Minute, []string{"ROOKET_NAME=" + clusterName},
			"k", "get", "nodes", "--no-headers")
		Expect(err).NotTo(HaveOccurred(), "rooket k get nodes:\n%s", out)
		Expect(nonEmptyLines(out)).To(HaveLen(numWorkers()+1), "expected control-plane + workers:\n%s", out)

		By("printing the kubeconfig path the suite already uses")
		out, err = rooketRun(time.Minute, "kubeconfig", "--path", "--name", clusterName)
		Expect(err).NotTo(HaveOccurred(), "rooket kubeconfig --path:\n%s", out)
		Expect(strings.TrimSpace(out)).To(Equal(os.Getenv("KUBECONFIG")))
	})

	It("prunes orphaned state dirs but spares the live cluster", func() {
		orphan := filepath.Join(filepath.Dir(stateDir), "rooket-e2e-orphan")
		Expect(os.MkdirAll(orphan, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(orphan, "registry-port"), []byte("5999\n"), 0o644)).To(Succeed())
		DeferCleanup(func() { _ = os.RemoveAll(orphan) })

		// Not live, but its recorded clone exists: prune keeps it as parked.
		parked := filepath.Join(filepath.Dir(stateDir), "rooket-e2e-parked")
		Expect(os.MkdirAll(parked, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(parked, "clone-path"), []byte(GinkgoT().TempDir()+"\n"), 0o644)).To(Succeed())
		DeferCleanup(func() { _ = os.RemoveAll(parked) })

		out, err := rooketRun(time.Minute, "prune", "--force")
		Expect(err).NotTo(HaveOccurred(), "rooket prune:\n%s", out)

		_, err = os.Stat(orphan)
		Expect(os.IsNotExist(err)).To(BeTrue(), "orphan state dir survived prune")
		_, err = os.Stat(filepath.Join(stateDir, "registry-port"))
		Expect(err).NotTo(HaveOccurred(), "live cluster's state was pruned")

		// The live cluster's build stamp names its existing clone, so prune
		// would keep its state dir as parked even if it missed the cluster as
		// live; only the absence of a "keeping" line shows it was seen live.
		// The parked fixture proves that line is still what prune prints.
		Expect(out).To(ContainSubstring("keeping "+parked+":"),
			"parked state dir not reported as kept:\n%s", out)
		Expect(out).NotTo(ContainSubstring("keeping "+stateDir+":"),
			"prune treated the live cluster as parked, not live:\n%s", out)
	})

	It("stays healthy when up is re-run (idempotent)", func() {
		// --skip-build: the image is already in the registry from the first up;
		// this exercises re-running create+deploy against a live cluster.
		args := []string{"up", "--skip-build", "--dir", rookDir, "--workers", workers, "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(15*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "re-running rooket up failed:\n%s", tail(out, 40))

		By("reusing the recorded registry port")
		portBytes, err := os.ReadFile(filepath.Join(stateDir, "registry-port"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(portBytes))).To(Equal(recordedPort),
			"re-up changed the persisted registry port")

		Eventually(func(g Gomega) {
			s := cephTool(g, "-s")
			m := reOsdUp.FindStringSubmatch(s)
			g.Expect(m).NotTo(BeNil(), "no osd line:\n%s", s)
			g.Expect(m[2]).To(Equal(workers), "not all OSDs up after re-up:\n%s", s)
			g.Expect(s).NotTo(ContainSubstring("HEALTH_ERR"), "unhealthy after re-up:\n%s", s)
		}, 3*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("auto-skips the build on an unchanged tree and rebuilds on change", func() {
		operatorImageID := func() string {
			out, err := kubectlNS("get", "pod", "-l", "app=rook-ceph-operator",
				"-o", "jsonpath={.items[0].status.containerStatuses[0].imageID}")
			Expect(err).NotTo(HaveOccurred(), "read operator imageID:\n%s", out)
			return strings.TrimSpace(out)
		}

		// No --skip-build: the auto-skip gate must detect the unchanged tree.
		args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(15*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "no-flag re-up failed:\n%s", tail(out, 40))
		Expect(out).To(ContainSubstring("build skipped: rook tree unchanged"),
			"auto-skip did not fire on an unchanged tree:\n%s", tail(out, 60))

		digestBefore := operatorImageID()

		By("rebuilding after a source change")
		// Appending an unreferenced package-level var changes the compiled
		// binary (rook's vet allows it) and must flip the fingerprint.
		mainGo := filepath.Join(rookDir, "cmd", "rook", "main.go")
		orig, err := os.ReadFile(mainGo)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(os.WriteFile(mainGo, orig, 0o644)).To(Succeed(),
				"restore %s after the rebuild spec", mainGo)
		})
		Expect(os.WriteFile(mainGo,
			append(append([]byte{}, orig...), []byte("\nvar rooketE2EProbe = \"rebuild\"\n")...), 0o644)).To(Succeed())

		out, err = rooketRun(30*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "re-up after source change failed:\n%s", tail(out, 40))
		Expect(out).NotTo(ContainSubstring("build skipped"),
			"build skipped despite a source change:\n%s", tail(out, 60))

		By("rolling the operator to the new image digest")
		Eventually(func(g Gomega) {
			out, err := kubectlNS("get", "pod", "-l", "app=rook-ceph-operator",
				"--field-selector", "status.phase=Running",
				"-o", "jsonpath={.items[0].status.containerStatuses[0].imageID}")
			g.Expect(err).NotTo(HaveOccurred())
			id := strings.TrimSpace(out)
			g.Expect(id).NotTo(BeEmpty(), "operator pod has no imageID yet")
			g.Expect(id).NotTo(Equal(digestBefore), "operator still runs the pre-change image")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())

		By("reverting the change reuses the earlier build instead of running make")
		Expect(os.WriteFile(mainGo, orig, 0o644)).To(Succeed())
		out, err = rooketRun(15*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "re-up after revert failed:\n%s", tail(out, 40))
		Expect(out).To(ContainSubstring("reusing the build of this rook tree"),
			"reverted tree was not served from the build cache:\n%s", tail(out, 60))
		Expect(out).NotTo(ContainSubstring("running make"),
			"make ran for a tree already built:\n%s", tail(out, 60))

		Eventually(func(g Gomega) {
			out, err := kubectlNS("get", "pod", "-l", "app=rook-ceph-operator",
				"--field-selector", "status.phase=Running",
				"-o", "jsonpath={.items[0].status.containerStatuses[0].imageID}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal(digestBefore), "operator does not run the reused pre-change image")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("tears the cluster down and leaves the disks clean", func() {
		// No --workers: down takes the count the cluster was created with.
		args := []string{"down", "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(10*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket down failed:\n%s", tail(out, 40))

		By("removing the kind cluster")
		clusters, err := kindClusters()
		Expect(err).NotTo(HaveOccurred(), "list the kind clusters")
		Expect(clusters).NotTo(ContainElement(clusterName))

		By("leaving the OSD disks clean")
		disks, dirty, err := osdDiskSignatures()
		Expect(err).NotTo(HaveOccurred(), "probe the OSD disks")
		Expect(disks).To(HaveLen(numWorkers()),
			"a plain down keeps each worker's iSCSI OSD disk attached")
		Expect(dirty).To(BeEmpty(), "OSD disks still carry a signature after down")

		By("showing the cluster as not live in 'rooket list'")
		out, err = rooketRun(time.Minute, "list")
		Expect(err).NotTo(HaveOccurred(), "rooket list:\n%s", out)
		row := listRow(out, clusterName)
		Expect(row).NotTo(BeEmpty(), "state-only cluster missing from list:\n%s", out)
		Expect(row).NotTo(ContainSubstring(eng), "cluster still live after down:\n%s", row)

		By("'rooket k' reporting the cluster is down instead of chasing a stale kubeconfig")
		out, err = rooketRunEnv(time.Minute, []string{"ROOKET_NAME=" + clusterName},
			"k", "get", "nodes")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("is it up?"), "unexpected rooket k failure:\n%s", out)
	})

	It("scopes 'down --all' to rooket-owned clusters", func() {
		// A real kind cluster rooket did not create: no state dir, no
		// <name>-registry container. Its kubeconfig goes to a throwaway file
		// so the fixture never touches rooket's per-cluster kubeconfig.
		const foreign = "rooket-e2e-foreign"
		kc := filepath.Join(GinkgoT().TempDir(), "kubeconfig")
		Expect(kindRun(5*time.Minute, kc, "create", "cluster", "--name", foreign,
			"--kubeconfig", kc)).To(Succeed(), "create foreign kind cluster")
		DeferCleanup(func() {
			_ = kindRun(3*time.Minute, kc, "delete", "cluster", "--name", foreign)
		})

		By("skipping the foreign cluster by default")
		out, err := rooketRun(2*time.Minute, "down", "--all", "--dry-run")
		Expect(err).NotTo(HaveOccurred(), "down --all --dry-run:\n%s", out)
		Expect(listRow(out, clusterName)).NotTo(BeEmpty(),
			"rooket's own state dir missing from the plan:\n%s", out)
		Expect(listRow(out, foreign)).To(BeEmpty(),
			"foreign cluster in the teardown plan:\n%s", out)
		Expect(out).To(ContainSubstring("unmanaged"), "no unmanaged notice:\n%s", out)
		Expect(out).To(ContainSubstring(foreign), "unmanaged notice does not name the cluster:\n%s", out)

		By("including it only with --include-unmanaged")
		out, err = rooketRun(2*time.Minute, "down", "--all", "--include-unmanaged", "--dry-run")
		Expect(err).NotTo(HaveOccurred(), "down --all --include-unmanaged --dry-run:\n%s", out)
		Expect(listRow(out, foreign)).NotTo(BeEmpty(),
			"foreign cluster missing from --include-unmanaged plan:\n%s", out)
	})
})

// recordedPort carries the registry port observed after the first up so the
// idempotent re-up can assert it is reused.
var recordedPort string

// listRow returns the line of tabular output whose first column is name.
func listRow(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == name {
			return l
		}
	}
	return ""
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func runningOSDs() int {
	out, _ := kubectlNS("get", "pods", "-l", "app=rook-ceph-osd", "--no-headers")
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[2] == "Running" {
			n++
		}
	}
	return n
}

func osdNodes() []string {
	out, _ := kubectlNS("get", "pods", "-l", "app=rook-ceph-osd", "-o", "wide", "--no-headers")
	set := map[string]struct{}{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(l); len(f) >= 7 {
			set[f[6]] = struct{}{}
		}
	}
	nodes := make([]string, 0, len(set))
	for n := range set {
		nodes = append(nodes, n)
	}
	return nodes
}

// hostLoopDevices returns the host's attached loop devices, one losetup line
// each.
func hostLoopDevices() ([]string, error) {
	out, err := enginePrivileged("losetup -a")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// kindNodeNames returns the node container names of the rooket cluster.
func kindNodeNames() ([]string, error) {
	out, err := runStdout(time.Minute, []string{"KIND_EXPERIMENTAL_PROVIDER=" + eng},
		"kind", "get", "nodes", "--name", clusterName)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// hostSensitiveDevsOnNode returns any sensitive host device still reachable
// from a kind node's /dev after the allowlist prune: block storage and its
// ioctl passthrough paths (nvme block/char/generic, device-mapper, SCSI-generic
// SG_IO and bsg, nbd, zram, and /dev/mapper entries other than the char
// 'control'), plus host memory and hardware nodes (/dev/mem, /dev/kvm,
// /dev/snapshot, tpm, hidraw, video, watchdog). A kind node needs none of these,
// so any that survive would be openable from the node and from the pods that
// bind-mount its /dev. /dev/bsg is matched by its contents, not the (harmless)
// empty directory the prune leaves behind. The allowlist prune must leave none.
func hostSensitiveDevsOnNode(node string) string {
	o, _ := runOut(30*time.Second, eng, "exec", node, "sh", "-c",
		`ls -d /dev/nvme* /dev/ng* /dev/dm-* /dev/sg[0-9]* /dev/bsg/* /dev/nbd* /dev/zram* `+
			`/dev/mem /dev/kmem /dev/port /dev/kvm /dev/snapshot /dev/nvram `+
			`/dev/tpm* /dev/hidraw* /dev/video* /dev/watchdog* 2>/dev/null; `+
			`ls /dev/mapper 2>/dev/null | grep -v '^control$'`)
	return strings.TrimSpace(o)
}

// waitClusterSettled blocks until mons are quorate, mgr is active, mds is up,
// all OSDs are up, PGs are active+clean, and the cluster isn't HEALTH_ERR.
//
// The window is deliberately generous. Two transients, both observed flaking
// this check in CI on the heaviest rook ref, resolve on their own given time:
//   - the kube-apiserver can briefly go unreachable (a control-plane container
//     restart under bring-up load) — every `ceph -s` here runs through
//     `kubectl exec`, so it fails until the apiserver is back;
//   - a PG or two is still peering while Rook finishes creating the last pools.
//
// A short window caught the bad instant and failed a cluster that was seconds
// from ready. Ten minutes lets a restarted apiserver recover and late pools
// finish; a cluster that is genuinely wedged still fails, just later.
//
// Call this before any spec that asserts on storage: `rooket up` returns as
// soon as helm has installed the charts, well before the OSD prepare jobs
// finish and Ceph has pools to bind PVCs against.
func waitClusterSettled() {
	By("settling: mons quorate, mgr active, mds up, all OSDs up, PGs active+clean, not unhealthy")
	Eventually(func(g Gomega) {
		s := cephTool(g, "-s")
		g.Expect(s).To(MatchRegexp(`mon:\s+\d+\s+daemons,\s+quorum`), "no mon quorum:\n%s", s)
		g.Expect(s).To(MatchRegexp(`mgr:.*\(active`), "no active mgr:\n%s", s)
		g.Expect(s).To(MatchRegexp(`mds:\s+\d+/\d+\s+daemons up`), "no mds up:\n%s", s)
		m := reOsdUp.FindStringSubmatch(s)
		g.Expect(m).NotTo(BeNil(), "no osd line in ceph -s:\n%s", s)
		g.Expect(m[1]).To(Equal(workers), "expected %s OSDs:\n%s", workers, s)
		g.Expect(m[2]).To(Equal(m[1]), "not all OSDs up:\n%s", s)
		g.Expect(s).NotTo(ContainSubstring("HEALTH_ERR"), "cluster unhealthy:\n%s", s)
		ok, detail := pgsSettledEnough(cephTool(g, "pg", "stat"))
		g.Expect(ok).To(BeTrue(), detail)
	}, 10*time.Minute, 10*time.Second).Should(Succeed())
}

func kindClusters() ([]string, error) {
	out, err := runStdout(time.Minute, []string{"KIND_EXPERIMENTAL_PROVIDER=" + eng},
		"kind", "get", "clusters")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// osdDiskSignatures probes the cluster's iSCSI OSD disks on the host with
// blkid's low-level probe, returning every disk found and those that still
// carry a signature (a filesystem, partition table, or bluestore label).
func osdDiskSignatures() (disks, dirty []string, err error) {
	script := `for l in /dev/disk/by-path/ip-127.0.0.1:3260-iscsi-iqn.*.local.rooket:` + clusterName + `-worker*-disk*-lun-0; do
  [ -e "$l" ] || continue
  blkid -p "$l" >/dev/null
  echo "$? $l"
done`
	out, err := enginePrivileged(script)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range nonEmptyLines(out) {
		rc, disk, _ := strings.Cut(line, " ")
		disks = append(disks, disk)
		// blkid -p exits 0 on a signature, 8 on several conflicting ones,
		// and 2 on none; anything else means the probe itself failed.
		switch rc {
		case "0", "8":
			dirty = append(dirty, disk)
		case "2":
		default:
			return nil, nil, fmt.Errorf("blkid -p %s exited %s", disk, rc)
		}
	}
	return disks, dirty, nil
}
