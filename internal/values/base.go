package values

import "fmt"

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
// An empty ImageRepo leaves the chart's own image, as a released chart pins one.
func OperatorBase(in OperatorInput) map[string]any {
	if in.ImageRepo == "" {
		return map[string]any{"csi": map[string]any{"provisionerReplicas": 1}}
	}
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
	// content changed; always-pull is required for the roll to matter. The
	// registry is on localhost, so the pull check is cheap.
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
	// Hosts is the number of workers the cluster's OSDs spread across; 0 when
	// unknown.
	Hosts int
	// ChartDefaults is the rook-ceph-cluster chart's own values.yaml, whose
	// pool lists a cluster of fewer than replicaHosts hosts gets rewritten.
	ChartDefaults map[string]any
}

// replicaHosts is the host count the rook-ceph-cluster chart sizes its mons and
// pools for.
const replicaHosts = 3

// ClusterBase builds rooket's generated layer for the rook-ceph-cluster chart.
//
// The cpu trims replace the chart's production-HA requests (1 cpu per mon and
// per OSD, half a cpu per detect-version job, a tenth per daemon's log
// collector): on a small host those fill each node's request budget until
// later components — the detect-version jobs, the mds — cannot schedule at
// all, seen as a wedged cluster on 4-vCPU CI runners. A detect-version job
// that cannot schedule leaves the CephCluster Progressing indefinitely, though
// Ceph itself is healthy. Memory requests and limits are left
// alone (rook derives osd_memory_target and the MDS cache limit from them). A
// standby mgr adds nothing to a disposable dev cluster and its requests eat a
// node's budget.
//
// Naming a device per node keeps rook from mis-attributing OSDs — every
// privileged kind node sees every host disk — so each worker gets exactly one
// OSD on its own disk via rook's direct device path, no local PV, no kubelet
// loop.
//
// A cluster of fewer than replicaHosts hosts gets one mon and its pools fitted
// to the hosts it has; without them it never settles. Its MDS and RGW get the
// same cpu trim and it runs no standby MDS (see fitPools): a kind node offers
// the whole host's cpus as its request budget, so one worker has a third of
// what three have, and there the chart's system-cluster-critical MDS pair and
// RGW preempt the operator itself.
func ClusterBase(in ClusterInput) map[string]any {
	spec := map[string]any{
		"mgr": map[string]any{"count": 1},
		"resources": map[string]any{
			"mon": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"osd": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"mgr": map[string]any{"requests": map[string]any{"cpu": "300m"}},
			// cmd-reporter is the detect-version job; logcollector is the
			// sidecar in every daemon pod.
			"cmd-reporter": map[string]any{"requests": map[string]any{"cpu": "100m"}},
			"logcollector": map[string]any{"requests": map[string]any{"cpu": "50m"}},
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
	out := map[string]any{
		"operatorNamespace": in.OperatorNamespace,
		"toolbox":           map[string]any{"enabled": true},
		"cephClusterSpec":   spec,
		"configOverride":    configOverride(in.Hosts),
	}
	if fewHosts(in.Hosts) {
		spec["mon"] = map[string]any{"count": 1}
		for _, list := range chartPoolLists {
			if entries, ok := in.ChartDefaults[list].([]any); ok {
				out[list] = fitPools(list, entries, in.Hosts)
			}
		}
	}
	return out
}

func fewHosts(hosts int) bool { return hosts > 0 && hosts < replicaHosts }

// configOverride is the ceph.conf every cluster gets. A kind node's
// /var/lib/rook sits on the host's root filesystem, and a mon refuses to start
// when that runs low on space; the check has to be off in ceph.conf because a
// mon reads the config database only once it is running. Fewer hosts also need
// the pools Ceph creates on its own, such as .mgr, sized to fit.
func configOverride(hosts int) string {
	conf := "[global]\nmon_data_avail_crit = 0\n"
	if fewHosts(hosts) {
		conf += fmt.Sprintf("osd_pool_default_size = %d\n", hosts)
	}
	return conf
}

// chartPoolLists are the rook-ceph-cluster chart's lists of pool-bearing
// resources.
var chartPoolLists = []string{"cephBlockPools", "cephFileSystems", "cephObjectStores"}

// fitPools returns a copy of one of the chart's pool lists with every pool
// fitted to hosts (see fitPool) and every daemon to a small cluster (see
// fitDaemon). The entries are copied whole because Helm replaces a list rather
// than merging it: an entry missing its StorageClass would deploy none.
func fitPools(list string, entries []any, hosts int) []any {
	out := deepCopy(entries).([]any)
	for _, e := range out {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		for _, pool := range poolSpecs(list, entry) {
			fitPool(pool, hosts)
		}
		fitDaemon(list, entry)
	}
	return out
}

// poolSpecs returns the pool specs inside one entry of a chart pool list: a
// CephBlockPool's spec is a pool, a CephFilesystem has a metadata pool and
// data pools, and a CephObjectStore has a metadata pool and a data pool.
func poolSpecs(list string, entry map[string]any) []map[string]any {
	spec, _ := entry["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	var pools []any
	switch list {
	case "cephBlockPools":
		pools = []any{spec}
	case "cephFileSystems":
		pools = []any{spec["metadataPool"]}
		if data, ok := spec["dataPools"].([]any); ok {
			pools = append(pools, data...)
		}
	case "cephObjectStores":
		pools = []any{spec["metadataPool"], spec["dataPool"]}
	}
	out := make([]map[string]any, 0, len(pools))
	for _, p := range pools {
		if m, ok := p.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// fitPool shrinks a pool to at most hosts replicas, in place. An erasure-coded
// pool needs a host per chunk, more than a small cluster has, so it becomes a
// replicated one.
func fitPool(pool map[string]any, hosts int) {
	size := hosts
	rep, _ := pool["replicated"].(map[string]any)
	if rep == nil {
		rep = map[string]any{}
	} else if n, ok := rep["size"].(int); ok && n < size {
		size = n
	}
	delete(pool, "erasureCoded")
	rep["size"] = size
	if size == 1 {
		// Rook refuses a single-replica pool unless told it is intended.
		rep["requireSafeReplicaSize"] = false
	}
	pool["replicated"] = rep
}

// fitDaemon trims, in place, the cpu request of the daemon one entry of a
// chart pool list runs: a CephFilesystem's MDS, which also loses its standby,
// and a CephObjectStore's RGW. An entry that declares no daemon is left alone.
func fitDaemon(list string, entry map[string]any) {
	spec, _ := entry["spec"].(map[string]any)
	if spec == nil {
		return
	}
	switch list {
	case "cephFileSystems":
		if mds, ok := spec["metadataServer"].(map[string]any); ok {
			mds["activeStandby"] = false
			requestCPU(mds, "500m")
		}
	case "cephObjectStores":
		if rgw, ok := spec["gateway"].(map[string]any); ok {
			requestCPU(rgw, "500m")
		}
	}
}

// requestCPU sets a daemon spec's cpu request in place, keeping the rest of its
// resources.
func requestCPU(daemon map[string]any, cpu string) {
	res, _ := daemon["resources"].(map[string]any)
	if res == nil {
		res = map[string]any{}
	}
	req, _ := res["requests"].(map[string]any)
	if req == nil {
		req = map[string]any{}
	}
	req["cpu"] = cpu
	res["requests"] = req
	daemon["resources"] = res
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
