package cmd

import "testing"

// Resume rebuilds a cluster only when the run asked for a different worker
// count than the cluster was created with — not whenever its node set looks
// wrong, which a missing container or a run racing another mid-create also
// produce, and which are reported rather than destroyed. The device check
// alone misses the request whenever the nodes coming or going hold no disks
// (--disk-count 0).
func TestWorkerCountChanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := writeShape("single", clusterShape{Workers: 1, DiskCount: 0, IQNDate: "2003-01"}); err != nil {
		t.Fatal(err)
	}

	if from, changed := workerCountChanged("single", 3); !changed || from != 1 {
		t.Errorf("workerCountChanged(single, 3) = (%d, %v), want (1, true)", from, changed)
	}
	if _, changed := workerCountChanged("single", 1); changed {
		t.Error("workerCountChanged(single, 1) = true for the recorded count, want false")
	}
	// A cluster from before rooket recorded shapes gives no basis to rebuild on.
	if _, changed := workerCountChanged("predates-records", 3); changed {
		t.Error("workerCountChanged with no record = true, want false")
	}
}
