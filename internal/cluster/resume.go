package cluster

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/run"
)

const (
	// apiServerTimeout bounds the wait for a resumed cluster's apiserver. A
	// restarted control-plane restarts etcd and the static pods, which is slower
	// than a cold start of the same components; past this the resume is reported
	// as failed rather than waited on indefinitely.
	apiServerTimeout = 3 * time.Minute
	// nodeReadyTimeout bounds the wait for resumed kubelets to re-register. It
	// is the shorter of the two because the apiserver is already answering by
	// the time it starts, so a kubelet that has not come back by now is not
	// coming back on its own.
	nodeReadyTimeout = 2 * time.Minute
)

// nodeStateFormat renders one node container as "<state> <bind>...". Both
// engines expose .State.Status and .HostConfig.Binds, and inspecting every node
// in a single call returns the results in argument order.
const nodeStateFormat = `{{.State.Status}} {{range .HostConfig.Binds}}{{.}} {{end}}`

// NodeState is a node container's run state together with the OSD block devices
// bound into it when the cluster was created.
type NodeState struct {
	Node string
	// State is the engine's status verbatim — running, exited, paused, created,
	// restarting, dead — rather than a bool. Destroying a cluster is gated on
	// every node being confirmed exited, and "not running" would fold a paused,
	// half-created, or dead node into that: states rooket did not produce and
	// cannot reason about, which deserve a report rather than a wipe.
	State string
	// OSDDevs are host device paths, sorted, as recorded in the container's
	// bind list — NOT what 'rooket block setup' resolves today. The gap between
	// the two is the whole point of CheckDevices.
	OSDDevs []string
}

// Running reports whether the node container is up.
func (s NodeState) Running() bool { return s.State == "running" }

// DeviceCheck compares the OSD devices bound into a node when the cluster was
// created against the devices resolved for that node in this run.
type DeviceCheck struct {
	Node    string
	Bound   []string
	Want    []string
	Drifted bool
}

// Inspect reports one NodeState per name in nodes, in that order — which holds
// only because a single inspect call answers in argument order, so a short or
// long result is a protocol change rather than a partial answer and is an error.
func Inspect(w io.Writer, eng engine.Engine, nodes []string) ([]NodeState, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	out, err := run.OutputTo(w, eng.String(),
		append([]string{"inspect", "--format", nodeStateFormat}, nodes...)...)
	if err != nil {
		return nil, fmt.Errorf("inspect node containers: %w", withStderr(err))
	}
	lines := strings.Split(out, "\n")
	if len(lines) != len(nodes) {
		return nil, fmt.Errorf("inspect returned %d lines for %d node containers", len(lines), len(nodes))
	}
	states := make([]NodeState, len(nodes))
	for i, node := range nodes {
		states[i] = parseNodeState(node, lines[i])
	}
	return states, nil
}

func parseNodeState(node, line string) NodeState {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return NodeState{Node: node}
	}
	return NodeState{
		Node:    node,
		State:   fields[0],
		OSDDevs: osdDeviceBinds(fields[1:]),
	}
}

// infraDevBinds are the /dev trees bound into a node for reasons other than an
// OSD disk, and so must not read as one. /dev/disk is rooket's own (see
// kindConfigTmpl); /dev/mapper is kind's, added by both its podman and docker
// providers whenever the engine's graphroot sits on btrfs, xfs, or zfs — xfs
// being the default on the RHEL family, so this is a normal host, not an exotic
// one. Counting either as an OSD device makes EVERY node read as drifted and
// discards a cluster that was perfectly resumable.
var infraDevBinds = []string{"/dev/disk", "/dev/mapper"}

// osdDeviceBinds picks the OSD disks out of a node container's bind list, each
// of which is "hostPath:containerPath:options". Anything under /dev that is not
// an infraDevBinds tree is one of the cluster's OSD devices.
func osdDeviceBinds(binds []string) []string {
	var devs []string
	for _, b := range binds {
		host, _, ok := strings.Cut(b, ":")
		if !ok || !strings.HasPrefix(host, "/dev/") || isInfraDevBind(host) {
			continue
		}
		devs = append(devs, host)
	}
	sort.Strings(devs)
	return devs
}

func isInfraDevBind(host string) bool {
	return slices.ContainsFunc(infraDevBinds, func(p string) bool {
		return host == p || strings.HasPrefix(host, p+"/")
	})
}

// AllRunning reports whether every node container is up. No nodes at all is
// false, not vacuously true: an empty inspect result must never short-circuit
// into a resume.
func AllRunning(states []NodeState) bool {
	for _, s := range states {
		if !s.Running() {
			return false
		}
	}
	return len(states) > 0
}

// AllExited reports whether every node container is confirmed stopped. The
// destructive half of drift recovery is gated on this rather than on
// !AllRunning: a paused, restarting, half-created or dead node means something
// other than a reboot happened, and rooket has no basis for wiping a cluster it
// cannot explain.
func AllExited(states []NodeState) bool {
	for _, s := range states {
		if s.State != "exited" {
			return false
		}
	}
	return len(states) > 0
}

