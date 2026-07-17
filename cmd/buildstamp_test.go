package cmd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func gitFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", ".gitignore")
	c.Dir = dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("git", "commit", "-q", "-m", "gitignore")
	c.Dir = dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTreeFingerprint(t *testing.T) {
	dir := gitFixture(t)

	fp1, err := treeFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := treeFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d := fingerprintDiff(fp1, fp2); d != "" {
		t.Fatalf("clean tree not stable: %s", d)
	}

	t.Run("ignored churn is invisible", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
		fp, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if d := fingerprintDiff(fp1, fp); d != "" {
			t.Fatalf("ignored file changed fingerprint: %s", d)
		}
	})

	t.Run("untracked content flips the fingerprint", func(t *testing.T) {
		p := filepath.Join(dir, "new.txt")
		if err := os.WriteFile(p, []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		fpA, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if d := fingerprintDiff(fp1, fpA); d == "" {
			t.Fatal("untracked file invisible to fingerprint")
		}
		// Same size, content change only.
		if err := os.WriteFile(p, []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
		fpB, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fpA.UntrackedSum == fpB.UntrackedSum {
			t.Fatal("same-size untracked content edit not detected")
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tracked edit flips the diff sum", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\nmore\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fp, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fp.DiffSum == fp1.DiffSum {
			t.Fatal("tracked edit invisible to diff sum")
		}
	})

	t.Run("build env flips the fingerprint", func(t *testing.T) {
		t.Setenv("BUILD_CONTAINER_IMAGE", "false")
		fp, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fp.BuildEnv == fp1.BuildEnv {
			t.Fatal("BUILD_CONTAINER_IMAGE invisible to fingerprint")
		}
	})

	t.Run("explicitly empty env differs from unset", func(t *testing.T) {
		t.Setenv("TAGS", "")
		fp, err := treeFingerprint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fp.BuildEnv == fp1.BuildEnv {
			t.Fatal("TAGS= (empty) indistinguishable from unset")
		}
	})

	t.Run("non-repo dir errors", func(t *testing.T) {
		if _, err := treeFingerprint(t.TempDir()); err == nil {
			t.Fatal("expected error outside a git repo")
		}
	})
}

func TestParseImageRef(t *testing.T) {
	host, repo, tag, err := parseImageRef("localhost:5001/rook/ceph:master")
	if err != nil || host != "localhost:5001" || repo != "rook/ceph" || tag != "master" {
		t.Fatalf("got (%q,%q,%q,%v)", host, repo, tag, err)
	}
	for _, bad := range []string{"noslash:tag", "host/repo-no-tag"} {
		if _, _, _, err := parseImageRef(bad); err == nil {
			t.Errorf("parseImageRef(%q): expected error", bad)
		}
	}
}

func TestManifestDigest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/v2/rook/ceph/manifests/master":
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			w.WriteHeader(http.StatusOK)
		case "/v2/rook/ceph/manifests/nodigest":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())

	digest, ok := manifestDigest(port, "rook/ceph", "master")
	if !ok || digest != "sha256:abc" {
		t.Fatalf("got (%q, %v)", digest, ok)
	}
	if _, ok := manifestDigest(port, "rook/ceph", "gone"); ok {
		t.Fatal("404 manifest reported present")
	}
	if _, ok := manifestDigest(port, "rook/ceph", "nodigest"); ok {
		t.Fatal("200 without a digest header must not count as present")
	}
}

func TestPortsOutputHasPort(t *testing.T) {
	if !portsOutputHasPort("127.0.0.1:5001\n", 5001) {
		t.Error("exact port not matched")
	}
	if portsOutputHasPort("127.0.0.1:50010\n", 5001) {
		t.Error(":50010 accepted for :5001")
	}
	if portsOutputHasPort("", 5001) {
		t.Error("empty output matched")
	}
	if !portsOutputHasPort("0.0.0.0:49153\n127.0.0.1:5001", 5001) {
		t.Error("multi-line output not matched")
	}
}

func TestBuildStampRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &buildStamp{
		Version: buildStampVersion,
		Dir:     "/src/rook",
		Fingerprint: treeFP{
			Head: "abc", Describe: "v1-1-gabc", DiffSum: "d", StatusSum: "s",
			UntrackedSum: "u", BuildEnv: "e",
		},
		GitRef: "master",
		Images: []stampImage{{
			Source: "build-x/ceph-amd64", SourceID: "sha256:local",
			Ref:  "localhost:5001/rook/ceph:master",
			Repo: "rook/ceph", Tag: "master", Digest: "sha256:abc",
		}},
	}
	if err := writeBuildStamp("stampcluster", s); err != nil {
		t.Fatal(err)
	}
	got := readBuildStamp("stampcluster")
	if got == nil || got.Images[0].Ref != s.Images[0].Ref || got.Fingerprint != s.Fingerprint {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	p, _ := buildStampPath("stampcluster")
	if err := os.WriteFile(p, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if readBuildStamp("stampcluster") != nil {
		t.Fatal("corrupt stamp parsed")
	}
	if readBuildStamp("neverstamped") != nil {
		t.Fatal("missing stamp parsed")
	}
}

func TestExpectedStampImages(t *testing.T) {
	stamp := &buildStamp{Images: []stampImage{{Source: "build-x/ceph-amd64", SourceID: "sha256:local"}}}

	imgs, err := expectedStampImages(stamp, 5001, "rook", "", "mybranch")
	if err != nil || len(imgs) != 1 {
		t.Fatalf("got (%v, %v)", imgs, err)
	}
	if imgs[0].Ref != "localhost:5001/rook/ceph:mybranch" || imgs[0].Repo != "rook/ceph" || imgs[0].Tag != "mybranch" {
		t.Fatalf("got %+v", imgs[0])
	}
	// The repush path verifies the local image against this identity; losing
	// it here silently disabled repush entirely once.
	if imgs[0].SourceID != "sha256:local" {
		t.Fatalf("SourceID dropped: %+v", imgs[0])
	}

	if _, err := expectedStampImages(stamp, 5001, "rook", "quay.io/other/img:v1", "b"); err == nil {
		t.Fatal("foreign --tag accepted")
	}
	if imgs, err := expectedStampImages(stamp, 5001, "rook", "localhost:5001/rook/ceph:pinned", "b"); err != nil || imgs[0].Tag != "pinned" {
		t.Fatalf("local --tag override: (%v, %v)", imgs, err)
	}
}
