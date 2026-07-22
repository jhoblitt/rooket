# Chart value overrides and profiles

Date: 2026-07-21
Status: approved, not yet implemented

## Problem

rooket's Helm values are hardcoded across three unrelated mechanisms, none of
which a user can influence without editing rooket's source:

- `--set` flags in `installRookCephOperator` and `installRookCephCluster`
  (`cmd/deploy.go`),
- the `csiDriversValues` Go string constant for the `ceph-csi-drivers` chart,
- a temp file built by string concatenation in `writeClusterValues`.

Changing anything — enabling an object store, giving the mons more memory,
turning on the NFS CSI driver — means patching rooket and rebuilding it. The
split between `--set` and the generated file is also accidental rather than
designed: Helm ranks `--set` above every `-f`, so the settings that are most
worth changing are the ones most firmly locked.

## Goals

- A user can override any value rooket supplies, for any of the three charts.
- Overrides are sticky per rook clone, and composable profiles can be toggled
  on top of them.
- rooket ships useful profiles out of the box, with integration test coverage.
- The effective values are inspectable before and after a deploy.

## Non-goals

- GitOps-style reconciliation (flux, fleet). Rejected: a reconciliation
  controller adds a second suspect to every "why didn't my change apply"
  question in a cluster whose purpose is debugging rook itself.
- Collapsing the three releases into one umbrella chart. Rejected: rooket
  installs them in a deliberate order with a retry loop, because the
  `csi.ceph.io` CRDs arrive with the operator chart and are not established
  when the drivers chart applies. Helm does not order subcharts.
- Waiting for resources to become ready (see "Readiness").

## Decomposition

Two projects, each with its own spec and plan:

- **Project A — the layering engine.** Headless, fully testable, useful alone.
  Specified in full below.
- **Project B — the `rooket values` TUI.** Built on A. Sketched at the end;
  gets its own spec once A exists and there are real profiles to render.

## Layout

The rook clone is the unit of identity, matching how rooket already derives
cluster names, state directories, and registry ports from the clone path.

```
~/github/rook3/                        # the rook clone
└── .rooket/
    ├── .gitignore                     # "*" — self-ignoring, keeps git status clean
    ├── config.yaml                    # sticky profile selection
    ├── values/
    │   ├── rook-ceph.yaml             # sticky per-chart overrides
    │   ├── rook-ceph-cluster.yaml
    │   └── ceph-csi-drivers.yaml
    └── templates/                     # sticky ad-hoc resources, always active
        └── scratch-pvc.yaml

~/.config/rooket/profiles/<name>/      # user profiles; shadow built-ins by name
```

`.rooket/.gitignore` containing `*` makes git suppress the directory entirely,
including the ignore file itself, so the rook checkout reports clean without
rooket touching `.git/info/exclude` or the tracked `.gitignore`. `git add -f`
still works for anyone who wants to commit one to a scratch branch.

`.rooket/templates/` is the sticky counterpart to `.rooket/values/` for
resources rather than values: drop a manifest in and the next deploy installs
it, delete it and the next deploy prunes it. It is always active — there is
nothing to enable — and it exists so that a one-off PVC or debug pod does not
require authoring a profile under `~/.config/rooket/`. It feeds the same
generated chart as profile templates (see "The generated profiles chart"), so it
gets the same lifecycle, the same Helm templating, and the same rooket context.

User overrides must not live in `~/.local/share/rooket/<name>/`: that directory
is removed by `down --delete-disks` and `down --all`.

`.rooket/config.yaml`:

```yaml
profiles: [rbd, rgw]     # ordered; later entries win
```

## Precedence

Lowest to highest:

1. the chart's own `values.yaml` (Helm)
2. rooket's generated base — image refs, digest annotation, iSCSI device
   pinning, dev-host cpu trims
3. `.rooket/values/<chart>.yaml` — sticky, this clone
4. active profiles, in list order (sticky list first, then `--with` appends)
5. `-f` / `--values` files, in command-line order
6. `--set` / `--set-string`, passed through to Helm

Nothing is locked. A user file can retarget `image.repository` at an upstream
image or replace the storage topology outright; rooket does not argue.

