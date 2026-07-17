package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhoblitt/rooket/internal/engine"
	"github.com/jhoblitt/rooket/internal/registry"
	"github.com/jhoblitt/rooket/internal/run"
)

// treeFP fingerprints everything that can change the artifacts a rook build
// produces. Fields are compared individually so skip/build messages can say
// WHAT changed. Ignored files (rook's _output/, .cache/, chart dep archives,
// Chart.lock) are invisible to every field, so make's own churn never
// invalidates a stamp.
type treeFP struct {
	// Head is the checked-out commit.
	Head string `json:"head"`
	// Describe covers tag changes: rook bakes `git describe` into VERSION
	// and the binary, so a tag add/move changes the build with an identical
	// tree.
	Describe string `json:"describe"`
	// DiffSum hashes HEAD-vs-worktree content (staged and unstaged).
	// --binary is load-bearing: text mode renders any binary edit as the
	// same "Binary files differ" line. --no-ext-diff/--no-textconv keep
	// user diff config from producing content-independent output.
	DiffSum string `json:"diffSum"`
	// StatusSum hashes the porcelain v2 status (paths, states).
	StatusSum string `json:"statusSum"`
	// UntrackedSum hashes untracked file content (metadata for files over
	// 1MiB) — porcelain output alone carries no untracked content signal.
	UntrackedSum string `json:"untrackedSum"`
	// BuildEnv hashes the env knobs rook's make honors plus the Go
	// toolchain version; e.g. BUILD_CONTAINER_IMAGE=false makes make print
	// the container-build line without building, which must not stamp.
	BuildEnv string `json:"buildEnv"`
}

var buildEnvVars = []string{
	"VERSION", "DEBUG", "TAGS", "BUILDFLAGS", "LDFLAGS", "GOFLAGS",
	"CEPH_VERSION", "BUILD_ARGS", "PULL", "BUILD_CONTAINER_IMAGE", "BUILD_REGISTRY",
	"GOARCH", "PLATFORM", "GOARM", "GOAMD64",
}

func gitOut(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

// treeFingerprint computes the fingerprint of the rook tree at dir. Any
// error disables skipping (the caller builds).
func treeFingerprint(dir string) (treeFP, error) {
	var fp treeFP
	var err error
	if fp.Head, err = gitOut(dir, "rev-parse", "HEAD"); err != nil {
		return fp, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if fp.Describe, err = gitOut(dir, "describe", "--tags", "--dirty", "--always"); err != nil {
		return fp, fmt.Errorf("git describe: %w", err)
	}

	diff := exec.Command("git", "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--binary", "--full-index", "HEAD")
	diff.Dir = dir
	h := sha256.New()
	stdout, err := diff.StdoutPipe()
	if err != nil {
		return fp, err
	}
	if err := diff.Start(); err != nil {
		return fp, err
	}
	if _, err := io.Copy(h, stdout); err != nil {
		return fp, err
	}
	if err := diff.Wait(); err != nil {
		return fp, fmt.Errorf("git diff: %w", err)
	}
	fp.DiffSum = hex.EncodeToString(h.Sum(nil))

	status := exec.Command("git", "status", "--porcelain=v2", "-uall", "-z")
	status.Dir = dir
	raw, err := status.Output()
	if err != nil {
		return fp, fmt.Errorf("git status: %w", err)
	}
	fp.StatusSum = fmt.Sprintf("%x", sha256.Sum256(raw))

	var untracked []string
	for _, rec := range strings.Split(string(raw), "\x00") {
		if p, ok := strings.CutPrefix(rec, "? "); ok {
			untracked = append(untracked, p)
		}
	}
	sort.Strings(untracked)
	uh := sha256.New()
	for _, p := range untracked {
		hashUntracked(uh, dir, p)
	}
	fp.UntrackedSum = hex.EncodeToString(uh.Sum(nil))

	env := make([]string, 0, len(buildEnvVars)+1)
	goVersion, err := exec.Command("go", "version").Output()
	if err != nil {
		goVersion = []byte("unknown")
	}
	env = append(env, "go="+strings.TrimSpace(string(goVersion)))
	for _, k := range buildEnvVars {
		// Unset and explicitly-empty are different make inputs.
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		} else {
			env = append(env, k+"=\x01unset")
		}
	}
	fp.BuildEnv = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(env, "\x00"))))
	return fp, nil
}

