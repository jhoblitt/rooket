package cluster

import (
	"slices"
	"testing"
)

func TestParseNodeState(t *testing.T) {
	const running = `running /dev/disk:/dev/disk:rslave,nosuid,rbind /dev/sdd:/dev/sdd:rslave,nosuid,rbind`
	got := parseNodeState("rook-worker", running)
	if !got.Running() {
		t.Errorf("expected a %q container to be running", "running")
	}
	if !slices.Equal(got.OSDDevs, []string{"/dev/sdd"}) {
		t.Errorf("OSDDevs = %v, want [/dev/sdd]", got.OSDDevs)
	}

	// The state that started this: a node stopped by a host reboot still lists
	// as a cluster member, so "exited" must not read as usable.
	stopped := parseNodeState("rook-worker", `exited /dev/sdd:/dev/sdd:rslave,nosuid,rbind`)
	if stopped.Running() {
		t.Errorf("an exited container must not report Running")
	}

	// The control-plane binds no disk, and inspect renders it with no binds.
	cp := parseNodeState("rook-control-plane", "created")
	if cp.Running() || len(cp.OSDDevs) != 0 {
		t.Errorf("bare state line parsed as %+v, want not-running with no devices", cp)
	}

	if empty := parseNodeState("rook-worker", ""); empty.Running() || len(empty.OSDDevs) != 0 {
		t.Errorf("empty line parsed as %+v, want the zero value", empty)
	}
}

func TestOSDDeviceBindsExcludesInfrastructureMounts(t *testing.T) {
	got := osdDeviceBinds([]string{
		"/run/udev:/run/udev:rslave,nosuid,rbind",
		"/dev/disk:/dev/disk:rslave,nosuid,rbind",
		"/dev/disk/by-path:/dev/disk/by-path:rslave,nosuid,rbind",
		// kind's own providers add this to every node when the engine's
		// graphroot is on btrfs, xfs, or zfs. Counting it as an OSD device
		// makes every node read as drifted and destroys a resumable cluster.
		"/dev/mapper:/dev/mapper:rslave,nosuid,rbind",
		"/dev/sde:/dev/sde:rslave,nosuid,rbind",
		"/dev/sdb:/dev/sdb:rslave,nosuid,rbind",
		"malformed-bind-without-a-colon",
	})
	// Sorted, so a comparison against the resolved devices is order-independent.
	if !slices.Equal(got, []string{"/dev/sdb", "/dev/sde"}) {
		t.Errorf("osdDeviceBinds = %v, want [/dev/sdb /dev/sde]", got)
	}
}

func TestAllRunning(t *testing.T) {
	up := []NodeState{{Node: "a", State: "running"}, {Node: "b", State: "running"}}
	if !AllRunning(up) {
		t.Errorf("expected all-running states to report true")
	}
	if AllRunning([]NodeState{{Node: "a", State: "running"}, {Node: "b", State: "exited"}}) {
		t.Errorf("a single stopped node must make AllRunning false")
	}
	// No nodes is not "all running": it must not short-circuit into a resume.
	if AllRunning(nil) {
		t.Errorf("no nodes must not report all-running")
	}
}

// TestCheckDevicesDetectsRenumberedDisk reproduces the failure this recovery
// path exists for: after a reboot the iSCSI initiator renumbered the LUNs, so a
// worker container is still bound to the /dev/sdX it was created against while
// its actual disk moved. Starting it would bind an unrelated host device into a
// privileged node.
func TestCheckDevicesDetectsRenumberedDisk(t *testing.T) {
	states := []NodeState{
		{Node: "rook-worker", OSDDevs: []string{"/dev/sdd"}},
		{Node: "rook-worker2", OSDDevs: []string{"/dev/sde"}},
		{Node: "rook-worker3", OSDDevs: []string{"/dev/sdb"}},
		{Node: "rook-control-plane"},
	}
	own := map[string][]string{
		"rook-worker":  {"/dev/sdd"},
		"rook-worker2": {"/dev/sde"},
		"rook-worker3": {"/dev/sdc"},
	}

	checks := CheckDevices(states, own)
	if len(checks) != len(states) {
		t.Fatalf("got %d checks for %d nodes", len(checks), len(states))
	}
	drifted := map[string]bool{}
	for _, c := range checks {
		drifted[c.Node] = c.Drifted
	}
	for node, want := range map[string]bool{
		"rook-worker":        false,
		"rook-worker2":       false,
		"rook-worker3":       true,
		"rook-control-plane": false,
	} {
		if drifted[node] != want {
			t.Errorf("%s drifted = %v, want %v", node, drifted[node], want)
		}
	}
	if !Drifted(checks) {
		t.Errorf("a renumbered disk must make the cluster non-resumable")
	}
}

// TestParseNodeStateRealInspectOutput runs verbatim podman output through the
// parser, so a change to nodeStateFormat or to what kind mounts cannot quietly
// start classifying the /var volume or /lib/modules as an OSD disk — which
// would read as drift on every node and recreate a perfectly resumable cluster.
func TestParseNodeStateRealInspectOutput(t *testing.T) {
	const worker = `exited 53445fcdc4ddde98fb2c4c3bd97c5125aa2d015d99f98baed401a540586d8a6e:/var:suid,exec,dev,rprivate,rbind /lib/modules:/lib/modules:ro,rprivate,rbind /run/udev:/run/udev:rslave,nosuid,nodev,rbind /dev/disk:/dev/disk:rslave,nosuid,rbind /dev/sdd:/dev/sdd:rslave,nosuid,rbind`
	const controlPlane = `exited f42e81809336698a3927852d71fd5442a1328485a826117381638d0527e09b00:/var:suid,exec,dev,rprivate,rbind /lib/modules:/lib/modules:ro,rprivate,rbind`

	if got := parseNodeState("n", worker); !slices.Equal(got.OSDDevs, []string{"/dev/sdd"}) {
		t.Errorf("worker OSDDevs = %v, want [/dev/sdd]", got.OSDDevs)
	}
	if got := parseNodeState("n", controlPlane); len(got.OSDDevs) != 0 {
		t.Errorf("control-plane OSDDevs = %v, want none", got.OSDDevs)
	}
}

