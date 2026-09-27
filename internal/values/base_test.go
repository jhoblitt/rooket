package values

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestOperatorBase(t *testing.T) {
	t.Run("without a digest", func(t *testing.T) {
		got := OperatorBase(OperatorInput{ImageRepo: "localhost:5001/rook/ceph", ImageTag: "master"})
		want := map[string]any{
			"image": map[string]any{
				"repository": "localhost:5001/rook/ceph",
				"tag":        "master",
			},
			"csi": map[string]any{"provisionerReplicas": 1},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})

	// A released chart pins its own operator image by tag, and a released tag
	// does not move, so there is nothing to override or roll.
	t.Run("without an image leaves the chart's own", func(t *testing.T) {
		got := OperatorBase(OperatorInput{})
		want := map[string]any{"csi": map[string]any{"provisionerReplicas": 1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})

	t.Run("with a digest pins pullPolicy and the roll annotation", func(t *testing.T) {
		got := OperatorBase(OperatorInput{
			ImageRepo: "localhost:5001/rook/ceph", ImageTag: "master", Digest: "sha256:abc",
		})
		want := map[string]any{
			"image": map[string]any{
				"repository": "localhost:5001/rook/ceph",
				"tag":        "master",
				"pullPolicy": "Always",
			},
			"csi":         map[string]any{"provisionerReplicas": 1},
			"annotations": map[string]any{"rooket-image-digest": "sha256:abc"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})
}

func TestClusterBase(t *testing.T) {
	t.Run("resource trims always present", func(t *testing.T) {
		got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph"})
		want := map[string]any{
			"operatorNamespace": "rook-ceph",
			"toolbox":           map[string]any{"enabled": true},
			"configOverride":    "[global]\nmon_data_avail_crit = 0\n",
			"cephClusterSpec": map[string]any{
				"mgr": map[string]any{"count": 1},
				"resources": map[string]any{
					"mon": map[string]any{"requests": map[string]any{"cpu": "500m"}},
					"osd": map[string]any{"requests": map[string]any{"cpu": "500m"}},
					"mgr": map[string]any{"requests": map[string]any{"cpu": "300m"}},
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})

	t.Run("pins one device per node", func(t *testing.T) {
		got := ClusterBase(ClusterInput{
			OperatorNamespace: "rook-ceph",
			Nodes: []StorageNode{
				{Name: "c-worker", Devices: []string{"/dev/sdb"}},
				{Name: "c-worker2", Devices: []string{"/dev/sdc"}},
			},
		})
		want := map[string]any{
			"operatorNamespace": "rook-ceph",
			"toolbox":           map[string]any{"enabled": true},
			"configOverride":    "[global]\nmon_data_avail_crit = 0\n",
			"cephClusterSpec": map[string]any{
				"mgr": map[string]any{"count": 1},
				"resources": map[string]any{
					"mon": map[string]any{"requests": map[string]any{"cpu": "500m"}},
					"osd": map[string]any{"requests": map[string]any{"cpu": "500m"}},
					"mgr": map[string]any{"requests": map[string]any{"cpu": "300m"}},
				},
				"storage": map[string]any{
					"useAllNodes":   false,
					"useAllDevices": false,
					"nodes": []any{
						map[string]any{
							"name": "c-worker",
							"devices": []any{
								map[string]any{"name": "/dev/sdb"},
							},
						},
						map[string]any{
							"name": "c-worker2",
							"devices": []any{
								map[string]any{"name": "/dev/sdc"},
							},
						},
					},
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})
}

// chartPools mirrors the pool lists of rook-ceph-cluster's values.yaml, down
// to a StorageClass parameter and the object store's erasure-coded data pool.
const chartPools = `
cephBlockPools:
  - name: ceph-blockpool
    spec:
      failureDomain: host
      replicated:
        size: 3
    storageClass:
      name: ceph-block
      parameters:
        imageFeatures: layering
cephFileSystems:
  - name: ceph-filesystem
    spec:
      metadataPool:
        replicated:
          size: 3
      dataPools:
        - failureDomain: host
          replicated:
            size: 3
          name: data0
    storageClass:
      name: ceph-filesystem
cephObjectStores:
  - name: ceph-objectstore
    spec:
      metadataPool:
        failureDomain: host
        replicated:
          size: 3
      dataPool:
        failureDomain: host
        erasureCoded:
          dataChunks: 2
          codingChunks: 1
        parameters:
          bulk: "true"
    storageClass:
      name: ceph-bucket
`

func loadChartPools(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(chartPools), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// dig walks a decoded values tree by map key or list index.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %q: %T is not a map", k, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at [%d]: %#v is not a list that long", k, v)
			}
			v = l[k]
		}
	}
	return v
}

// The chart sizes every pool for three hosts, and its object store's data pool
// is erasure-coded 2+1 across hosts, which no cluster of fewer hosts can place.
// Helm replaces a list wholesale, so each entry has to come through whole.
func TestClusterBaseFitsThePoolsOfOneHost(t *testing.T) {
	defaults := loadChartPools(t)
	got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph", Hosts: 1, ChartDefaults: defaults})

	single := map[string]any{"size": 1, "requireSafeReplicaSize": false}
	for _, c := range []struct {
		pool string
		path []any
	}{
		{"block pool", []any{"cephBlockPools", 0, "spec"}},
		{"filesystem metadata pool", []any{"cephFileSystems", 0, "spec", "metadataPool"}},
		{"filesystem data pool", []any{"cephFileSystems", 0, "spec", "dataPools", 0}},
		{"object store metadata pool", []any{"cephObjectStores", 0, "spec", "metadataPool"}},
		{"object store data pool", []any{"cephObjectStores", 0, "spec", "dataPool"}},
	} {
		pool := dig(t, got, c.path...).(map[string]any)
		if !reflect.DeepEqual(pool["replicated"], single) {
			t.Errorf("%s replicated = %#v, want %#v", c.pool, pool["replicated"], single)
		}
		if _, ok := pool["erasureCoded"]; ok {
			t.Errorf("%s still erasure-coded: %#v", c.pool, pool["erasureCoded"])
		}
	}

	if got := dig(t, got, "cephBlockPools", 0, "storageClass", "parameters", "imageFeatures"); got != "layering" {
		t.Errorf("block pool StorageClass imageFeatures = %#v, want the chart's layering carried through", got)
	}
	if got := dig(t, got, "cephObjectStores", 0, "spec", "dataPool", "parameters", "bulk"); got != "true" {
		t.Errorf("object store data pool bulk = %#v, want the chart's \"true\" carried through", got)
	}
	if got := dig(t, got, "cephClusterSpec", "mon", "count"); got != 1 {
		t.Errorf("mon count = %#v, want 1", got)
	}
	want := "[global]\nmon_data_avail_crit = 0\nosd_pool_default_size = 1\n"
	if got := got["configOverride"]; got != want {
		t.Errorf("configOverride = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(defaults, loadChartPools(t)) {
		t.Error("ClusterBase modified the chart defaults it was given")
	}
}

func TestClusterBaseFitsThePoolsOfTwoHosts(t *testing.T) {
	got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph", Hosts: 2, ChartDefaults: loadChartPools(t)})

	two := map[string]any{"size": 2}
	if pool := dig(t, got, "cephBlockPools", 0, "spec", "replicated"); !reflect.DeepEqual(pool, two) {
		t.Errorf("block pool replicated = %#v, want %#v", pool, two)
	}
	if pool := dig(t, got, "cephObjectStores", 0, "spec", "dataPool", "replicated"); !reflect.DeepEqual(pool, two) {
		t.Errorf("object store data pool replicated = %#v, want %#v", pool, two)
	}
}

// Fitting only ever shrinks a pool: one the chart already sizes below the host
// count keeps its size.
func TestClusterBaseNeverGrowsAPool(t *testing.T) {
	defaults := map[string]any{"cephBlockPools": []any{map[string]any{
		"name": "scratch",
		"spec": map[string]any{"replicated": map[string]any{"size": 1, "requireSafeReplicaSize": false}},
	}}}
	got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph", Hosts: 2, ChartDefaults: defaults})

	want := map[string]any{"size": 1, "requireSafeReplicaSize": false}
	if pool := dig(t, got, "cephBlockPools", 0, "spec", "replicated"); !reflect.DeepEqual(pool, want) {
		t.Errorf("single-replica pool on two hosts = %#v, want it left at %#v", pool, want)
	}
}

func TestClusterBaseLeavesThreeHostsToTheChart(t *testing.T) {
	got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph", Hosts: 3, ChartDefaults: loadChartPools(t)})

	for _, list := range []string{"cephBlockPools", "cephFileSystems", "cephObjectStores"} {
		if v, ok := got[list]; ok {
			t.Errorf("%s = %#v, want it left to the chart", list, v)
		}
	}
	if mon, ok := dig(t, got, "cephClusterSpec").(map[string]any)["mon"]; ok {
		t.Errorf("mon = %#v, want the chart's count", mon)
	}
}

func TestCSIBase(t *testing.T) {
	got := CSIBase()
	want := map[string]any{
		"operatorConfig": map[string]any{"namespace": "rook-ceph"},
		"drivers": map[string]any{
			"rbd": map[string]any{
				"name":           "rook-ceph.rbd.csi.ceph.com",
				"snapshotPolicy": "none",
			},
			"cephfs": map[string]any{
				"name":           "rook-ceph.cephfs.csi.ceph.com",
				"snapshotPolicy": "none",
			},
			"nfs":    map[string]any{"enabled": false},
			"nvmeof": map[string]any{"enabled": false},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}
