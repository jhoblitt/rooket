# Chart Value Overrides — Project A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace rooket's hardcoded Helm values with a layered override system — per-clone sticky files plus composable profiles — merged by rooket into one values file per chart.

**Architecture:** Four new leaf packages under `internal/` (`values`, `clone`, `profiles`, `profileschart`) with no dependencies on `cmd`. `cmd/deploy.go` stops using `--set` entirely and instead composes layers into one file per chart, written into the cluster's state dir. Profile-supplied Kubernetes resources are rendered into a generated `rooket-profiles` chart installed as a fourth Helm release, so disabling a profile prunes its resources.

**Tech Stack:** Go 1.26, cobra, `go.yaml.in/yaml/v3`, `go:embed`, ginkgo/gomega for e2e. No new third-party dependencies.

**Spec:** `docs/superpowers/specs/2026-07-21-chart-value-overrides-design.md`

## Global Constraints

- Module is `github.com/jhoblitt/rooket`; Go 1.26.
- **No new third-party dependencies.** YAML is `go.yaml.in/yaml/v3` (already required).
- Unit tests use stdlib `testing` with table-driven subtests, no testify, matching `cmd/chartdeps_test.go`.
- E2E tests live in `test/e2e/`, use ginkgo/gomega, and carry `//go:build e2e`.
- Comments are the exception: only non-obvious "why", constraints, or gotchas. Never narrate what the code does.
- Conventional-commit subjects (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`), matching repo history.
- Chart short names on the CLI are `operator`, `cluster`, `csi`; on disk the full chart names `rook-ceph`, `rook-ceph-cluster`, `ceph-csi-drivers`.
- Run `go build ./... && go vet ./... && go test ./...` before every commit.

## Deviations from the spec

Recorded here so a reader who checks the plan against the spec is not surprised:

1. **`values.Merge` returns no `error`.** The spec's signature includes one, but merging cannot fail — every failure mode (unparsable YAML, unreadable file) belongs to loading. Signature is `Merge(layers []Layer) (map[string]any, map[string]string)`.
2. **`rbd` and `rgw` profiles carry no values overlay**, and `nfs` carries no `cephFileSystems` overlay. The chart's own defaults already enable a block pool, a filesystem, and an object store. The spec's built-in table was corrected in the same branch.

## File Structure

| File | Responsibility |
|---|---|
| `internal/values/merge.go` | `Layer`, `Merge`, name-keyed list merging, provenance. Pure, no I/O. |
| `internal/values/load.go` | Read a YAML file into `map[string]any`; encode a map back to YAML. |
| `internal/values/base.go` | Build rooket's generated base values for each of the three charts. |
| `internal/clone/clone.go` | The `.rooket/` directory in a rook clone: create, read config, values paths, templates. |
| `internal/profiles/profiles.go` | Profile registry: embedded built-ins, user dir, shadowing, loading, forking. |
| `internal/profiles/builtin/` | Embedded built-in profile trees (`rbd/`, `rgw/`, `nfs/`). |
| `internal/profileschart/chart.go` | Render the generated `rooket-profiles` chart from template sources. |
| `cmd/compose.go` | Assemble layers into a composed file per chart; resolve the active profile list. |
| `cmd/values.go` | The `rooket values` command tree: `show`, `edit`, `profiles`, `profiles fork`. |
| `cmd/deploy.go` | Rewired: no `--set`, composed `-f` per chart, install/uninstall `rooket-profiles`. |
| `cmd/up.go` | Forward the new flags to deploy. |
| `test/e2e/profiles_test.go` | Profile install, prune, and the preview invariant. |

---

### Task 1: The merge engine

**Files:**
- Create: `internal/values/merge.go`
- Test: `internal/values/merge_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `values.Layer{Name string; Values map[string]any}`, and
  `values.Merge(layers []Layer) (merged map[string]any, provenance map[string]string)`.
  Later tasks call `Merge` with layers ordered lowest-priority first.

- [ ] **Step 1: Write the failing test**

Create `internal/values/merge_test.go`:

```go
package values

import (
	"reflect"
	"testing"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name   string
		layers []Layer
		want   map[string]any
	}{
		{
			name: "later scalar wins",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"a": 1}},
				{Name: "top", Values: map[string]any{"a": 2}},
			},
			want: map[string]any{"a": 2},
		},
		{
			name: "maps merge by key",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"m": map[string]any{"a": 1, "b": 2}}},
				{Name: "top", Values: map[string]any{"m": map[string]any{"b": 3}}},
			},
			want: map[string]any{"m": map[string]any{"a": 1, "b": 3}},
		},
		{
			name: "null deletes",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"a": 1, "b": 2}},
				{Name: "top", Values: map[string]any{"a": nil}},
			},
			want: map[string]any{"b": 2},
		},
		{
			name: "named lists merge by name",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 3},
				}}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "two", "size": 1},
				}}},
			},
			want: map[string]any{"pools": []any{
				map[string]any{"name": "one", "size": 3},
				map[string]any{"name": "two", "size": 1},
			}},
		},
		{
			name: "matching names deep-merge in place",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 3, "keep": true},
				}}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 1},
				}}},
			},
			want: map[string]any{"pools": []any{
				map[string]any{"name": "one", "size": 1, "keep": true},
			}},
		},
		{
			name: "unnamed lists replace",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"l": []any{1, 2, 3}}},
				{Name: "top", Values: map[string]any{"l": []any{9}}},
			},
			want: map[string]any{"l": []any{9}},
		},
		{
			name: "duplicate names disqualify name merging",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"l": []any{
					map[string]any{"name": "dup"}, map[string]any{"name": "dup"},
				}}},
				{Name: "top", Values: map[string]any{"l": []any{
					map[string]any{"name": "new"},
				}}},
			},
			want: map[string]any{"l": []any{map[string]any{"name": "new"}}},
		},
		{
			name: "null clears a named list before re-adding",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one"},
				}}},
				{Name: "mid", Values: map[string]any{"pools": nil}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "only"},
				}}},
			},
			want: map[string]any{"pools": []any{map[string]any{"name": "only"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Merge(tc.layers)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestMergeProvenance(t *testing.T) {
	_, prov := Merge([]Layer{
		{Name: "base", Values: map[string]any{
			"m":     map[string]any{"a": 1, "b": 2},
			"pools": []any{map[string]any{"name": "one", "size": 3}},
		}},
		{Name: "profile:rgw", Values: map[string]any{
			"m":     map[string]any{"b": 3},
			"pools": []any{map[string]any{"name": "one", "size": 1}},
		}},
	})

	want := map[string]string{
		"m.a":               "base",
		"m.b":               "profile:rgw",
		"pools[one].name":   "profile:rgw",
		"pools[one].size":   "profile:rgw",
	}
	for path, layer := range want {
		if prov[path] != layer {
			t.Errorf("provenance[%q] = %q, want %q", path, prov[path], layer)
		}
	}
}

func TestMergeDoesNotAliasInput(t *testing.T) {
	src := map[string]any{"l": []any{1, 2}}
	got, _ := Merge([]Layer{{Name: "only", Values: src}})
	got["l"].([]any)[0] = 99
	if src["l"].([]any)[0] != 1 {
		t.Error("Merge aliased the input layer's slice")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/values/ -v`
Expected: FAIL — `undefined: Layer`, `undefined: Merge`.

- [ ] **Step 3: Write the implementation**

Create `internal/values/merge.go`:

```go
package values

import "fmt"

// Layer is one contributor to a chart's values, named for provenance reporting.
type Layer struct {
	Name   string
	Values map[string]any
}

// Merge folds layers left to right, later layers winning, and returns the
// merged values alongside a map from each leaf path to the layer that set it.
//
// Maps merge by key and an explicit nil deletes. A list merges by the "name"
// field of its elements when both sides qualify (see namedList); every other
// list replaces, which is Helm's behaviour. Name-keyed merging is what lets two
// profiles contribute to the same list — under replace semantics the second
// would silently drop the first's entries.
func Merge(layers []Layer) (map[string]any, map[string]string) {
	out := map[string]any{}
	prov := map[string]string{}
	for _, l := range layers {
		mergeMap(out, l.Values, "", l.Name, prov)
	}
	return out, prov
}

func mergeMap(dst, src map[string]any, path, layer string, prov map[string]string) {
	for k, v := range src {
		p := k
		if path != "" {
			p = path + "." + k
		}
		switch tv := v.(type) {
		case nil:
			delete(dst, k)
			delete(prov, p)
		case map[string]any:
			sub, ok := dst[k].(map[string]any)
			if !ok {
				sub = map[string]any{}
				dst[k] = sub
			}
			mergeMap(sub, tv, p, layer, prov)
		case []any:
			if cur, ok := dst[k].([]any); ok && namedList(cur) && namedList(tv) {
				dst[k] = mergeNamed(cur, tv, p, layer, prov)
				continue
			}
			dst[k] = deepCopy(tv)
			prov[p] = layer
		default:
			dst[k] = v
			prov[p] = layer
		}
	}
}

// namedList reports whether every element is a map carrying a unique string
// "name", the condition under which two lists can be merged element-wise.
func namedList(l []any) bool {
	if len(l) == 0 {
		return false
	}
	seen := make(map[string]bool, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		n, ok := m["name"].(string)
		if !ok || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}

func mergeNamed(dst, src []any, path, layer string, prov map[string]string) []any {
	idx := make(map[string]int, len(dst))
	out := make([]any, 0, len(dst)+len(src))
	for i, e := range dst {
		idx[e.(map[string]any)["name"].(string)] = i
		out = append(out, deepCopy(e))
	}
	for _, e := range src {
		em := e.(map[string]any)
		name := em["name"].(string)
		p := fmt.Sprintf("%s[%s]", path, name)
		if i, ok := idx[name]; ok {
			mergeMap(out[i].(map[string]any), em, p, layer, prov)
			continue
		}
		nm := map[string]any{}
		mergeMap(nm, em, p, layer, prov)
		out = append(out, nm)
	}
	return out
}

func deepCopy(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(tv))
		for k, e := range tv {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/values/ -v`
Expected: PASS for `TestMerge` (all 8 subtests), `TestMergeProvenance`, `TestMergeDoesNotAliasInput`.

- [ ] **Step 5: Commit**

```bash
git add internal/values/
git commit -m "feat(values): add layered merge engine with name-keyed lists"
```

---

### Task 2: Loading and encoding values files

**Files:**
- Create: `internal/values/load.go`
- Test: `internal/values/load_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (same package, no shared symbols needed).
- Produces: `values.LoadFile(path string) (map[string]any, error)` — returns `(nil, nil)` when the file does not exist, so callers can treat an absent layer as an empty one. `values.Encode(m map[string]any) ([]byte, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/values/load_test.go`:

```go
package values

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is an empty layer", func(t *testing.T) {
		m, err := LoadFile(filepath.Join(dir, "absent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			t.Errorf("got %#v, want nil", m)
		}
	})

	t.Run("parses a mapping", func(t *testing.T) {
		p := filepath.Join(dir, "ok.yaml")
		if err := os.WriteFile(p, []byte("a:\n  b: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := LoadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sub, ok := m["a"].(map[string]any)
		if !ok || sub["b"] != 1 {
			t.Errorf("got %#v", m)
		}
	})

	t.Run("comment-only file is an empty layer", func(t *testing.T) {
		p := filepath.Join(dir, "comments.yaml")
		if err := os.WriteFile(p, []byte("# nothing here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := LoadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 0 {
			t.Errorf("got %#v, want empty", m)
		}
	})

	t.Run("parse error names the file", func(t *testing.T) {
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte("a: [1,\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadFile(p)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "bad.yaml") {
			t.Errorf("error %q does not name the file", err)
		}
	})
}

func TestEncodeRoundTrips(t *testing.T) {
	in := map[string]any{"a": map[string]any{"b": 1}}
	data, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].(map[string]any)["b"] != 1 {
		t.Errorf("got %#v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/values/ -run 'TestLoadFile|TestEncode' -v`
Expected: FAIL — `undefined: LoadFile`, `undefined: Encode`.

- [ ] **Step 3: Write the implementation**

Create `internal/values/load.go`:

```go
package values

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"go.yaml.in/yaml/v3"
)

// LoadFile parses a YAML mapping. A missing file yields a nil map so callers
// can add an absent layer unconditionally.
func LoadFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func Encode(m map[string]any) ([]byte, error) {
	data, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode values: %w", err)
	}
	return data, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/values/ -v`
Expected: PASS, including Task 1's tests.

- [ ] **Step 5: Commit**

```bash
git add internal/values/
git commit -m "feat(values): load and encode values files"
```

---

### Task 3: Generated base values for the three charts

**Files:**
- Create: `internal/values/base.go`
- Test: `internal/values/base_test.go`
- Reference (do not modify yet): `cmd/deploy.go:159-186`, `cmd/deploy.go:200-213`, `cmd/deploy.go:286-298`, `cmd/deploy.go:329-376`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type OperatorInput struct{ ImageRepo, ImageTag, Digest string }
  func OperatorBase(in OperatorInput) map[string]any

  type StorageNode struct{ Name string; Devices []string }
  type ClusterInput struct{ OperatorNamespace string; Nodes []StorageNode }
  func ClusterBase(in ClusterInput) map[string]any

  func CSIBase() map[string]any
  ```

These reproduce exactly what `cmd/deploy.go` supplies today via `--set` and the
`csiDriversValues` constant, as data rather than flags — that is the whole point
of the task, since `--set` outranks every `-f` and would make user layers
unreachable.

- [ ] **Step 1: Write the failing test**

Create `internal/values/base_test.go`:

```go
package values

import (
	"reflect"
	"testing"
)

func TestOperatorBase(t *testing.T) {
	t.Run("without a digest", func(t *testing.T) {
		got := OperatorBase(OperatorInput{ImageRepo: "localhost:5001/rook/ceph", ImageTag: "master"})
		want := map[string]any{
			"image": map[string]any{
				"repository": "localhost:5001/rook/ceph",
				"tag":        "master",
			},
			"csi": map[string]any{"provisionerReplicas": 1},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %#v\nwant %#v", got, want)
		}
	})

	t.Run("with a digest pins pullPolicy and the roll annotation", func(t *testing.T) {
		got := OperatorBase(OperatorInput{
			ImageRepo: "localhost:5001/rook/ceph", ImageTag: "master", Digest: "sha256:abc",
		})
		img := got["image"].(map[string]any)
		if img["pullPolicy"] != "Always" {
			t.Errorf("pullPolicy = %v, want Always", img["pullPolicy"])
		}
		ann := got["annotations"].(map[string]any)
		if ann["rooket-image-digest"] != "sha256:abc" {
			t.Errorf("annotation = %v", ann["rooket-image-digest"])
		}
	})
}

func TestClusterBase(t *testing.T) {
	t.Run("resource trims always present", func(t *testing.T) {
		got := ClusterBase(ClusterInput{OperatorNamespace: "rook-ceph"})
		spec := got["cephClusterSpec"].(map[string]any)
		res := spec["resources"].(map[string]any)
		if res["mon"].(map[string]any)["requests"].(map[string]any)["cpu"] != "500m" {
			t.Errorf("mon cpu = %#v", res["mon"])
		}
		if spec["mgr"].(map[string]any)["count"] != 1 {
			t.Errorf("mgr count = %#v", spec["mgr"])
		}
		if got["toolbox"].(map[string]any)["enabled"] != true {
			t.Errorf("toolbox = %#v", got["toolbox"])
		}
		if _, ok := spec["storage"]; ok {
			t.Error("storage must be absent when no nodes are given")
		}
	})

	t.Run("pins one device per node", func(t *testing.T) {
		got := ClusterBase(ClusterInput{
			OperatorNamespace: "rook-ceph",
			Nodes: []StorageNode{
				{Name: "c-worker", Devices: []string{"/dev/sdb"}},
				{Name: "c-worker2", Devices: []string{"/dev/sdc"}},
			},
		})
		storage := got["cephClusterSpec"].(map[string]any)["storage"].(map[string]any)
		if storage["useAllNodes"] != false || storage["useAllDevices"] != false {
			t.Errorf("storage = %#v", storage)
		}
		nodes := storage["nodes"].([]any)
		if len(nodes) != 2 {
			t.Fatalf("nodes = %#v", nodes)
		}
		first := nodes[0].(map[string]any)
		if first["name"] != "c-worker" {
			t.Errorf("node name = %v", first["name"])
		}
		devs := first["devices"].([]any)
		if devs[0].(map[string]any)["name"] != "/dev/sdb" {
			t.Errorf("devices = %#v", devs)
		}
	})
}

func TestCSIBase(t *testing.T) {
	got := CSIBase()
	drivers := got["drivers"].(map[string]any)
	if drivers["rbd"].(map[string]any)["name"] != "rook-ceph.rbd.csi.ceph.com" {
		t.Errorf("rbd = %#v", drivers["rbd"])
	}
	if drivers["nfs"].(map[string]any)["enabled"] != false {
		t.Errorf("nfs = %#v", drivers["nfs"])
	}
	if got["operatorConfig"].(map[string]any)["namespace"] != "rook-ceph" {
		t.Errorf("operatorConfig = %#v", got["operatorConfig"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/values/ -run 'Base' -v`
Expected: FAIL — `undefined: OperatorBase`.

- [ ] **Step 3: Write the implementation**

Create `internal/values/base.go`:

```go
package values

type OperatorInput struct {
	ImageRepo string
	ImageTag  string
	Digest    string
}

// OperatorBase builds rooket's generated layer for the rook-ceph chart.
//
// One provisioner per driver is plenty for a dev cluster, and the HA pair
// starves small hosts. Consumed only by refs where rook manages the CSI drivers
// itself (<= v1.19); newer refs take the drivers chart's default of one replica.
func OperatorBase(in OperatorInput) map[string]any {
	image := map[string]any{
		"repository": in.ImageRepo,
		"tag":        in.ImageTag,
	}
	out := map[string]any{
		"image": image,
		"csi":   map[string]any{"provisionerReplicas": 1},
	}
	// The deploy tag is a mutable branch name and the chart defaults to
	// IfNotPresent, so a rebuild pushing the same tag would neither roll the
	// Deployment nor beat a node-cached image. Pinning the registry's current
	// digest as a pod-template annotation rolls the operator exactly when image
	// content changed; always-pull is required for the roll to matter.
	if in.Digest != "" {
		image["pullPolicy"] = "Always"
		out["annotations"] = map[string]any{"rooket-image-digest": in.Digest}
	}
	return out
}

type StorageNode struct {
	Name    string
	Devices []string
}

type ClusterInput struct {
	OperatorNamespace string
	Nodes             []StorageNode
}

// ClusterBase builds rooket's generated layer for the rook-ceph-cluster chart.
//
// The cpu trims replace the chart's production-HA requests (1 cpu per mon and
// per OSD): on a small host those fill each node's request budget until later
// components — the detect-version jobs, the mds — cannot schedule at all, seen
// as a wedged cluster on 4-vCPU CI runners. Memory requests are left alone
// (rook derives osd_memory_target from them). A standby mgr adds nothing to a
// disposable dev cluster and its requests eat a node's budget.
//
// Naming a device per node keeps rook from mis-attributing OSDs — every
// privileged kind node sees every host disk — so each worker gets exactly one
// OSD on its own disk via rook's direct device path, no local PV, no kubelet
// loop.
func ClusterBase(in ClusterInput) map[string]any {
	spec := map[string]any{
		"mgr": map[string]any{"count": 1},
		"resources": map[string]any{
			"mon": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"osd": map[string]any{"requests": map[string]any{"cpu": "500m"}},
			"mgr": map[string]any{"requests": map[string]any{"cpu": "300m"}},
		},
	}
	if len(in.Nodes) > 0 {
		nodes := make([]any, 0, len(in.Nodes))
		for _, n := range in.Nodes {
			devices := make([]any, 0, len(n.Devices))
			for _, d := range n.Devices {
				devices = append(devices, map[string]any{"name": d})
			}
			nodes = append(nodes, map[string]any{"name": n.Name, "devices": devices})
		}
		spec["storage"] = map[string]any{
			"useAllNodes":   false,
			"useAllDevices": false,
			"nodes":         nodes,
		}
	}
	return map[string]any{
		"operatorNamespace": in.OperatorNamespace,
		"toolbox":           map[string]any{"enabled": true},
		"cephClusterSpec":   spec,
	}
}

// CSIBase builds rooket's generated layer for the ceph-csi-drivers chart. The
// RBD and CephFS driver names must carry the operator-namespace prefix that the
// rook-ceph-cluster chart's StorageClasses use as their provisioner; snapshot
// support stays off (kind clusters have no VolumeSnapshot CRDs, and the chart's
// cephfs driver defaults it on); nfs and nvmeof are off until a profile asks.
func CSIBase() map[string]any {
	return map[string]any{
		"operatorConfig": map[string]any{"namespace": "rook-ceph"},
		"drivers": map[string]any{
			"rbd": map[string]any{
				"name":           "rook-ceph.rbd.csi.ceph.com",
				"snapshotPolicy": "none",
			},
			"cephfs": map[string]any{
				"name":           "rook-ceph.cephfs.csi.ceph.com",
				"snapshotPolicy": "none",
			},
			"nfs":    map[string]any{"enabled": false},
			"nvmeof": map[string]any{"enabled": false},
		},
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/values/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/values/
git commit -m "feat(values): build generated base values for the three charts"
```

---

### Task 4: The clone's `.rooket/` directory

**Files:**
- Create: `internal/clone/clone.go`
- Test: `internal/clone/clone_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type Dir struct{ /* unexported root */ }
  func Open(rookDir string) Dir
  func (d Dir) Path() string
  func (d Dir) Ensure() error
  func (d Dir) ValuesPath(chart string) string
  func (d Dir) Profiles() ([]string, error)
  func (d Dir) SetProfiles(names []string) error
  func (d Dir) Templates() (map[string][]byte, error)
  ```
  `chart` is a full chart name (`rook-ceph-cluster`). `Templates` returns
  filename → content, empty when the directory is absent.

- [ ] **Step 1: Write the failing test**

Create `internal/clone/clone_test.go`:

```go
package clone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureWritesSelfIgnoringGitignore(t *testing.T) {
	root := t.TempDir()
	d := Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".rooket", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "*\n" {
		t.Errorf("gitignore = %q, want %q", data, "*\n")
	}

	if err := d.Ensure(); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
}

func TestProfiles(t *testing.T) {
	root := t.TempDir()
	d := Open(root)

	t.Run("absent config yields no profiles", func(t *testing.T) {
		got, err := d.Profiles()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v, want none", got)
		}
	})

	t.Run("round-trips order", func(t *testing.T) {
		if err := d.SetProfiles([]string{"rbd", "rgw"}); err != nil {
			t.Fatal(err)
		}
		got, err := d.Profiles()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{"rbd", "rgw"}) {
			t.Errorf("got %#v", got)
		}
	})
}

func TestTemplates(t *testing.T) {
	root := t.TempDir()
	d := Open(root)

	t.Run("absent directory yields none", func(t *testing.T) {
		got, err := d.Templates()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("reads yaml files only", func(t *testing.T) {
		dir := filepath.Join(root, ".rooket", "templates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"pvc.yaml":  "kind: PersistentVolumeClaim\n",
			"pod.yml":   "kind: Pod\n",
			"notes.txt": "ignore me",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := d.Templates()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %#v, want 2 entries", got)
		}
		if string(got["pvc.yaml"]) != "kind: PersistentVolumeClaim\n" {
			t.Errorf("pvc.yaml = %q", got["pvc.yaml"])
		}
	})
}

func TestValuesPath(t *testing.T) {
	got := Open("/x").ValuesPath("rook-ceph-cluster")
	want := filepath.Join("/x", ".rooket", "values", "rook-ceph-cluster.yaml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/clone/ -v`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Write the implementation**

Create `internal/clone/clone.go`:

```go
package clone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Dir is the .rooket directory inside a rook clone: the per-checkout sticky
// layer of values, profile selection, and ad-hoc templates.
type Dir struct{ root string }

func Open(rookDir string) Dir { return Dir{root: filepath.Join(rookDir, ".rooket")} }

func (d Dir) Path() string { return d.root }

// Ensure creates the directory tree and a .gitignore of "*". Git suppresses a
// directory whose every path is ignored, including the ignore file itself, so
// the rook checkout stays clean without touching .git/info/exclude or the
// tracked .gitignore.
func (d Dir) Ensure() error {
	for _, sub := range []string{"values", "templates"} {
		if err := os.MkdirAll(filepath.Join(d.root, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d.root, err)
		}
	}
	gi := filepath.Join(d.root, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gi, []byte("*\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", gi, err)
		}
	}
	return nil
}

func (d Dir) ValuesPath(chart string) string {
	return filepath.Join(d.root, "values", chart+".yaml")
}

func (d Dir) configPath() string { return filepath.Join(d.root, "config.yaml") }

type config struct {
	Profiles []string `yaml:"profiles"`
}

func (d Dir) Profiles() ([]string, error) {
	data, err := os.ReadFile(d.configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.configPath(), err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", d.configPath(), err)
	}
	return c.Profiles, nil
}

func (d Dir) SetProfiles(names []string) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	data, err := yaml.Marshal(config{Profiles: names})
	if err != nil {
		return fmt.Errorf("encode %s: %w", d.configPath(), err)
	}
	if err := os.WriteFile(d.configPath(), data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", d.configPath(), err)
	}
	return nil
}

func (d Dir) Templates() (map[string][]byte, error) {
	dir := filepath.Join(d.root, "templates")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, e.Name()), err)
		}
		out[e.Name()] = data
	}
	return out, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/clone/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/clone/
git commit -m "feat(clone): add the .rooket per-checkout config directory"
```

---

### Task 5: The profile registry

**Files:**
- Create: `internal/profiles/profiles.go`
- Create: `internal/profiles/builtin/rbd/profile.yaml` (placeholder content below; real templates arrive in Tasks 12-14)
- Test: `internal/profiles/profiles_test.go`

**Interfaces:**
- Consumes: `values.LoadFile` is *not* used here — profile values are parsed with `values.LoadFile`'s sibling logic on embedded bytes, so this package exposes raw maps.
- Produces:
  ```go
  type Profile struct {
      Name        string
      Description string
      BuiltIn     bool
      Values      map[string]map[string]any // chart name -> values
      Templates   map[string][]byte         // filename -> content
  }
  func List(userDir string) ([]Profile, error)
  func Load(userDir, name string) (Profile, error)
  func Fork(userDir, name string) (string, error) // returns the created directory
  ```
  `userDir` is `~/.config/rooket/profiles`. A user profile shadows a built-in of
  the same name entirely. `local` is reserved and `Load` rejects it.

- [ ] **Step 1: Write the failing test**

Create `internal/profiles/profiles_test.go`:

```go
package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, dir, name, desc, valuesChart, valuesBody string) {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(root, "values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profile.yaml"),
		[]byte("description: "+desc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if valuesChart != "" {
		if err := os.WriteFile(filepath.Join(root, "values", valuesChart+".yaml"),
			[]byte(valuesBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "templates", "10-thing.yaml"),
		[]byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUserProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "custom", "my thing", "rook-ceph-cluster", "toolbox:\n  enabled: false\n")

	p, err := Load(dir, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "my thing" {
		t.Errorf("description = %q", p.Description)
	}
	if p.BuiltIn {
		t.Error("BuiltIn should be false")
	}
	tb := p.Values["rook-ceph-cluster"]["toolbox"].(map[string]any)
	if tb["enabled"] != false {
		t.Errorf("values = %#v", p.Values)
	}
	if string(p.Templates["10-thing.yaml"]) != "kind: ConfigMap\n" {
		t.Errorf("templates = %#v", p.Templates)
	}
}

func TestUserProfileShadowsBuiltIn(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "rbd", "shadowed", "", "")

	p, err := Load(dir, "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if p.BuiltIn || p.Description != "shadowed" {
		t.Errorf("built-in was not shadowed: %+v", p)
	}
}

func TestLoadRejectsReservedName(t *testing.T) {
	_, err := Load(t.TempDir(), "local")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("err = %v, want a reserved-name error", err)
	}
}

func TestLoadUnknownNameLists(t *testing.T) {
	_, err := Load(t.TempDir(), "nope")
	if err == nil || !strings.Contains(err.Error(), "rbd") {
		t.Errorf("err = %v, want it to name the available profiles", err)
	}
}

func TestListIncludesBuiltInsAndUsers(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "mine", "user one", "", "")

	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.Name] = true
	}
	for _, want := range []string{"rbd", "mine"} {
		if !seen[want] {
			t.Errorf("List missing %q: %#v", want, seen)
		}
	}
}

func TestFork(t *testing.T) {
	dir := t.TempDir()
	out, err := Fork(dir, "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if out != filepath.Join(dir, "rbd") {
		t.Errorf("dir = %q", out)
	}
	if _, err := os.Stat(filepath.Join(out, "profile.yaml")); err != nil {
		t.Errorf("forked profile.yaml missing: %v", err)
	}

	if _, err := Fork(dir, "rbd"); err == nil {
		t.Error("forking over an existing profile should fail")
	}
}
```

- [ ] **Step 2: Create the minimal built-in tree so the tests have something to find**

Create `internal/profiles/builtin/rbd/profile.yaml`:

```yaml
description: RBD — a PVC on the default ceph-block StorageClass and a pod writing to it
```

Create `internal/profiles/builtin/rbd/templates/.keep` as an empty file (Tasks 12-14 replace this with real templates):

```bash
mkdir -p internal/profiles/builtin/rbd/templates
touch internal/profiles/builtin/rbd/templates/.keep
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/profiles/ -v`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 4: Write the implementation**

Create `internal/profiles/profiles.go`:

```go
package profiles

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed all:builtin
var builtinFS embed.FS

// Reserved is the prefix rooket gives the clone's own templates in the
// generated chart, so no profile may claim it.
const Reserved = "local"

type Profile struct {
	Name        string
	Description string
	BuiltIn     bool
	Values      map[string]map[string]any
	Templates   map[string][]byte
}

type meta struct {
	Description string `yaml:"description"`
}

// Load returns a profile, preferring a user profile over a built-in of the same
// name; a user profile shadows a built-in entirely rather than merging with it.
func Load(userDir, name string) (Profile, error) {
	if name == Reserved {
		return Profile{}, fmt.Errorf("profile name %q is reserved for the clone's own templates", Reserved)
	}
	if dir := filepath.Join(userDir, name); isDir(dir) {
		return fromFS(os.DirFS(dir), name, false)
	}
	sub, err := fs.Sub(builtinFS, filepath.Join("builtin", name))
	if err == nil && fsHasFile(sub, "profile.yaml") {
		return fromFS(sub, name, true)
	}
	avail, _ := List(userDir)
	names := make([]string, 0, len(avail))
	for _, p := range avail {
		names = append(names, p.Name)
	}
	return Profile{}, fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(names, ", "))
}

func List(userDir string) ([]Profile, error) {
	byName := map[string]Profile{}

	builtins, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("read embedded profiles: %w", err)
	}
	for _, e := range builtins {
		if !e.IsDir() {
			continue
		}
		p, err := Load(userDir, e.Name())
		if err != nil {
			return nil, err
		}
		byName[e.Name()] = p
	}

	entries, err := os.ReadDir(userDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", userDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == Reserved {
			continue
		}
		p, err := Load(userDir, e.Name())
		if err != nil {
			return nil, err
		}
		byName[e.Name()] = p
	}

	out := make([]Profile, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Fork copies a built-in profile into the user directory so it can be edited.
func Fork(userDir, name string) (string, error) {
	p, err := Load(userDir, name)
	if err != nil {
		return "", err
	}
	if !p.BuiltIn {
		return "", fmt.Errorf("profile %q is already a user profile at %s", name, filepath.Join(userDir, name))
	}
	dst := filepath.Join(userDir, name)
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%s already exists", dst)
	}
	src, err := fs.Sub(builtinFS, filepath.Join("builtin", name))
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("fork profile %q: %w", name, err)
	}
	return dst, nil
}

func fromFS(fsys fs.FS, name string, builtIn bool) (Profile, error) {
	p := Profile{
		Name:      name,
		BuiltIn:   builtIn,
		Values:    map[string]map[string]any{},
		Templates: map[string][]byte{},
	}

	data, err := fs.ReadFile(fsys, "profile.yaml")
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q: read profile.yaml: %w", name, err)
	}
	var m meta
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Profile{}, fmt.Errorf("profile %q: parse profile.yaml: %w", name, err)
	}
	p.Description = m.Description

	valueFiles, err := fs.ReadDir(fsys, "values")
	if err == nil {
		for _, e := range valueFiles {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			raw, err := fs.ReadFile(fsys, filepath.Join("values", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			var v map[string]any
			if err := yaml.Unmarshal(raw, &v); err != nil {
				return Profile{}, fmt.Errorf("profile %q: parse %s: %w", name, e.Name(), err)
			}
			p.Values[strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")] = v
		}
	}

	tmplFiles, err := fs.ReadDir(fsys, "templates")
	if err == nil {
		for _, e := range tmplFiles {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			raw, err := fs.ReadFile(fsys, filepath.Join("templates", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			p.Templates[e.Name()] = raw
		}
	}
	return p, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fsHasFile(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/profiles/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/profiles/
git commit -m "feat(profiles): add the profile registry with embedded built-ins"
```

---

### Task 6: The generated profiles chart

**Files:**
- Create: `internal/profileschart/chart.go`
- Test: `internal/profileschart/chart_test.go`

**Interfaces:**
- Consumes: nothing (takes plain `map[string][]byte` so it depends on neither `clone` nor `profiles`).
- Produces:
  ```go
  type Source struct{ Prefix string; Files map[string][]byte }
  type Context struct{ ClusterName, Namespace, OperatorNamespace string; Workers int }
  func Render(dir string, ctx Context, sources []Source) (bool, error)
  ```
  `Render` clears and rewrites `dir`, and reports whether any template was
  written — `false` means the caller should uninstall the release instead of
  installing it.

- [ ] **Step 1: Write the failing test**

Create `internal/profileschart/chart_test.go`:

```go
package profileschart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderWritesPrefixedTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	ok, err := Render(dir, Context{ClusterName: "c", Namespace: "rook-ceph"}, []Source{
		{Prefix: "local", Files: map[string][]byte{"scratch.yaml": []byte("kind: PersistentVolumeClaim\n")}},
		{Prefix: "rgw", Files: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("Render reported nothing to install")
	}
	for name, want := range map[string]string{
		"local-scratch.yaml": "kind: PersistentVolumeClaim\n",
		"rgw-20-obc.yaml":    "kind: ObjectBucketClaim\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, "templates", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Errorf("Chart.yaml missing: %v", err)
	}
}

func TestRenderExposesContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	if _, err := Render(dir, Context{ClusterName: "my-cluster", Namespace: "rook-ceph",
		OperatorNamespace: "rook-ceph", Workers: 3}, []Source{
		{Prefix: "p", Files: map[string][]byte{"a.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clusterName: my-cluster", "workers: 3"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("values.yaml missing %q:\n%s", want, data)
		}
	}
}

func TestRenderWithNoTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	ok, err := Render(dir, Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Render should report nothing to install")
	}
}

func TestRenderClearsStaleTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	if _, err := Render(dir, Context{}, []Source{
		{Prefix: "old", Files: map[string][]byte{"gone.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(dir, Context{}, []Source{
		{Prefix: "new", Files: map[string][]byte{"here.yaml": []byte("kind: ConfigMap\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "templates", "old-gone.yaml")); err == nil {
		t.Error("stale template survived a re-render")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/profileschart/ -v`
Expected: FAIL — `undefined: Render`.

- [ ] **Step 3: Write the implementation**

Create `internal/profileschart/chart.go`:

```go
package profileschart

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Source is one contributor of Kubernetes resources: the clone's own templates
// or one active profile's.
type Source struct {
	Prefix string
	Files  map[string][]byte
}

// Context is exposed to templates as .Values.rooket.
type Context struct {
	ClusterName       string `yaml:"clusterName"`
	Namespace         string `yaml:"namespace"`
	OperatorNamespace string `yaml:"operatorNamespace"`
	Workers           int    `yaml:"workers"`
}

const chartYAML = `apiVersion: v2
name: rooket-profiles
description: Resources contributed by rooket's active profiles and the clone's templates
type: application
version: 0.0.0
appVersion: "0.0.0"
`

// Render writes a chart holding every source's templates and reports whether
// any were written. Helm owns their lifecycle, so a resource whose source is
// gone is pruned on the next upgrade rather than leaking as kubectl apply would.
func Render(dir string, ctx Context, sources []Source) (bool, error) {
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("clear %s: %w", dir, err)
	}
	tmplDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", tmplDir, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		return false, fmt.Errorf("write Chart.yaml: %w", err)
	}
	vals, err := yaml.Marshal(map[string]any{"rooket": ctx})
	if err != nil {
		return false, fmt.Errorf("encode chart values: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), vals, 0o644); err != nil {
		return false, fmt.Errorf("write values.yaml: %w", err)
	}

	count := 0
	for _, s := range sources {
		names := make([]string, 0, len(s.Files))
		for n := range s.Files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out := filepath.Join(tmplDir, s.Prefix+"-"+n)
			if err := os.WriteFile(out, s.Files[n], 0o644); err != nil {
				return false, fmt.Errorf("write %s: %w", out, err)
			}
			count++
		}
	}
	return count > 0, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profileschart/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profileschart/
git commit -m "feat(profileschart): render a chart from profile and clone templates"
```

---

### Task 7: Composing layers in `cmd`

**Files:**
- Create: `cmd/compose.go`
- Test: `cmd/compose_test.go`

**Interfaces:**
- Consumes: `values.Layer`, `values.Merge`, `values.LoadFile`, `values.Encode`, `clone.Open`, `profiles.Load`, `profiles.Profile`.
- Produces:
  ```go
  const (
      chartOperator = "rook-ceph"
      chartCluster  = "rook-ceph-cluster"
      chartCSI      = "ceph-csi-drivers"
  )
  func chartName(short string) (string, error)
  func userProfileDir() (string, error)
  func activeProfileNames(cloneDir clone.Dir, with []string, withOnly []string, withOnlySet bool) ([]string, error)
  func loadProfiles(names []string) ([]profiles.Profile, error)

  type composed struct {
      Merged     map[string]any
      Provenance map[string]string
  }
  func composeChart(chart string, base map[string]any, cloneDir clone.Dir,
      active []profiles.Profile, extraFiles []string) (composed, error)
  func (c composed) write(path string) error
  ```

- [ ] **Step 1: Write the failing test**

Create `cmd/compose_test.go`:

```go
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestChartName(t *testing.T) {
	for in, want := range map[string]string{
		"operator":          chartOperator,
		"rook-ceph":         chartOperator,
		"cluster":           chartCluster,
		"rook-ceph-cluster": chartCluster,
		"csi":               chartCSI,
		"ceph-csi-drivers":  chartCSI,
	} {
		got, err := chartName(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("chartName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := chartName("nope"); err == nil {
		t.Error("want an error for an unknown chart")
	}
}

func TestActiveProfileNames(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.SetProfiles([]string{"sticky"}); err != nil {
		t.Fatal(err)
	}

	t.Run("with appends to the sticky list", func(t *testing.T) {
		got, err := activeProfileNames(d, []string{"extra"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "sticky" || got[1] != "extra" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("with-only replaces it", func(t *testing.T) {
		got, err := activeProfileNames(d, nil, []string{"just-this"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "just-this" {
			t.Errorf("got %#v", got)
		}
	})

	t.Run("empty with-only clears", func(t *testing.T) {
		got, err := activeProfileNames(d, nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %#v", got)
		}
	})
}

func TestComposeChartLayerOrder(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ValuesPath(chartCluster),
		[]byte("a: from-clone\nb: from-clone\nc: from-clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "extra.yaml")
	if err := os.WriteFile(extra, []byte("c: from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := composeChart(chartCluster,
		map[string]any{"a": "from-base", "b": "from-base", "c": "from-base", "d": "from-base"},
		d,
		[]profiles.Profile{{
			Name:   "p",
			Values: map[string]map[string]any{chartCluster: {"b": "from-profile", "c": "from-profile"}},
		}},
		[]string{extra},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"a": "from-clone",
		"b": "from-profile",
		"c": "from-file",
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

func TestComposedWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "values.yaml")
	c := composed{Merged: map[string]any{"a": 1}}
	if err := c.write(p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Errorf("got %q", data)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run 'TestChartName|TestActiveProfileNames|TestCompose' -v`
Expected: FAIL — `undefined: chartName`.

- [ ] **Step 3: Write the implementation**

Create `cmd/compose.go`:

```go
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/values"
)

const (
	chartOperator = "rook-ceph"
	chartCluster  = "rook-ceph-cluster"
	chartCSI      = "ceph-csi-drivers"
)

var chartShortNames = map[string]string{
	"operator": chartOperator,
	"cluster":  chartCluster,
	"csi":      chartCSI,
}

func chartName(short string) (string, error) {
	if full, ok := chartShortNames[short]; ok {
		return full, nil
	}
	for _, full := range chartShortNames {
		if short == full {
			return full, nil
		}
	}
	return "", fmt.Errorf("unknown chart %q (want operator, cluster, or csi)", short)
}

func userProfileDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(cfg, "rooket", "profiles"), nil
}

// activeProfileNames resolves the clone's sticky list against the flags:
// --with appends to it, --with-only replaces it. withOnlySet distinguishes an
// unset flag from --with-only "", which clears the selection.
func activeProfileNames(cloneDir clone.Dir, with, withOnly []string, withOnlySet bool) ([]string, error) {
	if withOnlySet {
		return withOnly, nil
	}
	sticky, err := cloneDir.Profiles()
	if err != nil {
		return nil, err
	}
	return append(sticky, with...), nil
}

func loadProfiles(names []string) ([]profiles.Profile, error) {
	dir, err := userProfileDir()
	if err != nil {
		return nil, err
	}
	out := make([]profiles.Profile, 0, len(names))
	for _, n := range names {
		p, err := profiles.Load(dir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

type composed struct {
	Merged     map[string]any
	Provenance map[string]string
}

// composeChart stacks every layer for one chart, lowest first: rooket's
// generated base, the clone's sticky file, each active profile in selection
// order, then any -f files. --set is not represented here; helm applies it
// above everything rooket writes.
func composeChart(chart string, base map[string]any, cloneDir clone.Dir,
	active []profiles.Profile, extraFiles []string) (composed, error) {

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

	for _, f := range extraFiles {
		v, err := values.LoadFile(f)
		if err != nil {
			return composed{}, err
		}
		if v == nil {
			return composed{}, fmt.Errorf("values file %s does not exist", f)
		}
		layers = append(layers, values.Layer{Name: "-f " + f, Values: v})
	}

	merged, prov := values.Merge(layers)
	return composed{Merged: merged, Provenance: prov}, nil
}

func (c composed) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := values.Encode(c.Merged)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/ -v -run 'TestChartName|TestActiveProfileNames|TestCompose'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/compose.go cmd/compose_test.go
git commit -m "feat(cmd): compose chart values from base, clone, profile, and file layers"
```

---

### Task 8: Rewire `deploy` onto composed values

**Files:**
- Modify: `cmd/deploy.go` (replace `--set` usage in `installRookCephOperator`, `installCephCsiDrivers`, `installRookCephCluster`; delete `csiDriversValues` and `writeClusterValues`)
- Test: `cmd/deploy_test.go` (add to the existing file)

**Interfaces:**
- Consumes: Task 3's `values.OperatorBase/ClusterBase/CSIBase`, Task 7's `composeChart`, `composed.write`, `chartOperator`/`chartCluster`/`chartCSI`.
- Produces: `deployValuesDir(cluster string) (string, error)` returning
  `~/.local/share/rooket/<cluster>/values`, and package-level flag vars
  `deployWith []string`, `deployWithOnly []string`, `deployValueFiles []string`,
  `deploySets []string`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/deploy_test.go`:

```go
func TestClusterStorageNodesFromDisks(t *testing.T) {
	got := clusterStorageNodes("c", 2, 1, func(iqn string) (string, error) {
		return "/dev/disk/by-path/" + iqn, nil
	})
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Name != "c-worker" || got[1].Name != "c-worker2" {
		t.Errorf("node names = %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[0].Devices) != 1 {
		t.Errorf("devices = %#v", got[0].Devices)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestClusterStorageNodes -v`
Expected: FAIL — `undefined: clusterStorageNodes`.

- [ ] **Step 3: Replace `writeClusterValues` with a node builder**

In `cmd/deploy.go`, delete `writeClusterValues` (lines 318-376) and the
`csiDriversValues` constant (lines 194-213), and add:

```go
// clusterStorageNodes resolves each worker's iSCSI disks to the device paths
// rook should claim. resolve is injectable so the mapping can be tested without
// an iSCSI session.
func clusterStorageNodes(cluster string, workers, disks int,
	resolve func(iqn string) (string, error)) []values.StorageNode {

	out := make([]values.StorageNode, 0, workers)
	for i := 0; i < workers; i++ {
		node := values.StorageNode{Name: workerNodeName(cluster, i)}
		for d := 0; d < disks; d++ {
			iqn := fmt.Sprintf("iqn.%s.local.rooket:%s-worker%d-disk%d", deployIQNDate, cluster, i, d)
			dev, err := resolve(iqn)
			if err != nil {
				run.Printf("    warning: worker %d disk %d unresolved: %v\n", i, d, err)
				continue
			}
			node.Devices = append(node.Devices, dev)
		}
		out = append(out, node)
	}
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/ -run TestClusterStorageNodes -v`
Expected: PASS.

- [ ] **Step 5: Rewire the three installs**

Replace the body of `installRookCephOperator` (from the `args := []string{...}`
block through the `run.CmdWithEnv` call) with:

```go
	base := values.OperatorBase(values.OperatorInput{
		ImageRepo: imageRepo,
		ImageTag:  imageTag,
		Digest:    digestOrEmpty(deployRegistryPort, deployNamespace+"/"+deployImageName, imageTag),
	})
	valuesPath, err := writeComposed(chartOperator, base, dir)
	if err != nil {
		return err
	}

	if err := run.CmdWithEnv(deployHelmEnv, "helm",
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployOperatorName, chartPath,
		"-f", valuesPath,
	); err != nil {
		return err
	}

	return installCephCsiDrivers(dir)
```

Add the two helpers:

```go
func digestOrEmpty(port int, repo, tag string) string {
	digest, ok := manifestDigest(port, repo, tag)
	if !ok {
		return ""
	}
	return digest
}

// writeComposed stacks every layer for chart and writes the result into the
// cluster's state dir, where it survives a failed deploy for inspection.
func writeComposed(chart string, base map[string]any, rookDir string) (string, error) {
	cloneDir := clone.Open(rookDir)
	names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
	if err != nil {
		return "", err
	}
	active, err := loadProfiles(names)
	if err != nil {
		return "", err
	}
	c, err := composeChart(chart, base, cloneDir, active, deployValueFiles)
	if err != nil {
		return "", err
	}
	dir, err := deployValuesDir(deployName)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, chart+".yaml")
	return path, c.write(path)
}

func deployValuesDir(cluster string) (string, error) {
	state, err := stateDirPath(cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "values"), nil
}
```

In `installCephCsiDrivers`, replace the `run.CmdWithStdinEnv(strings.NewReader(csiDriversValues), ...)`
call with a composed file:

```go
	valuesPath, err := writeComposed(chartCSI, values.CSIBase(), dir)
	if err != nil {
		return err
	}

	var installErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if installErr = run.CmdWithEnv(deployHelmEnv, "helm",
			"--kube-context", deployKubeContext,
			"-n", "rook-ceph",
			"upgrade", "--install",
			"ceph-csi-drivers", "ceph-csi-drivers",
			"--repo", "https://ceph.github.io/ceph-csi-operator",
			"--version", version,
			"-f", valuesPath,
		); installErr == nil {
			return nil
		}
		// The csi.ceph.io CRDs arrive with the rook-ceph chart applied moments
		// earlier and may not be established yet.
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("install ceph-csi-drivers chart: %w", installErr)
```

In `installRookCephCluster`, replace the `args` slice and the
`writeClusterValues` call with:

```go
	var nodes []values.StorageNode
	if deployWorkers > 0 && deployDiskCount > 0 {
		run.Printf("    storage:    %d node-device OSD(s) (one per worker)\n", deployWorkers*deployDiskCount)
		nodes = clusterStorageNodes(deployName, deployWorkers, deployDiskCount, waitForISCSIDevice)
	}
	base := values.ClusterBase(values.ClusterInput{OperatorNamespace: "rook-ceph", Nodes: nodes})
	valuesPath, err := writeComposed(chartCluster, base, dir)
	if err != nil {
		return err
	}

	return run.CmdWithEnv(deployHelmEnv, "helm",
		"--kube-context", deployKubeContext,
		"-n", "rook-ceph",
		"upgrade", "--install", "--create-namespace",
		deployClusterName, chartPath,
		"-f", valuesPath,
	)
```

Add the flag variables and registrations. In the `var (...)` block at the top of
`cmd/deploy.go` add:

```go
	deployWith         []string
	deployWithOnly     []string
	deployWithOnlySet  bool
	deployValueFiles   []string
```

and in `init()`, after the existing persistent flags:

```go
	pf.StringArrayVar(&deployWith, "with", nil, "profile to enable, in addition to the clone's sticky list (repeatable)")
	pf.StringArrayVar(&deployWithOnly, "with-only", nil, "profile to enable, replacing the clone's sticky list (repeatable)")
	pf.StringArrayVarP(&deployValueFiles, "values", "f", nil, "additional values file, applied above profiles (repeatable)")
```

Set `deployWithOnlySet` in `deploySetup`, which already receives the command.
It may only ever be set true here: `up` forwards its own `--with-only` through
`applyUpValueFlags` before calling `deployCmd.RunE`, and `deployCmd`'s own flag
is unset on that path, so an unconditional assignment would erase what `up`
just forwarded.

```go
	if cmd.Flags().Changed("with-only") {
		deployWithOnlySet = true
	}
```

Add `"github.com/jhoblitt/rooket/internal/clone"` and
`"github.com/jhoblitt/rooket/internal/values"` to the imports, and drop
`"strings"` if it is now unused.

- [ ] **Step 6: Verify the whole package builds and its tests pass**

Run: `go build ./... && go vet ./... && go test ./cmd/ ./internal/...`
Expected: PASS, with no reference to `csiDriversValues` or `writeClusterValues` remaining:

Run: `grep -rn 'csiDriversValues\|writeClusterValues\|"--set"' cmd/`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add cmd/deploy.go cmd/deploy_test.go
git commit -m "refactor(deploy): compose values files instead of --set"
```

---

### Task 9: Install the generated profiles chart

**Files:**
- Modify: `cmd/deploy.go` (call after `installRookCephCluster`)
- Create: `cmd/profilesrelease.go`
- Test: `cmd/profilesrelease_test.go`

**Interfaces:**
- Consumes: `profileschart.Render`, `profileschart.Source`, `profileschart.Context`, `clone.Dir.Templates`, `profiles.Profile.Templates`.
- Produces: `profileSources(cloneDir clone.Dir, active []profiles.Profile) ([]profileschart.Source, error)` and `installProfilesChart(dir string) error`.

- [ ] **Step 1: Write the failing test**

Create `cmd/profilesrelease_test.go`:

```go
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestProfileSourcesOrdersCloneFirst(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rooket", "templates", "scratch.yaml"),
		[]byte("kind: PersistentVolumeClaim\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := profileSources(d, []profiles.Profile{
		{Name: "rgw", Templates: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Prefix != "local" {
		t.Errorf("first prefix = %q, want local", got[0].Prefix)
	}
	if got[1].Prefix != "rgw" {
		t.Errorf("second prefix = %q, want rgw", got[1].Prefix)
	}
}

func TestProfileSourcesSkipsEmptyClone(t *testing.T) {
	got, err := profileSources(clone.Open(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want none", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestProfileSources -v`
Expected: FAIL — `undefined: profileSources`.

- [ ] **Step 3: Write the implementation**

Create `cmd/profilesrelease.go`:

```go
package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/profileschart"
	"github.com/jhoblitt/rooket/internal/run"
)

const profilesRelease = "rooket-profiles"

func profileSources(cloneDir clone.Dir, active []profiles.Profile) ([]profileschart.Source, error) {
	var out []profileschart.Source

	local, err := cloneDir.Templates()
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		out = append(out, profileschart.Source{Prefix: profiles.Reserved, Files: local})
	}
	for _, p := range active {
		if len(p.Templates) > 0 {
			out = append(out, profileschart.Source{Prefix: p.Name, Files: p.Templates})
		}
	}
	return out, nil
}

// installProfilesChart installs the resources contributed by the clone and the
// active profiles as their own release, so disabling a profile prunes what it
// owned on the next deploy.
func installProfilesChart(rookDir string) error {
	cloneDir := clone.Open(rookDir)
	names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
	if err != nil {
		return err
	}
	active, err := loadProfiles(names)
	if err != nil {
		return err
	}
	sources, err := profileSources(cloneDir, active)
	if err != nil {
		return err
	}

	state, err := stateDirPath(deployName)
	if err != nil {
		return err
	}
	chartDir := filepath.Join(state, "profiles-chart")

	any, err := profileschart.Render(chartDir, profileschart.Context{
		ClusterName:       deployName,
		Namespace:         "rook-ceph",
		OperatorNamespace: "rook-ceph",
		Workers:           deployWorkers,
	}, sources)
	if err != nil {
		return err
	}

	if !any {
		if err := run.CmdWithEnv(deployHelmEnv, "helm",
			"--kube-context", deployKubeContext, "-n", "rook-ceph",
			"uninstall", profilesRelease, "--ignore-not-found"); err != nil {
			return fmt.Errorf("uninstall %s: %w", profilesRelease, err)
		}
		return nil
	}

	run.Printf("==> deploying %s (%d source(s))\n", profilesRelease, len(sources))
	return run.CmdWithEnv(deployHelmEnv, "helm",
		"--kube-context", deployKubeContext, "-n", "rook-ceph",
		"upgrade", "--install", profilesRelease, chartDir)
}
```

- [ ] **Step 4: Call it from deploy**

In `cmd/deploy.go`, in `deployCmd.RunE` and `deployClusterCmd.RunE`, after the
`installRookCephCluster(dir)` call succeeds, add:

```go
		if err := installProfilesChart(dir); err != nil {
			return err
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./cmd/ -v -run TestProfileSources`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/profilesrelease.go cmd/profilesrelease_test.go cmd/deploy.go
git commit -m "feat(deploy): install profile and clone templates as their own release"
```

---

### Task 10: Forward the flags from `up`

**Files:**
- Modify: `cmd/up.go` (flag vars, `init()`, and the `[4/4] deploy` step at lines 113-128)
- Test: `cmd/up_values_test.go`

**Interfaces:**
- Consumes: `deployWith`, `deployWithOnly`, `deployWithOnlySet`, `deployValueFiles` from Task 8.
- Produces: `upWith`, `upWithOnly`, `upValueFiles` flag variables.

- [ ] **Step 1: Write the failing test**

Create `cmd/up_values_test.go`:

```go
package cmd

import "testing"

func TestUpForwardsValueFlags(t *testing.T) {
	t.Cleanup(func() {
		upWith, upWithOnly, upValueFiles = nil, nil, nil
		deployWith, deployWithOnly, deployValueFiles = nil, nil, nil
		deployWithOnlySet = false
	})

	upWith = []string{"rgw"}
	upWithOnly = []string{"rbd"}
	upValueFiles = []string{"/tmp/x.yaml"}

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
	if len(deployValueFiles) != 1 {
		t.Errorf("deployValueFiles = %#v", deployValueFiles)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestUpForwardsValueFlags -v`
Expected: FAIL — `undefined: upWith`.

- [ ] **Step 3: Write the implementation**

In `cmd/up.go`, add to the `var (...)` block:

```go
	upWith       []string
	upWithOnly   []string
	upValueFiles []string
```

Add the forwarding helper:

```go
func applyUpValueFlags(withOnlySet bool) {
	deployWith = upWith
	deployWithOnly = upWithOnly
	deployWithOnlySet = withOnlySet
	deployValueFiles = upValueFiles
}
```

Inside the `[4/4] deploy` step closure, alongside the other `deploy*`
assignments, add:

```go
			applyUpValueFlags(cmd.Flags().Changed("with-only"))
```

In `init()` for `upCmd`, register:

```go
	upCmd.Flags().StringArrayVar(&upWith, "with", nil, "profile to enable, in addition to the clone's sticky list (repeatable)")
	upCmd.Flags().StringArrayVar(&upWithOnly, "with-only", nil, "profile to enable, replacing the clone's sticky list (repeatable)")
	upCmd.Flags().StringArrayVarP(&upValueFiles, "values", "f", nil, "additional values file, applied above profiles (repeatable)")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./cmd/ -run TestUpForwardsValueFlags -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/up.go cmd/up_values_test.go
git commit -m "feat(up): forward profile and values flags to deploy"
```

---

### Task 11: `rooket values show`

**Files:**
- Create: `cmd/values.go`
- Test: `cmd/values_show_test.go`

**Interfaces:**
- Consumes: `composeChart`, `chartName`, `values.OperatorBase/ClusterBase/CSIBase`, `resolveRookDir`.
- Produces: `valuesCmd` registered on `rootCmd`, and
  `renderShow(c composed, withLayers bool) (string, error)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/values_show_test.go`:

```go
package cmd

import (
	"strings"
	"testing"
)

func TestRenderShow(t *testing.T) {
	c := composed{
		Merged:     map[string]any{"a": 1, "m": map[string]any{"b": 2}},
		Provenance: map[string]string{"a": "rooket base", "m.b": "profile:rgw"},
	}

	t.Run("plain yaml", func(t *testing.T) {
		got, err := renderShow(c, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "a: 1") {
			t.Errorf("got %q", got)
		}
		if strings.Contains(got, "profile:rgw") {
			t.Errorf("provenance leaked into plain output: %q", got)
		}
	})

	t.Run("with layers", func(t *testing.T) {
		got, err := renderShow(c, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"a", "rooket base", "m.b", "profile:rgw"} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestRenderShow -v`
Expected: FAIL — `undefined: renderShow`.

- [ ] **Step 3: Write the implementation**

Create `cmd/values.go`:

```go
package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/values"
)

var (
	valuesDir        string
	valuesShowLayers bool
)

var valuesCmd = &cobra.Command{
	Use:   "values",
	Short: "Inspect and edit the Helm values rooket deploys",
	Long: `values manages the layered chart values rooket supplies to the rook charts.

Layers, lowest first: rooket's generated base, the clone's .rooket/values/,
each active profile in selection order, then any -f files.
`,
}

var valuesShowCmd = &cobra.Command{
	Use:   "show [chart]",
	Short: "Print the merged values rooket would deploy",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		charts := []string{chartOperator, chartCluster, chartCSI}
		if len(args) == 1 {
			c, err := chartName(args[0])
			if err != nil {
				return err
			}
			charts = []string{c}
		}

		cloneDir := clone.Open(dir)
		names, err := activeProfileNames(cloneDir, deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		active, err := loadProfiles(names)
		if err != nil {
			return err
		}

		for i, chart := range charts {
			c, err := composeChart(chart, showBase(chart), cloneDir, active, deployValueFiles)
			if err != nil {
				return err
			}
			out, err := renderShow(c, valuesShowLayers)
			if err != nil {
				return err
			}
			if i > 0 {
				fmt.Println("---")
			}
			fmt.Printf("# %s\n%s", chart, out)
		}
		return nil
	},
}

// showBase reproduces the generated layer without contacting the registry or
// an iSCSI session: show runs against a cluster that may not exist, so the
// image digest and resolved device paths are deliberately absent.
func showBase(chart string) map[string]any {
	switch chart {
	case chartOperator:
		return values.OperatorBase(values.OperatorInput{
			ImageRepo: fmt.Sprintf("localhost:%d/%s/%s", deployRegistryPort, deployNamespace, deployImageName),
			ImageTag:  "<git ref>",
		})
	case chartCSI:
		return values.CSIBase()
	default:
		return values.ClusterBase(values.ClusterInput{OperatorNamespace: "rook-ceph"})
	}
}

func renderShow(c composed, withLayers bool) (string, error) {
	data, err := values.Encode(c.Merged)
	if err != nil {
		return "", err
	}
	if !withLayers {
		return string(data), nil
	}
	paths := make([]string, 0, len(c.Provenance))
	for p := range c.Provenance {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.Write(data)
	b.WriteString("\n# layers\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "#   %-60s %s\n", p, c.Provenance[p])
	}
	return b.String(), nil
}

func init() {
	rootCmd.AddCommand(valuesCmd)
	valuesCmd.AddCommand(valuesShowCmd)

	// cmd.Flags() on a subcommand includes inherited persistent flags, so this
	// sees --with-only wherever it was given under `values`.
	valuesCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		deployWithOnlySet = cmd.Flags().Changed("with-only")
		return nil
	}

	pf := valuesCmd.PersistentFlags()
	pf.StringVar(&valuesDir, "dir", "", "path to the rook source directory (default: current directory)")
	// Bound to deploy's variables so the profile selection a user previews here
	// is the same one composeChart resolves during a deploy.
	pf.StringArrayVar(&deployWith, "with", nil, "profile to enable, in addition to the clone's sticky list (repeatable)")
	pf.StringArrayVar(&deployWithOnly, "with-only", nil, "profile to enable, replacing the clone's sticky list (repeatable)")
	pf.StringArrayVarP(&deployValueFiles, "values", "f", nil, "additional values file, applied above profiles (repeatable)")

	valuesShowCmd.Flags().BoolVar(&valuesShowLayers, "layers", false, "annotate each key with the layer that set it")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./cmd/ -run TestRenderShow -v`
Expected: PASS.

Then confirm the command wires up:

Run: `go run . values show cluster --dir $(pwd)`
Expected: YAML including `toolbox:\n  enabled: true` and `cephClusterSpec:`.

- [ ] **Step 5: Commit**

```bash
git add cmd/values.go cmd/values_show_test.go
git commit -m "feat(values): add 'rooket values show'"
```

---

### Task 12: `rooket values edit`

**Files:**
- Create: `cmd/valuesedit.go`
- Test: `cmd/valuesedit_test.go`

**Interfaces:**
- Consumes: `clone.Open`, `clone.Dir.Ensure`, `clone.Dir.ValuesPath`, `values.LoadFile`, `values.Encode`, `showBase`.
- Produces: `editValues(path string, seed []byte, edit func(string) error) error` — injectable editor for testing — and `seedFor(chart string) ([]byte, error)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/valuesedit_test.go`:

```go
package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditValuesSeedsWhenAbsent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	seed := []byte("# rooket base\n# toolbox:\n#   enabled: true\n")

	var sawContent string
	err := editValues(p, seed, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sawContent = string(data)
		return os.WriteFile(path, []byte("toolbox:\n  enabled: false\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawContent, "# toolbox:") {
		t.Errorf("editor did not see the seed: %q", sawContent)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "enabled: false") {
		t.Errorf("saved file = %q", got)
	}
}

func TestEditValuesRemovesEmptyResult(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := editValues(p, nil, func(path string) error {
		return os.WriteFile(path, []byte("# everything commented out\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty result should remove the layer file")
	}
}

func TestEditValuesReopensOnParseError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "values.yaml")
	calls := 0
	err := editValues(p, nil, func(path string) error {
		calls++
		if calls == 1 {
			return os.WriteFile(path, []byte("a: [1,\n"), 0o644)
		}
		return os.WriteFile(path, []byte("a: 1\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("editor called %d times, want 2", calls)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a: 1\n" {
		t.Errorf("saved %q", data)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestEditValues -v`
Expected: FAIL — `undefined: editValues`.

- [ ] **Step 3: Write the implementation**

Create `cmd/valuesedit.go`:

```go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/run"
	"github.com/jhoblitt/rooket/internal/values"
)

var valuesEditCmd = &cobra.Command{
	Use:   "edit [chart]",
	Short: "Edit this clone's values overrides in $EDITOR",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		charts := []string{chartOperator, chartCluster, chartCSI}
		if len(args) == 1 {
			c, err := chartName(args[0])
			if err != nil {
				return err
			}
			charts = []string{c}
		}

		cloneDir := clone.Open(dir)
		if err := cloneDir.Ensure(); err != nil {
			return err
		}
		for _, chart := range charts {
			seed, err := seedFor(chart)
			if err != nil {
				return err
			}
			if err := editValues(cloneDir.ValuesPath(chart), seed, launchEditor); err != nil {
				return err
			}
		}
		return nil
	},
}

// seedFor renders rooket's generated layer as commented YAML. Knowing which of
// the chart's keys exist and what rooket already set is the hard part of
// overriding one, so a new file starts as the answer to both.
func seedFor(chart string) ([]byte, error) {
	data, err := values.Encode(showBase(chart))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s overrides for this clone.\n", chart)
	b.WriteString("# Uncomment and edit to override; delete everything to drop this layer.\n")
	b.WriteString("# Below is rooket's generated base — your values merge on top of it.\n#\n")
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		fmt.Fprintf(&b, "# %s\n", line)
	}
	return []byte(b.String()), nil
}

// editValues opens path in the editor, reopening on a parse error rather than
// saving a broken layer that would fail later inside helm upgrade. A result
// with no keys removes the file instead of leaving an empty layer.
func editValues(path string, seed []byte, edit func(string) error) error {
	tmp, err := os.CreateTemp("", "rooket-values-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		_, err = tmp.Write(existing)
	case os.IsNotExist(err):
		_, err = tmp.Write(seed)
	}
	if err != nil {
		return fmt.Errorf("seed temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	for {
		if err := edit(tmp.Name()); err != nil {
			return err
		}
		m, err := values.LoadFile(tmp.Name())
		if err == nil {
			if len(m) == 0 {
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					return fmt.Errorf("remove %s: %w", path, rmErr)
				}
				run.Printf("==> %s left empty; layer removed\n", filepath.Base(path))
				return nil
			}
			data, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			run.Printf("==> wrote %s\n", path)
			return nil
		}
		run.Printf("==> %v\n==> reopening the editor\n", err)
	}
}

func launchEditor(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	// $EDITOR routinely carries arguments ("code --wait", "emacsclient -nw"), so
	// it has to go through a shell rather than exec.Command(ed, path). The path
	// is passed as the positional $1 instead of being interpolated into the
	// command string, so it cannot inject; the only code that runs is whatever
	// the user already put in $EDITOR.
	c := exec.Command("sh", "-c", ed+" \"$1\"", "sh", path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func init() {
	valuesCmd.AddCommand(valuesEditCmd)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./cmd/ -run TestEditValues -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/valuesedit.go cmd/valuesedit_test.go
git commit -m "feat(values): add 'rooket values edit' with a seeded editor"
```

---

### Task 13: `rooket values profiles` and `fork`

**Files:**
- Create: `cmd/valuesprofiles.go`
- Test: `cmd/valuesprofiles_test.go`

**Interfaces:**
- Consumes: `profiles.List`, `profiles.Fork`, `clone.Dir.Profiles`, `userProfileDir`.
- Produces: `renderProfileList(all []profiles.Profile, active []string) string`.

- [ ] **Step 1: Write the failing test**

Create `cmd/valuesprofiles_test.go`:

```go
package cmd

import (
	"strings"
	"testing"

	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestRenderProfileList(t *testing.T) {
	got := renderProfileList([]profiles.Profile{
		{Name: "rbd", Description: "block storage", BuiltIn: true},
		{Name: "mine", Description: "my thing", BuiltIn: false},
	}, []string{"rbd"})

	if !strings.Contains(got, "built-in") {
		t.Errorf("missing built-in marker:\n%s", got)
	}
	if !strings.Contains(got, "user") {
		t.Errorf("missing user marker:\n%s", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	var rbdLine string
	for _, l := range lines {
		if strings.Contains(l, "rbd") {
			rbdLine = l
		}
	}
	if !strings.Contains(rbdLine, "*") {
		t.Errorf("active profile not marked: %q", rbdLine)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/ -run TestRenderProfileList -v`
Expected: FAIL — `undefined: renderProfileList`.

- [ ] **Step 3: Write the implementation**

Create `cmd/valuesprofiles.go`:

```go
package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
	"github.com/jhoblitt/rooket/internal/run"
)

var valuesProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List the available profiles, marking the ones this clone enables",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := resolveRookDir(valuesDir)
		if err != nil {
			return err
		}
		userDir, err := userProfileDir()
		if err != nil {
			return err
		}
		all, err := profiles.List(userDir)
		if err != nil {
			return err
		}
		active, err := activeProfileNames(clone.Open(dir), deployWith, deployWithOnly, deployWithOnlySet)
		if err != nil {
			return err
		}
		fmt.Print(renderProfileList(all, active))
		return nil
	},
}

var valuesProfilesForkCmd = &cobra.Command{
	Use:   "fork <profile>",
	Short: "Copy a built-in profile into ~/.config/rooket/profiles to edit",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		userDir, err := userProfileDir()
		if err != nil {
			return err
		}
		dst, err := profiles.Fork(userDir, args[0])
		if err != nil {
			return err
		}
		run.Printf("==> forked %s to %s\n", args[0], dst)
		return nil
	},
}

func renderProfileList(all []profiles.Profile, active []string) string {
	on := make(map[string]bool, len(active))
	for _, n := range active {
		on[n] = true
	}
	var b strings.Builder
	for _, p := range all {
		mark := " "
		if on[p.Name] {
			mark = "*"
		}
		origin := "user"
		if p.BuiltIn {
			origin = "built-in"
		}
		fmt.Fprintf(&b, " %s %-12s (%-8s) %s\n", mark, p.Name, origin, p.Description)
	}
	return b.String()
}

func init() {
	valuesCmd.AddCommand(valuesProfilesCmd)
	valuesProfilesCmd.AddCommand(valuesProfilesForkCmd)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./cmd/ -run TestRenderProfileList -v`
Expected: PASS.

Run: `go run . values profiles --dir $(pwd)`
Expected: a line for `rbd` marked `(built-in)`.

- [ ] **Step 5: Commit**

```bash
git add cmd/valuesprofiles.go cmd/valuesprofiles_test.go
git commit -m "feat(values): list and fork profiles"
```

---

### Task 14: The `rbd` built-in profile

**Files:**
- Modify: `internal/profiles/builtin/rbd/profile.yaml`
- Create: `internal/profiles/builtin/rbd/templates/10-pvc.yaml`
- Create: `internal/profiles/builtin/rbd/templates/20-pod.yaml`
- Delete: `internal/profiles/builtin/rbd/templates/.keep`
- Test: `internal/profiles/builtin_test.go`

**Interfaces:**
- Consumes: `profiles.Load`.
- Produces: a loadable `rbd` profile with two templates and no values overlay.

The chart's default `values.yaml` already enables `cephBlockPools[ceph-blockpool]`
with the `ceph-block` StorageClass, so this profile needs no values overlay —
only a consumer of that StorageClass. Templates are adapted from
`deploy/examples/csi/rbd/pvc.yaml` and `deploy/examples/csi/rbd/pod.yaml`.

- [ ] **Step 1: Write the failing test**

Create `internal/profiles/builtin_test.go`:

```go
package profiles

import (
	"strings"
	"testing"
)

func TestBuiltInProfilesLoad(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"rbd"} {
		t.Run(name, func(t *testing.T) {
			p, err := Load(dir, name)
			if err != nil {
				t.Fatal(err)
			}
			if !p.BuiltIn {
				t.Error("want BuiltIn")
			}
			if p.Description == "" {
				t.Error("want a description")
			}
			if len(p.Templates) == 0 {
				t.Error("want at least one template")
			}
			for file, data := range p.Templates {
				if !strings.Contains(string(data), "kind:") {
					t.Errorf("%s has no kind: %q", file, data)
				}
			}
		})
	}
}

func TestRBDProfileHasNoValuesOverlay(t *testing.T) {
	p, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Values) != 0 {
		t.Errorf("rbd should rely on the chart's default block pool, got %#v", p.Values)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/profiles/ -run TestBuiltInProfilesLoad -v`
Expected: FAIL — no templates (only `.keep`, which is not YAML).

- [ ] **Step 3: Write the profile**

Replace `internal/profiles/builtin/rbd/profile.yaml`:

```yaml
description: RBD — a PVC on the chart's default ceph-block StorageClass and a pod writing to it
```

Create `internal/profiles/builtin/rbd/templates/10-pvc.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rooket-rbd-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ceph-block
```

Create `internal/profiles/builtin/rbd/templates/20-pod.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rooket-rbd-smoke
spec:
  containers:
    - name: writer
      image: busybox:1.36
      command: ["sh", "-c", "echo rooket > /mnt/rbd/hello && sleep infinity"]
      volumeMounts:
        - name: vol
          mountPath: /mnt/rbd
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: rooket-rbd-pvc
```

Delete the placeholder:

```bash
rm internal/profiles/builtin/rbd/templates/.keep
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profiles/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profiles/
git commit -m "feat(profiles): add the rbd built-in profile"
```

---

### Task 15: The `rgw` built-in profile

**Files:**
- Create: `internal/profiles/builtin/rgw/profile.yaml`
- Create: `internal/profiles/builtin/rgw/templates/10-objectstoreuser.yaml`
- Create: `internal/profiles/builtin/rgw/templates/20-obc.yaml`
- Create: `internal/profiles/builtin/rgw/templates/30-pod.yaml`
- Modify: `internal/profiles/builtin_test.go` (extend the name list)

**Interfaces:**
- Consumes: `profiles.Load`.
- Produces: a loadable `rgw` profile with three templates and no values overlay.

The chart's defaults already enable `cephObjectStores[ceph-objectstore]` and its
`ceph-bucket` StorageClass. Templates are adapted from
`deploy/examples/object-user.yaml` and `deploy/examples/object-bucket-claim-a.yaml`.

- [ ] **Step 1: Extend the failing test**

In `internal/profiles/builtin_test.go`, change the name list:

```go
	for _, name := range []string{"rbd", "rgw"} {
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/profiles/ -run TestBuiltInProfilesLoad -v`
Expected: FAIL — `unknown profile "rgw"`.

- [ ] **Step 3: Write the profile**

Create `internal/profiles/builtin/rgw/profile.yaml`:

```yaml
description: Object store — a CephObjectStoreUser, an OBC on the default ceph-bucket StorageClass, and an s3 client pod
```

Create `internal/profiles/builtin/rgw/templates/10-objectstoreuser.yaml`:

```yaml
apiVersion: ceph.rook.io/v1
kind: CephObjectStoreUser
metadata:
  name: rooket-rgw-user
spec:
  store: ceph-objectstore
  displayName: rooket smoke user
```

Create `internal/profiles/builtin/rgw/templates/20-obc.yaml`:

```yaml
apiVersion: objectbucket.io/v1alpha1
kind: ObjectBucketClaim
metadata:
  name: rooket-rgw-bucket
spec:
  generateBucketName: rooket
  storageClassName: ceph-bucket
```

Create `internal/profiles/builtin/rgw/templates/30-pod.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rooket-rgw-smoke
spec:
  containers:
    - name: s3
      image: amazon/aws-cli:2.15.17
      command: ["sh", "-c", "sleep infinity"]
      env:
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: rooket-rgw-bucket
              key: AWS_ACCESS_KEY_ID
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: rooket-rgw-bucket
              key: AWS_SECRET_ACCESS_KEY
        - name: BUCKET_NAME
          valueFrom:
            configMapKeyRef:
              name: rooket-rgw-bucket
              key: BUCKET_NAME
        - name: BUCKET_HOST
          valueFrom:
            configMapKeyRef:
              name: rooket-rgw-bucket
              key: BUCKET_HOST
        - name: BUCKET_PORT
          valueFrom:
            configMapKeyRef:
              name: rooket-rgw-bucket
              key: BUCKET_PORT
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profiles/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profiles/
git commit -m "feat(profiles): add the rgw built-in profile"
```

---

### Task 16: The `nfs` built-in profile

**Files:**
- Create: `internal/profiles/builtin/nfs/profile.yaml`
- Create: `internal/profiles/builtin/nfs/values/ceph-csi-drivers.yaml`
- Create: `internal/profiles/builtin/nfs/templates/10-cephnfs.yaml`
- Create: `internal/profiles/builtin/nfs/templates/20-storageclass.yaml`
- Create: `internal/profiles/builtin/nfs/templates/30-pvc.yaml`
- Create: `internal/profiles/builtin/nfs/templates/40-pod.yaml`
- Modify: `internal/profiles/builtin_test.go`

**Interfaces:**
- Consumes: `profiles.Load`.
- Produces: a loadable `nfs` profile whose values overlay flips
  `drivers.nfs.enabled` to `true` on the `ceph-csi-drivers` chart.

This is the profile that proves the layering reaches every chart: rooket's
generated CSI base sets `drivers.nfs.enabled: false`, and only a layer above it
can turn the driver on. The chart's default `cephFileSystems` already provides
the filesystem backing the exports. Templates are adapted from
`deploy/examples/nfs.yaml` and `deploy/examples/csi/nfs/{storageclass,pvc,pod}.yaml`.

- [ ] **Step 1: Extend the failing test**

In `internal/profiles/builtin_test.go`, change the name list and add an overlay assertion:

```go
	for _, name := range []string{"rbd", "rgw", "nfs"} {
```

```go
func TestNFSProfileEnablesTheDriver(t *testing.T) {
	p, err := Load(t.TempDir(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	drivers, ok := p.Values["ceph-csi-drivers"]["drivers"].(map[string]any)
	if !ok {
		t.Fatalf("values = %#v", p.Values)
	}
	if drivers["nfs"].(map[string]any)["enabled"] != true {
		t.Errorf("nfs driver not enabled: %#v", drivers["nfs"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/profiles/ -run 'TestBuiltInProfilesLoad|TestNFSProfile' -v`
Expected: FAIL — `unknown profile "nfs"`.

- [ ] **Step 3: Write the profile**

Create `internal/profiles/builtin/nfs/profile.yaml`:

```yaml
description: NFS — enables the NFS CSI driver, a CephNFS server, and a pod mounting an export
```

Create `internal/profiles/builtin/nfs/values/ceph-csi-drivers.yaml`:

```yaml
drivers:
  nfs:
    enabled: true
```

Create `internal/profiles/builtin/nfs/templates/10-cephnfs.yaml`:

```yaml
apiVersion: ceph.rook.io/v1
kind: CephNFS
metadata:
  name: rooket-nfs
spec:
  server:
    active: 1
```

Create `internal/profiles/builtin/nfs/templates/20-storageclass.yaml`:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: rooket-nfs
provisioner: rook-ceph.nfs.csi.ceph.com
parameters:
  nfsCluster: rooket-nfs
  server: rook-ceph-nfs-rooket-nfs-a
  clusterID: {{ .Values.rooket.namespace }}
  fsName: ceph-filesystem
  pool: ceph-filesystem-data0
  csi.storage.k8s.io/provisioner-secret-name: rook-csi-cephfs-provisioner
  csi.storage.k8s.io/provisioner-secret-namespace: {{ .Values.rooket.namespace }}
  csi.storage.k8s.io/controller-expand-secret-name: rook-csi-cephfs-provisioner
  csi.storage.k8s.io/controller-expand-secret-namespace: {{ .Values.rooket.namespace }}
  csi.storage.k8s.io/node-stage-secret-name: rook-csi-cephfs-node
  csi.storage.k8s.io/node-stage-secret-namespace: {{ .Values.rooket.namespace }}
reclaimPolicy: Delete
```

Create `internal/profiles/builtin/nfs/templates/30-pvc.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rooket-nfs-pvc
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 1Gi
  storageClassName: rooket-nfs
```

Create `internal/profiles/builtin/nfs/templates/40-pod.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rooket-nfs-smoke
spec:
  containers:
    - name: writer
      image: busybox:1.36
      command: ["sh", "-c", "echo rooket > /mnt/nfs/hello && sleep infinity"]
      volumeMounts:
        - name: vol
          mountPath: /mnt/nfs
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: rooket-nfs-pvc
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profiles/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profiles/
git commit -m "feat(profiles): add the nfs built-in profile"
```

---

### Task 17: End-to-end coverage

**Files:**
- Create: `test/e2e/profiles_test.go`
- Reference: `test/e2e/updown_test.go` (helpers `rooketRun`, `tail`, `rookDir`, `clusterName`, `workers`, `skipBlock`)

**Interfaces:**
- Consumes: the e2e suite's existing helpers; `rooket up`, `rooket deploy cluster`, `rooket values show`, `rooket helm`, `rooket k`.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/profiles_test.go`:

```go
//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func kubectl(args ...string) (string, error) {
	return rooketRun(2*time.Minute, append([]string{"k"}, args...)...)
}

func podPhase(name string) string {
	out, err := kubectl("-n", "rook-ceph", "get", "pod", name, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func pvcPhase(name string) string {
	out, err := kubectl("-n", "rook-ceph", "get", "pvc", name, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

var _ = Describe("rooket profiles", Ordered, func() {
	scratch := filepath.Join(rookDir, ".rooket", "templates", "scratch-cm.yaml")

	BeforeAll(func() {
		Expect(os.MkdirAll(filepath.Dir(scratch), 0o755)).To(Succeed())
		Expect(os.WriteFile(scratch, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: rooket-scratch
data:
  from: clone-templates
`), 0o644)).To(Succeed())
	})

	It("installs every built-in profile", func() {
		args := []string{"up", "--dir", rookDir, "--workers", workers, "--name", clusterName,
			"--with-only", "rbd", "--with-only", "rgw", "--with-only", "nfs"}
		if skipBlock {
			args = append(args, "--skip-block")
		}
		out, err := rooketRun(40*time.Minute, args...)
		Expect(err).NotTo(HaveOccurred(), "rooket up failed:\n%s", tail(out, 40))

		By("binding the rbd PVC and running its pod")
		Eventually(func() string { return pvcPhase("rooket-rbd-pvc") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-rbd-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("binding the OBC and running the s3 pod")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "obc", "rooket-rgw-bucket",
				"-o", "jsonpath={.status.phase}")
			return strings.TrimSpace(out)
		}, 5*time.Minute, 10*time.Second).Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-rgw-smoke") }, 5*time.Minute, 10*time.Second).
			Should(Equal("Running"))

		By("binding the nfs PVC and running its pod")
		Eventually(func() string { return pvcPhase("rooket-nfs-pvc") }, 10*time.Minute, 15*time.Second).
			Should(Equal("Bound"))
		Eventually(func() string { return podPhase("rooket-nfs-smoke") }, 10*time.Minute, 15*time.Second).
			Should(Equal("Running"))

		By("installing the clone's own template")
		Eventually(func() string {
			out, _ := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch",
				"-o", "jsonpath={.data.from}")
			return strings.TrimSpace(out)
		}, 2*time.Minute, 5*time.Second).Should(Equal("clone-templates"))
	})

	It("prunes the profiles that are switched off, keeping clone templates", func() {
		out, err := rooketRun(15*time.Minute, "deploy", "cluster",
			"--dir", rookDir, "--name", clusterName, "--with-only", "rbd")
		Expect(err).NotTo(HaveOccurred(), "deploy failed:\n%s", tail(out, 40))

		Eventually(func() string { return podPhase("rooket-rgw-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "rgw pod was not pruned")
		Eventually(func() string { return podPhase("rooket-nfs-smoke") }, 3*time.Minute, 5*time.Second).
			Should(BeEmpty(), "nfs pod was not pruned")

		Expect(podPhase("rooket-rbd-smoke")).To(Equal("Running"), "rbd pod should survive")

		cm, err := kubectl("-n", "rook-ceph", "get", "cm", "rooket-scratch", "-o", "jsonpath={.data.from}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(cm)).To(Equal("clone-templates"), "clone template must not be pruned")
	})

	It("shows exactly the values helm received", func() {
		for _, c := range []struct{ chart, release string }{
			{"cluster", "rook-ceph-cluster"},
			{"operator", "rook-ceph"},
		} {
			shown, err := rooketRun(2*time.Minute, "values", "show", c.chart,
				"--dir", rookDir, "--with-only", "rbd")
			Expect(err).NotTo(HaveOccurred())

			supplied, err := rooketRun(2*time.Minute, "helm", "-n", "rook-ceph",
				"get", "values", c.release, "-o", "yaml")
			Expect(err).NotTo(HaveOccurred())

			for _, key := range []string{"toolbox", "cephClusterSpec", "image"} {
				if strings.Contains(supplied, key+":") {
					Expect(shown).To(ContainSubstring(key+":"),
						"%s: preview is missing %q that helm received", c.chart, key)
				}
			}
		}
	})

	AfterAll(func() {
		Expect(os.Remove(scratch)).To(Succeed())
	})
})
```

- [ ] **Step 2: Verify the suite compiles**

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output.

- [ ] **Step 3: Run the suite**

Run: `go test -tags e2e ./test/e2e/ -run TestE2E -v -timeout 90m`
Expected: PASS. This needs a working host (podman/docker, iSCSI tooling, kind).

- [ ] **Step 4: Commit**

```bash
git add test/e2e/profiles_test.go
git commit -m "test(e2e): cover profile install, prune, and the values preview invariant"
```

---

### Task 18: Document the feature

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a section after "Clusters and state"**

```markdown
## Chart values and profiles

rooket composes the Helm values for each chart from layers, lowest first:

1. the chart's own `values.yaml`
2. rooket's generated base (image refs, OSD device pinning, dev-host cpu trims)
3. `<rook clone>/.rooket/values/<chart>.yaml` — sticky, this clone
4. active profiles, in selection order
5. `-f` files, then `--set`

Nothing is locked: a values file can retarget the operator image or replace the
storage topology outright.

```console
$ rooket values show cluster          # what would be deployed
$ rooket values show cluster --layers # ...and which layer set each key
$ rooket values edit cluster          # $EDITOR, seeded with the generated base
$ rooket values profiles              # available profiles, active ones marked *
$ rooket values profiles fork rgw     # copy a built-in to hack on
```

Profiles bundle values overrides with Kubernetes resources the rook charts do
not template. Built-ins: `rbd` (PVC + pod on the default `ceph-block` class),
`rgw` (object store user, OBC, s3 client pod), and `nfs` (enables the NFS CSI
driver, a CephNFS server, and a pod mounting an export).

```console
$ rooket up --with rgw                # sticky list plus rgw
$ rooket up --with-only rbd           # rbd alone, this run
```

Enable profiles for a clone by listing them in `.rooket/config.yaml`; later
entries win when two profiles set the same key.

```yaml
profiles: [rbd, rgw]
```

Drop any manifest into `.rooket/templates/` and it is installed alongside the
active profiles' resources — and pruned when you delete the file. Both live in
a generated `rooket-profiles` Helm release, so removing a source removes its
resources.
```

- [ ] **Step 2: Verify the examples**

Run: `go run . values profiles --dir $(pwd) && go run . values show cluster --dir $(pwd) | head -20`
Expected: the profile list and merged cluster values print without error.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document chart value overrides and profiles"
```

---

## Self-Review Notes

**Spec coverage.** Every spec section maps to a task: layout → 4; precedence →
7; merge semantics → 1; profile anatomy → 5; generated profiles chart → 6, 9;
readiness (no waiting) → 9 installs without `--wait`; built-in profiles → 14-16;
CLI → 11-13; changes to existing code → 8, 10; testing → every task plus 17;
deferred items are not implemented, as intended.

**Known gaps, deliberate.** `$ROOKET_PROFILES` is specified but has no task —
it is one `os.Getenv` call in `activeProfileNames` and is folded into Task 7's
implementation only if wanted; it is not covered by a test there. Flag it during
execution if you want it, or drop it from the spec. The TUI (`rooket values`
with no subcommand) is Project B and intentionally absent, so bare `rooket
values` prints cobra's help until then.
