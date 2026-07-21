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
		img := got["image"].(map[string]any)
		if img["pullPolicy"] != "Always" {
			t.Errorf("pullPolicy = %v, want Always", img["pullPolicy"])
		}
		ann := got["annotations"].(map[string]any)
		if ann["rooket-image-digest"] != "sha256:abc" {
			t.Errorf("annotation = %v", ann["rooket-image-digest"])
		}
	})
}

func TestClusterBase(t *testing.T) {
	t.Run("resource trims always present", func(t *testing.T) {
		got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph"})
		spec := got["cephClusterSpec"].(map[string]any)
		res := spec["resources"].(map[string]any)
		if res["mon"].(map[string]any)["requests"].(map[string]any)["cpu"] != "500m" {
			t.Errorf("mon cpu = %#v", res["mon"])
		}
		if spec["mgr"].(map[string]any)["count"] != 1 {
			t.Errorf("mgr count = %#v", spec["mgr"])
		}
		if got["toolbox"].(map[string]any)["enabled"] != true {
			t.Errorf("toolbox = %#v", got["toolbox"])
		}
		if _, ok := spec["storage"]; ok {
			t.Error("storage must be absent when no nodes are given")
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
		storage := got["cephClusterSpec"].(map[string]any)["storage"].(map[string]any)
		if storage["useAllNodes"] != false || storage["useAllDevices"] != false {
			t.Errorf("storage = %#v", storage)
		}
		nodes := storage["nodes"].([]any)
		if len(nodes) != 2 {
			t.Fatalf("nodes = %#v", nodes)
		}
		first := nodes[0].(map[string]any)
		if first["name"] != "c-worker" {
			t.Errorf("node name = %v", first["name"])
		}
		devs := first["devices"].([]any)
		if devs[0].(map[string]any)["name"] != "/dev/sdb" {
			t.Errorf("devices = %#v", devs)
		}
	})
}

func TestCSIBase(t *testing.T) {
	got := CSIBase()
	drivers := got["drivers"].(map[string]any)
	if drivers["rbd"].(map[string]any)["name"] != "rook-ceph.rbd.csi.ceph.com" {
		t.Errorf("rbd = %#v", drivers["rbd"])
	}
	if drivers["nfs"].(map[string]any)["enabled"] != false {
		t.Errorf("nfs = %#v", drivers["nfs"])
	}
	if got["operatorConfig"].(map[string]any)["namespace"] != "rook-ceph" {
		t.Errorf("operatorConfig = %#v", got["operatorConfig"])
	}
}
