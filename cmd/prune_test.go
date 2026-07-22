package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/jhoblitt/rooket/internal/engine"
)

func TestParseStrandedByPathLink(t *testing.T) {
	cases := []struct {
		name          string
		link          string
		wantOK        bool
		wantCluster   string
		wantBackstore string
		wantTargetIQN string
	}{
		{
			name:          "cluster name containing dashes",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0-lun-0",
			wantOK:        true,
			wantCluster:   "home-jhoblitt-github-rook",
			wantBackstore: "home-jhoblitt-github-rook-worker0-disk0",
			wantTargetIQN: "iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0",
		},
		{
			name:          "short cluster name",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker0-disk0-lun-0",
			wantOK:        true,
			wantCluster:   "rook3",
			wantBackstore: "rook3-worker0-disk0",
			wantTargetIQN: "iqn.2003-01.local.rooket:rook3-worker0-disk0",
		},
		{
			name:          "multi-worker multi-disk index",
			link:          "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook6-worker2-disk3-lun-0",
			wantOK:        true,
			wantCluster:   "rook6",
			wantBackstore: "rook6-worker2-disk3",
			wantTargetIQN: "iqn.2003-01.local.rooket:rook6-worker2-disk3",
		},
		{
			name:   "non-rooket iSCSI target ignored",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.com.example:cluster-worker0-disk0-lun-0",
			wantOK: false,
		},
		{
			name:   "non-iSCSI by-path entry ignored",
			link:   "pci-0000:00:1f.2-ata-1.0-part1",
			wantOK: false,
		},
		{
			name:   "malformed: no cluster before worker/disk suffix",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:worker0-disk0-lun-0",
			wantOK: false,
		},
		{
			name:   "malformed: empty backstore name",
			link:   "ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:-lun-0",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disk, cluster, ok := parseStrandedByPathLink(tc.link)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (disk=%+v cluster=%q)", ok, tc.wantOK, disk, cluster)
			}
			if !ok {
				if cluster != "" {
					t.Errorf("cluster = %q on a rejected link, want empty", cluster)
				}
				return
			}
			if cluster != tc.wantCluster {
				t.Errorf("cluster = %q, want %q", cluster, tc.wantCluster)
			}
			if disk.backstoreName != tc.wantBackstore {
				t.Errorf("backstoreName = %q, want %q", disk.backstoreName, tc.wantBackstore)
			}
			if disk.targetIQN != tc.wantTargetIQN {
				t.Errorf("targetIQN = %q, want %q", disk.targetIQN, tc.wantTargetIQN)
			}
		})
	}
}

func TestDiscoverStrandedByPath(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker0-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:rook3-worker1-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.local.rooket:home-jhoblitt-github-rook-worker0-disk0-lun-0",
		"ip-127.0.0.1:3260-iscsi-iqn.2003-01.com.example:other-worker0-disk0-lun-0", // non-rooket, ignored
		"pci-0000:00:1f.2-ata-1.0-part1", // non-iSCSI, ignored
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	found, err := discoverStrandedByPath(dir)
	if err != nil {
		t.Fatalf("discoverStrandedByPath: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d clusters, want 2: %+v", len(found), found)
	}
	if len(found["rook3"]) != 2 {
		t.Errorf("rook3 disks = %d, want 2", len(found["rook3"]))
	}
	if len(found["home-jhoblitt-github-rook"]) != 1 {
		t.Errorf("home-jhoblitt-github-rook disks = %d, want 1", len(found["home-jhoblitt-github-rook"]))
	}

	t.Run("missing directory is not an error", func(t *testing.T) {
		found, err := discoverStrandedByPath(filepath.Join(dir, "nonexistent"))
		if err != nil {
			t.Fatalf("discoverStrandedByPath on missing dir: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("found = %+v, want none", found)
		}
	})
}

func TestStrandableClusters(t *testing.T) {
	found := map[string][]iscsiDisk{
		"orphaned":  {{targetIQN: "iqn.2003-01.local.rooket:orphaned-worker0-disk0"}},
		"live":      {{targetIQN: "iqn.2003-01.local.rooket:live-worker0-disk0"}},
		"has-state": {{targetIQN: "iqn.2003-01.local.rooket:has-state-worker0-disk0"}},
	}
	live := map[string][]engine.Engine{
		"live": {engine.Podman},
	}
	hasState := map[string]bool{
		"has-state": true,
	}

	got := strandableClusters(found, live, hasState)
	want := []string{"orphaned"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("strandableClusters = %v, want %v", got, want)
	}
}
