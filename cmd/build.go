package cmd

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	buildName         string
	buildDir          string
	buildRegistryPort int
	buildNamespace    string
	buildTag          string
	buildForce        bool
)

var (
	buildPushName         string
	buildPushDir          string
	buildPushRegistryPort int
	buildPushNamespace    string
	buildPushTag          string
	buildPushSource       string
)

// containerBuildRe matches lines like "=== container build build-98fc4431/ceph-amd64".
var containerBuildRe = regexp.MustCompile(`^=== container build (\S+)`)

// archSuffixRe matches common architecture suffixes appended to image names.
var archSuffixRe = regexp.MustCompile(`-(amd64|arm64|arm|386|s390x|ppc64le)$`)

var buildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build rook, then tag and push the image to the local registry",
	Long: `build runs 'make' in the rook source directory, detects built container
images from lines matching "=== container build <image>", retags them for the
local registry using the current git branch as the image tag, and pushes them.

Example:
  rooket build --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := buildDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
		}

		name := clusterName(buildName)
		port, perr := resolveRegistryPort(name, buildRegistryPort, cmd.Flags().Changed("registry-port"))
		if perr != nil {
			return perr
		}
		buildRegistryPort = port

		gitRef, err := gitHeadRef(dir)
		if err != nil {
			fmt.Printf("warning: could not determine git branch (%v); using \"latest\"\n", err)
			gitRef = "latest"
		}

		// The fingerprint is computed BEFORE make: edits made while make
		// runs must invalidate the next stamp, not be attributed to this
		// build.
		fp, fpErr := treeFingerprint(dir)
		if !buildForce {
			stamp := readBuildStamp(name)
			reason, repush := buildSkipCheck(fp, fpErr, stamp, containerEngine, dir, name, port, buildNamespace, buildTag, gitRef)
			if reason == "" {
				run.Printf("==> build skipped: rook tree unchanged since last push; %s present in registry (--force-build to rebuild)\n",
					stampRefs(stamp.Images))
				return nil
			}
			if repush != nil {
				run.Printf("==> rook tree unchanged since last build (%s); pushing the existing image without make\n", reason)
				imgs, rerr := repushStampedImages(repush, port)
				if rerr == nil {
					stampBuild(name, dir, fp, fpErr, gitRef, imgs)
					return nil
				}
				run.Printf("==> cannot reuse the previous image (%v); building\n", rerr)
			} else if stamp != nil {
				run.Printf("==> building: %s\n", reason)
			}
		}

		if err := pruneStaleChartDeps(dir); err != nil {
			return fmt.Errorf("prune stale chart dependency archives: %w", err)
		}

		// Isolate the helm runs inside rook's make from the host helm config:
		// rook's `helm dependency build` refreshes EVERY repo in whatever
		// repositories.yaml it sees, so the host's repo list (27 entries here,
		// some timing out) turns into dead time on each build that trips it.
		makeEnv, err := helmEnv(name, "make")
		if err != nil {
			return err
		}

		fmt.Printf("==> running make in %s\n", dir)
		builtImages, err := runMakeCapture(dir, makeEnv)
		if err != nil {
			return fmt.Errorf("make: %w", err)
		}

		if len(builtImages) == 0 {
			return fmt.Errorf("no container images detected in make output " +
				"(expected lines matching '=== container build <image>')")
		}

		registryHost := fmt.Sprintf("localhost:%d", buildRegistryPort)
		var stamped []stampImage
		local := true
		for _, src := range builtImages {
			target := buildTag
			if target == "" {
				target = deriveTag(registryHost, buildNamespace, src, gitRef)
			}
			if err := pushImage(src, target); err != nil {
				return err
			}
			host, repo, tag, perr := parseImageRef(target)
			if perr != nil || (host != registryHost && host != fmt.Sprintf("127.0.0.1:%d", buildRegistryPort)) {
				// A --tag override pointing elsewhere can't be verified
				// against this cluster's registry, so it is never stamped.
				local = false
				continue
			}
			digest, _ := manifestDigest(buildRegistryPort, repo, tag)
			id := localImageID(src)
			stamped = append(stamped, stampImage{Source: src, SourceID: id, Ref: target, Repo: repo, Tag: tag, Digest: digest})
		}
		if local {
			stampBuild(name, dir, fp, fpErr, gitRef, stamped)
		}
		return nil
	},
}

// stampRefs renders the refs of a stamp's images for messages.
func stampRefs(imgs []stampImage) string {
	refs := make([]string, len(imgs))
	for i, img := range imgs {
		refs[i] = img.Ref
	}
	return strings.Join(refs, ", ")
}

// pushImage tags a source image and pushes it to the local registry.
func pushImage(src, target string) error {
	fmt.Printf("==> tagging %s → %s\n", src, target)
	if err := run.Cmd(containerEngine.String(), "tag", src, target); err != nil {
		return fmt.Errorf("tag %s: %w", src, err)
	}
	fmt.Printf("==> pushing %s\n", target)
	if err := run.Cmd(containerEngine.String(), containerEngine.PushArgs(target)...); err != nil {
		return fmt.Errorf("push %s: %w", target, err)
	}
	fmt.Printf("pushed %s\n", target)
	return nil
}

// repushStampedImages re-publishes the images recorded by the last build —
// used when the tree is unchanged but the registry no longer has (or never
// had) the expected refs, e.g. after 'rooket down' recreated it or a branch
// rename retagged the deploy ref. Publication state alone never justifies
// re-running make.
func repushStampedImages(imgs []stampImage, port int) ([]stampImage, error) {
	for _, img := range imgs {
		id := localImageID(img.Source)
		if id == "" {
			return nil, fmt.Errorf("source image %s no longer present locally", img.Source)
		}
		// The source tag is mutable — a later build (another branch, another
		// cluster) may have retagged it. Only the stamped content may be
		// republished under this stamp.
		if img.SourceID == "" || id != img.SourceID {
			return nil, fmt.Errorf("source image %s changed since it was stamped", img.Source)
		}
	}
	out := make([]stampImage, 0, len(imgs))
	for _, img := range imgs {
		if err := pushImage(img.Source, img.Ref); err != nil {
			return nil, err
		}
		digest, ok := manifestDigest(port, img.Repo, img.Tag)
		if !ok {
			return nil, fmt.Errorf("pushed %s but could not read its digest back", img.Ref)
		}
		img.Digest = digest
		out = append(out, img)
	}
	return out, nil
}

// localImageID returns the engine's image ID for a local ref, or "".
func localImageID(ref string) string {
	id, err := run.Output(containerEngine.String(), "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

// stampBuild records a fully-pushed build. Failures only warn: a missing
// stamp merely costs a rebuild next time.
func stampBuild(name, dir string, fp treeFP, fpErr error, gitRef string, imgs []stampImage) {
	if fpErr != nil || len(imgs) == 0 {
		return
	}
	// With BUILD_CONTAINER_IMAGE=false rook's make prints the container-build
	// line without building, so the pushed image is whatever the source tag
	// already pointed at — never bless that as this fingerprint's output.
	if os.Getenv("BUILD_CONTAINER_IMAGE") == "false" {
		return
	}
	for _, img := range imgs {
		if img.Digest == "" || img.SourceID == "" {
			return
		}
	}
	s := &buildStamp{
		Version:       buildStampVersion,
		Dir:           dir,
		Fingerprint:   fp,
		GitRef:        gitRef,
		Images:        imgs,
		PushedAt:      time.Now().UTC().Format(time.RFC3339),
		RooketVersion: rooketVersion(),
	}
	if err := writeBuildStamp(name, s); err != nil {
		fmt.Printf("warning: could not record build stamp: %v\n", err)
	}
}

// runMakeCapture runs make in dir, streams its stdout to the terminal, and
// returns all image names found on lines matching "=== container build <image>".
// stderr is passed through to the terminal directly.
func runMakeCapture(dir string, extraEnv []string) ([]string, error) {
	c := exec.Command("make")
	c.Dir = dir
	c.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		c.Env = append(os.Environ(), extraEnv...)
	}

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}

	run.Printf("+ make\n")
	if err := c.Start(); err != nil {
		return nil, err
	}

	var images []string
	scanner := bufio.NewScanner(io.TeeReader(stdout, os.Stdout))
	for scanner.Scan() {
		if m := containerBuildRe.FindStringSubmatch(scanner.Text()); m != nil {
			images = append(images, m[1])
		}
	}

	if err := c.Wait(); err != nil {
		return nil, err
	}
	return images, nil
}

// gitHeadRef returns the current branch name (or short commit hash if detached
// HEAD), sanitized for use as an OCI image tag (slashes replaced with dashes).
func gitHeadRef(dir string) (string, error) {
	c := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(string(out))
	if ref == "HEAD" {
		// Detached HEAD: fall back to short commit hash.
		c2 := exec.Command("git", "rev-parse", "--short", "HEAD")
		c2.Dir = dir
		if out2, err := c2.Output(); err == nil {
			ref = strings.TrimSpace(string(out2))
		}
	}
	// OCI tags cannot contain '/'; replace with '-'.
	ref = strings.ReplaceAll(ref, "/", "-")
	return ref, nil
}

// deriveTag converts a build image name (e.g. "build-98fc4431/ceph-amd64") to
// a local registry tag (e.g. "localhost:5001/rook/ceph:master").
func deriveTag(registry, namespace, srcImage, gitRef string) string {
	// Extract the basename after the last '/'.
	base := srcImage
	if i := strings.LastIndex(srcImage, "/"); i >= 0 {
		base = srcImage[i+1:]
	}
	// Strip architecture suffix (e.g. "-amd64").
	base = archSuffixRe.ReplaceAllString(base, "")

	if namespace != "" {
		return fmt.Sprintf("%s/%s/%s:%s", registry, namespace, base, gitRef)
	}
	return fmt.Sprintf("%s/%s:%s", registry, base, gitRef)
}

// buildRegistryName computes rook's BUILD_REGISTRY prefix for a given source
// directory: build-<first 8 hex chars of sha256("hostname-dir\n")>.
func buildRegistryName(dir string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(hostname + "-" + dir + "\n"))
	return fmt.Sprintf("build-%x", sum[:4]), nil
}

var buildPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Retag and push the rook image built by 'make' to the local registry (skips make)",
	Long: `push retags the rook image that was previously built by 'make' and pushes
