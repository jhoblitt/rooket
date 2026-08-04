package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cache"
	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
)

var (
	createName            string
	createWorkers         int
	createRegistryPort    int
	createDiskCount       int
	createISCSIQNDate     string
	createPromCRDsVersion string
	createPromCRDsRelease string
	createNodeImage       string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a kind cluster with a local OCI registry for Rook development",
	Long: `create performs the following steps:

  1. Locate iSCSI block devices set up by 'rooket block setup' and bind-mount
     them — together with /run/udev, which ceph-volume needs to inventory the
     disks — into each worker node via the kind config.
  2. Create the kind cluster (via the selected engine's kind provider). An
     existing cluster whose nodes are stopped — the usual state after a host
     reboot — is started again when its OSD device bindings still match the
     disks resolved in step 1. If they no longer match, that cluster CANNOT be
     resumed and is deleted and recreated, which wipes its OSD disks; every
     other unusable state is reported so you can decide.
  3. Prepare every node: remount /sys read-write and install lvm2 and cryptsetup,
     which Rook needs to provision LVM-backed and encrypted OSDs.
  4. Start a local OCI registry container joined to the kind network, bound to
     localhost:<registry-port> on the host. The registry must be created after
     the cluster so that the "kind" network exists.
  5. Configure containerd on every node to mirror localhost:<registry-port>
     to the registry container (reachable by name on the kind network), and
     each proxied upstream registry to the shared cache.
  6. Apply the standard local-registry-hosting ConfigMap to kube-public.
  7. Install the prometheus-operator-crds helm chart.
  8. Start the shared OCI pull-through cache: a single zot container reused by
     every rooket cluster on the host, so an upstream image is downloaded once
     per workstation instead of once per node per cluster.

Steps 3, 4, 6, 7, and 8 run concurrently; step 5 follows them because it needs
the registry and the cache, and must not exec into a node while step 3 does.

Run 'rooket block setup' before 'rooket cluster create' to prepare block devices.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := useCluster(createName)
		if err != nil {
			return err
		}
		createName = name
		return createClusterRun(os.Stdout, name, createRegistryPort,
			cmd.Flags().Changed("registry-port"), createWorkers, createDiskCount,
			createISCSIQNDate, createPromCRDsVersion, createPromCRDsRelease, createNodeImage)
	},
}

// createClusterRun is the cluster-create core, writing every rooket-emitted
// line and child stream to out. It must not mutate process-global state
// (useCluster's Setenv stays in the cobra wrapper) so a caller can run it
// concurrently with other phases.
func createClusterRun(out io.Writer, name string, requestedPort int, portExplicit bool,
	workers, diskCount int, iqnDate, promVersion, promRelease, nodeImage string) error {
	port, err := resolveRegistryPort(name, requestedPort, portExplicit)
	if err != nil {
		return err
	}
	regName := registry.ContainerName(name)

	// A recorded port can go stale: something else (typically another rooket
	// cluster's registry) now holds it. Our own RUNNING registry is the one
	// thing that legitimately does, so anything else means the port must be
	// re-picked — the steps below (re)wire containerd and the ConfigMap to
	// whatever port ends up in use.
	//
	// A stopped registry of ours cannot be the holder, and cannot be started
	// onto a port it no longer owns, so it has to go: leaving it there made the
	// repair unreachable and turned every later run into the same fatal bind
	// error. Its images are re-pushed by the next 'rooket build push'.
	if !portFree(port) && !registryRunningWithPort(out, containerEngine, regName, port) {
		if registry.Exists(out, containerEngine, regName) {
			run.Fprintf(out, "registry %q is stopped and port %d is held by something else; removing it\n", regName, port)
			if err := registry.Delete(out, containerEngine, regName); err != nil {
				return fmt.Errorf("remove stale registry %q: %w", regName, err)
			}
		}
		old := port
		if port, err = freePort(5001); err != nil {
			return err
		}
		run.Fprintf(out, "recorded registry port %d is now in use elsewhere; using %d instead\n", old, port)
	}
	if err := writeRegistryPort(name, port); err != nil {
		return err
	}

	// --- Step 1: Locate iSCSI block devices ---
	workerDisks := make(map[int][]cluster.Disk)
	if diskCount > 0 {
		run.Fprintf(out, "==> locating iSCSI block devices\n")
		for i := 0; i < workers; i++ {
			for d := 0; d < diskCount; d++ {
				iqn := fmt.Sprintf("iqn.%s.local.rooket:%s-worker%d-disk%d",
					iqnDate, name, i, d)
				dev, err := waitForISCSIDevice(iqn)
				if err != nil {
					return fmt.Errorf("iSCSI device for worker %d disk %d not found "+
						"(run 'rooket block setup' first): %w", i, d, err)
				}
				run.Fprintf(out, "worker%d disk%d: %s\n", i, d, dev)
				workerDisks[i] = append(workerDisks[i], cluster.Disk{
					HostPath:      dev,
					ContainerPath: dev,
				})
			}
		}
	}

	// --- Step 2: kind cluster ---
	// This also creates the "kind" network used by the registry.
	run.Fprintf(out, "==> kind cluster\n")
	clusterCfg := cluster.Config{
		Name:             name,
		Workers:          workers,
		RegistryName:     regName,
		RegistryHostPort: port,
		NodeImage:        nodeImage,
		WorkerDisks:      workerDisks,
	}
	// PrepareNodes keys its per-node device allowlist by node name; the same map
	// tells a resume whether the existing node containers are still bound to
	// these devices.
	ownDevsByNode := make(map[string][]string)
	for i := 0; i < workers; i++ {
		node := workerNodeName(name, i)
		for _, d := range workerDisks[i] {
			ownDevsByNode[node] = append(ownDevsByNode[node], d.HostPath)
		}
	}
	exists, err := cluster.Exists(out, containerEngine, name)
	if err != nil {
		return fmt.Errorf("check cluster existence: %w", err)
	}
	usable := false
	if exists {
		if usable, err = resumeCluster(out, name, ownDevsByNode); err != nil {
			return err
		}
	}
	if !usable {
		if err := cluster.Create(out, clusterCfg); err != nil {
			return fmt.Errorf("create cluster: %w", err)
		}
	}

	// --- Steps 3, 4, 6, 7, 8 run concurrently ---
	// They share only the cluster created above (Step 2) and otherwise touch
	// disjoint subsystems: node prep execs into the worker containers, the
	// registry and the shared cache are host-side containers on the "kind"
	// network, and the ConfigMap and prometheus CRDs go to the apiserver.
	// Step 5 (containerd wiring) is the one that cannot join them — it needs the
	// registry from Step 4 and the cache from Step 8, AND execs into the same
	// nodes as Step 3, and two per-node script passes must not run at once — so
	// it follows the group.
	//
	// cacheReady and cacheErr are written by exactly one branch of the group and
	// read only after runConcurrent joins, so they need no synchronization of
	// their own.
	cacheReady := true
	var cacheErr error
	hEnv, err := helmEnv(name, "rooket")
	if err != nil {
		return err
	}
	// Created after the cluster so the "kind" network exists; --network=kind
	// makes the container reachable by name from cluster nodes.
	regCfg := registry.Config{
		Engine:   containerEngine,
		Name:     regName,
		HostPort: port,
		Network:  "kind",
	}
	run.Fprintf(out, "==> preparing nodes, registry, cache, ConfigMap, and prometheus CRDs (concurrent)\n")
	if err := runConcurrent(out,
		func(w io.Writer) error { // Step 3: prepare nodes for OSD provisioning
			run.Fprintf(w, "==> preparing nodes for OSD provisioning\n")
			if err := cluster.PrepareNodes(w, containerEngine, name, ownDevsByNode); err != nil {
				return fmt.Errorf("prepare nodes: %w", err)
			}
			return nil
		},
		func(w io.Writer) error { // Step 4: local OCI registry
			run.Fprintf(w, "==> creating local OCI registry on the kind network\n")
			if err := registry.Create(w, regCfg); err != nil {
				return fmt.Errorf("create registry: %w", err)
			}
			return nil
		},
		func(w io.Writer) error { // Step 6: registry ConfigMap
			run.Fprintf(w, "==> applying local-registry-hosting ConfigMap\n")
			if err := cluster.ApplyRegistryConfigMap(w, name, regName, port); err != nil {
				return fmt.Errorf("apply registry ConfigMap: %w", err)
			}
			return nil
		},
		func(w io.Writer) error { // Step 7: prometheus-operator CRDs
			run.Fprintf(w, "==> installing prometheus-operator-crds helm chart\n")
			if err := cluster.InstallPrometheusOperatorCRDs(w, name, promRelease, promVersion, hEnv); err != nil {
				return fmt.Errorf("install prometheus-operator-crds: %w", err)
			}
			return nil
		},
		func(w io.Writer) error { // Step 8: shared OCI pull-through cache
			run.Fprintf(w, "==> starting the shared OCI pull-through cache\n")
			if err := setupCache(w); err != nil {
				cacheReady, cacheErr = false, err
				run.Fprintf(w, "warning: image cache unavailable (%v); nodes will pull directly from upstream\n", err)
			}
			return nil
		},
	); err != nil {
		return err
	}

	// --- Step 5: Configure containerd mirrors on each node ---
	// After the concurrent group: it needs the registry (Step 4) and the cache
	// (Step 8), and it execs into the same nodes as node prep (Step 3), so two
	// per-node script passes never run into the same node at once. Registry and
	// cache wiring compose into that one pass for the same reason.
	cacheUpstreams := cache.Upstreams
	if !cacheReady {
		cacheUpstreams = nil
	}
	run.Fprintf(out, "==> configuring containerd mirrors on cluster nodes\n")
	if err := cluster.ConfigureContainerd(out, containerEngine, name, regName, port,
		cache.InClusterAddr(), cacheUpstreams); err != nil {
		return fmt.Errorf("configure containerd mirrors on nodes: %w", err)
	}

	run.Fprintf(out, `
Cluster %q is ready.

  kubectl:           rooket k <args>   (or: export KUBECONFIG="$(rooket kubeconfig --path)")
  local registry:    localhost:%d
  push images with:  %s push localhost:%d/<image>
  image cache:       %s

`, name, port, containerEngine.String(), port, cacheSummary(cacheReady, cacheErr))
	return nil
}

// resumeCluster makes an already-existing cluster usable again and reports
// whether the caller can skip creating it.
//
// `kind get clusters` answers "do containers labelled with this cluster exist",
// not "is the cluster up": a workstation reboot leaves every node Exited — no
// engine restarts containers at boot, and podman's --restart=always covers only
// the podman service restarting — while the cluster still lists. Taking that
// for "ready" is what sent node prep exec'ing into dead containers.
//
// A cluster may only be kept while its OSD device bindings still hold. The kind
// config binds each worker's disk by host path, but the iSCSI initiator hands
// out /dev/sdX in login order, so a reboot can renumber them: a node created
// against /dev/sdb comes back holding whatever /dev/sdb is now — another
// cluster's LUN, or a host disk — inside a privileged container, which is
// exactly what the per-node /dev mask exists to prevent.
//
// Drift on a STOPPED cluster is recovered by discarding it and building a fresh
// one against the resolved paths. Everything else that merely looks wrong —
// an inspect that fails, a cluster that lists with no containers, a resume that
// does not come back — is reported, not recovered: those are indistinguishable
// from a transient engine hiccup or a second rooket run mid-create, and the
// recovery destroys the user's OSD data. 'rooket cluster delete' is the command
// that throws a cluster away, and it is the one the error names.
func resumeCluster(out io.Writer, name string, ownDevsByNode map[string][]string) (bool, error) {
	nodes, err := cluster.Nodes(out, name)
	if err != nil {
		return false, fmt.Errorf("list cluster nodes: %w", err)
	}
	if len(nodes) == 0 {
		return false, fmt.Errorf("cluster %q exists but has no node containers; %s", name, deleteHint(name))
	}
	states, err := cluster.Inspect(out, containerEngine, nodes)
	if err != nil {
		return false, fmt.Errorf("inspect cluster %q: %w", name, err)
	}

	// --disk-count 0 resolves no devices, so there is nothing to compare against
	// and every bound disk would read as drift — which would cost the user a
	// cluster they only meant to skip block setup for. But "cannot check" is not
	// "nothing to check": starting a stopped cluster whose disks were never
	// validated is the stale-bind hazard itself, so that combination is refused
	// rather than either destroyed or resumed blind.
	if len(ownDevsByNode) == 0 && !cluster.AllRunning(states) && cluster.BoundDevices(states) {
		return false, fmt.Errorf("cluster %q is stopped and has OSD disks bound, but this run resolved "+
			"none (--disk-count 0), so its device bindings cannot be checked; re-run with the "+
			"--disk-count it was created with, or %s", name, deleteHint(name))
	}
	if len(ownDevsByNode) > 0 {
		run.Fprintf(out, "checking OSD device bindings against current paths\n")
		checks := cluster.CheckDevices(states, ownDevsByNode)
		for _, c := range checks {
			status := "ok"
			if c.Drifted {
				status = "DRIFTED"
			}
			run.Fprintf(out, "  %s %s -> %s %s\n", c.Node, devList(c.Bound), devList(c.Want), status)
		}
		if cluster.Drifted(checks) {
			if !cluster.AllExited(states) {
				return false, fmt.Errorf("cluster %q has OSD device bindings that no longer match the disks "+
					"resolved for this run, and its nodes are not all stopped (%s), so rooket will not "+
					"discard it on its own; %s", name, nodeStates(states), deleteHint(name))
			}
			run.Fprintf(out, "==> device paths changed since the cluster was created; deleting and recreating\n")
			return false, deleteClusterAndZap(out, name, true)
		}
	}

	if cluster.AllRunning(states) {
		run.Fprintf(out, "cluster %q already exists and is running, skipping creation\n", name)
		// Running node containers say nothing about the control plane inside
		// them, and the steps after this one all talk to the apiserver.
		return true, cluster.WaitAPIServer(out, name)
	}

	run.Fprintf(out, "cluster %q exists but is stopped\n", name)
	run.Fprintf(out, "==> resuming the stopped cluster\n")
	if err := cluster.Start(out, containerEngine, name, nodes); err != nil {
		return false, fmt.Errorf("resume the stopped cluster %q: %w; %s", name, err, deleteHint(name))
	}
	return true, nil
}

// nodeStates renders the per-node engine states for an error that refuses to
// act on a cluster rooket cannot explain, so the user sees which node is in
// which state without a second command.
func nodeStates(states []cluster.NodeState) string {
	parts := make([]string, 0, len(states))
	for _, s := range states {
		parts = append(parts, fmt.Sprintf("%s=%s", s.Node, s.State))
	}
	return strings.Join(parts, " ")
}

func deleteHint(name string) string {
	return fmt.Sprintf("run 'rooket cluster delete --name %s' to discard it (this wipes its OSD disks) and try again", name)
}

// devList renders a node's OSD devices for the drift report, naming the
// control-plane's empty set rather than printing nothing.
func devList(devs []string) string {
	if len(devs) == 0 {
		return "(none)"
	}
	return strings.Join(devs, ",")
}

func init() {
	clusterCmd.AddCommand(createCmd)

	createCmd.Flags().StringVar(&createName, "name", "", "kind cluster name")
	createCmd.Flags().IntVar(&createWorkers, "workers", 3, "number of worker nodes")
	createCmd.Flags().IntVar(&createRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	createCmd.Flags().IntVar(&createDiskCount, "disk-count", 1, "number of iSCSI disks per worker (0 to skip)")
	createCmd.Flags().StringVar(&createISCSIQNDate, "iqn-date", "2003-01", "IQN date component matching 'rooket block setup' (YYYY-MM)")
	createCmd.Flags().StringVar(&createPromCRDsVersion, "prometheus-operator-crds-version", "29.0.0", "version of the prometheus-operator-crds helm chart to install (exact versions enable the reinstall skip)")
	createCmd.Flags().StringVar(&createPromCRDsRelease, "prometheus-operator-crds-release", cluster.DefaultPromCRDsRelease, "helm release name for prometheus-operator-crds")
	createCmd.Flags().StringVar(&createNodeImage, "node-image", defaultNodeImage, "kindest/node image for 'kind create cluster --image' (pin tag@digest for a reproducible Kubernetes version)")
}