func TestCheckDevicesUnchangedIsResumable(t *testing.T) {
	states := []NodeState{
		{Node: "rook-worker", OSDDevs: []string{"/dev/sdd"}},
		{Node: "rook-control-plane"},
	}
	checks := CheckDevices(states, map[string][]string{"rook-worker": {"/dev/sdd"}})
	if Drifted(checks) {
		t.Errorf("matching bindings must be resumable: %+v", checks)
	}
}

// A worker with several disks must compare as a set: the resolved order comes
// from the disk loop, the bind order from the engine, and neither is canonical.
func TestCheckDevicesIgnoresDeviceOrder(t *testing.T) {
	states := []NodeState{{Node: "rook-worker", OSDDevs: osdDeviceBinds([]string{
		"/dev/sde:/dev/sde:rslave", "/dev/sdc:/dev/sdc:rslave",
	})}}
	checks := CheckDevices(states, map[string][]string{"rook-worker": {"/dev/sde", "/dev/sdc"}})
	if Drifted(checks) {
		t.Errorf("device order must not count as drift: %+v", checks)
	}
}

// A node that lost or gained a disk is drift too, not just a renumbered one.
func TestCheckDevicesCountChangeIsDrift(t *testing.T) {
	states := []NodeState{
		{Node: "rook-worker", OSDDevs: []string{"/dev/sdd"}},
		{Node: "rook-worker2", OSDDevs: []string{"/dev/sde"}},
	}
	checks := CheckDevices(states, map[string][]string{
		"rook-worker": {"/dev/sdd", "/dev/sdf"},
	})
	if !checks[0].Drifted {
		t.Errorf("an added disk must count as drift: %+v", checks[0])
	}
	// worker2 is absent from the map — a cluster rebuilt with fewer disks — so
	// its recorded bind is now unaccounted for.
	if !checks[1].Drifted {
		t.Errorf("a disk no longer resolved for the node must count as drift: %+v", checks[1])
	}
}

// A node the run resolved disks for that the cluster does not have is drift too.
// Checking only the observed containers would resume a 3-worker cluster for a
// --workers 5 run and report it ready.
func TestCheckDevicesMissingNodeIsDrift(t *testing.T) {
	checks := CheckDevices(
		[]NodeState{{Node: "rook-worker", OSDDevs: []string{"/dev/sdd"}}},
		map[string][]string{"rook-worker": {"/dev/sdd"}, "rook-worker2": {"/dev/sde"}},
	)
	if !Drifted(checks) {
		t.Fatalf("a node the cluster does not have must count as drift: %+v", checks)
	}
	if len(checks) != 2 || checks[1].Node != "rook-worker2" || len(checks[1].Bound) != 0 {
		t.Errorf("checks = %+v, want a second entry for rook-worker2 with nothing bound", checks)
	}
}

// Bound is sorted by CheckDevices rather than trusted: NodeState is exported and
// constructible by hand, and an unsorted Bound comparing as drift costs a cluster.
func TestCheckDevicesSortsBothSides(t *testing.T) {
	checks := CheckDevices(
		[]NodeState{{Node: "rook-worker", OSDDevs: []string{"/dev/sde", "/dev/sdc"}}},
		map[string][]string{"rook-worker": {"/dev/sdc", "/dev/sde"}},
	)
	if Drifted(checks) {
		t.Errorf("unsorted input must not read as drift: %+v", checks)
	}
}

// AllExited is the gate on destroying a cluster, so it must accept ONLY the
// state a reboot produces. "not running" is not good enough: a paused or
// half-created node means something rooket did not do happened to the cluster.
func TestAllExitedAcceptsOnlyStoppedNodes(t *testing.T) {
	if !AllExited([]NodeState{{Node: "a", State: "exited"}, {Node: "b", State: "exited"}}) {
		t.Errorf("all-exited nodes must be eligible for recovery")
	}
	for _, state := range []string{"paused", "created", "restarting", "dead", "running", ""} {
		states := []NodeState{{Node: "a", State: "exited"}, {Node: "b", State: state}}
		if AllExited(states) {
			t.Errorf("a %q node must not be eligible for destructive recovery", state)
		}
	}
	if AllExited(nil) {
		t.Errorf("no nodes must not be eligible for destructive recovery")
	}
}

// A --disk-count 0 run resolves nothing to compare against, so it has to tell
// "nothing to check" apart from "cannot check what is bound" — the latter must
// not be resumed blind.
func TestBoundDevices(t *testing.T) {
	if BoundDevices([]NodeState{{Node: "cp"}, {Node: "w"}}) {
		t.Errorf("a cluster with no OSD binds has nothing to check")
	}
	if !BoundDevices([]NodeState{{Node: "cp"}, {Node: "w", OSDDevs: []string{"/dev/sdd"}}}) {
		t.Errorf("a bound OSD disk must be visible to the --disk-count 0 guard")
	}
}