Profiles outrank the sticky layer because a profile is explicitly requested for
this run while the sticky layer is ambient. The cost — a sticky tweak silently
undone by enabling a profile — is mitigated by provenance reporting
(`values show --layers`, and the TUI's preview in Project B).

Layers 2-5 are merged by rooket into a single file per chart. Layer 6 is handed
to Helm verbatim and therefore is *not* reflected in `values show`; that command
prints a note when `--set` was used.

## Merge semantics

`internal/values`:

```go
type Layer struct {
    Name   string           // "rooket base", ".rooket/values", "profile:rgw", "-f extra.yaml"
    Values map[string]any
}

func Merge(layers []Layer) (merged map[string]any, provenance map[string]string, err error)
```

Rules, applying layers lowest first:

- maps deep-merge;
- an explicit `null` deletes the key (Helm-compatible);
- a list merges **by name** when it is non-empty, every element is a map, every
  element has a string `name`, and those names are unique within the list.
  Matching elements deep-merge; new names append in layer order.
- every other list replaces.

Name-keyed merging is what makes simultaneous profiles work: `rbd` and a second
pool profile both writing `cephBlockPools[]` would, under Helm's replace
semantics, silently leave only one pool and strand the other profile's test pod
on a StorageClass that no longer exists. The rule matches `kubectl` strategic
merge for named lists, so it is not an invented idiom. To replace a named list
instead of merging it, set it to `null` in a lower position and re-add.

`provenance` maps each leaf path to the layer that won it, using
`cephBlockPools[replicapool].spec.replicated.size` form for name-keyed lists.

Because rooket composes exactly one file per chart, the preview is the file that
Helm receives — it cannot drift from reality. This is enforced by test, not
convention: see "Testing".

Composed files are written to `~/.local/share/rooket/<name>/values/<chart>.yaml`
rather than `os.CreateTemp` with `defer os.Remove`, so the exact input to a
failed deploy survives for inspection.

## Profile anatomy

A profile is a bundle, not a values file: the resources these profiles need —
`ObjectBucketClaim`, `CephObjectStoreUser`, `CephNFS`, PVCs, test pods — have no
representation in the rook charts. `CephObjectStoreUser` and `CephNFS` appear in
`deploy/charts/rook-ceph/templates/resources.yaml` only as CRD definitions;
`rook-ceph-cluster` templates neither.

```
profiles/rgw/
├── profile.yaml          # description: <one line, shown in listings and the TUI>
├── values/
│   ├── rook-ceph-cluster.yaml
│   └── ceph-csi-drivers.yaml
└── templates/
    ├── 10-objectstoreuser.yaml
    ├── 20-obc.yaml
    └── 30-test-pod.yaml
```

Built-ins are embedded with `go:embed` in `internal/profiles`. A user profile of
the same name shadows a built-in **entirely** — it is not merged with it.
`rooket values profiles fork <name>` copies a built-in into
`~/.config/rooket/profiles/<name>/`.

## The generated profiles chart

Templates are not applied with `kubectl apply`, which has no lifecycle:
disabling a profile or deleting a file would leave its resources behind,
accumulating orphaned test pods and a wedged OBC pinning an object store you are
trying to delete.

Instead rooket generates a chart from every active template source — the clone's
`.rooket/templates/` and each active profile's `templates/`:

```
~/.local/share/rooket/<name>/profiles-chart/
├── Chart.yaml                        # name: rooket-profiles, version 0.0.0
├── values.yaml                       # rooket context, see below
└── templates/
    ├── local-scratch-pvc.yaml        # from .rooket/templates/
    ├── rgw-10-objectstoreuser.yaml   # <profile>-<original filename>
    ├── rgw-20-obc.yaml
    └── rbd-10-pvc.yaml
```

installed as a fourth release after `rook-ceph-cluster`:

```
helm upgrade --install rooket-profiles <generated chart> -n rook-ceph
```

Removing a template file or disabling a profile prunes exactly those resources,
with no tracking code in rooket. When no source contributes a template and a
`rooket-profiles` release exists, it is uninstalled.

Clone templates are prefixed `local-`, so `local` is a reserved profile name and
loading a profile called `local` is an error. Two sources defining the same
resource is a Helm duplicate-resource error at install time, which is a legible
enough failure not to warrant pre-checking.

Templates from either source pass through Helm's template engine, so they may
reference rooket context — `{{ .Values.rooket.clusterName }}`, `.namespace`, `.operatorNamespace`,
`.workers` — and any literal `{{` must be escaped.

## Readiness

The profiles chart installs **without** `--wait`, and rooket does not poll. `up`
stays fast and never blocks on Ceph settling. The e2e suite does its own polling,
which it must implement for its assertions regardless.

## Built-in profiles

| Profile | Values overlay | Templates |
|---|---|---|
| `rbd` | none | PVC on the chart's default `ceph-block` StorageClass (no pod: CI found krbd maps but can't mount on a kind node — the device node lands in the host's `/dev`, not the node's per-container tmpfs) |
| `rgw` | none | CephObjectStoreUser, OBC on the chart's default `ceph-bucket` StorageClass, s3 client pod |
| `nfs` | `ceph-csi-drivers`: `drivers.nfs.enabled: true` | CephNFS, NFS StorageClass, PVC, pod |

`rbd` and `rgw` need no values overlay because `rook-ceph-cluster`'s default
`values.yaml` already enables `cephBlockPools[ceph-blockpool]`,
`cephFileSystems[]`, and `cephObjectStores[ceph-objectstore]` with their
StorageClasses — rooket has always deployed a pool, a filesystem, and an RGW.
The default filesystem also backs the NFS exports, so `nfs` needs no
`cephFileSystems` overlay either; it only flips a driver rooket hardcodes to
`false` in a Go constant, which still makes it the profile proving the layering
reaches every chart.

Templates are adapted from rook's own checked-in examples
(`deploy/examples/csi/rbd/pvc.yaml`, `deploy/examples/object-user.yaml`,
`deploy/examples/object-bucket-claim-a.yaml`, `deploy/examples/nfs.yaml`,
`deploy/examples/csi/nfs/{storageclass,pvc,pod}.yaml`) so they stay idiomatic.

