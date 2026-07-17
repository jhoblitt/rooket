package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	upName            string
	upWorkers         int
	upDiskCount       int
	upDiskSizeGB      int
	upRegistryPort    int
	upIQNDate         string
	upRookDir         string
	upPromVersion     string
	upPromRelease     string
	upOperatorRelease string
	upClusterRelease  string
	upSkipBlock       bool
	upSkipBuild       bool
	upSkipDeploy      bool
	upForceBuild      bool
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Bring up a complete rook-on-kind environment in one step",
	Long: `up runs the full bring-up sequence:

  1. rooket block setup    — create disk images and iSCSI targets
  2. rooket cluster create — start the kind cluster with the local registry
  3. rooket build          — build rook and push to the local registry
  4. rooket deploy         — install the rook-ceph and rook-ceph-cluster charts

Use --skip-block, --skip-build, or --skip-deploy to omit individual steps.
Setting --disk-count 0 also skips the block-setup step automatically.

Example:
  rooket up --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		upStart := time.Now()
		// Resolve the rook source dir up front so a missing clone fails fast,
		// before we stand up a cluster. Only build and deploy consume it.
		var rookDir string
		if !upSkipBuild || !upSkipDeploy {
			var err error
			rookDir, err = resolveRookDir(upRookDir)
			if err != nil {
				return err
			}
		}

		name, err := useCluster(upName)
		if err != nil {
			return err
		}
		upName = name
		// Resolve the port for fail-fast flag-conflict checking; the create step
		// re-resolves, repairs a stale recording, and persists the final choice.
		port, err := resolveRegistryPort(upName, upRegistryPort, cmd.Flags().Changed("registry-port"))
		if err != nil {
			return err
		}
		upRegistryPort = port

		if err := upStep("[1/4] block setup", upSkipBlock || upDiskCount == 0, func() error {
			blockSetupName = upName
			blockSetupWorkers = upWorkers
			blockSetupDiskCount = upDiskCount
			blockSetupDiskSizeGB = upDiskSizeGB
			blockSetupIQNDate = upIQNDate
			if err := blockSetupRun(nil, nil); err != nil {
				return fmt.Errorf("block setup: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}

		portExplicit := cmd.Flags().Changed("registry-port")
		createRun := func(w io.Writer) error {
			if err := createClusterRun(w, upName, upRegistryPort, portExplicit,
				upWorkers, upDiskCount, upIQNDate, upPromVersion, upPromRelease); err != nil {
				return fmt.Errorf("cluster create: %w", err)
			}
			return nil
		}

		if upSkipBuild {
			if err := upStep("[2/4] cluster create", false, func() error {
				return createRun(os.Stdout)
			}); err != nil {
				return err
			}
			// create may have repaired a stale recorded port; pick up its choice.
			if p := readRegistryPort(upName); p != 0 {
				upRegistryPort = p
			}
			run.Printf("==> [3/4] build (skipped)\n")
		} else if err := upCreateAndBuild(createRun, rookDir); err != nil {
			return err
		}

		if err := upStep("[4/4] deploy", upSkipDeploy, func() error {
			deployDir = rookDir
			deployRegistryPort = upRegistryPort
			deployKubeContext = "kind-" + upName
			deployOperatorName = upOperatorRelease
			deployClusterName = upClusterRelease
			deployName = upName
			deployWorkers = upWorkers
			deployDiskCount = upDiskCount
			deployDiskSizeGB = upDiskSizeGB
			deployIQNDate = upIQNDate
			if err := deployCmd.RunE(deployCmd, nil); err != nil {
				return fmt.Errorf("deploy: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}

		run.Printf(`
rooket up complete in %s. cluster %q is ready.

  kubectl:     rooket k <args>
  kubeconfig:  export KUBECONFIG="$(rooket kubeconfig --path)"
