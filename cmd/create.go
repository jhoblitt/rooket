package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	createName            string
	createWorkers         int
	createRegistryPort    int
	createDiskCount       int
	createISCSIQNDate     string
	createPromCRDsVersion string
	createPromCRDsRelease string
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
     to the registry container (reachable by name on the kind network).
  6. Apply the standard local-registry-hosting ConfigMap to kube-public.

Run 'rooket block setup' before 'rooket cluster create' to prepare block devices.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := useCluster(createName)
		if err != nil {
			return err
		}
		createName = name

		port, err := resolveRegistryPort(createName, createRegistryPort, cmd.Flags().Changed("registry-port"))
		if err != nil {
			return err
		}
		if err := writeRegistryPort(createName, port); err != nil {
			return err
		}
		createRegistryPort = port

		regName := registry.ContainerName(createName)

		// --- Step 1: Locate iSCSI block devices ---
		workerDisks := make(map[int][]cluster.Disk)
		if createDiskCount > 0 {
			fmt.Println("==> locating iSCSI block devices")
			for i := 0; i < createWorkers; i++ {
				for d := 0; d < createDiskCount; d++ {
					iqn := fmt.Sprintf("iqn.%s.local.rooket:%s-worker%d-disk%d",
						createISCSIQNDate, createName, i, d)
					dev, err := waitForISCSIDevice(iqn)
					if err != nil {
						return fmt.Errorf("iSCSI device for worker %d disk %d not found "+
							"(run 'rooket block setup' first): %w", i, d, err)
					}
					fmt.Printf("worker%d disk%d: %s\n", i, d, dev)
					workerDisks[i] = append(workerDisks[i], cluster.Disk{
						HostPath:      dev,
						ContainerPath: dev,
					})
				}
			}
		}

		// --- Step 2: kind cluster ---
		// This also creates the "kind" network used by the registry.
		fmt.Println("==> creating kind cluster")
		clusterCfg := cluster.Config{
			Name:             createName,
			Workers:          createWorkers,
			RegistryName:     regName,
			RegistryHostPort: createRegistryPort,
			WorkerDisks:      workerDisks,
		}
		exists, err := cluster.Exists(createName)
		if err != nil {
			return fmt.Errorf("check cluster existence: %w", err)
		}
		if exists {
			fmt.Printf("cluster %q already exists, skipping creation\n", createName)
		} else {
			if err := cluster.Create(clusterCfg); err != nil {
				return fmt.Errorf("create cluster: %w", err)
			}
		}

		// --- Step 3: Prepare nodes for OSD provisioning ---
		fmt.Println("==> preparing nodes for OSD provisioning")
		ownDevsByNode := make(map[string][]string)
		for i := 0; i < createWorkers; i++ {
			name := workerNodeName(createName, i)
			for _, d := range workerDisks[i] {
				ownDevsByNode[name] = append(ownDevsByNode[name], d.HostPath)
			}
		}
		if err := cluster.PrepareNodes(containerEngine, createName, ownDevsByNode); err != nil {
			return fmt.Errorf("prepare nodes: %w", err)
		}

		// --- Step 4: Registry ---
		// Created after the cluster so the "kind" network exists.
		// --network=kind makes the container reachable by name from cluster nodes.
		fmt.Println("==> creating local OCI registry on the kind network")
		regCfg := registry.Config{
			Engine:   containerEngine,
			Name:     regName,
			HostPort: createRegistryPort,
			Network:  "kind",
		}
		if err := registry.Create(regCfg); err != nil {
			return fmt.Errorf("create registry: %w", err)
		}

		// --- Step 5: Configure containerd registry on each node ---
		fmt.Println("==> configuring containerd registry on cluster nodes")
		if err := cluster.ConfigureRegistry(containerEngine, createName, regName, createRegistryPort); err != nil {
			return fmt.Errorf("configure registry on nodes: %w", err)
		}

		// --- Step 6: Registry ConfigMap ---
		fmt.Println("==> applying local-registry-hosting ConfigMap")
		if err := cluster.ApplyRegistryConfigMap(createName, regName, createRegistryPort); err != nil {
			return fmt.Errorf("apply registry ConfigMap: %w", err)
		}

		// --- Step 7: prometheus-operator CRDs ---
		fmt.Println("==> installing prometheus-operator-crds helm chart")
		if err := cluster.InstallPrometheusOperatorCRDs(
			createName, createPromCRDsRelease, createPromCRDsVersion,
		); err != nil {
			return fmt.Errorf("install prometheus-operator-crds: %w", err)
		}

		fmt.Printf(`
Cluster %q is ready.

  kubectl context:   kind-%s
  local registry:    localhost:%d
  push images with:  %s push localhost:%d/<image>

`, createName, createName, createRegistryPort, containerEngine.String(), createRegistryPort)
		return nil
	},
}

func init() {
	clusterCmd.AddCommand(createCmd)

	createCmd.Flags().StringVar(&createName, "name", "", "kind cluster name")
	createCmd.Flags().IntVar(&createWorkers, "workers", 3, "number of worker nodes")
	createCmd.Flags().IntVar(&createRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	createCmd.Flags().IntVar(&createDiskCount, "disk-count", 1, "number of iSCSI disks per worker (0 to skip)")
	createCmd.Flags().StringVar(&createISCSIQNDate, "iqn-date", "2003-01", "IQN date component matching 'rooket block setup' (YYYY-MM)")
	createCmd.Flags().StringVar(&createPromCRDsVersion, "prometheus-operator-crds-version", "29.0.0", "version of the prometheus-operator-crds helm chart to install")
	createCmd.Flags().StringVar(&createPromCRDsRelease, "prometheus-operator-crds-release", "my-prometheus-operator-crds", "helm release name for prometheus-operator-crds")
}
