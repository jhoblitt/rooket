// No build tag: this runs in the plain `go test` unit job, unlike the rest of
// the e2e package. See health.go.

package e2e

import "testing"

func TestPgsSettledEnough(t *testing.T) {
	// The exact line that flaked CI: 264/265, one PG peering. Functionally
	// healthy (data flowing) and one short — must count as settled.
	const flaked = "265 pgs: 1 peering, 264 active+clean; 621 KiB data, 137 MiB used, 30 GiB / 30 GiB avail; 1.2 KiB/s rd, 2 op/s"

	// The other line that failed CI, on a cluster that was fully healthy: all
	// 265 PGs are active+clean, 6 of them additionally scrubbing.
	const scrubbing = "265 pgs: 6 active+clean+scrubbing, 259 active+clean; 3.8 MiB data, 196 MiB used, 30 GiB / 30 GiB avail; 938 B/s rd, 1 op/s"

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"all clean", "265 pgs: 265 active+clean", true},
		{"scrubbing PGs are clean, the CI failure", scrubbing, true},
		{"deep scrubbing is clean too", "265 pgs: 3 active+clean+scrubbing+deep, 262 active+clean", true},
		{"every PG scrubbing", "265 pgs: 265 active+clean+scrubbing", true},
		{"scrubbing does not mask a real straggler", "265 pgs: 6 active+clean+scrubbing, 256 active+clean, 3 peering", false},
		{"one peering, the CI flake", flaked, true},
		{"two short on a ~265 cluster", "265 pgs: 2 peering, 263 active+clean", true},
		{"three short exceeds ~1% tolerance", "265 pgs: 3 peering, 262 active+clean", false},
		{"small cluster, one short is tolerated", "64 pgs: 1 peering, 63 active+clean", true},
		{"small cluster, two short is not", "64 pgs: 2 peering, 62 active+clean", false},
		{"half the cluster stuck is a real failure", "265 pgs: 130 peering, 135 active+clean", false},
		{"nothing clean yet", "265 pgs: 265 creating", false},
		{"no pg total", "waiting for the mgr", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := pgsSettledEnough(c.in)
			if got != c.want {
				t.Errorf("pgsSettledEnough(%q) = %v (detail=%q), want %v", c.in, got, detail, c.want)
			}
			if !got && detail == "" {
				t.Errorf("a not-settled result must carry a detail message for the assertion")
			}
		})
	}
}

// TestPgSettleTolerance pins the ~1%-but-at-least-1 scaling, so the tolerance
// stays proportional rather than a fixed count that means different things on a
// 60-PG versus a 600-PG cluster.
func TestPgSettleTolerance(t *testing.T) {
	for _, c := range []struct{ total, want int }{
		{0, 1}, {64, 1}, {100, 1}, {200, 2}, {265, 2}, {600, 6},
	} {
		if got := pgSettleTolerance(c.total); got != c.want {
			t.Errorf("pgSettleTolerance(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}
