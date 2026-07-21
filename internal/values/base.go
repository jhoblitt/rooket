package values

type OperatorInput struct {
	ImageRepo string
	ImageTag  string
	Digest    string
}

// OperatorBase builds rooket's generated layer for the rook-ceph chart.
//
// One provisioner per driver is plenty for a dev cluster, and the HA pair
// starves small hosts. Consumed only by refs where rook manages the CSI drivers
// itself (<= v1.19); newer refs take the drivers chart's default of one replica.
func OperatorBase(in OperatorInput) map[string]any {
	image := map[string]any{
		"repository": in.ImageRepo,
		"tag":        in.ImageTag,
	}
	out := map[string]any{
		"image": image,
		"csi":   map[string]any{"provisionerReplicas": 1},
	}
	// The deploy tag is a mutable branch name and the chart defaults to
	// IfNotPresent, so a rebuild pushing the same tag would neither roll the
	// Deployment nor beat a node-cached image. Pinning the registry's current
	// digest as a pod-template annotation rolls the operator exactly when image
	// content changed; always-pull is required for the roll to matter.
	if in.Digest != "" {
		image["pullPolicy"] = "Always"
		out["annotations"] = map[string]any{"rooket-image-digest": in.Digest}
	}
	return out
}

type StorageNode struct {
	Name    string
	Devices []string
}

type ClusterInput struct {
	OperatorNamespace string
	Nodes             []StorageNode
}

// ClusterBase builds rooket's generated layer for the rook-ceph-cluster chart.
//
// The cpu trims replace the chart's production-HA requests (1 cpu per mon and
// per OSD): on a small host those fill each node's request budget until later
// components — the detect-version jobs, the mds — cannot schedule at all, seen
// as a wedged cluster on 4-vCPU CI runners. Memory requests are left alone
// (rook derives osd_memory_target from them). A standby mgr adds nothing to a
// disposable dev cluster and its requests eat a node's budget.
//
// Naming a device per node keeps rook from mis-attributing OSDs — every
// privileged kind node sees every host disk — so each worker gets exactly one
// OSD on its own disk via rook's direct device path, no local PV, no kubelet
// loop.
func ClusterBase(in ClusterInput) map[string]any {
	spec := map[string]any{
		"mgr": map[string]any{"count": 1},
		"resources": map[string]any{
			"mon": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"osd": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"mgr": map[string]any{"requests": map[string]any{"cpu": "300m"}},
		},
	}
	if len(in.Nodes) > 0 {
		nodes := make([]any, 0, len(in.Nodes))
		for _, n := range in.Nodes {
			devices := make([]any, 0, len(n.Devices))
			for _, d := range n.Devices {
				devices = append(devices, map[string]any{"name": d})
			}
			nodes = append(nodes, map[string]any{"name": n.Name, "devices": devices})
		}
		spec["storage"] = map[string]any{
			"useAllNodes":   false,
			"useAllDevices": false,
			"nodes":         nodes,
		}
	}
	return map[string]any{
		"operatorNamespace": in.OperatorNamespace,
		"toolbox":           map[string]any{"enabled": true},
		"cephClusterSpec":   spec,
	}
}

// CSIBase builds rooket's generated layer for the ceph-csi-drivers chart. The
// RBD and CephFS driver names must carry the operator-namespace prefix that the
// rook-ceph-cluster chart's StorageClasses use as their provisioner; snapshot
// support stays off (kind clusters have no VolumeSnapshot CRDs, and the chart's
// cephfs driver defaults it on); nfs and nvmeof are off until a profile asks.
func CSIBase() map[string]any {
	return map[string]any{
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
}
