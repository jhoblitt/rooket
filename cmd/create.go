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

  1. Start a local OCI registry container (podman) on localhost:<registry-port>.
  2. Create a kind cluster (via podman provider) with containerd configured to
     mirror localhost:<registry-port> to the registry container.
  3. Connect the registry container to the kind podman network so nodes can
     pull images by container name.
  4. (Unless --skip-disks) Create sparse disk image files, attach them as loop
     devices, and pass the loop devices into each worker node so Rook/Ceph can
     use them as raw block OSDs.
  5. Apply the standard local-registry-hosting ConfigMap to kube-public.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regName := registry.ContainerName(createName)
		regCfg := registry.Config{
			Name:     regName,
			HostPort: createRegistryPort,
		}

		// Resolve data directory.
		dataDir := createDataDir
		if dataDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home dir: %w", err)
			}
			dataDir = filepath.Join(home, ".local", "share", "rooket", createName)
		}

		// --- Step 1: Registry ---
		fmt.Println("==> creating local OCI registry")
		if err := registry.Create(regCfg); err != nil {
			return fmt.Errorf("create registry: %w", err)
		}

		// --- Step 2: Disk images & loop devices ---
		workerDisks := make(map[int][]string)
		if !createSkipDisks && createDiskCount > 0 {
			fmt.Println("==> creating OSD disk images and loop devices")
			for i := 0; i < createWorkers; i++ {
				diskCfg := disks.Config{
					DataDir:     dataDir,
					WorkerIndex: i,
					Count:       createDiskCount,
					SizeGB:      createDiskSizeGB,
				}
				devs, err := disks.Create(diskCfg)
				if err != nil {
					return fmt.Errorf("create disks for worker %d: %w", i, err)
				}
				workerDisks[i] = devs
			}
		}

		// --- Step 3: kind cluster ---
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

		// --- Step 4: Connect registry to kind network ---
		fmt.Println("==> connecting registry to kind podman network")
		if err := registry.ConnectNetwork(regName, "kind"); err != nil {
			return fmt.Errorf("connect registry to kind network: %w", err)
		}

		// --- Step 5: Configure containerd registry on each node ---
		fmt.Println("==> configuring containerd registry on cluster nodes")
		if err := cluster.ConfigureRegistry(createName, regName, createRegistryPort); err != nil {
			return fmt.Errorf("configure registry on nodes: %w", err)
		}

		// --- Step 6: Registry ConfigMap ---
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
	createCmd.Flags().BoolVar(&createSkipDisks, "skip-disks", false, "skip disk image and loop device creation")
}
