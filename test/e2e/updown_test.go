//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	rePgsTotal = regexp.MustCompile(`(\d+)\s+pgs:`)
	rePgsClean = regexp.MustCompile(`(\d+)\s+active\+clean`)
	reOsdUp    = regexp.MustCompile(`osd:\s+(\d+)\s+osds:\s+(\d+)\s+up`)
)

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
		Expect(loopCount()).To(Equal(0))

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
			pgsSettled(g)
		}, 5*time.Minute, 15*time.Second).Should(Succeed())

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

	It("stays healthy when up is re-run (idempotent)", func() {
		// --skip-build: the image is already in the registry from the first up;
		// this exercises re-running create+deploy against a live cluster.
		args := []string{"up", "--skip-build", "--dir", rookDir, "--workers", workers, "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(15*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "re-running rooket up failed:\n%s", tail(out, 40))

		Eventually(func(g Gomega) {
			s := cephTool(g, "-s")
			m := reOsdUp.FindStringSubmatch(s)
			g.Expect(m).NotTo(BeNil(), "no osd line:\n%s", s)
			g.Expect(m[2]).To(Equal(workers), "not all OSDs up after re-up:\n%s", s)
			g.Expect(s).NotTo(ContainSubstring("HEALTH_ERR"), "unhealthy after re-up:\n%s", s)
		}, 3*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("tears the cluster down and leaves the disks clean", func() {
		args := []string{"down", "--workers", workers, "--name", clusterName}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(10*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket down failed:\n%s", tail(out, 40))

		By("removing the kind cluster")
		Expect(kindClusters()).NotTo(ContainElement(clusterName))

		By("leaving the OSD disks clean")
		Expect(disksDirty()).To(Equal(0))
	})
})

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

func loopCount() int {
	n, _ := strconv.Atoi(strings.TrimSpace(podmanPrivileged("losetup -a 2>/dev/null | wc -l")))
	return n
}

func pgsSettled(g Gomega) {
	out := cephTool(g, "pg", "stat")
	tot := rePgsTotal.FindStringSubmatch(out)
	cln := rePgsClean.FindStringSubmatch(out)
	g.Expect(tot).NotTo(BeNil(), "no pg total:\n%s", out)
	g.Expect(cln).NotTo(BeNil(), "no active+clean pgs:\n%s", out)
	g.Expect(cln[1]).To(Equal(tot[1]), "not all PGs active+clean:\n%s", out)
}

func kindClusters() []string {
	cmd := exec.Command("kind", "get", "clusters")
	cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER=podman")
	out, _ := cmd.Output() // cluster names on stdout; provider notes go to stderr
	var cs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			cs = append(cs, l)
		}
	}
	return cs
}

func disksDirty() int {
	script := `n=0
for l in /dev/disk/by-path/ip-127.0.0.1:3260-iscsi-iqn.*.local.rooket:` + clusterName + `-worker*-disk*-lun-0; do
  [ -e "$l" ] || continue
  blkid -p "$l" >/dev/null 2>&1 && n=$((n+1))
done
echo $n`
	n, _ := strconv.Atoi(strings.TrimSpace(podmanPrivileged(script)))
	return n
}
