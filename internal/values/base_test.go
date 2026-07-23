package values

import (
	"reflect"
	"testing"
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
