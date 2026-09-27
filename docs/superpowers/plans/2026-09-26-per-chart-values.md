# Per-chart values from a profile directory — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a caller give rooket values for each chart separately, from a profile directory passed by path, and remove the `-f`/`--set` flags that sent every value to every chart.

**Architecture:** Profiles already route values by filename (`values/<chart>.yaml` → one chart). Task 1 adds directory loading (`profiles.LoadDir`). Task 2 deletes `-f`/`--set` and their plumbing, before anything is built on top of them, so no commit tests or documents a flag that a later commit removes. Task 3 makes `--with`/`--with-only` accept a path and makes loading stricter in `cmd`: a values file named for no chart, and two different profiles sharing one name, become errors. Task 4 lists active path profiles. Task 5 proves the routing end to end.

**Tech Stack:** Go 1.26 (`go.mod`), cobra, `go.yaml.in/yaml/v3`. Unit tests use the standard `testing` package, the consistent idiom of `cmd/` and `internal/profiles/`, which outranks the canon's Ginkgo rule there. The e2e suite (`test/e2e/`, build tag `e2e`) is Ginkgo v2 + Gomega.

**Spec:** `docs/superpowers/specs/2026-09-26-per-chart-values-design.md`

## Global Constraints

- The three chart names, exactly: `rook-ceph`, `rook-ceph-cluster`, `ceph-csi-drivers` (`chartOperator`, `chartCluster`, `chartCSI` in `cmd/compose.go`).
- A `--with`/`--with-only` value is a path when it contains `/`, or is exactly `.` or `..`. Every other value is a profile name.
- A relative path resolves against the working directory rooket was invoked from, never against `--dir`.
- A path profile's name is the basename of its absolute path. Its provenance label is the path exactly as given.
- Paths are accepted only on `--with`/`--with-only`, never in `.rooket/config.yaml`'s `profiles:`.
- Precedence after this change, lowest first: chart `values.yaml` → rooket base → `.rooket/values/<chart>.yaml` → active profiles in selection order.
- Commits: Conventional Commits. The removal commit is `feat!:` with a `BREAKING CHANGE:` footer. Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Gates before each commit, matching CI (`.github/workflows/integration.yml`): `gofmt -l` prints nothing for the changed files, `go vet github.com/jhoblitt/rooket/...`, and `go test github.com/jhoblitt/rooket/<pkg>` for the touched packages. Use full import paths: this harness's worktree guard rejects `./pkg/` arguments.
- Sandbox note: `TestCheckTrustedBinary` and `TestCheckAncestorDirsAcceptsRealTrustedBinaries` in `cmd` fail inside the Bash sandbox, where root-owned files appear as uid 65534. They pass unsandboxed and have nothing to do with this work. Run the `cmd` package with `-run` filters, or run it unsandboxed, and don't treat those two as regressions.

## Review Focus

1. **Spellings of one directory.** `./mytest`, `mytest/`, `./mytest/` and `.` (from inside it) must all name the profile `mytest`, and two spellings of the same directory must count as one source. Pinned in Task 1 (`TestLoadDirRelativeSpellings`, `TestSameSource`) and Task 3 (`the same directory twice is one source`).
2. **A relative path while `--dir` points elsewhere.** `--dir` sets the rook clone, not the working directory. A relative profile path must resolve against the working directory. Pinned in Task 1 (`TestLoadDirRelativeSpellings` chdirs, then resolves).
3. **A path that is not a profile.** A missing path, a regular file, a directory without `profile.yaml`, or a directory named `local` must each fail with an error that names the path. Pinned in Task 1 (`TestLoadDirErrors`).
4. **Two values files for one chart.** `values/rook-ceph.yaml` and `values/rook-ceph.yml` in one profile must be an error, not a silent overwrite. A lone `.yml` must still be accepted. Pinned in Task 1 (`TestLoadRejectsTwoValuesFilesForOneChart`, `TestLoadAcceptsYmlValuesFile`).
5. **A path profile whose basename matches a built-in.** It must work alone and fail only when the built-in is active too. Every built-in must pass the new values-file check. Pinned in Task 3 (`TestLoadProfilesDuplicateNames`, `TestBuiltInProfilesPassValueChartCheck`).

---

### Task 1: Load a profile from a directory path

**Files:**
- Modify: `internal/profiles/profiles.go` (`Profile` struct; new `LoadDir`, `Label`, `SameSource`; duplicate-stem check in `fromFS`)
- Test: `internal/profiles/profiles_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `func LoadDir(path string) (Profile, error)`
  - `Profile.Path string` — the directory exactly as the caller gave it; empty for a profile loaded by name.
  - `func (p Profile) Label() string` — `p.Path` if set, else `p.Name`.
  - `func (p Profile) SameSource(q Profile) bool` — for two profiles of the same name: true when both were loaded by name, or both from the same absolute directory.

- [ ] **Step 1: Write the failing tests**

Append to `internal/profiles/profiles_test.go` (it already imports `os`, `path/filepath`, `strings`, `testing`, and defines `writeProfile(t, dir, name, desc, valuesChart, valuesBody)`, which writes `profile.yaml`, an optional `values/<valuesChart>.yaml`, and `templates/10-thing.yaml`):

```go
func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "mytest", "path profile", "rook-ceph-cluster", "toolbox:\n  enabled: false\n")
	path := filepath.Join(root, "mytest")

	p, err := LoadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mytest" || p.Path != path || p.Label() != path {
		t.Errorf("Name=%q Path=%q Label=%q, want mytest and %q twice", p.Name, p.Path, p.Label(), path)
	}
	if p.BuiltIn || p.Description != "path profile" {
		t.Errorf("got %+v", p)
	}
	tb := p.Values["rook-ceph-cluster"]["toolbox"].(map[string]any)
	if tb["enabled"] != false {
		t.Errorf("values = %#v", p.Values)
	}
	if string(p.Templates["10-thing.yaml"]) != "kind: ConfigMap\n" {
		t.Errorf("templates = %#v", p.Templates)
	}
}