it to the local OCI registry, using the same tag logic as 'rooket build'.

The source image is auto-detected from the rook BUILD_REGISTRY formula unless
overridden with --source.

Example:
  rooket build push --dir ~/github/rook
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := buildPushDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
		}

		name := clusterName(buildPushName)
		port, perr := resolveRegistryPort(name, buildPushRegistryPort, cmd.Flags().Changed("registry-port"))
		if perr != nil {
			return perr
		}
		buildPushRegistryPort = port

		src := buildPushSource
		if src == "" {
			regName, err := buildRegistryName(dir)
			if err != nil {
				return fmt.Errorf("compute build registry name: %w", err)
			}
			src = fmt.Sprintf("%s/ceph-%s", regName, runtime.GOARCH)
		}

		gitRef, err := gitHeadRef(dir)
		if err != nil {
			fmt.Printf("warning: could not determine git branch (%v); using \"latest\"\n", err)
			gitRef = "latest"
		}

		registry := fmt.Sprintf("localhost:%d", buildPushRegistryPort)
		target := buildPushTag
		if target == "" {
			target = deriveTag(registry, buildPushNamespace, src, gitRef)
		}

		fmt.Printf("==> tagging %s → %s\n", src, target)
		if err := run.Cmd(containerEngine.String(), "tag", src, target); err != nil {
			return fmt.Errorf("tag %s: %w", src, err)
		}

		fmt.Printf("==> pushing %s\n", target)
		if err := run.Cmd(containerEngine.String(), containerEngine.PushArgs(target)...); err != nil {
			return fmt.Errorf("push %s: %w", target, err)
		}

		fmt.Printf("pushed %s\n", target)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(buildCmd)
	buildCmd.AddCommand(buildPushCmd)

	buildCmd.Flags().StringVar(&buildName, "name", "", "cluster name (selects the registry port)")
	buildCmd.Flags().StringVar(&buildDir, "dir", "", "path to the rook source directory (default: current directory)")
	buildCmd.Flags().IntVar(&buildRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	buildCmd.Flags().StringVar(&buildNamespace, "namespace", "rook", "image namespace prefix in the registry")
	buildCmd.Flags().StringVar(&buildTag, "tag", "", "override the full target image reference (e.g. localhost:5001/rook/ceph:v1.2.3)")
	buildCmd.Flags().BoolVar(&buildForce, "force-build", false, "run make even when the rook tree is unchanged since the last push")

	buildPushCmd.Flags().StringVar(&buildPushName, "name", "", "cluster name (selects the registry port)")
	buildPushCmd.Flags().StringVar(&buildPushDir, "dir", "", "path to the rook source directory (default: current directory)")
	buildPushCmd.Flags().IntVar(&buildPushRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	buildPushCmd.Flags().StringVar(&buildPushNamespace, "namespace", "rook", "image namespace prefix in the registry")
	buildPushCmd.Flags().StringVar(&buildPushTag, "tag", "", "override the full target image reference")
	buildPushCmd.Flags().StringVar(&buildPushSource, "source", "", "source image to retag (default: auto-detected from rook BUILD_REGISTRY)")
}
