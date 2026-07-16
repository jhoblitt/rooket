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

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	buildName         string
	buildDir          string
	buildRegistryPort int
	buildNamespace    string
	buildTag          string
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

		if err := pruneStaleChartDeps(dir); err != nil {
			return fmt.Errorf("prune stale chart dependency archives: %w", err)
		}

		fmt.Printf("==> running make in %s\n", dir)
		builtImages, err := runMakeCapture(dir)
		if err != nil {
			return fmt.Errorf("make: %w", err)
		}

		if len(builtImages) == 0 {
			return fmt.Errorf("no container images detected in make output " +
				"(expected lines matching '=== container build <image>')")
		}

		registry := fmt.Sprintf("localhost:%d", buildRegistryPort)
		for _, src := range builtImages {
			target := buildTag
			if target == "" {
				target = deriveTag(registry, buildNamespace, src, gitRef)
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
		}

		return nil
	},
}

// runMakeCapture runs make in dir, streams its stdout to the terminal, and
// returns all image names found on lines matching "=== container build <image>".
// stderr is passed through to the terminal directly.
func runMakeCapture(dir string) ([]string, error) {
	c := exec.Command("make")
	c.Dir = dir
	c.Stderr = os.Stderr

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}

	fmt.Printf("+ make\n")
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

	buildPushCmd.Flags().StringVar(&buildPushName, "name", "", "cluster name (selects the registry port)")
	buildPushCmd.Flags().StringVar(&buildPushDir, "dir", "", "path to the rook source directory (default: current directory)")
	buildPushCmd.Flags().IntVar(&buildPushRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	buildPushCmd.Flags().StringVar(&buildPushNamespace, "namespace", "rook", "image namespace prefix in the registry")
	buildPushCmd.Flags().StringVar(&buildPushTag, "tag", "", "override the full target image reference")
	buildPushCmd.Flags().StringVar(&buildPushSource, "source", "", "source image to retag (default: auto-detected from rook BUILD_REGISTRY)")
}
