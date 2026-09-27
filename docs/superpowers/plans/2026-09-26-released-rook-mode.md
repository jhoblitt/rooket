# Released-Rook Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `rooket up --rook-version v1.20.7` deploys a released Rook from `https://charts.rook.io/release` with no rook clone and no build, with sticky configuration in a named directory and a per-cluster record later commands read.

**Architecture:** The rook directory's three roles are separated behind one value, `rookSource` (charts, configuration home, released version). Released charts are pulled once per version into a host-wide cache laid out like a clone (`<entry>/deploy/charts/<chart>`), so every existing chart read works against it unchanged. A `source.json` record in the cluster's state dir, beside `shape.json`, carries the version and any named configuration directory.

**Tech Stack:** Go 1.27, cobra, helm CLI (`helm pull --untar`), stdlib `testing` (the `cmd` and `internal` packages' idiom), GitHub Actions.

**Spec:** `/home/jhoblitt/github/rooket/.claude/worktrees/released-rook/docs/superpowers/specs/2026-09-26-released-rook-mode-design.md`

## Global Constraints

- The chart repository is exactly `https://charts.rook.io/release`.
- A rook version is one exact released tag, pre-releases included (`v1.20.7`, `v1.21.0-beta.0`); ranges and untagged forms are rejected before any work starts.
- rooket never writes a `.gitignore` into a directory named with `--config-dir` or `$ROOKET_CONFIG_DIR`.
- Clone mode is unchanged for every user who never passes `--rook-version` or `--config-dir`.
- Configuration home order: flag, then `$ROOKET_CONFIG_DIR`, then the cluster's record, then the `.rooket/` of the rook clone the command runs against or within, then none.
- A command given `--rook-version` refuses the fixed fallback name `rook`.
- Tests are stdlib `testing` with `t.Run` tables, matching every existing test in `cmd/` and `internal/`.
- Machine facts for whoever runs this plan on jhoblitt's workstation: `~/go/bin/go` is a broken `go1.26.1` wrapper, so run Go as
  `GO="env GOTOOLCHAIN=local /home/jhoblitt/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.1.linux-amd64/bin/go"` and then `$GO test ...`.
  `cmd`'s sudoers tests fail inside the Bash sandbox (it maps root-owned files to uid 65534), so run the full `cmd` suite with the sandbox disabled.

## Review Focus

- A `--config-dir` (or `$ROOKET_CONFIG_DIR`) naming a directory that does not exist, e.g. a typo: expect a clear error naming the path, never a silently empty configuration. Pinned in Task 3.
- `$ROOKET_CONFIG_DIR` given as a relative path: expect it recorded as an absolute path, so a later `deploy` from another directory still finds it. Pinned in Task 3.
- A chart cache entry directory that exists but is missing a chart (created by hand, or damaged): expect an error naming the entry to delete, not a helm failure deep in a deploy. Pinned in Task 2.
- `up --rook-version` run from inside a rook clone: expect the clone's name and `.rooket/` configuration, and no build. The name and configuration are pinned in Task 3 (`TestReleasedName`, `TestConfigHome`); skipping the build follows from `released` alone in Task 6.
- A clone-built cluster later given `--rook-version`: expect the record to switch it to released mode for every later command. Pinned in Task 3.

---

### Task 1: A configuration home the user names

**Files:**
- Modify: `internal/clone/clone.go`
- Test: `internal/clone/clone_test.go`

**Interfaces:**
- Produces: `clone.At(dir string) clone.Dir` — a named configuration directory; `Ensure()` on it writes nothing. The zero `clone.Dir{}` means "no configuration home": `Profiles()` and `Templates()` return nil, `ValuesPath(chart)` returns `""`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/clone/clone_test.go`:

```go
// A directory the user names is one they mean to commit — rgw-go keeps its in
// its own repository — so rooket must not hide it from git the way it hides a
// clone's .rooket.
func TestNamedDirIsNeverGivenAGitignore(t *testing.T) {
	root := t.TempDir()
	d := At(root)
	if err := d.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Ensure wrote %d entries into a named directory, want none", len(entries))
	}
	if got := d.ValuesPath("rook-ceph"); got != filepath.Join(root, "values", "rook-ceph.yaml") {
		t.Errorf("ValuesPath = %q, want it under the named directory itself", got)
	}
}

