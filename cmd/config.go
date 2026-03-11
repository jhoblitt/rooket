package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/cluster"
	"github.com/jhoblitt/rooket/internal/registry"
)

var (
	configName         string
	configWorkers      int
	configRegistryPort int
	configDiskCount    int
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print the kind cluster configuration that would be used by 'create'",
	RunE: func(cmd *cobra.Command, args []string) error {
		regName := registry.ContainerName(configName)

		// Build a representative disk map (actual devices unknown at this point).
		workerDisks := make(map[int][]string)
		if configDiskCount > 0 {
			for i := 0; i < configWorkers; i++ {
				for d := 0; d < configDiskCount; d++ {
					workerDisks[i] = append(workerDisks[i],
						fmt.Sprintf("/dev/loop<worker%d-disk%d>", i, d))
				}
			}
		}

		cfg := cluster.Config{
			Name:             configName,
			Workers:          configWorkers,
			RegistryName:     regName,
			RegistryHostPort: configRegistryPort,
			WorkerDisks:      workerDisks,
		}

		b, err := cluster.GenerateConfig(cfg)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	},
}

func init() {
	rootCmd.AddCommand(configCmd)

	configCmd.Flags().StringVar(&configName, "name", "rook", "kind cluster name")
	configCmd.Flags().IntVar(&configWorkers, "workers", 3, "number of worker nodes")
	configCmd.Flags().IntVar(&configRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	configCmd.Flags().IntVar(&configDiskCount, "disk-count", 1, "disks per worker (for illustration)")
}