`, fmtDur(time.Since(upStart)), upName)
		return nil
	},
}

// upCreateAndBuild runs cluster create concurrently with the make portion of
// build. make owns the terminal (it is the long pole a user watches); create
// writes into a switchWriter whose backlog flushes — then streams live — the
// moment the build side stops streaming. The pre-create skip probe is only a
// scheduling hint: the authoritative gate runs after the join, against the
// registry and port that create may have just repaired, so a wrong hint
// degrades to sequential speed, never to a wrong image. When the hint says
// make is needed it starts immediately; a built result goes straight to the
// push phase with no gate re-run (a re-run would see the still-unstamped
// mismatch and build a second time).
//
// Neither side is cancelled on the other's failure: killing make would
// orphan the container build it spawned, and its warm caches help the retry.
// The failing side prints an immediate one-line notice; both errors surface
// after the join.
func upCreateAndBuild(createRun func(io.Writer) error, rookDir string) error {
	buildForce = upForceBuild
	gitRef, gitErr := gitHeadRef(rookDir)
	if gitErr != nil {
		gitRef = "latest"
	}
	fp, fpErr := treeFingerprint(rookDir)

	startMake := upForceBuild
	makeReason := "forced by --force-build"
	if !startMake {
		stamp := readBuildStamp(upName)
		// The hint probe is silent (io.Discard): when it misses, the
		// authoritative post-join gate repeats and traces the same probes.
		reason, repush := buildSkipCheck(io.Discard, fp, fpErr, stamp, containerEngine, rookDir, upName,
			upRegistryPort, buildNamespace, "", gitRef)
		// Only a compile-side miss starts make early; publish-side gaps wait
		// for the post-join gate (the registry may not even exist yet).
		startMake = reason != "" && repush == nil
		makeReason = reason
	}
	if gitErr != nil && startMake {
		run.Printf("warning: could not determine git branch (%v); using \"latest\"\n", gitErr)
	}

	sw := newSwitchWriter("=== cluster create output (ran concurrently with build) ===\n")
	run.Printf("==> [2/4] cluster create (concurrent with [3/4] build)\n")
	run.Printf("==> [3/4] build\n")

	var (
		wg                 sync.WaitGroup
		createErr, makeErr error
		images             []string
		createDur, makeDur time.Duration
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		started := time.Now()
		createErr = createRun(sw)
		createDur = time.Since(started)
		if createErr != nil {
			run.Printf("cluster create failed (%v)\n", createErr)
		}
	}()
	go func() {
		defer wg.Done()
		started := time.Now()
		if startMake {
			run.Printf("==> building: %s\n", makeReason)
			images, makeErr = buildMakePhase(os.Stdout, rookDir, upName)
			if makeErr != nil {
				makeErr = fmt.Errorf("build: %w", makeErr)
				run.Printf("build failed (%v)\n", makeErr)
			}
		}
		makeDur = time.Since(started)
		sw.Promote(os.Stdout)
	}()
	wg.Wait()
	sw.Promote(os.Stdout)
	if createErr != nil {
		run.Printf("==> [2/4] cluster create failed after %s\n", fmtDur(createDur))
	} else {
		run.Printf("==> [2/4] cluster create done in %s\n", fmtDur(createDur))
	}
	if makeErr != nil {
		run.Printf("==> [3/4] build failed after %s\n", fmtDur(makeDur))
	}
	if createErr != nil || makeErr != nil {
		return errors.Join(createErr, makeErr)
	}

	// create may have repaired a stale recorded port; pick up its choice
	// before the authoritative publish-side work.
	if p := readRegistryPort(upName); p != 0 {
		upRegistryPort = p
	}

	buildStarted := time.Now()
	buildErr := func() error {
		if startMake {
			return buildPushPhase(os.Stdout, images, upRegistryPort, buildNamespace, "",
				gitRef, upName, rookDir, fp, fpErr)
		}
		// The full gate, against the final port and live registry.
		return buildRun(os.Stdout, rookDir, upName, upRegistryPort)
	}()
	if buildErr != nil {
		run.Printf("==> [3/4] build failed after %s\n", fmtDur(makeDur+time.Since(buildStarted)))
		return fmt.Errorf("build: %w", buildErr)
	}
	run.Printf("==> [3/4] build done in %s\n", fmtDur(makeDur+time.Since(buildStarted)))
	return nil
}

// upStep runs one numbered up step, printing its banner and, when it ran, its
// duration on completion.
func upStep(banner string, skipped bool, fn func() error) error {
	if skipped {
		run.Printf("==> %s (skipped)\n", banner)
		return nil
	}
	run.Printf("==> %s\n", banner)
	started := time.Now()
	if err := fn(); err != nil {
		run.Printf("==> %s failed after %s\n", banner, fmtDur(time.Since(started)))
		return err
	}
	run.Printf("==> %s done in %s\n", banner, fmtDur(time.Since(started)))
	return nil
}

func fmtDur(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

func init() {
	rootCmd.AddCommand(upCmd)

	upCmd.Flags().StringVar(&upName, "name", "", "kind cluster name")
	upCmd.Flags().IntVar(&upWorkers, "workers", 3, "number of worker nodes")
	upCmd.Flags().IntVar(&upDiskCount, "disk-count", 1, "iSCSI disks per worker (0 skips block setup)")
	upCmd.Flags().IntVar(&upDiskSizeGB, "disk-size", 10, "disk size in GiB")
	upCmd.Flags().IntVar(&upRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	upCmd.Flags().StringVar(&upIQNDate, "iqn-date", "2003-01", "IQN date component (YYYY-MM)")
	upCmd.Flags().StringVar(&upRookDir, "dir", "", "path to the rook source directory (default: $ROOK_DIR, else the rook clone found by walking up from the current directory)")
	upCmd.Flags().StringVar(&upPromVersion, "prometheus-operator-crds-version", "29.0.0", "version of the prometheus-operator-crds helm chart (exact versions enable the reinstall skip)")
	upCmd.Flags().StringVar(&upPromRelease, "prometheus-operator-crds-release", "my-prometheus-operator-crds", "helm release name for prometheus-operator-crds")
	upCmd.Flags().StringVar(&upOperatorRelease, "operator-release", "rook-ceph", "rook-ceph operator helm release name")
	upCmd.Flags().StringVar(&upClusterRelease, "cluster-release", "rook-ceph-cluster", "rook-ceph-cluster helm release name")
	upCmd.Flags().BoolVar(&upSkipBlock, "skip-block", false, "skip 'block setup'")
	upCmd.Flags().BoolVar(&upSkipBuild, "skip-build", false, "skip 'build'")
	upCmd.Flags().BoolVar(&upSkipDeploy, "skip-deploy", false, "skip 'deploy'")
	upCmd.Flags().BoolVar(&upForceBuild, "force-build", false, "run make even when the rook tree is unchanged since the last push")
	upCmd.MarkFlagsMutuallyExclusive("skip-build", "force-build")
}