Note for profile authors: rooket's name-keyed list merge applies only among
rooket's own layers. At the Helm boundary the chart's `values.yaml` still uses
replace semantics, so a list entry that overrides a chart default must be
**complete** — a partial `cephBlockPools` entry would drop the default's CSI
secret parameters and yield a StorageClass that fails to provision.

## CLI

```console
$ rooket values                      # Project B TUI; plain summary when not a tty
$ rooket values show [chart]         # merged effective values; --layers shows provenance
$ rooket values edit [chart]         # $EDITOR on .rooket/values/<chart>.yaml
$ rooket values profiles             # list built-in + user profiles, active ones marked
$ rooket values profiles fork rgw    # copy a built-in into ~/.config/rooket/profiles/

$ rooket up --with rgw --with rbd    # append to the clone's sticky list
$ rooket up --with-only rbd          # replace it for this run
$ rooket up --with-only ""           # no profiles this run
$ rooket deploy cluster -f extra.yaml --set cephClusterSpec.mgr.count=2
```

Charts are addressed by the short names `deploy` already uses — `operator`,
`cluster`, `csi` — while files on disk use the full chart name. Both forms are
accepted on the command line. Omitting the chart argument from `show` or `edit`
acts on all three: `show` prints them as separate YAML documents, `edit` opens
them as separate files in one editor invocation.

`$ROOKET_PROFILES` (comma-separated) behaves as `--with-only`; the flags beat the
env var.

### `values edit`

Opens `$VISUAL`, else `$EDITOR`, else `vi`. When the file does not exist it is
seeded with rooket's generated base for that chart, commented out, plus a header
pointing at the chart's own `values.yaml` path — so overriding a key is
uncommenting a line rather than reading the chart source. On save the YAML is
parsed; on a parse error the message is printed and the editor reopens
(`visudo` semantics). A file left empty or comment-only is removed rather than
kept as an empty layer.

## Changes to existing code

- Every `--set` in `cmd/deploy.go` (operator: lines 165-186; cluster: lines
  292-297) moves into the generated base layer. `--set` survives only as user
  passthrough.
- `csiDriversValues` (`cmd/deploy.go:200`) becomes the csi chart's generated base
  layer, replacing the stdin heredoc install.
- `writeClusterValues` (`cmd/deploy.go:329`) becomes `internal/values`' base
  builder for the cluster chart, returning a typed `map[string]any` instead of
  concatenated strings, so it can participate in the merge.
- `up` gains and forwards `--with`, `--with-only`, `-f`, and `--set`.

## Testing

Unit:

- merge table tests: name-keyed list merging, `null` deletion, layer ordering,
  the negative cases where a list must replace (elements without `name`,
  duplicate names, non-map elements);
- profile discovery, built-in shadowing, `fork`;
- generated profiles-chart rendering: prefixing from both sources, the
  reserved-`local`-profile-name error, and the no-templates-at-all uninstall
  path;
- `values edit` round-trip against a fake `$EDITOR` script, covering the
  reopen-on-parse-error and remove-if-empty paths.

E2E (ginkgo, `test/e2e`):

- `rooket up --with rbd --with rgw --with nfs` → rgw and nfs pods Ready, rbd
  and nfs PVCs Bound, OBC Bound;
- then `rooket up --with-only rbd` → rgw and nfs resources pruned by the
  profiles chart, while a resource from `.rooket/templates/` survives, since it
  is not tied to any profile;
- **preview invariant**: `rooket values show <chart>` equals
  `helm get values <release> -o yaml` for each chart, with no `--set`
  passthrough in play. `helm get values` returns exactly the user-supplied
  values Helm received, so this asserts that what the preview shows is what the
  cluster got — catching merge, ordering, and drift bugs in one assertion.

## Deferred

- Project B (the TUI), specified separately.
- `--set` reflected in `values show`: would require reimplementing Helm's
  `strvals` parser or importing the Helm module and its dependency tree.
- Rook-version constraints on profiles (e.g. a profile valid only for rook
  >= 1.20). Revisit when a built-in profile actually breaks across refs.
- Profiles sourced from git or any remote location.
- Clone-local *profiles* (a `.rooket/profiles/` directory), generalizing
  `.rooket/templates/` into named toggleable bundles that live with the
  checkout, and a command to promote clone templates into a profile. Revisit if
  scratch resources in `.rooket/templates/` start wanting to be toggled.
- A watch mode that redeploys on values-file change.

## Project B — `rooket values` TUI (sketch)

A bubbletea application: a profile list where `space` toggles and `J`/`K`
reorders (reordering *is* priority selection, which no flag interface expresses
well), a chart selector, and a live merged-values pane coloured by provenance.
`e` shells out to `$EDITOR` for the selected chart's sticky file; `s` writes
`.rooket/config.yaml`. Not a tty, or `--list`: plain output instead.

bubbletea over tview because its `Update` is a pure function, so the entire UI
is testable with synthetic key messages and no pty. `rooket config` is already
taken by the kind-config printer, hence `rooket values` as the umbrella noun.