// hashUntracked mixes one untracked path into the fingerprint: content for
// regular files up to 1MiB, metadata otherwise. Untracked files are
// typically few and small, and metadata alone leaves a same-size/same-mtime
// hole.
func hashUntracked(h hash.Hash, dir, rel string) {
	full := filepath.Join(dir, rel)
	fmt.Fprintf(h, "%s\x00", rel)
	fi, err := os.Lstat(full)
	if err != nil {
		io.WriteString(h, "vanished\x00")
		return
	}
	switch {
	case fi.Mode().IsRegular() && fi.Size() <= 1<<20:
		fmt.Fprintf(h, "%v\x00", fi.Mode())
		f, err := os.Open(full)
		if err != nil {
			io.WriteString(h, "unreadable\x00")
			return
		}
		defer f.Close()
		io.Copy(h, f)
		io.WriteString(h, "\x00")
	case fi.Mode()&os.ModeSymlink != 0:
		target, _ := os.Readlink(full)
		fmt.Fprintf(h, "link\x00%s\x00", target)
	default:
		fmt.Fprintf(h, "%v\x00%d\x00%d\x00", fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
	}
}

// fingerprintDiff names the first fingerprint field that differs, or "".
// Content fields are checked before Describe: a dirty edit also flips
// describe's -dirty suffix, and the content reason is the useful one — a
// describe change alone means tags moved on a clean tree.
func fingerprintDiff(old, new treeFP) string {
	switch {
	case old.Head != new.Head:
		return "HEAD moved"
	case old.DiffSum != new.DiffSum:
		return "worktree diff changed"
	case old.StatusSum != new.StatusSum:
		return "tracked file states changed"
	case old.UntrackedSum != new.UntrackedSum:
		return "untracked files changed"
	case old.Describe != new.Describe:
		return "git describe changed (tags)"
	case old.BuildEnv != new.BuildEnv:
		return "build environment changed"
	}
	return ""
}

const buildStampVersion = 1

// stampImage records one image the last successful build pushed. Source is
// the make-reported name (e.g. build-xxxx/ceph-amd64) so a skip candidate
// can recompute expected refs — and repush — without running make.
type stampImage struct {
	Source string `json:"source"`
	// SourceID pins the LOCAL image identity behind the mutable Source tag,
	// so a repush can prove it would publish the stamped content and not
	// whatever a later build of another branch retagged over it.
	SourceID string `json:"sourceID"`
	Ref      string `json:"ref"`
	Repo     string `json:"repo"`
	Tag      string `json:"tag"`
	Digest   string `json:"digest"`
}

type buildStamp struct {
	Version       int          `json:"version"`
	Dir           string       `json:"dir"`
	Fingerprint   treeFP       `json:"fingerprint"`
	GitRef        string       `json:"gitRef"`
	Images        []stampImage `json:"images"`
	PushedAt      string       `json:"pushedAt"`
	RooketVersion string       `json:"rooketVersion"` // informational only
}

func buildStampPath(name string) (string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "build-stamp.json"), nil
}

