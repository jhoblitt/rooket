package cmd

import (
	"fmt"
	"io"
	"os"

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
  2. Create the kind cluster (via the selected engine's kind provider).
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

	// A recorded port can go stale: this cluster's registry is gone and
	// something else (typically another rooket cluster's registry) now holds
	// the port. With no registry container of our own to preserve, re-pick a
	// free port — the steps below (re)wire containerd and the ConfigMap to
	// whatever port ends up in use.
	if !registry.Exists(out, containerEngine, regName) && !portFree(port) {
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
	run.Fprintf(out, "==> creating kind cluster\n")
	clusterCfg := cluster.Config{
		Name:             name,
		Workers:          workers,
		RegistryName:     regName,
		RegistryHostPort: port,
		NodeImage:        nodeImage,
		WorkerDisks:      workerDisks,
	}
	exists, err := cluster.Exists(out, containerEngine, name)
	if err != nil {
		return fmt.Errorf("check cluster existence: %w", err)
	}
	if exists {
		run.Fprintf(out, "cluster %q already exists, skipping creation\n", name)
	} else {
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
	// cacheReady is written by exactly one branch of the group and read only
	// after runConcurrent joins, so it needs no synchronization of its own.
	cacheReady := true
	ownDevsByNode := make(map[string][]string)
	for i := 0; i < workers; i++ {
		node := workerNodeName(name, i)
		for _, d := range workerDisks[i] {
			ownDevsByNode[node] = append(ownDevsByNode[node], d.HostPath)
		}
	}
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
				cacheReady = false
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

`, name, port, containerEngine.String(), port)
	return nil
}

func init() {
	clusterCmd.AddCommand(createCmd)

	createCmd.Flags().StringVar(&createName, "name", "", "kind cluster name")
	createCmd.Flags().IntVar(&createWorkers, "workers", 3, "number of worker nodes")
	createCmd.Flags().IntVar(&createRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	createCmd.Flags().IntVar(&createDiskCount, "disk-count", 1, "number of iSCSI disks per worker (0 to skip)")
	createCmd.Flags().StringVar(&createISCSIQNDate, "iqn-date", "2003-01", "IQN date component matching 'rooket block setup' (YYYY-MM)")
	createCmd.Flags().StringVar(&createPromCRDsVersion, "prometheus-operator-crds-version", "29.0.0", "version of the prometheus-operator-crds helm chart to install (exact versions enable the reinstall skip)")
	createCmd.Flags().StringVar(&createPromCRDsRelease, "prometheus-operator-crds-release", "my-prometheus-operator-crds", "helm release name for prometheus-operator-crds")
	createCmd.Flags().StringVar(&createNodeImage, "node-image", defaultNodeImage, "kindest/node image for 'kind create cluster --image' (pin tag@digest for a reproducible Kubernetes version)")
}