// BoundDevices reports whether any node still has an OSD disk bound into it.
// A run that resolved no devices of its own (--disk-count 0) uses this to tell
// "nothing to check" apart from "cannot check what is there".
func BoundDevices(states []NodeState) bool {
	return slices.ContainsFunc(states, func(s NodeState) bool { return len(s.OSDDevs) > 0 })
}

// CheckDevices compares each node's recorded OSD bindings against the devices
// resolved for it now, keyed by node name exactly as PrepareNodes keys them.
// A node absent from ownDevsByNode (the control-plane) must have no OSD bind.
//
// It reports on the union of both sides, not just the observed containers: a
// node the caller resolved devices for that the cluster does not have is drift
// too — the cluster is smaller than the one being asked for — and checking only
// `states` would resume a 3-worker cluster for a --workers 5 run and call it
// ready.
func CheckDevices(states []NodeState, ownDevsByNode map[string][]string) []DeviceCheck {
	checks := make([]DeviceCheck, 0, len(states))
	seen := make(map[string]bool, len(states))
	for _, s := range states {
		seen[s.Node] = true
		checks = append(checks, newDeviceCheck(s.Node, s.OSDDevs, ownDevsByNode[s.Node]))
	}
	for _, node := range slices.Sorted(maps.Keys(ownDevsByNode)) {
		if !seen[node] {
			checks = append(checks, newDeviceCheck(node, nil, ownDevsByNode[node]))
		}
	}
	return checks
}

// newDeviceCheck sorts both sides rather than trusting either: NodeState is
// exported and constructible by hand, and an unsorted Bound would compare as
// drift and cost the caller its cluster.
func newDeviceCheck(node string, bound, want []string) DeviceCheck {
	bound, want = slices.Clone(bound), slices.Clone(want)
	sort.Strings(bound)
	sort.Strings(want)
	return DeviceCheck{Node: node, Bound: bound, Want: want, Drifted: !slices.Equal(bound, want)}
}

// Drifted reports whether any node's OSD bindings no longer match.
func Drifted(checks []DeviceCheck) bool {
	return slices.ContainsFunc(checks, func(c DeviceCheck) bool { return c.Drifted })
}

// Start starts a stopped cluster's node containers and waits for it to serve
// again. Starting an already-running container is a no-op, so a cluster that
// stopped only partway is handled by the same call.
//
// The waits are what make a resume safe to hand back to the caller: the
// concurrent steps that follow creation talk to the apiserver, and the deploy
// after them schedules pods, so returning at "containers started" would race
// both.
func Start(w io.Writer, eng engine.Engine, clusterName string, nodes []string) error {
	if err := run.CmdTo(w, eng.String(), append([]string{"start"}, nodes...)...); err != nil {
		return fmt.Errorf("start node containers: %w", err)
	}
	if err := WaitAPIServer(w, clusterName); err != nil {
		return err
	}
	// NOTE: on a resume this is close to a no-op — the Node objects come back
	// from etcd still carrying the Ready they had when the cluster stopped, and
	// kube-controller-manager only flips them to Unknown a grace period after IT
	// starts. It still covers the case where a kubelet never comes back at all.
	run.Fprintf(w, "waiting for nodes to re-register\n")
	return run.CmdTo(w, "kubectl", "--context", "kind-"+clusterName,
		"wait", "--for=condition=Ready", "nodes", "--all",
		"--timeout="+nodeReadyTimeout.String())
}

// WaitAPIServer blocks until the cluster's apiserver serves /readyz. Every
// caller that hands a cluster to the steps after creation needs it, resumed or
// merely found running: "the node containers are up" says nothing about whether
// the control plane inside them is.
func WaitAPIServer(w io.Writer, clusterName string) error {
	run.Fprintf(w, "waiting for the apiserver to answer\n")
	return waitForAPIServer(clusterName, apiServerTimeout)
}

// waitForAPIServer polls /readyz until the apiserver answers. The polls are
// silent (their trace would otherwise repeat for minutes) and the last failure —
// stderr included — is folded into the timeout error so a caller reports why it
// never came up.
//
// --request-timeout is what makes the deadline enforceable: kubectl waits
// forever by default, and one poll against an apiserver that completes the TLS
// handshake but never answers would otherwise hang the whole command silently,
// past any deadline checked between polls.
func waitForAPIServer(clusterName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := run.OutputTo(io.Discard, "kubectl",
			"--context", "kind-"+clusterName, "--request-timeout=10s",
			"get", "--raw", "/readyz")
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("apiserver did not become ready within %s: %w", timeout, withStderr(err))
		}
		time.Sleep(2 * time.Second)
	}
}

// withStderr folds a failed command's captured stderr into its error. The
// Output helpers capture it onto the ExitError, where "exit status 1" is all a
// caller would otherwise be able to report — and these errors are the ones that
// decide whether a cluster is discarded.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
