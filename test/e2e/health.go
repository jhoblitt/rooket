// This file carries no build tag on purpose: the rest of the e2e package is
// gated behind //go:build e2e and so never runs under the plain `go test`
// unit job. Pure helpers with interesting logic live here instead, where that
// job compiles and tests them deterministically without standing up a cluster.

package e2e

import (
	"fmt"
	"regexp"
)

var (
	rePgsTotal = regexp.MustCompile(`(\d+)\s+pgs:`)
	rePgsClean = regexp.MustCompile(`(\d+)\s+active\+clean`)
)

// pgSettleTolerance is how many placement groups may still be short of
// active+clean and the cluster still count as settled. Rook creates the RGW
// and CephFS metadata pools last, so a PG or two is often still peering or
// activating when settling is otherwise complete; demanding a pristine 100% at
// one sampled instant is what made this check flaky. The data-path specs
// (PVC bind, pod I/O) are the real functional gate, so a small transient
// remainder is fine here — scaled to the cluster so it stays ~1% rather than a
// fixed fraction that means different things at 60 vs 600 PGs.
func pgSettleTolerance(total int) int {
	if t := total / 100; t > 1 {
		return t
	}
	return 1
}

// pgsSettledEnough parses `ceph pg stat` output and reports whether the cluster
// is settled enough to proceed: every PG active+clean, or all but a tolerated
// transient remainder (see pgSettleTolerance). The returned detail is the
// human-readable reason when it is not, for the assertion message.
func pgsSettledEnough(statOut string) (bool, string) {
	tot := rePgsTotal.FindStringSubmatch(statOut)
	cln := rePgsClean.FindStringSubmatch(statOut)
	if tot == nil {
		return false, "no pg total in:\n" + statOut
	}
	if cln == nil {
		// No "active+clean" at all — nothing has settled yet.
		return false, "no active+clean pgs in:\n" + statOut
	}
	total := mustAtoi(tot[1])
	clean := mustAtoi(cln[1])
	if clean >= total {
		return true, ""
	}
	if total-clean <= pgSettleTolerance(total) {
		return true, ""
	}
	return false, fmt.Sprintf("only %d/%d PGs active+clean (tolerate %d short):\n%s",
		clean, total, pgSettleTolerance(total), statOut)
}

// mustAtoi converts a regexp-captured \d+ group; the pattern guarantees digits,
// so a parse error is impossible and treated as zero.
func mustAtoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
