package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/disks"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	createName         string
	createWorkers      int
	createRegistryPort int
	createDiskCount    int
	createDiskSizeGB   int
	createDataDir      string
	createSkipDisks    bool
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a kind cluster with a local OCI registry for Rook development",
	Long: `create performs the following steps:

  1. Create sparse disk image files on the host (no root required).
  2. Create the kind cluster (via podman provider). The disk images are
     bind-mounted into each worker node via the kind config.
  3. Start a local OCI registry container joined to the kind podman network,
     bound to localhost:<registry-port> on the host. The registry must be
     created after the cluster so that the "kind" podman network exists.
     (With rootless podman, network membership is set at container creation;
     'podman network connect' is not supported with the default pasta mode.)
  4. Configure containerd on every node to mirror localhost:<registry-port>
     to the registry container (reachable by name on the kind network).
  5. Attach disk images as loop devices inside each worker node (losetup runs
     inside the container where it has root access, not on the host).
  6. Apply the standard local-registry-hosting ConfigMap to kube-public.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regName := registry.ContainerName(createName)

		dataDir := createDataDir
		if dataDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home dir: %w", err)
			}
			dataDir = filepath.Join(home, ".local", "share", "rooket", createName)
		}

		// --- Step 1: Disk images (host side only, no losetup yet) ---
		workerDisks := make(map[int][]disks.Disk)
		if !createSkipDisks && createDiskCount > 0 {
			fmt.Println("==> creating OSD disk images")
			for i := 0; i < createWorkers; i++ {
				diskCfg := disks.Config{
					DataDir:     dataDir,
					WorkerIndex: i,
					Count:       createDiskCount,
					SizeGB:      createDiskSizeGB,
				}
				d, err := disks.Create(diskCfg)
				if err != nil {
					return fmt.Errorf("create disks for worker %d: %w", i, err)
				}
				workerDisks[i] = d
			}
		}

		// --- Step 2: kind cluster ---
		// This also creates the "kind" podman network used by the registry.
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

		// --- Step 3: Registry ---
		// Created after the cluster so the "kind" podman network exists.
		// --network=kind makes the container reachable by name from cluster nodes.
		fmt.Println("==> creating local OCI registry on the kind network")
		regCfg := registry.Config{
			Name:     regName,
			HostPort: createRegistryPort,
			Network:  "kind",
		}
		if err := registry.Create(regCfg); err != nil {
			return fmt.Errorf("create registry: %w", err)
		}

		// --- Step 4: Configure containerd registry on each node ---
		fmt.Println("==> configuring containerd registry on cluster nodes")
		if err := cluster.ConfigureRegistry(createName, regName, createRegistryPort); err != nil {
			return fmt.Errorf("configure registry on nodes: %w", err)
		}

		// Loop devices were set up via sudo before the cluster was created and
		// bind-mounted into worker nodes via extraMounts in the kind config.
		// crun automatically adds bind-mounted device files to the container's
		// cgroup device allowlist, so no in-node setup is required here.

		// --- Step 5: Registry ConfigMap ---
		fmt.Println("==> applying local-registry-hosting ConfigMap")
		if err := cluster.ApplyRegistryConfigMap(createName, regName, createRegistryPort); err != nil {
			return fmt.Errorf("apply registry ConfigMap: %w", err)
		}

		fmt.Printf(`
Cluster %q is ready.

  kubectl context:   kind-%s
  local registry:    localhost:%d
  push images with:  podman push localhost:%d/<image>

`, createName, createName, createRegistryPort, createRegistryPort)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(createCmd)

	createCmd.Flags().StringVar(&createName, "name", "rook", "kind cluster name")
	createCmd.Flags().IntVar(&createWorkers, "workers", 3, "number of worker nodes")
	createCmd.Flags().IntVar(&createRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	createCmd.Flags().IntVar(&createDiskCount, "disk-count", 1, "number of OSD disks per worker (0 to skip)")
	createCmd.Flags().IntVar(&createDiskSizeGB, "disk-size", 10, "size of each OSD disk image in GiB")
	createCmd.Flags().StringVar(&createDataDir, "data-dir", "", "directory for disk images (default: ~/.local/share/rooket/<name>)")
	createCmd.Flags().BoolVar(&createSkipDisks, "skip-disks", false, "skip disk image creation")
}