// readBuildStamp returns the recorded stamp, or nil when there is none or it
// is unreadable — every failure just means "no skip".
func readBuildStamp(name string) *buildStamp {
	p, err := buildStampPath(name)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var s buildStamp
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

// writeBuildStamp records a successful build+push atomically (temp+rename),
// so a torn write can only ever produce an unreadable stamp, never a wrong
// one.
func writeBuildStamp(name string, s *buildStamp) error {
	if _, err := ensureStateDir(name); err != nil {
		return err
	}
	p, err := buildStampPath(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// parseImageRef splits "host:port/repo/name:tag" into host, repo path, and
// tag.
func parseImageRef(ref string) (host, repo, tag string, err error) {
	i := strings.Index(ref, "/")
	if i < 0 {
		return "", "", "", fmt.Errorf("image ref %q has no registry host", ref)
	}
	host, rest := ref[:i], ref[i+1:]
	j := strings.LastIndex(rest, ":")
	if j < 0 {
		return "", "", "", fmt.Errorf("image ref %q has no tag", ref)
	}
	return host, rest[:j], rest[j+1:], nil
}

// manifestDigest asks this cluster's registry for an image manifest via a
// HEAD request, returning the digest header and whether the manifest exists.
// The registry binds 127.0.0.1, so the probe cannot hit anything else.
func manifestDigest(port int, repo, tag string) (string, bool) {
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/%s/manifests/%s", port, repo, tag)
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	// A missing digest header would make later comparisons vacuous; treat
	// it as absence rather than letting "" become a wildcard.
	digest := resp.Header.Get("Docker-Content-Digest")
	return digest, digest != ""
}

// registryRunningWithPort reports whether this cluster's own registry
// container is running AND publishes the expected host port — a stopped
// container, or a foreign registry that happens to hold a stale recorded
// port, must not validate a skip.
func registryRunningWithPort(eng engine.Engine, container string, port int) bool {
	state, err := run.Output(eng.String(), "inspect", "-f", "{{.State.Running}}", container)
	if err != nil || strings.TrimSpace(state) != "true" {
		return false
	}
	ports, err := run.Output(eng.String(), "port", container, "5000/tcp")
	return err == nil && portsOutputHasPort(ports, port)
}

// portsOutputHasPort reports whether an `engine port` output publishes
// exactly the given host port (substring matching would accept :50010 for
// :5001).
func portsOutputHasPort(out string, port int) bool {
	want := strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, ":"); i >= 0 && line[i+1:] == want {
			return true
		}
	}
	return false
}

// expectedStampImages recomputes, from the stamped sources, the refs this
// invocation would push — catching registry-port changes, branch switches
// (deploy tags by branch, even at the same commit), and --namespace/--tag
// overrides. A --tag pointing anywhere but this cluster's registry disables
// skipping entirely.
func expectedStampImages(stamp *buildStamp, port int, namespace, tagOverride, gitRef string) ([]stampImage, error) {
	registry := fmt.Sprintf("localhost:%d", port)
	imgs := make([]stampImage, 0, len(stamp.Images))
	for _, img := range stamp.Images {
		ref := tagOverride
		if ref == "" {
			ref = deriveTag(registry, namespace, img.Source, gitRef)
		}
		host, repo, tag, err := parseImageRef(ref)
		if err != nil {
			return nil, err
		}
		if host != registry && host != fmt.Sprintf("127.0.0.1:%d", port) {
			return nil, fmt.Errorf("--tag targets %s, not this cluster's registry", host)
		}
		imgs = append(imgs, stampImage{Source: img.Source, Ref: ref, Repo: repo, Tag: tag})
	}
	return imgs, nil
}

// buildSkipCheck decides what the build step must do. Returns:
//   - "" and nil: everything current — skip make AND push.
//   - reason and non-nil images: the tree is unchanged but the publish side
//     is not current (registry recreated, ref/tag changed, digest gone) —
//     re-pushing the stamped local source images suffices.
//   - reason and nil: run make.
func buildSkipCheck(fp treeFP, fpErr error, stamp *buildStamp, eng engine.Engine, dir, name string, port int, namespace, tagOverride, gitRef string) (string, []stampImage) {
	if fpErr != nil {
		return fmt.Sprintf("fingerprint unavailable: %v", fpErr), nil
	}
	if stamp == nil {
		return "no build stamp for this cluster", nil
	}
	if stamp.Version != buildStampVersion {
		return "build stamp format changed", nil
	}
	if stamp.Dir != dir {
		return "rook source dir changed", nil
	}
	if len(stamp.Images) == 0 {
		return "build stamp records no images", nil
	}
	if reason := fingerprintDiff(stamp.Fingerprint, fp); reason != "" {
		return reason, nil
	}

	expected, err := expectedStampImages(stamp, port, namespace, tagOverride, gitRef)
	if err != nil {
		return err.Error(), nil
	}
	if !registryRunningWithPort(eng, registry.ContainerName(name), port) {
		return "cluster registry not running on the expected port", expected
	}
	for i, img := range expected {
		stamped := stamp.Images[i]
		digest, ok := manifestDigest(port, img.Repo, img.Tag)
		switch {
		case img.Ref != stamped.Ref:
			return fmt.Sprintf("image ref changed (was %s, now %s)", stamped.Ref, img.Ref), expected
		case !ok:
			return fmt.Sprintf("%s missing from registry", img.Ref), expected
		case stamped.Digest == "":
			return fmt.Sprintf("stamped digest for %s unknown", img.Ref), expected
		case digest != stamped.Digest:
			return fmt.Sprintf("%s digest changed in registry", img.Ref), expected
		}
	}
	return "", nil
}

func rooketVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "unknown"
}
