package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/run"
)

var (
	buildDir          string
	buildRegistryPort int
	buildNamespace    string
	buildTag          string
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

		gitRef, err := gitHeadRef(dir)
		if err != nil {
			fmt.Printf("warning: could not determine git branch (%v); using \"latest\"\n", err)
			gitRef = "latest"
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
			if err := run.Cmd("podman", "tag", src, target); err != nil {
				return fmt.Errorf("tag %s: %w", src, err)
			}

			fmt.Printf("==> pushing %s\n", target)
			if err := run.Cmd("podman", "push", "--tls-verify=false", target); err != nil {
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

func init() {
	rootCmd.AddCommand(buildCmd)

	buildCmd.Flags().StringVar(&buildDir, "dir", "", "path to the rook source directory (default: current directory)")
	buildCmd.Flags().IntVar(&buildRegistryPort, "registry-port", 5001, "host port for the local OCI registry")
	buildCmd.Flags().StringVar(&buildNamespace, "namespace", "rook", "image namespace prefix in the registry")
	buildCmd.Flags().StringVar(&buildTag, "tag", "", "override the full target image reference (e.g. localhost:5001/rook/ceph:v1.2.3)")
}