func TestLoadDirRelativeSpellings(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "mytest", "d", "", "")
	t.Chdir(root)

	for _, given := range []string{"./mytest", "mytest/", "./mytest/"} {
		p, err := LoadDir(given)
		if err != nil {
			t.Fatalf("%s: %v", given, err)
		}
		if p.Name != "mytest" || p.Path != given {
			t.Errorf("%s: Name=%q Path=%q", given, p.Name, p.Path)
		}
	}

	t.Chdir(filepath.Join(root, "mytest"))
	p, err := LoadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mytest" {
		t.Errorf(`LoadDir(".") Name = %q, want mytest`, p.Name)
	}
}

func TestLoadDirErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.yaml")
	if err := os.WriteFile(file, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noMeta := filepath.Join(root, "nometa")
	if err := os.MkdirAll(filepath.Join(noMeta, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, root, Reserved, "reserved", "", "")

	for name, tc := range map[string]struct{ path, want string }{
		"missing":         {filepath.Join(root, "nope"), "no such file"},
		"not a directory": {file, "not a directory"},
		"no profile.yaml": {noMeta, "profile.yaml"},
		"reserved name":   {filepath.Join(root, Reserved), "reserved"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadDir(tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), filepath.Base(tc.path)) {
				t.Errorf("err = %v, want it to mention %q and the path", err, tc.want)
			}
		})
	}
}

func TestSameSource(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "rbd", "shadow", "", "")
	t.Chdir(root)

	rel, err := LoadDir("./rbd")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := LoadDir(filepath.Join(root, "rbd"))
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}

	if !rel.SameSource(abs) {
		t.Error("two spellings of one directory must be the same source")
	}
	if rel.SameSource(builtin) {
		t.Error("a path profile and the built-in of the same name must be different sources")
	}
	if !builtin.SameSource(again) {
		t.Error("the same name loaded twice must be the same source")
	}
}

func TestLoadRejectsTwoValuesFilesForOneChart(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "dup", "d", "rook-ceph", "a: 1\n")
	if err := os.WriteFile(filepath.Join(dir, "dup", "values", "rook-ceph.yml"), []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir, "dup")
	if err == nil || !strings.Contains(err.Error(), "rook-ceph.yaml") || !strings.Contains(err.Error(), "rook-ceph.yml") {
		t.Errorf("err = %v, want it to name both files", err)
	}
}

