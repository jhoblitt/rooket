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
	upNodeImage       string
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

		if err := validateIQNDate(upIQNDate); err != nil {
			return err
		}

		// The infra that cluster create depends on — block setup (the iSCSI OSD
		// devices create bind-mounts into the nodes) and the kind node-image
		// pre-pull — is built here as one concurrent unit that runs ahead of
		// create. Both are independent of the make portion of build, so when
		// make runs and block setup needs no terminal prompt this whole unit
		// overlaps make rather than sitting in front of it; see
		// docs/design/concurrency.md.
		blockSkipped := upSkipBlock || upDiskCount == 0
		var runBlock func(io.Writer) error
		blockPromptFree := true
		if !blockSkipped {
			dataDir, err := blockDataDir(upName, "")
			if err != nil {
				return err
			}
			disks := iscsiDiskList(upName, dataDir, upIQNDate, upWorkers, upDiskCount)
			blockPromptFree = blockSetupPromptFree(disks)
			runBlock = func(w io.Writer) error {
				if err := blockSetupRunTo(w, upName, dataDir, upIQNDate, upWorkers, upDiskCount, upDiskSizeGB); err != nil {
					return fmt.Errorf("block setup: %w", err)
				}
				return nil
			}
		}
		infra := func(w io.Writer) error {
			return runConcurrent(w,
				func(bw io.Writer) error {
					if runBlock != nil {
						return runBlock(bw)
					}
					return nil
				},
				func(pw io.Writer) error { prePullNodeImage(pw, upNodeImage); return nil },
			)
		}
		// Only block setup can prompt (via pkexec); the pre-pull never does. When
		// it would prompt, the unit must own the terminal and so cannot overlap a
		// streaming make.
		infraOverlapSafe := blockSkipped || blockPromptFree

		portExplicit := cmd.Flags().Changed("registry-port")
		createRun := func(w io.Writer) error {
			if err := createClusterRun(w, upName, upRegistryPort, portExplicit,
				upWorkers, upDiskCount, upIQNDate, upPromVersion, upPromRelease, upNodeImage); err != nil {
				return fmt.Errorf("cluster create: %w", err)
			}
			return nil
		}

		if upSkipBuild {
			if err := upStep("[1/4] block setup + node image", false, func() error {
				return infra(os.Stdout)
			}); err != nil {
				return err
			}
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
		} else if err := upCreateAndBuild(createRun, infra, infraOverlapSafe, rookDir); err != nil {
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

// upCreateAndBuild runs the infra-plus-create side concurrently with the make
// portion of build. make owns the terminal (it is the long pole a user
// watches); the other side writes into a switchWriter whose backlog flushes —
// then streams live — the moment the build side stops streaming. The
// pre-create skip probe is only a scheduling hint: the authoritative gate runs
// after the join, against the registry and port that create may have just
// repaired, so a wrong hint degrades to sequential speed, never to a wrong
// image. When the hint says make is needed it starts immediately; a built
// result goes straight to the push phase with no gate re-run (a re-run would
// see the still-unstamped mismatch and build a second time).
//
// infra (block setup + node-image pre-pull) is the work create depends on. It
// joins the concurrent region — overlapping make — only when make actually runs
// and infraOverlapSafe says block setup will not prompt on the terminal;
// otherwise it runs serially in front, where it can own the terminal and there
// is no make to overlap anyway.
//
// Neither side is cancelled on the other's failure: killing make would
// orphan the container build it spawned, and its warm caches help the retry.
// The failing side prints an immediate one-line notice; both errors surface
// after the join.
func upCreateAndBuild(createRun, infra func(io.Writer) error, infraOverlapSafe bool, rookDir string) error {
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

	// Overlap infra with make only when make runs AND block setup needs no
	// terminal prompt; otherwise run it serially in front.
	overlap := startMake && infraOverlapSafe
	if !overlap {
		if err := upStep("[1/4] block setup + node image", false, func() error { return infra(os.Stdout) }); err != nil {
			return err
		}
		infra = nil
	}

	sw := newSwitchWriter("=== cluster create output (ran concurrently with build) ===\n")
	createBanner := "[2/4] cluster create"
	if infra != nil {
		createBanner = "[1-2/4] block setup + node image + cluster create"
	}
	run.Printf("==> %s (concurrent with [3/4] build)\n", createBanner)
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
		// In the overlap case infra runs here, ahead of create and concurrent
		// with make; a failed infra skips create and surfaces after the join.
		if infra != nil {
			if createErr = infra(sw); createErr != nil {
				createDur = time.Since(started)
				run.Printf("setup failed (%v)\n", createErr)
				return
			}
		}
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
		run.Printf("==> %s failed after %s\n", createBanner, fmtDur(createDur))
	} else {
		run.Printf("==> %s done in %s\n", createBanner, fmtDur(createDur))
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
	upCmd.Flags().StringVar(&upNodeImage, "node-image", defaultNodeImage, "kindest/node image for the cluster, pre-pulled before create (pin tag@digest for a reproducible Kubernetes version)")
	upCmd.MarkFlagsMutuallyExclusive("skip-build", "force-build")
}