// With neither a named directory nor a clone there is no configuration, and
// reading it must not fall back to the working directory's config.yaml.
func TestZeroDirHasNoConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("config.yaml", []byte("profiles: [rbd]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var d Dir
	if names, err := d.Profiles(); err != nil || names != nil {
		t.Errorf("Profiles = (%v, %v), want (nil, nil)", names, err)
	}
	if files, err := d.Templates(); err != nil || files != nil {
		t.Errorf("Templates = (%v, %v), want (nil, nil)", files, err)
	}
	if got := d.ValuesPath("rook-ceph"); got != "" {
		t.Errorf("ValuesPath = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `$GO test ./internal/clone/`
Expected: FAIL — `undefined: At`.

- [ ] **Step 3: Implement**

In `internal/clone/clone.go`, replace the `Dir` type, `Open`, and the start of `Ensure`, and guard the readers:

```go
// Dir is a configuration home: the .rooket directory inside a rook clone, or
// a directory the user named with --config-dir. The zero Dir is no
// configuration at all.
type Dir struct {
	root string
	// named marks a directory the user chose. It is meant to be committed, so
	// rooket never gives it the self-ignoring .gitignore a clone's .rooket gets.
	named bool
}

func Open(rookDir string) Dir { return Dir{root: filepath.Join(rookDir, ".rooket")} }

// At opens a configuration directory the user named.
func At(dir string) Dir { return Dir{root: dir, named: true} }
```

At the top of `Ensure`:

```go
	if d.named || d.root == "" {
		return nil
	}
```

`ValuesPath` becomes:

```go
func (d Dir) ValuesPath(chart string) string {
	if d.root == "" {
		return ""
	}
	return filepath.Join(d.root, "values", chart+".yaml")
}
```

At the top of `Profiles` and of `Templates`:

```go
	if d.root == "" {
		return nil, nil
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `$GO test ./internal/clone/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/clone/clone.go internal/clone/clone_test.go
git commit -m "feat(clone): open a configuration directory the user names"
```

The body says why a named directory gets no `.gitignore`. End every commit message in this plan with the attribution lines the session's system reminder specifies.

---

### Task 2: The released-chart cache

**Files:**
- Create: `internal/chartcache/chartcache.go`
- Test: `internal/chartcache/chartcache_test.go`

**Interfaces:**
- Produces:
  - `chartcache.Repo` (`"https://charts.rook.io/release"`), `chartcache.Charts` (`[]string{"rook-ceph", "rook-ceph-cluster"}`)
  - `func ValidVersion(v string) error`
  - `type Puller func(dir, chart, version string) error` — unpacks one chart as `dir/<chart>`
  - `func Ensure(root, version string, pull Puller) (string, error)` — returns the entry directory `root/<version>`, which holds `deploy/charts/<chart>/Chart.yaml` for every chart
  - `func DefaultRoot() (string, error)` — `<user cache dir>/rooket/charts`

- [ ] **Step 1: Write the failing tests**

Create `internal/chartcache/chartcache_test.go`:

```go
package chartcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePull unpacks a stand-in chart the way helm pull --untar does, counting
// calls per chart.
func fakePull(calls map[string]int) Puller {
	return func(dir, chart, version string) error {
		calls[chart]++
		c := filepath.Join(dir, chart)
		if err := os.MkdirAll(c, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c, "Chart.yaml"), []byte("name: "+chart+"\nversion: "+version+"\n"), 0o644)
	}
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"v1.20.7", "v1.21.0-beta.0"} {
		if err := ValidVersion(v); err != nil {
			t.Errorf("ValidVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"", "1.20.7", "v1.20", "v1.20.x", "~1.20", "v1.20.7 "} {
		if err := ValidVersion(v); err == nil {
			t.Errorf("ValidVersion(%q) = nil, want it rejected", v)
		}
	}
}

func TestEnsurePullsEachChartOnce(t *testing.T) {
	root := t.TempDir()
	calls := map[string]int{}

	entry, err := Ensure(root, "v1.20.7", fakePull(calls))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != filepath.Join(root, "v1.20.7") {
		t.Errorf("entry = %q, want %q", entry, filepath.Join(root, "v1.20.7"))
	}
	for _, c := range Charts {
		if _, err := os.Stat(filepath.Join(entry, "deploy", "charts", c, "Chart.yaml")); err != nil {
			t.Errorf("chart %s not laid out like a clone's: %v", c, err)
		}
	}
	if _, err := Ensure(root, "v1.20.7", fakePull(calls)); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	for _, c := range Charts {
		if calls[c] != 1 {
			t.Errorf("%s pulled %d times, want once: a present entry is reused", c, calls[c])
		}
	}
}

// A pull that fails part way must leave nothing a later run would trust.
func TestEnsureLeavesNoEntryAfterAFailedPull(t *testing.T) {
	root := t.TempDir()
	boom := errors.New("network down")
	pull := func(dir, chart, version string) error {
		if chart == "rook-ceph-cluster" {
			return boom
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	if _, err := Ensure(root, "v1.20.7", pull); !errors.Is(err, boom) {
		t.Fatalf("Ensure = %v, want the pull's error", err)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("root holds %d entries after a failed pull, want none", len(left))
	}
}

// Two runs pulling the same version at once: the one that loses the rename
// uses the winner's entry instead of failing.
func TestEnsureYieldsToAConcurrentWinner(t *testing.T) {
	root := t.TempDir()
	winner := filepath.Join(root, "v1.20.7")
	pull := func(dir, chart, version string) error {
		if chart == Charts[len(Charts)-1] {
			for _, c := range Charts {
				cd := filepath.Join(winner, "deploy", "charts", c)
				if err := os.MkdirAll(cd, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(cd, "Chart.yaml"), []byte("name: "+c+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
		return fakePull(map[string]int{})(dir, chart, version)
	}

	entry, err := Ensure(root, "v1.20.7", pull)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if entry != winner {
		t.Errorf("entry = %q, want the winner's %q", entry, winner)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("root holds %d entries, want only the winner's: the loser's copy is discarded", len(left))
	}
}

func TestEnsureRejectsAnIncompleteEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v1.20.7", "deploy", "charts", "rook-ceph"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(root, "v1.20.7", fakePull(map[string]int{}))
	if err == nil {
		t.Fatal("Ensure = nil error, want the incomplete entry reported")
	}
	if want := filepath.Join(root, "v1.20.7"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the entry %s to delete", err, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `$GO test ./internal/chartcache/`
Expected: FAIL — `undefined: Puller` (the package has no source yet).

- [ ] **Step 3: Implement**

Create `internal/chartcache/chartcache.go`:

```go
// Package chartcache keeps the charts of released Rook versions on disk, one
// entry per version, laid out like a rook clone's deploy/charts so everything
// that reads a clone's charts reads an entry the same way.
package chartcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Repo is where released Rook charts are published.
const Repo = "https://charts.rook.io/release"

// Charts are the charts a released Rook version is installed from.
var Charts = []string{"rook-ceph", "rook-ceph-cluster"}

// versionRE matches one released tag. A range would resolve anew on each
// deploy and drift under a cluster whose record says what it runs.
var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// ValidVersion reports whether v names exactly one released Rook version.
func ValidVersion(v string) error {
	if !versionRE.MatchString(v) {
		return fmt.Errorf("rook version %q is not a single released version such as v1.20.7", v)
	}
	return nil
}

// Puller unpacks one chart at one version into dir, as dir/<chart>.
type Puller func(dir, chart, version string) error

// DefaultRoot is the host-wide cache, shared by every cluster.
func DefaultRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	return filepath.Join(dir, "rooket", "charts"), nil
}

// Ensure returns the cache entry for version under root, pulling it first when
// absent. An entry is pulled into a temporary sibling and renamed into place,
// so an interrupted pull never leaves one behind; a run that loses that rename
// to a concurrent one uses the winner's entry.
func Ensure(root, version string, pull Puller) (string, error) {
	if err := ValidVersion(version); err != nil {
		return "", err
	}
	entry := filepath.Join(root, version)
	if _, err := os.Stat(entry); err == nil {
		return entry, complete(entry)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create chart cache %s: %w", root, err)
	}
	tmp, err := os.MkdirTemp(root, "."+version+"-")
	if err != nil {
		return "", fmt.Errorf("create chart cache entry for %s: %w", version, err)
	}
	defer os.RemoveAll(tmp)

	charts := filepath.Join(tmp, "deploy", "charts")
	if err := os.MkdirAll(charts, 0o755); err != nil {
		return "", err
	}
	for _, c := range Charts {
		if err := pull(charts, c, version); err != nil {
			return "", fmt.Errorf("pull chart %s %s from %s: %w", c, version, Repo, err)
		}
	}
	if err := os.Rename(tmp, entry); err != nil {
		if _, statErr := os.Stat(entry); statErr == nil {
			return entry, complete(entry)
		}
		return "", fmt.Errorf("install chart cache entry %s: %w", entry, err)
	}
	return entry, nil
}

// complete checks that an entry still holds every chart. Entries are only
// ever renamed into place whole, so a missing chart means something changed
// the entry afterwards.
func complete(entry string) error {
	for _, c := range Charts {
		_, err := os.Stat(filepath.Join(entry, "deploy", "charts", c, "Chart.yaml"))
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("chart cache entry %s has no %s chart; delete the entry to pull it again", entry, c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `$GO test -race ./internal/chartcache/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/chartcache/
git commit -m "feat(chartcache): keep released Rook charts on disk, one entry per version"
```

---

### Task 3: The cluster's source record, and how commands resolve it

**Files:**
- Create: `cmd/source.go`
- Modify: `cmd/state.go` (split `helmEnv` so a helm home can live outside a state dir)
- Test: `cmd/source_test.go`

**Interfaces:**
- Consumes: `chartcache.ValidVersion`, `chartcache.Ensure`, `chartcache.DefaultRoot`, `chartcache.Repo` (Task 2); `clone.At`, `clone.Open`, zero `clone.Dir` (Task 1).
- Produces:
  - `type clusterSource struct { RookVersion string; ConfigDir string }` with JSON tags `rookVersion,omitempty` / `configDir,omitempty`
  - `func readSource(name string) (clusterSource, bool)`, `func readSourceAt(stateDir string) (clusterSource, bool)`, `func writeSource(name string, s clusterSource) error`
  - `func resolveSource(name, version string, versionSet bool, configDir string, configSet bool) (clusterSource, bool, error)` — the bool is "differs from the record"
  - `func configHome(src clusterSource, rookDir string) clone.Dir`
  - `func nameIsFallback(flagName string) bool`, `func releasedName(flagName string) error`
  - `var chartPuller func(env []string) chartcache.Puller` (tests replace it), `func releasedCharts(version string) (string, error)`
  - `func helmEnvAt(base string) ([]string, error)`; `helmEnv(name, purpose)` keeps its signature and behavior and calls it

- [ ] **Step 1: Write the failing tests**

Create `cmd/source_test.go`:

```go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/chartcache"
)

func TestSourceRecordRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := clusterSource{RookVersion: "v1.20.7", ConfigDir: "/work/rgw-go/hack/rooket"}
	if err := writeSource("c1", want); err != nil {
		t.Fatal(err)
	}
	if got, ok := readSource("c1"); !ok || got != want {
		t.Errorf("readSource = (%+v, %v), want (%+v, true)", got, ok, want)
	}
	if _, ok := readSource("never-deployed"); ok {
		t.Error("readSource of a cluster with no record = ok, want none")
	}
}

func TestResolveSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_CONFIG_DIR", "")
	cfg := t.TempDir()

	t.Run("record fills unset flags", func(t *testing.T) {
		if err := writeSource("rec", clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
		got, changed, err := resolveSource("rec", "", false, "", false)
		if err != nil || changed || got != (clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}) {
			t.Errorf("resolveSource = (%+v, %v, %v), want the record unchanged", got, changed, err)
		}
	})

	// A clone-built cluster given --rook-version moves to released mode, and the
	// change is reported so the caller records it for every later command.
	t.Run("a version flag replaces the record", func(t *testing.T) {
		got, changed, err := resolveSource("clone-built", "v1.20.7", true, "", false)
		if err != nil || !changed || got.RookVersion != "v1.20.7" {
			t.Errorf("resolveSource = (%+v, %v, %v), want v1.20.7 and changed", got, changed, err)
		}
	})

	t.Run("a range is rejected", func(t *testing.T) {
		if _, _, err := resolveSource("x", "v1.20.x", true, "", false); err == nil {
			t.Error("resolveSource accepted v1.20.x, want it rejected")
		}
	})

	t.Run("the environment names a configuration directory", func(t *testing.T) {
		t.Setenv("ROOKET_CONFIG_DIR", cfg)
		got, _, err := resolveSource("env", "", false, "", false)
		if err != nil || got.ConfigDir != cfg {
			t.Errorf("resolveSource = (%+v, %v), want ConfigDir %s", got, err, cfg)
		}
	})

	t.Run("a flag beats the environment", func(t *testing.T) {
		t.Setenv("ROOKET_CONFIG_DIR", t.TempDir())
		got, _, err := resolveSource("both", "", false, cfg, true)
		if err != nil || got.ConfigDir != cfg {
			t.Errorf("resolveSource = (%+v, %v), want the flag's %s", got, err, cfg)
		}
	})

	t.Run("a missing configuration directory is an error naming it", func(t *testing.T) {
		missing := filepath.Join(cfg, "typo")
		_, _, err := resolveSource("typo", "", false, missing, true)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("resolveSource = %v, want an error naming %s", err, missing)
		}
	})

	// Recorded relative, it would name a different directory from wherever the
	// next command runs.
	t.Run("a relative directory is recorded absolute", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Mkdir(filepath.Join(parent, "hack"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(parent)
		t.Setenv("ROOKET_CONFIG_DIR", "hack")
		got, _, err := resolveSource("rel", "", false, "", false)
		if err != nil || got.ConfigDir != filepath.Join(parent, "hack") {
			t.Errorf("resolveSource = (%+v, %v), want ConfigDir %s", got, err, filepath.Join(parent, "hack"))
		}
	})
}

func TestConfigHome(t *testing.T) {
	named := t.TempDir()
	clone := t.TempDir()
	if got := configHome(clusterSource{ConfigDir: named}, clone).ValuesPath("rook-ceph"); got != filepath.Join(named, "values", "rook-ceph.yaml") {
		t.Errorf("with a named directory, ValuesPath = %q, want it under %s", got, named)
	}
	if got := configHome(clusterSource{RookVersion: "v1.20.7"}, clone).ValuesPath("rook-ceph"); got != filepath.Join(clone, ".rooket", "values", "rook-ceph.yaml") {
		t.Errorf("released inside a clone, ValuesPath = %q, want the clone's .rooket", got)
	}
	if got := configHome(clusterSource{RookVersion: "v1.20.7"}, "").ValuesPath("rook-ceph"); got != "" {
		t.Errorf("with neither, ValuesPath = %q, want no configuration", got)
	}
}

func TestReleasedName(t *testing.T) {
	t.Setenv("ROOKET_NAME", "")
	t.Chdir(t.TempDir())
	if err := releasedName(""); err == nil {
		t.Error("releasedName outside a clone with no name = nil, want the fallback refused")
	}
	if err := releasedName("rgw-go"); err != nil {
		t.Errorf("releasedName with --name = %v, want nil", err)
	}
	t.Setenv("ROOKET_NAME", "rgw-go")
	if err := releasedName(""); err != nil {
		t.Errorf("releasedName with $ROOKET_NAME = %v, want nil", err)
	}

	clone := t.TempDir()
	writeGoMod(t, clone, rookModulePath)
	t.Setenv("ROOKET_NAME", "")
	t.Chdir(clone)
	if err := releasedName(""); err != nil {
		t.Errorf("releasedName inside a clone = %v, want nil: the clone names the cluster", err)
	}
}

// stubChartPuller makes releasedCharts unpack stand-in charts into a
// throwaway cache instead of running helm.
func stubChartPuller(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	prev := chartPuller
	chartPuller = func([]string) chartcache.Puller {
		return func(dir, chart, version string) error {
			c := filepath.Join(dir, chart)
			if err := os.MkdirAll(c, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(c, "Chart.yaml"), []byte("name: "+chart+"\n"), 0o644)
		}
	}
	t.Cleanup(func() { chartPuller = prev })
}

func TestReleasedChartsUsesTheHostWideCache(t *testing.T) {
	stubChartPuller(t)
	entry, err := releasedCharts("v1.20.7")
	if err != nil {
		t.Fatalf("releasedCharts: %v", err)
	}
	root, _ := chartcache.DefaultRoot()
	if entry != filepath.Join(root, "v1.20.7") {
		t.Errorf("entry = %q, want it under %s", entry, root)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `$GO test -run 'Source|ConfigHome|ReleasedName|ReleasedCharts' ./cmd/`
Expected: FAIL — `undefined: writeSource` and the other new names.

- [ ] **Step 3: Split `helmEnv`**

In `cmd/state.go`, `helmEnv` keeps its doc comment and becomes:

```go
func helmEnv(name, purpose string) ([]string, error) {
	dir, err := stateDirPath(name)
	if err != nil {
		return nil, err
	}
	return helmEnvAt(filepath.Join(dir, "helm", purpose))
}

// helmEnvAt returns environment variables pointing helm at a config/cache/data
// triplet under base, creating the directories; see helmEnv.
func helmEnvAt(base string) ([]string, error) {
```

The body of `helmEnvAt` is the old body of `helmEnv` from `homes := map[string]string{}` onward, unchanged.

- [ ] **Step 4: Implement `cmd/source.go`**

```go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/chartcache"
	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/run"
)

// sourceFile names the record, in a cluster's state directory, of where its
// Rook comes from and where its configuration lives. Every command after the
// one that set them reads it, so neither has to be repeated.
const sourceFile = "source.json"

// clusterSource is what a cluster was last deployed from. An empty
// RookVersion means a rook clone. ConfigDir is set only when a configuration
// directory was named, and is always absolute.
type clusterSource struct {
	RookVersion string `json:"rookVersion,omitempty"`
	ConfigDir   string `json:"configDir,omitempty"`
}

func readSource(name string) (clusterSource, bool) {
	dir, err := stateDirPath(name)
	if err != nil {
		return clusterSource{}, false
	}
	return readSourceAt(dir)
}

// readSourceAt reads the record from a state directory, for prune, which
// walks directories rather than names.
func readSourceAt(stateDir string) (clusterSource, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, sourceFile))
	if err != nil {
		return clusterSource{}, false
	}
	var s clusterSource
	if json.Unmarshal(data, &s) != nil {
		return clusterSource{}, false
	}
	return s, true
}

// writeSource records a cluster's source atomically (temp+rename), so a torn
// write leaves an unreadable record rather than a wrong one.
func writeSource(name string, s clusterSource) error {
	dir, err := ensureStateDir(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, sourceFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("record cluster source: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("record cluster source: %w", err)
	}
	return nil
}

// resolveSource settles a command's rook version and configuration directory.
// The version is the flag when the user passed it, else the record. The
// directory is the flag, else $ROOKET_CONFIG_DIR, else the record; a named one
// must exist, and is made absolute so the record means the same thing from
// wherever the next command runs. It also reports whether the result differs
// from the record, which the commands that deploy then write back.
func resolveSource(name, version string, versionSet bool, configDir string, configSet bool) (clusterSource, bool, error) {
	rec, _ := readSource(name)
	out := rec
	if versionSet {
		if err := chartcache.ValidVersion(version); err != nil {
			return clusterSource{}, false, err
		}
		out.RookVersion = version
	}
	dir := ""
	switch {
	case configSet:
		dir = configDir
	case os.Getenv("ROOKET_CONFIG_DIR") != "":
		dir = os.Getenv("ROOKET_CONFIG_DIR")
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return clusterSource{}, false, fmt.Errorf("resolve configuration directory %s: %w", dir, err)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return clusterSource{}, false, fmt.Errorf("configuration directory %s does not exist", abs)
		}
		out.ConfigDir = abs
	}
	return out, out != rec, nil
}

// configHome is where a command's sticky values, profile list, and templates
// come from: a named directory, else the .rooket of the rook clone the command
// runs against or within, else nowhere.
func configHome(src clusterSource, rookDir string) clone.Dir {
	if src.ConfigDir != "" {
		return clone.At(src.ConfigDir)
	}
	if rookDir != "" {
		return clone.Open(rookDir)
	}
	return clone.Dir{}
}

// nameIsFallback reports whether clusterName(flagName) would settle on the
// fixed name "rook" for want of anything else to go on.
func nameIsFallback(flagName string) bool {
	if flagName != "" || os.Getenv("ROOKET_NAME") != "" {
		return false
	}
	wd, err := os.Getwd()
	return err != nil || findRookRoot(wd) == ""
}

// releasedName refuses the fallback name for a command deploying released
// Rook: two unrelated consumers on one host would otherwise share a cluster.
func releasedName(flagName string) error {
	if nameIsFallback(flagName) {
		return fmt.Errorf("--rook-version outside a rook clone needs a cluster name: pass --name or set $ROOKET_NAME")
	}
	return nil
}

// chartPuller unpacks one released chart with helm; tests replace it.
var chartPuller = func(env []string) chartcache.Puller {
	return func(dir, chart, version string) error {
		return run.CmdWithEnv(env, "helm", "pull", chart,
			"--repo", chartcache.Repo, "--version", version, "--untar", "--untardir", dir)
	}
}

// releasedCharts returns the chart cache entry for a released Rook version,
// pulling it on first use. The pull gets a helm home of its own inside the
// cache, because the cache outlives any one cluster's state dir.
func releasedCharts(version string) (string, error) {
	root, err := chartcache.DefaultRoot()
	if err != nil {
		return "", err
	}
	env, err := helmEnvAt(filepath.Join(root, ".helm"))
	if err != nil {
		return "", err
	}
	return chartcache.Ensure(root, version, chartPuller(env))
}
```

`writeGoMod` and `rookModulePath` already exist in the `cmd` package's tests and source.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `$GO test -run 'Source|ConfigHome|ReleasedName|ReleasedCharts|HelmEnv' ./cmd/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/source.go cmd/source_test.go cmd/state.go
git commit -m "feat(cmd): record where a cluster's Rook and configuration come from"
```

---

### Task 4: An operator base without an image

**Files:**
- Modify: `internal/values/base.go` (`OperatorBase`)
- Test: `internal/values/base_test.go`

**Interfaces:**
- Produces: `values.OperatorBase(values.OperatorInput{})` — with an empty `ImageRepo`, no `image` and no `annotations` keys; `csi.provisionerReplicas` stays.

- [ ] **Step 1: Write the failing test**

Add a subtest to `TestOperatorBase` in `internal/values/base_test.go`:

```go
	// A released chart pins its own operator image by tag, and a released tag
	// does not move, so there is nothing to override or roll.
	t.Run("without an image leaves the chart's own", func(t *testing.T) {
		got := OperatorBase(OperatorInput{})
		want := map[string]any{"csi": map[string]any{"provisionerReplicas": 1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `$GO test -run TestOperatorBase ./internal/values/`
Expected: FAIL — `got` carries an `image` map with empty repository and tag.

- [ ] **Step 3: Implement**

At the top of `OperatorBase`, before `image := ...`:

```go
	if in.ImageRepo == "" {
		return map[string]any{"csi": map[string]any{"provisionerReplicas": 1}}
	}
```

Extend the function's doc comment with one sentence: "An empty ImageRepo leaves the chart's own image, as a released chart pins one."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `$GO test ./internal/values/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/values/base.go internal/values/base_test.go
git commit -m "feat(values): leave a released chart's operator image alone"
```

---

### Task 5: Deploy from a `rookSource`

**Files:**
- Modify: `cmd/deploy.go`, `cmd/profilesrelease.go`, `cmd/compose.go` (`composeChart` tolerates no configuration)
- Test: `cmd/deploy_test.go`

**Interfaces:**
- Consumes: `resolveSource`, `writeSource`, `configHome`, `releasedName`, `releasedCharts`, `chartPuller` (Task 3); `values.OperatorBase(values.OperatorInput{})` (Task 4).
- Produces:
  - `type rookSource struct { charts string; config clone.Dir; released string }`
  - `func deploySetup(cmd *cobra.Command) (rookSource, []profiles.Profile, error)`
  - `installRookCephOperator(src rookSource, active []profiles.Profile) error`, `installCephCsiDrivers(src rookSource, active ...)`, `installRookCephCluster(src rookSource, active ...)`, `installProfilesChart(config clone.Dir, active ...)`, `writeComposed(chart string, base map[string]any, config clone.Dir, active ...) (string, error)`
  - deploy flags `--rook-version` (`deployRookVersion`) and `--config-dir` (`deployConfigDir`)

- [ ] **Step 1: Write the failing tests**

In `cmd/deploy_test.go`, extend `isolateDeploySetup` to also save and restore `deployRookVersion` and `deployConfigDir`, setting both to `""`, and to set `t.Setenv("ROOKET_CONFIG_DIR", "")`. Then append:

```go
// released reads a deploy's source the way up hands it over: deployCmd with
// --rook-version set.
func parseDeployFlags(t *testing.T, args ...string) {
	t.Helper()
	if err := deployCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"rook-version", "config-dir", "workers"} {
			f := deployCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
}

func TestDeploySetupReleasedUsesTheChartCacheAndRecordsIt(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	parseDeployFlags(t, "--rook-version=v1.20.7")

	src, _, err := deploySetup(deployCmd)
	if err != nil {
		t.Fatalf("deploySetup: %v", err)
	}
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want v1.20.7", src.released)
	}
	if _, err := os.Stat(filepath.Join(src.charts, "deploy", "charts", chartCluster, "Chart.yaml")); err != nil {
		t.Errorf("charts %s is not a pulled cache entry: %v", src.charts, err)
	}
	if rec, ok := readSource("released"); !ok || rec.RookVersion != "v1.20.7" {
		t.Errorf("record = (%+v, %v), want v1.20.7 recorded for later commands", rec, ok)
	}
}

func TestDeploySetupTakesTheReleasedVersionFromTheRecord(t *testing.T) {
	isolateDeploySetup(t, "released", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7"}); err != nil {
		t.Fatal(err)
	}

	src, _, err := deploySetup(deployCmd)
	if err != nil {
		t.Fatalf("deploySetup: %v", err)
	}
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want the recorded v1.20.7", src.released)
	}
}

func TestDeploySetupReleasedRefusesTheFallbackName(t *testing.T) {
	isolateDeploySetup(t, "unused", clusterShape{Workers: 1, DiskCount: 1, IQNDate: "2003-01"})
	stubChartPuller(t)
	t.Setenv("ROOKET_NAME", "")
	t.Chdir(t.TempDir())
	deployName = ""
	parseDeployFlags(t, "--rook-version=v1.20.7")

	if _, _, err := deploySetup(deployCmd); err == nil {
		t.Fatal("deploySetup = nil error, want the fallback name refused")
	}
}
```

Also change `TestDeploySetupRejectsAContradictingWorkersFlag` to call `parseDeployFlags(t, "--workers=3")` in place of its own `ParseFlags` call and flag-reset cleanup, so every test resets the same flags the same way.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `$GO test -run DeploySetup ./cmd/`
Expected: FAIL — `undefined: deployRookVersion`, and `deploySetup` returns a string.

- [ ] **Step 3: Add the type and flags**

In `cmd/deploy.go`, add to the `var (...)` block:

```go
	deployRookVersion  string
	deployConfigDir    string
```

and above `deploySetup`:

```go
// rookSource is where a deploy's charts, configuration, and operator image
// come from.
type rookSource struct {
	// charts holds deploy/charts/<chart>: a rook clone, or the chart cache
	// entry of a released version.
	charts string
	// config is the configuration home values and templates compose from.
	config clone.Dir
	// released is the Rook version deployed from its published charts and
	// images; empty for a clone, whose operator image rooket built.
	released string
}
```

In `init`, after the `--dir` flag:

```go
	pf.StringVar(&deployRookVersion, "rook-version", "", "deploy this released Rook version from "+chartcache.Repo+" instead of a rook clone (default: the cluster's recorded version)")
	pf.StringVar(&deployConfigDir, "config-dir", "", "configuration directory laid out like .rooket/ (default: $ROOKET_CONFIG_DIR, else the cluster's recorded one, else the rook clone's .rooket)")
```

- [ ] **Step 4: Rewrite `deploySetup`**

Its doc comment's first sentence becomes "deploySetup resolves everything a deploy needs: ... and where its Rook and configuration come from. It returns that source and the resolved profiles." The body, from the top to the profile resolution:

```go
func deploySetup(cmd *cobra.Command) (rookSource, []profiles.Profile, error) {
	versionSet := cmd.Flags().Changed("rook-version")
	if versionSet {
		if err := releasedName(deployName); err != nil {
			return rookSource{}, nil, err
		}
	}
	name, err := useCluster(deployName)
	if err != nil {
		return rookSource{}, nil, err
	}
	deployName = name
```

Keep the shape, kube-context, with-only, helm env, and registry-port blocks as they are, with `rookSource{}` in their error returns. Replace from `dir := deployDir` through the `cloneDir.Ensure()` check with:

```go
	rec, changed, err := resolveSource(name, deployRookVersion, versionSet,
		deployConfigDir, cmd.Flags().Changed("config-dir"))
	if err != nil {
		return rookSource{}, nil, err
	}
	if changed {
		if err := writeSource(name, rec); err != nil {
			return rookSource{}, nil, err
		}
	}

	src := rookSource{released: rec.RookVersion}
	rookDir := deployDir
	if src.released == "" {
		if rookDir == "" {
			if rookDir, err = os.Getwd(); err != nil {
				return rookSource{}, nil, fmt.Errorf("get working directory: %w", err)
			}
		}
		src.charts = rookDir
	} else {
		if rookDir == "" {
			if wd, err := os.Getwd(); err == nil {
				rookDir = findRookRoot(wd)
			}
		}
		if src.charts, err = releasedCharts(src.released); err != nil {
			return rookSource{}, nil, err
		}
	}
	src.config = configHome(rec, rookDir)
	if err := src.config.Ensure(); err != nil {
		return rookSource{}, nil, err
	}
	names, err := activeProfileNames(src.config, deployWith, deployWithOnly, deployWithOnlySet)
```

and the end returns `src, active, nil`. Clone mode keeps its existing rook directory: `--dir`, else the working directory.

- [ ] **Step 5: Thread `rookSource` through the installers**

`deployCmd`, `deployOperatorCmd`, and `deployClusterCmd` bind `src, active, err := deploySetup(cmd)` and pass `src` to the installers and `src.config` to `installProfilesChart`.

`installRookCephOperator(src rookSource, active []profiles.Profile) error`:

```go
	chartPath := filepath.Join(src.charts, "deploy", "charts", chartOperator)
	var in values.OperatorInput
	image := "the chart's own (released " + src.released + ")"
	if src.released == "" {
		gitRef, err := gitHeadRef(src.charts)
		if err != nil {
			return fmt.Errorf("determine git ref in %s: %w", src.charts, err)
		}
		registry := fmt.Sprintf("localhost:%d", deployRegistryPort)
		in = values.OperatorInput{
			ImageRepo: fmt.Sprintf("%s/%s/%s", registry, deployNamespace, deployImageName),
			ImageTag:  gitRef, // already sanitized by gitHeadRef
		}
		in.Digest = digestOrEmpty(deployRegistryPort, deployNamespace+"/"+deployImageName, in.ImageTag)
		image = in.ImageRepo + ":" + in.ImageTag
		// Shares the "make" purpose helm home (see helmEnv) with
		// installRookCephCluster's ensureChartDeps call — the two must never run
		// concurrently (invariant 2). They already can't: this whole operator
		// install (including ceph-csi-drivers) completes before cluster starts.
		// A released chart ships its dependencies unpacked, so it has none to
		// restore.
		if err := ensureChartDeps(src.charts, chartOperator); err != nil {
			return err
		}
	}
```

then:

```go
	run.Printf("==> deploying rook-ceph operator\n")
	run.Printf("    chart:      %s\n", chartPath)
	run.Printf("    image:      %s\n", image)
	run.Printf("    release:    %s\n", deployOperatorName)
	run.Printf("    namespace:  rook-ceph\n")

	valuesPath, err := writeComposed(chartOperator, values.OperatorBase(in), src.config, active)
	if err != nil {
		return err
	}
```

followed by the existing helm `upgrade --install` of `chartPath`, unchanged, and `return installCephCsiDrivers(src, active)`.

`installCephCsiDrivers(src rookSource, active ...)` reads `filepath.Join(src.charts, "deploy", "charts", chartOperator, "Chart.yaml")` and calls `writeComposed(chartCSI, values.CSIBase(), src.config, active)`.

`installRookCephCluster(src rookSource, active ...)` uses `chartPath := filepath.Join(src.charts, "deploy", "charts", chartCluster)`, runs `ensureChartDeps(src.charts, chartCluster)` only when `src.released == ""` (keep its invariant-2 comment above that `if`), builds `clusterBase(src.charts, deployWorkers, nodes)`, and calls `writeComposed(chartCluster, base, src.config, active)`.

`writeComposed(chart string, base map[string]any, config clone.Dir, active []profiles.Profile)` drops its `clone.Open` and calls `config.Ensure()` then `composeChart(chart, base, config, active, deployValueFiles)`.

In `cmd/profilesrelease.go`, `installProfilesChart(config clone.Dir, active []profiles.Profile)` uses `profileSources(config, active)` in place of `clone.Open(rookDir)`.

In `cmd/compose.go`, `composeChart` loads the sticky layer only when there is a configuration home:

```go
	if p := cloneDir.ValuesPath(chart); p != "" {
		sticky, err := values.LoadFile(p)
		if err != nil {
			return composed{}, err
		}
		if sticky != nil {
			layers = append(layers, values.Layer{Name: ".rooket/values", Values: sticky})
		}
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run (sandbox disabled, for the sudoers tests): `$GO test -race ./cmd/`
Expected: PASS, including every existing deploy and values test.

- [ ] **Step 7: Commit**

```bash
git add cmd/deploy.go cmd/deploy_test.go cmd/profilesrelease.go cmd/compose.go
git commit -m "feat(deploy): deploy a released Rook from the chart cache"
```

The body says clone mode is unchanged and why the dependency restore is skipped for a released chart.

---

### Task 6: `up` in released mode

**Files:**
- Modify: `cmd/up.go`
- Test: `cmd/up_values_test.go`

**Interfaces:**
- Consumes: `releasedName`, `resolveSource`, `writeSource`, `releasedCharts` (Task 3); the deploy path from Task 5 reads the record `up` writes.
- Produces: `up` flags `--rook-version` (`upRookVersion`) and `--config-dir` (`upConfigDir`); `func releasedBuildConflict(released bool, forceBuild bool) error`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/up_values_test.go`:

```go
// A released version is deployed from its published images, so there is no
// tree to build — --force-build asks for something that cannot happen.
func TestReleasedBuildConflict(t *testing.T) {
	if err := releasedBuildConflict(true, true); err == nil {
		t.Error("releasedBuildConflict(released, force) = nil, want --force-build refused")
	}
	for _, c := range []struct{ released, force bool }{{true, false}, {false, true}, {false, false}} {
		if err := releasedBuildConflict(c.released, c.force); err != nil {
			t.Errorf("releasedBuildConflict(%v, %v) = %v, want nil", c.released, c.force, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `$GO test -run ReleasedBuildConflict ./cmd/`
Expected: FAIL — `undefined: releasedBuildConflict`.

- [ ] **Step 3: Implement**

Add `upRookVersion string` and `upConfigDir string` to `up.go`'s `var (...)` block, and in `init`:

```go
	upCmd.Flags().StringVar(&upRookVersion, "rook-version", "", "deploy this released Rook version from "+chartcache.Repo+", skipping the build (default: the cluster's recorded version, else a rook clone)")
	upCmd.Flags().StringVar(&upConfigDir, "config-dir", "", "configuration directory laid out like .rooket/ (default: $ROOKET_CONFIG_DIR, else the cluster's recorded one, else the rook clone's .rooket)")
```

Add:

```go
// releasedBuildConflict refuses --force-build for a cluster deployed from a
// released Rook, whose images are published rather than built.
func releasedBuildConflict(released, forceBuild bool) error {
	if released && forceBuild {
		return fmt.Errorf("--force-build has nothing to build: a released Rook is deployed from its published images")
	}
	return nil
}
```

In `upCmd.RunE`, delete the up-front `rookDir` block (the comment "Resolve the rook source dir up front..." and its `if`), and before `name, err := useCluster(upName)` insert:

```go
		if cmd.Flags().Changed("rook-version") {
			if err := releasedName(upName); err != nil {
				return err
			}
		}
```

After the `useRecordedShape` call, insert:

```go
		// Where the cluster's Rook comes from is settled before anything is
		// stood up, so a missing clone or an unreachable chart repository fails
		// fast. The record is written now because deploy, below, reads it.
		src, srcChanged, err := resolveSource(name, upRookVersion, cmd.Flags().Changed("rook-version"),
			upConfigDir, cmd.Flags().Changed("config-dir"))
		if err != nil {
			return err
		}
		released := src.RookVersion != ""
		if err := releasedBuildConflict(released, upForceBuild); err != nil {
			return err
		}
		var rookDir string
		if !released && (!upSkipBuild || !upSkipDeploy) {
			if rookDir, err = resolveRookDir(upRookDir); err != nil {
				return err
			}
		}
		if released && !upSkipDeploy {
			if _, err := releasedCharts(src.RookVersion); err != nil {
				return err
			}
		}
		if srcChanged {
			if err := writeSource(name, src); err != nil {
				return err
			}
		}
```

Change `if upSkipBuild {` to `if upSkipBuild || released {`, and its build banner to:

```go
			if released {
				run.Printf("==> [3/4] build (skipped: deploying released Rook %s)\n", src.RookVersion)
			} else {
				run.Printf("==> [3/4] build (skipped)\n")
			}
```

In the deploy step, `deployDir = rookDir` stays: it is empty in released mode, and `deploySetup` then takes the version from the record `up` just wrote.

- [ ] **Step 4: Run the tests to verify they pass**

Run (sandbox disabled): `$GO test -race ./cmd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/up.go cmd/up_values_test.go
git commit -m "feat(up): bring up a released Rook with no clone and no build"
```

---

### Task 7: `values` commands in released mode

**Files:**
- Modify: `cmd/values.go`, `cmd/valuesedit.go`, `cmd/valuesprofiles.go`
- Test: `cmd/values_show_test.go`

**Interfaces:**
- Consumes: `rookSource` (Task 5); `resolveSource`, `configHome`, `releasedCharts`, `stubChartPuller` (Task 3).
- Produces: `func valuesSource(cmd *cobra.Command) (rookSource, error)`; `showBase(chart string, src rookSource) (map[string]any, error)`; `seedFor(chart string, src rookSource) ([]byte, error)`; `values` persistent flags `--rook-version` (`valuesRookVersion`) and `--config-dir` (`valuesConfigDir`).

- [ ] **Step 1: Write the failing tests**

In `cmd/values_show_test.go`, change `TestClusterBaseReadsTheChartsPools` to leave its call as is, and change `TestShowBaseUsesTheRecordedShape`'s call to `showBase(chartCluster, rookSource{charts: rookCloneWithBlockPool(t)})`. Append:

```go
func TestShowBaseOfAReleasedOperatorKeepsTheChartsImage(t *testing.T) {
	base, err := showBase(chartOperator, rookSource{released: "v1.20.7"})
	if err != nil {
		t.Fatalf("showBase: %v", err)
	}
	if _, ok := base["image"]; ok {
		t.Errorf("image = %#v, want none: a released chart pins its own", base["image"])
	}
}

// values show renders what a deploy of the cluster in scope would: for a
// released cluster, the recorded version's cached charts.
func TestValuesSourceOfAReleasedCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROOKET_NAME", "released")
	t.Setenv("ROOKET_CONFIG_DIR", "")
	stubChartPuller(t)
	cfg := t.TempDir()
	if err := writeSource("released", clusterSource{RookVersion: "v1.20.7", ConfigDir: cfg}); err != nil {
		t.Fatal(err)
	}

	src, err := valuesSource(valuesShowCmd)
	if err != nil {
		t.Fatalf("valuesSource: %v", err)
	}
	if src.released != "v1.20.7" {
		t.Errorf("released = %q, want the recorded v1.20.7", src.released)
	}
	if got := src.config.ValuesPath(chartCluster); got != filepath.Join(cfg, "values", chartCluster+".yaml") {
		t.Errorf("config ValuesPath = %q, want the recorded directory's", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `$GO test -run 'ShowBase|ValuesSource|ClusterBaseReads' ./cmd/`
Expected: FAIL — `undefined: valuesSource`, and `showBase` takes a string.

- [ ] **Step 3: Implement**

In `cmd/values.go`, add `valuesRookVersion string` and `valuesConfigDir string` to the `var (...)` block, and register them in `init` beside `--dir`:

```go
	pf.StringVar(&valuesRookVersion, "rook-version", "", "render for this released Rook version (default: the cluster's recorded version, else the rook clone)")
	pf.StringVar(&valuesConfigDir, "config-dir", "", "configuration directory laid out like .rooket/ (default: $ROOKET_CONFIG_DIR, else the cluster's recorded one, else the rook clone's .rooket)")
```

Add:

```go
// valuesSource resolves the charts and configuration home a values command
// renders for, as a deploy of the cluster in scope would. It writes no record:
// only a deploy changes what a cluster runs.
func valuesSource(cmd *cobra.Command) (rookSource, error) {
	rec, _, err := resolveSource(clusterName(""), valuesRookVersion, cmd.Flags().Changed("rook-version"),
		valuesConfigDir, cmd.Flags().Changed("config-dir"))
	if err != nil {
		return rookSource{}, err
	}
	if rec.RookVersion == "" {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return rookSource{}, err
		}
		return rookSource{charts: dir, config: configHome(rec, dir)}, nil
	}
	charts, err := releasedCharts(rec.RookVersion)
	if err != nil {
		return rookSource{}, err
	}
	rookDir := valuesDir
	if rookDir == "" {
		if wd, err := os.Getwd(); err == nil {
			rookDir = findRookRoot(wd)
		}
	}
	return rookSource{charts: charts, config: configHome(rec, rookDir), released: rec.RookVersion}, nil
}
```

`showBase(chart string, src rookSource)`: the operator case returns `values.OperatorBase(values.OperatorInput{}), nil` when `src.released != ""`, else the existing local-registry input; the default case becomes `clusterBase(src.charts, shape.Workers, nil)`.

`valuesShowCmd.RunE` binds `src, err := valuesSource(cmd)` in place of `resolveRookDir`, calls `activeProfileNames(src.config, ...)`, `showBase(chart, src)`, and `composeChart(chart, base, src.config, active, deployValueFiles)`.

In `cmd/valuesedit.go`, `valuesEditCmd.RunE` binds `src, err := valuesSource(cmd)`, and before the loop refuses a missing home:

```go
		if src.config.ValuesPath(chartOperator) == "" {
			return fmt.Errorf("no configuration to edit: pass --config-dir, set $ROOKET_CONFIG_DIR, or run inside a rook clone")
		}
		if err := src.config.Ensure(); err != nil {
			return err
		}
```

then uses `src.config.ValuesPath(chart)` and `seedFor(chart, src)`. `seedFor(chart string, src rookSource)` calls `showBase(chart, src)`.

In `cmd/valuesprofiles.go`, `valuesProfilesCmd.RunE` binds `src, err := valuesSource(cmd)` in place of `resolveRookDir`, and calls `activeProfileNames(src.config, ...)`. Drop the now-unused `clone` import there.

- [ ] **Step 4: Run the tests to verify they pass**

Run (sandbox disabled): `$GO test -race ./cmd/`
Expected: PASS, including `TestValuesShowInheritsWithOnlyFlag`.

- [ ] **Step 5: Commit**

```bash
git add cmd/values.go cmd/valuesedit.go cmd/valuesprofiles.go cmd/values_show_test.go
git commit -m "feat(values): render and edit values for a released cluster"
```

---

### Task 8: Prune keeps released clusters that nothing can show abandoned

**Files:**
- Modify: `cmd/cloneprovenance.go`, `cmd/prune.go`
- Test: `cmd/cloneprovenance_test.go`

**Interfaces:**
- Consumes: `readSourceAt`, `clusterSource` (Task 3).
- Produces: `func ownerGone(stateDir string) bool`, `func parkedBecause(stateDir string) string`; `prunePlan` calls `ownerGone` in place of `cloneGone`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/cloneprovenance_test.go`:

```go
func TestOwnerGone(t *testing.T) {
	record := func(t *testing.T, s clusterSource) string {
		t.Helper()
		dir := t.TempDir()
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sourceFile), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("released, its configuration directory still there", func(t *testing.T) {
		if ownerGone(record(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: t.TempDir()})) {
			t.Error("ownerGone = true, want parked while its configuration directory exists")
		}
	})
	t.Run("released, its configuration directory removed", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "removed")
		if !ownerGone(record(t, clusterSource{RookVersion: "v1.20.7", ConfigDir: gone})) {
			t.Error("ownerGone = false, want abandoned once its only owner is gone")
		}
	})
	// Nothing on disk will ever disappear to say such a cluster was abandoned,
	// and its record proves it is not a leftover from before provenance.
	t.Run("released with no owner at all", func(t *testing.T) {
		if ownerGone(record(t, clusterSource{RookVersion: "v1.20.7"})) {
			t.Error("ownerGone = true, want parked")
		}
	})
	t.Run("a clone-built cluster is judged by its clone as before", func(t *testing.T) {
		if !ownerGone(t.TempDir()) {
			t.Error("ownerGone of an unrecorded state dir = false, want abandoned, as cloneGone says")
		}
	})
}
```

Add `"encoding/json"` to the file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `$GO test -run OwnerGone ./cmd/`
Expected: FAIL — `undefined: ownerGone`.

- [ ] **Step 3: Implement**

Append to `cmd/cloneprovenance.go`:

```go
// ownerGone reports whether a cluster's state dir is abandoned rather than
// parked. A cluster deployed from a released Rook is owned by its recorded
// configuration directory and by the clone it was created in, when it has
// them: it is parked while either exists, and always when it has neither,
// since nothing on disk will then disappear to say it was abandoned and its
// record shows it is no leftover from before provenance was recorded. Any
// other cluster is judged by its clone alone (see cloneGone).
func ownerGone(stateDir string) bool {
	src, ok := readSourceAt(stateDir)
	if !ok || src.RookVersion == "" {
		return cloneGone(stateDir)
	}
	owned := false
	for _, owner := range []string{cloneDir(stateDir), src.ConfigDir} {
		if owner == "" {
			continue
		}
		owned = true
		if _, err := os.Stat(owner); err == nil {
			return false
		}
	}
	return owned
}

// parkedBecause says why prune kept a parked cluster.
func parkedBecause(stateDir string) string {
	src, ok := readSourceAt(stateDir)
	if !ok || src.RookVersion == "" {
		return fmt.Sprintf("its clone %s still exists", cloneDir(stateDir))
	}
	for _, owner := range []string{cloneDir(stateDir), src.ConfigDir} {
		if owner == "" {
			continue
		}
		if _, err := os.Stat(owner); err == nil {
			return fmt.Sprintf("%s, which deployed it, still exists", owner)
		}
	}
	return fmt.Sprintf("it deploys released Rook %s and names no clone or configuration directory", src.RookVersion)
}
```

Add `"fmt"` to its imports. In `cmd/prune.go`, `prunePlan` calls `ownerGone` in place of `cloneGone`, and the parked message becomes:

```go
			run.Printf("keeping %s: %s, so it is parked by 'rooket down', not abandoned "+
				"(remove it with 'rooket down --delete-disks', or sweep it here with --include-parked)\n",
				filepath.Join(root, p), parkedBecause(filepath.Join(root, p)))
```

Update `prunePlan`'s doc comment: "a cluster whose rook clone still exists is parked" becomes "a cluster whose owner — its rook clone, or for a released cluster its configuration directory — still exists is parked", and "see clonePathFile for how the clone is known" becomes "see ownerGone".

- [ ] **Step 4: Run the tests to verify they pass**

Run (sandbox disabled): `$GO test -race ./cmd/`
Expected: PASS, including every existing prune test.

- [ ] **Step 5: Commit**

```bash
git add cmd/cloneprovenance.go cmd/cloneprovenance_test.go cmd/prune.go
git commit -m "feat(prune): keep released clusters that nothing can show abandoned"
```

---

### Task 9: A clone-free e2e job

**Files:**
- Modify: `test/e2e/suite_test.go`, `test/e2e/updown_test.go`, `test/e2e/krbd_test.go`, `test/e2e/profiles_test.go`
- Modify: `.github/workflows/integration.yml`

**Interfaces:**
- Produces: e2e env `ROOKET_ROOK_VERSION`; helpers `sourceArgs() []string` and `needsClone()`.

- [ ] **Step 1: Add the suite helpers**

In `test/e2e/suite_test.go`, add to the `var (...)` block:

```go
	rookVersion = os.Getenv("ROOKET_ROOK_VERSION")
```

and below `numWorkers`:

```go
// sourceArgs selects what a spec deploys: the rook checkout at ROOK_DIR, or,
// with ROOKET_ROOK_VERSION set, that released version and no checkout at all.
func sourceArgs() []string {
	if rookVersion != "" {
		return []string{"--rook-version", rookVersion}
	}
	return []string{"--dir", rookDir}
}

// needsClone skips a spec that builds rook or edits its clone, which a run
// against a released version has neither of.
func needsClone() {
	if rookVersion != "" {
		Skip("needs a rook checkout; this run deploys released Rook " + rookVersion)
	}
}
```

In `BeforeSuite`, the skip becomes:

```go
	if rookDir == "" && rookVersion == "" {
		Skip("neither ROOK_DIR nor ROOKET_ROOK_VERSION set; skipping rooket e2e (needs a Rook source tree or version, and iSCSI block devices)")
	}
```

and the `GinkgoWriter.Printf` adds `rookVersion` (`" version=%s"`).

- [ ] **Step 2: Route the specs through the helpers**

In `test/e2e/updown_test.go`, "brings up a healthy rook-ceph cluster that settles":

```go
		args := append([]string{"up"}, sourceArgs()...)
		args = append(args, "--workers", workers, "--name", clusterName)
```

replacing `args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName}`.

"stays healthy when up is re-run (idempotent)" — a released `up` skips its build anyway, so `--skip-build` stays:

```go
		args := append([]string{"up", "--skip-build"}, sourceArgs()...)
		args = append(args, "--workers", workers, "--name", clusterName)
```

In `test/e2e/krbd_test.go`'s `BeforeAll`, the same two lines as the first spec replace its `args := []string{"up", "--dir", rookDir, ...}`.

Skip what needs a checkout. In `updown_test.go`, the first statement of "auto-skips the build on an unchanged tree and rebuilds on change" — which edits and rebuilds the clone — becomes:

```go
		needsClone()
```

and its `up` keeps `--dir`. In `profiles_test.go`, which writes into the clone's `.rooket/templates/`, the first statement of the container's `BeforeAll` becomes `needsClone()`, skipping the whole ordered container.

- [ ] **Step 3: Vet the suite**

Run: `$GO vet -tags e2e ./test/e2e/`
Expected: no output.

- [ ] **Step 4: Add the released job to the CI matrix**

In `.github/workflows/integration.yml`, the `e2e` job's matrix becomes:

```yaml
      matrix:
        include:
          - rook-ref: release-1.19
          - rook-ref: release-1.20
          - rook-ref: master
          # A released Rook with no checkout and no build, on one worker: the
          # consumer's path, jhoblitt/rooket#58. Bump by hand to the newest
          # release; it is a pin, not a range.
          - rook-version: v1.20.7
            workers: "1"
```

The job's `name` becomes `rooket up/down e2e (docker, ${{ matrix.rook-ref || format('released {0}', matrix.rook-version) }})`. In its `env`, `WORKERS` becomes `${{ github.event.inputs.workers || matrix.workers || '3' }}`, and add:

```yaml
      ROOKET_ROOK_VERSION: ${{ matrix.rook-version }}
```

Add `if: matrix.rook-version == ''` to the "Checkout rook", "Resolve rook's Go version", and "Set up Go for rook" steps. In "Run e2e suite", `ROOK_DIR` becomes conditional:

```yaml
        run: |
          rook_dir=""
          if [ -z "$ROOKET_ROOK_VERSION" ]; then rook_dir="${{ github.workspace }}/rook"; fi
          ROOK_DIR="$rook_dir" \
          ROOKET_BIN="${{ github.workspace }}/rooket" \
          ROOKET_WORKERS="$WORKERS" \
          ROOKET_SKIP_BLOCK=true \
            ./e2e.test -test.v -test.timeout 100m \
              -ginkgo.v -ginkgo.poll-progress-after=10m -ginkgo.poll-progress-interval=5m
```

Keep the job's existing comments; extend the matrix comment to say the released entry covers the no-checkout path.

- [ ] **Step 5: Lint the workflow**

Run: `actionlint .github/workflows/integration.yml`
Expected: no output. actionlint runs shellcheck on the `run:` blocks; fix anything it reports.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/ .github/workflows/integration.yml
git commit -m "ci: run the e2e suite against a released Rook with no checkout"
```

---

### Task 10: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Write the README changes**

- Prerequisites: the last bullet becomes "A Go toolchain (to build rooket) and, unless you deploy a released Rook (see below), a rook source checkout."
- Add a section after "Quick start", titled "Released Rook":

````markdown
## Released Rook

To use rooket as a test harness for something that consumes Rook rather than
develops it, deploy a published release. No rook checkout is needed and
nothing is built:

```console
$ rooket up --name rgw-test --rook-version v1.20.7 --workers 1
```

The charts are pulled from `https://charts.rook.io/release` once per version
into `~/.cache/rooket/charts/` and reused by every cluster after that; the
images are the ones the release pins. The version is an exact tag, never a
range, and is recorded with the cluster, so a later `rooket deploy` or `rooket
values show` needs no flag. Passing a different `--rook-version` to `up` or
`deploy` upgrades the cluster to it.

Outside a rook clone a cluster needs a name, from `--name` or `$ROOKET_NAME`.
Sticky configuration lives in any directory laid out like `.rooket/`
(`values/<chart>.yaml`, `templates/`, `config.yaml`), named with
`--config-dir` or `$ROOKET_CONFIG_DIR` and recorded with the cluster. rooket
never writes a `.gitignore` into it, so it can live in your own repository.
````

- "Clusters and state": add a bullet "the Rook version it deploys, when released, and the configuration directory it was given, so later commands need neither flag."
- The Commands table's `rooket up` row mentions `--rook-version`.

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: deploying a released Rook without a clone"
```

---

### Task 11: Verify end to end, review, open the PR

- [ ] **Step 1: The full local gate**

Run: `gofmt -l cmd internal test main.go`, `$GO vet ./...`, `$GO vet -tags e2e ./test/e2e/`, and (sandbox disabled) `$GO test -race ./...`.
Expected: no gofmt output, no vet output, every package `ok`.

- [ ] **Step 2: A live released cluster from outside any clone**

From a scratch directory that is not inside a rook clone, with a built binary and a throwaway configuration directory holding `config.yaml` with `profiles: [rgw]`:

```bash
rooket up --name rel-probe --rook-version v1.20.7 --workers 1 --config-dir "$CFG"
ROOKET_NAME=rel-probe rooket k -n rook-ceph get cephcluster,cephobjectstore
ROOKET_NAME=rel-probe rooket k -n rook-ceph get deploy rook-ceph-operator -o jsonpath='{.spec.template.spec.containers[0].image}'
ROOKET_NAME=rel-probe rooket values show cluster
rooket down --name rel-probe --delete-disks
```

Expected: `up` prints the skipped-build banner; the CephCluster and CephObjectStore reach `Ready`; the operator image is `docker.io/rook/ceph:v1.20.7`; `values show` needs no `--rook-version`; `ls "$CFG"` shows no `.gitignore`; `down` tears it down from the record alone.

- [ ] **Step 3: Pre-PR review**

Run `go-conventions:go-review` over the branch against `origin/main`, fix what it finds, and fold each fix into the commit it belongs to.

- [ ] **Step 4: Open the PR**

Push the branch as `released-rook-mode` and open a draft PR assigned to its author, per github-conventions, with a description of at most 100 words citing #58 item 1, stating the live verification and anything left unverified. Start one background CI watcher in the same turn.