func TestLoadAcceptsYmlValuesFile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "yml", "d", "", "")
	if err := os.WriteFile(filepath.Join(dir, "yml", "values", "rook-ceph-cluster.yml"),
		[]byte("toolbox:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(dir, "yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Values["rook-ceph-cluster"]; !ok {
		t.Errorf("values = %#v, want a rook-ceph-cluster entry from the .yml file", p.Values)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test github.com/jhoblitt/rooket/internal/profiles`
Expected: build failure: `undefined: LoadDir`, `p.Path undefined`, `p.Label undefined`, `rel.SameSource undefined`.

- [ ] **Step 3: Implement**

In `internal/profiles/profiles.go`, replace the `Profile` struct:

```go
type Profile struct {
	Name        string
	Description string
	BuiltIn     bool
	// Path is the directory exactly as the caller gave it, for a profile
	// loaded by LoadDir; empty for one loaded by name.
	Path      string
	Values    map[string]map[string]any
	Templates map[string][]byte

	// dir is Path made absolute, so two spellings of one directory compare
	// equal in SameSource.
	dir string
}

// Label is how the user selected the profile: its path for one loaded by
// LoadDir, else its name.
func (p Profile) Label() string {
	if p.Path != "" {
		return p.Path
	}
	return p.Name
}

// SameSource reports whether two profiles of the same name were loaded from
// the same place. Load always resolves a name to the same profile, so two
// loaded by name match; a path profile matches only the same directory.
func (p Profile) SameSource(q Profile) bool { return p.dir == q.dir }
```

Add `LoadDir` directly after `Load`:

```go
// LoadDir loads the profile in the directory at path, naming it for the
// directory's basename. A relative path resolves against the working
// directory.
func LoadDir(path string) (Profile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	if !fi.IsDir() {
		return Profile{}, fmt.Errorf("profile %s: not a directory", path)
	}
	name := filepath.Base(abs)
	if name == Reserved {
		return Profile{}, fmt.Errorf("profile %s: directory name %q is reserved for the clone's own templates", path, Reserved)
	}
	p, err := fromFS(os.DirFS(abs), name, false)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	p.Path, p.dir = path, abs
	return p, nil
}
```

In `fromFS`, reject two files for one chart. Replace the `values/` loop body's tail, from `var v map[string]any` through the `p.Values[...] = v` assignment, and declare `seen` before the loop:

```go
	valueFiles, err := fs.ReadDir(fsys, "values")
	if err == nil {
		seen := map[string]string{}
		for _, e := range valueFiles {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			chart := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")
			if prev, ok := seen[chart]; ok {
				return Profile{}, fmt.Errorf("profile %q: values/%s and values/%s both target %s", name, prev, e.Name(), chart)
			}
			seen[chart] = e.Name()
			raw, err := fs.ReadFile(fsys, path.Join("values", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			var v map[string]any
			if err := yaml.Unmarshal(raw, &v); err != nil {
				return Profile{}, fmt.Errorf("profile %q: parse %s: %w", name, e.Name(), err)
			}
			p.Values[chart] = v
		}
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/profiles && go vet github.com/jhoblitt/rooket/internal/profiles && go test github.com/jhoblitt/rooket/internal/profiles`
Expected: no gofmt output, vet clean, `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/profiles/profiles.go internal/profiles/profiles_test.go
git commit -F - <<'EOF'
feat(profiles): load a profile from a directory path

LoadDir reads a profile from any directory, naming it for the directory's
basename and keeping the path as given for provenance. Two values files
for one chart (rook-ceph.yaml and rook-ceph.yml) are now an error instead
of the later silently replacing the earlier.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 2: Remove `-f` and `--set`

This comes before the selection work (Task 3) so that no commit in the series tests or documents a flag that a later commit deletes.

**Files:**
- Modify: `cmd/compose.go` (`composeChart` loses `extraFiles`)
- Modify: `cmd/deploy.go` (vars, flags, `writeComposed`, three Helm call sites, delete `helmValueArgs`)
- Modify: `cmd/up.go` (vars, `applyUpValueFlags`, flags)
- Modify: `cmd/values.go` (`Long`, flags, delete `printSetsNote`, imports)
- Modify: `README.md` (layer list)
- Test: `cmd/compose_test.go`, `cmd/deploy_test.go`, `cmd/up_values_test.go`, `cmd/values_show_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `func composeChart(chart string, base map[string]any, cloneDir clone.Dir, active []profiles.Profile) (composed, error)`, with no `extraFiles` parameter.

- [ ] **Step 1: Write the failing test**

Append to `cmd/up_values_test.go`:

```go
// TestChartAgnosticValueFlagsRemoved pins the removal of -f/--values and
// --set: both reached every chart, so a key meant for one chart landed in
// all of them. Per-chart values come from a profile directory instead.
func TestChartAgnosticValueFlagsRemoved(t *testing.T) {
	for name, c := range map[string]*cobra.Command{"up": upCmd, "deploy": deployCmd, "values": valuesCmd} {
		// Command.Flag checks local and persistent flags, climbing to parents.
		for _, flag := range []string{"values", "set"} {
			if c.Flag(flag) != nil {
				t.Errorf("%s still defines --%s", name, flag)
			}
		}
		if c.Flags().ShorthandLookup("f") != nil || c.PersistentFlags().ShorthandLookup("f") != nil {
			t.Errorf("%s still defines -f", name)
		}
	}
}
```

Replace the whole of `TestUpForwardsValueFlags` in the same file (it sets `upValueFiles`/`upSets`, which this task deletes), and change the file's imports to:

```go
import (
	"testing"

	"github.com/spf13/cobra"
)
```

cobra is already a direct requirement in `go.mod`. `github.com/spf13/pflag` is only indirect, so the test deliberately never names a pflag type.

```go
func TestUpForwardsValueFlags(t *testing.T) {
	t.Cleanup(func() {
		upWith, upWithOnly = nil, nil
		deployWith, deployWithOnly = nil, nil
		deployWithOnlySet = false
	})

	upWith = []string{"rgw"}
	upWithOnly = []string{"rbd"}

	applyUpValueFlags(true)

	if len(deployWith) != 1 || deployWith[0] != "rgw" {
		t.Errorf("deployWith = %#v", deployWith)
	}
	if len(deployWithOnly) != 1 || deployWithOnly[0] != "rbd" {
		t.Errorf("deployWithOnly = %#v", deployWithOnly)
	}
	if !deployWithOnlySet {
		t.Error("deployWithOnlySet not propagated")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run 'TestChartAgnosticValueFlagsRemoved' github.com/jhoblitt/rooket/cmd`
Expected: FAIL: `up still defines --values`, `up still defines --set`, `up still defines -f`, and the same for deploy and values.

- [ ] **Step 3: Implement the removal**

`cmd/compose.go`: replace `composeChart` and its doc comment:

```go
// composeChart stacks every layer for one chart, lowest first: rooket's
// generated base, the clone's sticky file, then each active profile in
// selection order.
func composeChart(chart string, base map[string]any, cloneDir clone.Dir,
	active []profiles.Profile) (composed, error) {

	layers := []values.Layer{{Name: "rooket base", Values: base}}

	sticky, err := values.LoadFile(cloneDir.ValuesPath(chart))
	if err != nil {
		return composed{}, err
	}
	if sticky != nil {
		layers = append(layers, values.Layer{Name: ".rooket/values", Values: sticky})
	}

	for _, p := range active {
		if v, ok := p.Values[chart]; ok {
			layers = append(layers, values.Layer{Name: "profile:" + p.Name, Values: v})
		}
	}

	merged, prov := values.Merge(layers)
	return composed{Merged: merged, Provenance: prov}, nil
}
```

`cmd/deploy.go`:
- Delete `deployValueFiles   []string` and `deploySets         []string` from the `var (...)` block.
- Delete the `pf.StringArrayVarP(&deployValueFiles, "values", ...)` and `pf.StringArrayVar(&deploySets, "set", ...)` lines in `init`.
- In `writeComposed`: `c, err := composeChart(chart, base, cloneDir, active)`.
- Delete `helmValueArgs` and its doc comment.
- The operator install becomes:

```go
	args := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployOperatorName, chartPath,
		"-f", valuesPath,
	}
```

- The ceph-csi-drivers install becomes:

```go
	csiArgs := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install",
		"ceph-csi-drivers", "ceph-csi-drivers",
		"--repo", "https://ceph.github.io/ceph-csi-operator",
		"--version", version,
		"-f", valuesPath,
	}
```

- The cluster install becomes:

```go
	clusterArgs := []string{
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName, chartPath,
		"-f", valuesPath,
	}
```

`cmd/up.go`:
- Delete `upValueFiles      []string` and `upSets            []string` from the `var (...)` block.
- Delete `deployValueFiles = upValueFiles` and `deploySets = upSets` from `applyUpValueFlags`.
- Delete the `upCmd.Flags().StringArrayVarP(&upValueFiles, "values", ...)` and `upCmd.Flags().StringArrayVar(&upSets, "set", ...)` lines.

`cmd/values.go`:
- `valuesCmd.Long` becomes:

```go
	Long: `values manages the layered chart values rooket supplies to the rook charts.

Layers, lowest first: rooket's generated base, the clone's .rooket/values/,
then each active profile in selection order.
`,
```

- Delete the `printSetsNote(os.Stderr, deploySets)` call and its preceding blank line.
- Delete `printSetsNote` and its doc comment.
- In the `for i, chart := range charts` loop: `c, err := composeChart(chart, showBase(chart), cloneDir, active)`.
- Delete the `pf.StringArrayVarP(&deployValueFiles, "values", ...)` and `pf.StringArrayVar(&deploySets, "set", ...)` lines.
- Remove the now-unused `"io"` and `"os"` imports.

Tests:
- `cmd/deploy_test.go`: delete `TestHelmValueArgs` and the `"reflect"` import, which nothing else there uses.
- `cmd/values_show_test.go`: delete `TestPrintSetsNote`. In `TestValuesShowInheritsWithOnlyFlag`'s cleanup, replace `deployWith, deployWithOnly, deployValueFiles, deploySets = nil, nil, nil, nil` with `deployWith, deployWithOnly = nil, nil`.
- `cmd/compose_test.go`: delete `TestComposeChartMissingValuesFileErrors`. In `TestComposeChartProfileOrder`, drop the trailing `nil,` argument. Replace `TestComposeChartLayerOrder` with:

```go
func TestComposeChartLayerOrder(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ValuesPath(chartCluster),
		[]byte("a: from-clone\nb: from-clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := composeChart(chartCluster,
		map[string]any{"a": "from-base", "b": "from-base", "d": "from-base"},
		d,
		[]profiles.Profile{{
			Name:   "p",
			Values: map[string]map[string]any{chartCluster: {"b": "from-profile"}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a": "from-clone",
		"b": "from-profile",
		"d": "from-base",
	}
	for k, v := range want {
		if got.Merged[k] != v {
			t.Errorf("%s = %v, want %v", k, got.Merged[k], v)
		}
	}
	if got.Provenance["b"] != "profile:p" {
		t.Errorf("provenance[b] = %q", got.Provenance["b"])
	}
}
```

  Remove the `"strings"` import from `cmd/compose_test.go`: only the deleted `TestComposeChartMissingValuesFileErrors` used it. `path/filepath` stays, because `TestComposedWrite` uses it.

`README.md`, section "Chart values and profiles": in the numbered layer list, delete item `5. \`-f\` files, then \`--set\``. Items 1–4 stay as they are.

- [ ] **Step 4: Run the gates**

Run: `gofmt -l cmd && go vet github.com/jhoblitt/rooket/... && go test -run 'TestChartAgnosticValueFlagsRemoved|TestUpForwardsValueFlags|TestComposeChart|TestValuesShow|TestRenderShow|TestWriteComposed' github.com/jhoblitt/rooket/cmd`
Expected: no gofmt output, vet clean, `ok`. Then `grep -rn -E 'deployValueFiles|deploySets|upValueFiles|upSets|helmValueArgs|printSetsNote' cmd/` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add cmd/compose.go cmd/deploy.go cmd/up.go cmd/values.go README.md \
  cmd/compose_test.go cmd/deploy_test.go cmd/up_values_test.go cmd/values_show_test.go
git commit -F - <<'EOF'
feat!: remove -f and --set, which applied to every chart

Every -f file and every --set reached rook-ceph, rook-ceph-cluster and
ceph-csi-drivers alike, so a key meant for one chart was safe only while
no other chart defined it. Per-chart values belong in a profile's
values/<chart>.yaml.

BREAKING CHANGE: up, deploy and values no longer accept -f/--values or
--set. Move each key into values/<chart>.yaml of a profile directory,
split by the chart it belongs to, and select it with --with or --with-only.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 3: Select profile directories by path, and reject what composition would drop

**Files:**
- Modify: `cmd/compose.go` (new `allCharts`, `isProfilePath`, `checkValueCharts`; `activeProfileNames` rejects sticky paths; `loadProfiles` loads paths and checks names; provenance uses `Label`)
- Modify: `cmd/values.go` (`values show` iterates `allCharts`; `--with`/`--with-only` help)
- Modify: `cmd/deploy.go`, `cmd/up.go` (`--with`/`--with-only` help)
- Modify: `README.md` (new subsection)
- Test: `cmd/compose_test.go`

**Interfaces:**
- Consumes (Task 1): `profiles.LoadDir(path string) (Profile, error)`, `Profile.Path`, `Profile.Label()`, `Profile.SameSource(Profile) bool`. Consumes (Task 2): `composeChart(chart, base, cloneDir, active)`.
- Produces:
  - `var allCharts = []string{chartOperator, chartCluster, chartCSI}`
  - `func isProfilePath(s string) bool`
  - `loadProfiles(names []string) ([]profiles.Profile, error)`, same signature with stricter behavior

- [ ] **Step 1: Write the failing tests**

Add `"strings"` back to `cmd/compose_test.go`'s imports (Task 2 removed it), then append:

```go
// writePathProfile creates a profile directory called name under root, with
// one values file per entry of vals (file name → body), and returns its path.
func writePathProfile(t *testing.T, root, name string, vals map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte("description: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for file, body := range vals {
		if err := os.WriteFile(filepath.Join(dir, "values", file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestIsProfilePath(t *testing.T) {
	for in, want := range map[string]bool{
		"rbd":           false,
		"my.profile":    false,
		"./mytest":      true,
		"/srv/t/mytest": true,
		"tests/mytest":  true,
		".":             true,
		"..":            true,
	} {
		if got := isProfilePath(in); got != want {
			t.Errorf("isProfilePath(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPathProfileReachesOnlyItsCharts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := writePathProfile(t, t.TempDir(), "mytest", map[string]string{
		"rook-ceph.yaml":         "marker: operator\n",
		"rook-ceph-cluster.yaml": "marker: cluster\n",
	})
	active, err := loadProfiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	d := clone.Open(t.TempDir())
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}

	for chart, want := range map[string]string{chartOperator: "operator", chartCluster: "cluster", chartCSI: ""} {
		c, err := composeChart(chart, map[string]any{}, d, active)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := c.Merged["marker"].(string)
		if got != want {
			t.Errorf("%s: marker = %q, want %q", chart, got, want)
		}
		if want != "" && c.Provenance["marker"] != "profile:"+path {
			t.Errorf("%s: provenance = %q, want profile:%s", chart, c.Provenance["marker"], path)
		}
	}
}

func TestLoadProfilesRejectsMisnamedValuesFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := writePathProfile(t, t.TempDir(), "mytest", map[string]string{"cluster.yaml": "a: 1\n"})

	_, err := loadProfiles([]string{path})
	if err == nil || !strings.Contains(err.Error(), "values/cluster.*") || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name values/cluster.* and %s", err, path)
	}
}

func TestBuiltInProfilesPassValueChartCheck(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := loadProfiles([]string{"rbd", "rgw", "nfs"}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProfilesDuplicateNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	shadow := writePathProfile(t, root, "rbd", nil)
	a := writePathProfile(t, filepath.Join(root, "a"), "mytest", nil)
	b := writePathProfile(t, filepath.Join(root, "b"), "mytest", nil)

	t.Run("a path profile alongside the built-in of the same name", func(t *testing.T) {
		_, err := loadProfiles([]string{"rbd", shadow})
		if err == nil || !strings.Contains(err.Error(), shadow) {
			t.Errorf("err = %v, want it to name %s", err, shadow)
		}
	})

	t.Run("two directories with one basename", func(t *testing.T) {
		_, err := loadProfiles([]string{a, b})
		if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
			t.Errorf("err = %v, want it to name %s and %s", err, a, b)
		}
	})

	t.Run("the same directory twice is one source", func(t *testing.T) {
		t.Chdir(filepath.Join(root, "a"))
		if _, err := loadProfiles([]string{a, "./mytest"}); err != nil {
			t.Error(err)
		}
	})

	t.Run("a path profile may reuse a built-in's name on its own", func(t *testing.T) {
		got, err := loadProfiles([]string{shadow})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Name != "rbd" || got[0].Path != shadow || got[0].BuiltIn {
			t.Errorf("got %+v", got[0])
		}
	})
}

func TestActiveProfileNamesRejectsStickyPath(t *testing.T) {
	d := clone.Open(t.TempDir())
	if err := d.SetProfiles([]string{"rbd", "./mytest"}); err != nil {
		t.Fatal(err)
	}

	_, err := activeProfileNames(d, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "./mytest") || !strings.Contains(err.Error(), "--with-only") {
		t.Errorf("err = %v, want it to name ./mytest and point at --with/--with-only", err)
	}

	// --with-only never reads the sticky list, so a path there is fine.
	got, err := activeProfileNames(d, nil, []string{"./mytest"}, true)
	if err != nil || len(got) != 1 || got[0] != "./mytest" {
		t.Errorf("got %#v, %v", got, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestIsProfilePath|TestPathProfile|TestLoadProfiles|TestBuiltInProfilesPass|TestActiveProfileNamesRejects' github.com/jhoblitt/rooket/cmd`
Expected: build failure, `undefined: isProfilePath`.

- [ ] **Step 3: Implement**

`cmd/compose.go`: add `"maps"`, `"slices"`, and `"strings"` to the imports. After the `chartShortNames` map add:

```go
// allCharts is every chart rooket composes values for.
var allCharts = []string{chartOperator, chartCluster, chartCSI}

// isProfilePath reports whether a --with/--with-only value names a profile
// directory rather than a profile.
func isProfilePath(s string) bool {
	return strings.Contains(s, "/") || s == "." || s == ".."
}
```

In `activeProfileNames`, between reading `sticky` and the `return append(...)`:

```go
	// A relative path here would resolve against whichever directory rooket
	// happened to run from, so the sticky list takes names only.
	for _, s := range sticky {
		if isProfilePath(s) {
			return nil, fmt.Errorf("%s lists %q, a path: profile directories are accepted only on --with and --with-only",
				filepath.Join(cloneDir.Path(), "config.yaml"), s)
		}
	}
```

Replace `loadProfiles`:

```go
// loadProfiles resolves each selected profile, by name or by directory, and
// rejects what composition would otherwise get silently wrong: a values file
// no chart looks up, and two different profiles sharing the name that
// prefixes their templates.
func loadProfiles(names []string) ([]profiles.Profile, error) {
	dir, err := userProfileDir()
	if err != nil {
		return nil, err
	}
	out := make([]profiles.Profile, 0, len(names))
	byName := make(map[string]profiles.Profile, len(names))
	for _, n := range names {
		var p profiles.Profile
		if isProfilePath(n) {
			p, err = profiles.LoadDir(n)
		} else {
			p, err = profiles.Load(dir, n)
		}
		if err != nil {
			return nil, err
		}
		if err := checkValueCharts(p); err != nil {
			return nil, err
		}
		if prev, ok := byName[p.Name]; ok && !prev.SameSource(p) {
			return nil, fmt.Errorf("two active profiles are named %q: %s and %s", p.Name, prev.Label(), p.Label())
		}
		byName[p.Name] = p
		out = append(out, p)
	}
	return out, nil
}

// checkValueCharts rejects a profile values file named for no chart:
// composeChart looks profile values up by chart name, so such a file would
// load and then never apply.
func checkValueCharts(p profiles.Profile) error {
	for _, chart := range slices.Sorted(maps.Keys(p.Values)) {
		if !slices.Contains(allCharts, chart) {
			return fmt.Errorf("profile %s: values/%s.* is named for no chart (want one of %s)",
				p.Label(), chart, strings.Join(allCharts, ", "))
		}
	}
	return nil
}
```

In `composeChart`, label profile layers by how they were selected:

```go
			layers = append(layers, values.Layer{Name: "profile:" + p.Label(), Values: v})
```

`cmd/values.go`, in `valuesShowCmd`: replace `charts := []string{chartOperator, chartCluster, chartCSI}` with `charts := allCharts`.

Help text: in `cmd/values.go`, `cmd/deploy.go` and `cmd/up.go`, change the two profile flags' usage strings to:

```go
"profile to enable, by name or by directory path (./dir), in addition to the clone's sticky list (repeatable)"
"profile to enable, by name or by directory path (./dir), replacing the clone's sticky list (repeatable)"
```

`README.md`: insert this subsection at the end of "Chart values and profiles", before "## Commands":

````markdown
### Per-run chart values (integration tests)

A program that drives rooket, such as an integration test, can give it values
for each chart without touching the rook clone or `~/.config/rooket`: pass a
profile directory by path to `--with` or `--with-only`. A value containing
`/` (or exactly `.` or `..`) is a path; a relative one resolves against the
directory rooket runs in.

```text
mytest/
├── profile.yaml                  # description: my integration test
├── values/
│   ├── rook-ceph.yaml            # the rook-ceph (operator) chart only
│   └── rook-ceph-cluster.yaml    # the rook-ceph-cluster chart only
└── templates/                    # optional extra manifests
```

```yaml
# mytest/values/rook-ceph.yaml
logLevel: DEBUG
```

```yaml
# mytest/values/rook-ceph-cluster.yaml
cephClusterSpec:
  dashboard:
    enabled: false
```

```console
$ rooket values show cluster --with-only ./mytest --layers   # preview
$ rooket up --with-only ./mytest
```

Each file reaches only the chart it is named for, so no key can leak into
another chart. A values file named for no chart (`values/cluster.yaml`), a
path in `.rooket/config.yaml`, and two active profiles sharing a directory
name are errors. `--with-only` replaces the clone's sticky profile list but
still applies its `.rooket/values/`.
````

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l cmd && go vet github.com/jhoblitt/rooket/... && go test -run 'TestIsProfilePath|TestPathProfile|TestLoadProfiles|TestBuiltInProfilesPass|TestActiveProfileNames|TestComposeChart|TestValuesShow' github.com/jhoblitt/rooket/cmd`
Expected: no gofmt output, vet clean, `ok`.

- [ ] **Step 5: Commit**

```bash
git add cmd/compose.go cmd/compose_test.go cmd/values.go cmd/deploy.go cmd/up.go README.md
git commit -F - <<'EOF'
feat(values): accept a profile directory by path on --with and --with-only

A caller, such as an integration test, can now hand rooket values for each
chart separately: values/<chart>.yaml in a profile directory reaches that
chart and no other. A values file named for no chart used to load and never
apply; it is now an error, as are a path in the clone's sticky profile list
and two different profiles sharing a name.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 4: List active path profiles in `values profiles`

**Files:**
- Modify: `cmd/valuesprofiles.go` (new `listedProfiles`; `renderProfileList` shows path profiles)
- Test: `cmd/valuesprofiles_test.go`

**Interfaces:**
- Consumes: `profiles.LoadDir`, `Profile.Label()`, `Profile.Path` (Task 1); `isProfilePath` (Task 3); the `writePathProfile` test helper (Task 3, same package).
- Produces: `func listedProfiles(userDir string, active []string) ([]profiles.Profile, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/valuesprofiles_test.go` (it imports `strings`, `testing`, `profiles`):

```go
func TestRenderProfileListShowsActivePathProfile(t *testing.T) {
	got := renderProfileList([]profiles.Profile{
		{Name: "rbd", Description: "block storage", BuiltIn: true},
		{Name: "mytest", Path: "./mytest", Description: "my test"},
	}, []string{"./mytest"})

	var pathLine, rbdLine string
	for l := range strings.Lines(got) {
		switch {
		case strings.Contains(l, "./mytest"):
			pathLine = l
		case strings.Contains(l, "rbd"):
			rbdLine = l
		}
	}
	if !strings.Contains(pathLine, "*") || !strings.Contains(pathLine, "path") {
		t.Errorf("path profile line = %q, want it marked active with origin path", pathLine)
	}
	if strings.Contains(rbdLine, "*") {
		t.Errorf("rbd line = %q, want it unmarked", rbdLine)
	}
}

func TestListedProfilesAddsActivePaths(t *testing.T) {
	path := writePathProfile(t, t.TempDir(), "mytest", nil)

	got, err := listedProfiles(t.TempDir(), []string{"rbd", path})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range got {
		if p.Path != "" {
			paths = append(paths, p.Path)
		}
	}
	if len(paths) != 1 || paths[0] != path {
		t.Errorf("path profiles listed = %#v, want exactly [%s]", paths, path)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestRenderProfileList|TestListedProfiles' github.com/jhoblitt/rooket/cmd`
Expected: build failure, `undefined: listedProfiles`.

- [ ] **Step 3: Implement**

In `cmd/valuesprofiles.go`, add:

```go
// listedProfiles returns every discoverable profile plus each active path
// profile, which nothing discovers and so is listed only when selected.
func listedProfiles(userDir string, active []string) ([]profiles.Profile, error) {
	all, err := profiles.List(userDir)
	if err != nil {
		return nil, err
	}
	for _, n := range active {
		if !isProfilePath(n) {
			continue
		}
		p, err := profiles.LoadDir(n)
		if err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	return all, nil
}
```

In `valuesProfilesCmd.RunE`, compute `active` before the listing and call `listedProfiles` in place of `profiles.List`:

```go
		userDir, err := userProfileDir()
		if err != nil {
			return err
		}
		active, err := activeProfileNames(clone.Open(dir), deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		all, err := listedProfiles(userDir, active)
		if err != nil {
			return err
		}
		fmt.Print(renderProfileList(all, active))
		return nil
```

Replace `renderProfileList`'s loop body so it keys on `Label` and names the path origin:

```go
	for _, p := range all {
		mark := " "
		if on[p.Label()] {
			mark = "*"
		}
		origin := "user"
		switch {
		case p.Path != "":
			origin = "path"
		case p.BuiltIn:
			origin = "built-in"
		}
		fmt.Fprintf(&b, " %s %-12s (%-8s) %s\n", mark, p.Label(), origin, p.Description)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l cmd && go vet github.com/jhoblitt/rooket/cmd && go test -run 'TestRenderProfileList|TestListedProfiles' github.com/jhoblitt/rooket/cmd`
Expected: no gofmt output, vet clean, `ok`.

- [ ] **Step 5: Commit**

```bash
git add cmd/valuesprofiles.go cmd/valuesprofiles_test.go
git commit -F - <<'EOF'
feat(values): list active path profiles in 'values profiles'

A profile directory selected with --with or --with-only is not
discoverable, so the listing now appends each active one, marked with
origin "path" under the path it was selected by.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 5: e2e: a path profile's values reach only their chart

**Files:**
- Modify: `test/e2e/profiles_test.go` (a new `It` between "shows exactly the values helm received" and "prunes the profile on disable, keeping clone templates"; add `"slices"` to the imports)

**Interfaces:**
- Consumes: the suite's existing `rooketRun`, `withOnlyArgs`, `decodeValues`, `tail`, `rookDir`, `clusterName`. The CLI behavior from Tasks 1–3.
- Produces: nothing.

- [ ] **Step 1: Write the spec**

Insert into the `Describe("rooket profiles", Ordered, ...)` block, directly after the `It("shows exactly the values helm received", ...)` block:

```go
	It("routes a path profile's values only to the chart each file names", func() {
		// One key, a different value per rook chart. If values were ever
		// broadcast to every chart again, a release would receive the other
		// chart's value, or ceph-csi-drivers would receive one. The key is
		// unknown to both charts, so setting it changes no manifest.
		dir := filepath.Join(GinkgoT().TempDir(), "e2e-path-profile")
		Expect(os.MkdirAll(filepath.Join(dir, "values"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "profile.yaml"),
			[]byte("description: e2e path profile\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "values", "rook-ceph.yaml"),
			[]byte("rooketE2eMarker: operator\n"), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "values", "rook-ceph-cluster.yaml"),
			[]byte("rooketE2eMarker: cluster\n"), 0o644)).To(Succeed())

		deployArgs := append([]string{"deploy", "--dir", rookDir, "--name", clusterName},
			withOnlyArgs([]string{"rbd", dir})...)
		out, err := rooketRun(15*time.Minute, deployArgs...)
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		want := map[string]string{"rook-ceph": "operator", "rook-ceph-cluster": "cluster"}
		// Only rook refs from v1.20 on install the ceph-csi-drivers release.
		releases, err := rooketRun(2*time.Minute, "helm", "-n", "rook-ceph", "list", "-q")
		Expect(err).NotTo(HaveOccurred())
		if slices.Contains(strings.Fields(releases), "ceph-csi-drivers") {
			want["ceph-csi-drivers"] = ""
		}

		for release, marker := range want {
			raw, err := rooketRun(2*time.Minute, "helm", "-n", "rook-ceph",
				"get", "values", release, "-o", "yaml")
			Expect(err).NotTo(HaveOccurred())
			vals, err := decodeValues(raw)
			Expect(err).NotTo(HaveOccurred(), "%s: parse helm values:\n%s", release, raw)
			if marker == "" {
				Expect(vals).NotTo(HaveKey("rooketE2eMarker"), "%s received a path-profile value", release)
			} else {
				Expect(vals).To(HaveKeyWithValue("rooketE2eMarker", marker), "%s did not receive its own value", release)
			}
		}
	})
```

The next spec, "prunes the profile on disable...", deploys with `--with-only ""`, which also drops this path profile. No extra cleanup is needed.

- [ ] **Step 2: Compile-check**

Run: `gofmt -l test/e2e && go vet -tags e2e github.com/jhoblitt/rooket/test/e2e`
Expected: no output from either.

- [ ] **Step 3: Run it**

The suite needs `ROOK_DIR`, iSCSI block devices, and root for block setup. It brings up its own disposable kind cluster through `rooket up`. It never uses the ambient kubectl context. CI runs it on the PR against rook master, release-1.20 and release-1.19. That set covers both the ceph-csi-drivers flow and the pre-1.20 flow that has no CSI release. Running it locally is optional:

Run: `ROOK_DIR=<a rook clone> go test -tags e2e github.com/jhoblitt/rooket/test/e2e -timeout 60m -ginkgo.focus 'rooket profiles'`
Expected: PASS. If it isn't run locally, say so when reporting, and rely on the PR's CI run.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/profiles_test.go
git commit -F - <<'EOF'
test(e2e): assert a path profile's values reach only their own chart

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

## Final verification (whole branch)

- [ ] `gofmt -l $(git ls-files '*.go')` prints nothing.
- [ ] `go vet github.com/jhoblitt/rooket/... && go vet -tags e2e github.com/jhoblitt/rooket/test/e2e` is clean.
- [ ] `go test -race github.com/jhoblitt/rooket/...` passes. Run it unsandboxed so the two sudoers ownership tests can see real uids.
- [ ] `grep -rn -E -- '(^|[^-])-f [a-z./]*\.yaml|--set |--values' README.md cmd/` finds no leftover reference to the removed flags in docs or help text.
- [ ] A throwaway build shows a path profile routed per chart:
  `go build -o /tmp/claude/rooket-verify . && mkdir -p /tmp/claude/vp/t/values && echo 'description: t' > /tmp/claude/vp/t/profile.yaml && echo 'logLevel: DEBUG' > /tmp/claude/vp/t/values/rook-ceph.yaml && /tmp/claude/rooket-verify values show --dir ~/github/rook --with-only /tmp/claude/vp/t --layers`
  Expected: `logLevel: DEBUG` with layer `profile:/tmp/claude/vp/t` under `# rook-ceph` only, and no `logLevel` under `# rook-ceph-cluster` or `# ceph-csi-drivers`.
