# rooket

rooket stands up a complete [Rook](https://rook.io) development environment on
[kind](https://kind.sigs.k8s.io): a multi-node cluster with a local OCI
registry, iSCSI-backed block devices for real (loop-free) OSDs, and the
rook-ceph + rook-ceph-cluster helm charts built and deployed from a rook
source checkout.

## Prerequisites

- A container engine: **podman** (rootful, with the system socket running) or
  **docker**. By default rooket probes podman and falls back to docker if
  podman is rootless or unusable; select explicitly with `--engine` or
  `$ROOKET_ENGINE`.
- `kind`, `kubectl`, `helm` on `PATH`.
- iSCSI tooling for the OSD disks: `targetcli`, `iscsiadm` (package
  `open-iscsi`/`iscsi-initiator-utils`), and `lvm2`. Only configuring and
  removing targets needs root: the first `rooket block setup` and a
  `rooket down --delete-disks`. See "Passwordless iSCSI setup" below for how
  that privilege is obtained. Day-to-day `up`/`down` cycles reuse the existing
  targets and never prompt.
- A Go toolchain (to build rooket) and, unless you deploy a released Rook
  (see below), a [rook](https://github.com/rook/rook) source checkout.

### Passwordless iSCSI setup

Creating and removing iSCSI targets needs root. By default rooket asks for it
once per privileged run via `pkexec`. To remove the prompt, install a sudoers
rule scoped to the commands rooket runs:

```console
$ rooket sudoers print      # inspect the rule first
$ rooket sudoers install    # authenticate once
$ rooket sudoers status     # "up to date"; exits non-zero if absent or stale
$ rooket sudoers uninstall
```

The grant is **root-equivalent**: `targetcli` can expose any file as a fileio
backstore and the disk images are user-writable, so anyone holding it can
obtain root. It is a convenience for a single-user development workstation, not
a privilege boundary. rooket works without it.

Independently, `sudo systemctl enable target.service` makes LIO restore its
configuration at boot, so targets survive a reboot and setup needs root far
less often. The trade-off is that targets belonging to deleted clusters also
survive until `rooket down --all --delete-disks` clears them.

## Installation

Install straight from source with the Go toolchain — no clone needed:

```console
$ go install github.com/jhoblitt/rooket@latest
```

This drops a `rooket` binary in `$(go env GOBIN)`, or `$(go env GOPATH)/bin`
(usually `~/go/bin`) when `GOBIN` is unset; make sure that directory is on your
`PATH`. rooket is untagged, so `@latest` resolves to the newest commit on
`main` as a pseudo-version; pin a specific one with `@<commit-sha>` in place of
`@latest`.

To hack on rooket itself, build from a checkout instead:

```console
$ git clone https://github.com/jhoblitt/rooket && cd rooket
$ go build -o ~/bin/rooket .
```

## Quick start

```console
$ cd ~/github/rook            # any directory inside a rook clone
$ rooket up                   # block setup → kind cluster → build → deploy
$ rooket k get pods -n rook-ceph
$ rooket k -n rook-ceph exec deploy/rook-ceph-tools -- ceph -s
$ rooket down                 # cluster gone, disks kept for the next up (no root)
$ rooket down --delete-disks  # full teardown: targets, images, state (needs root)
```

To free the whole machine in one shot, `rooket down --all` tears down every
**rooket** cluster — all state dirs (orphans included) plus the live kind
clusters rooket owns — after showing the plan and prompting (`--force` skips,
`--dry-run` stops at the plan). A live cluster counts as rooket's only if it
has a state dir or a rooket registry container, so a foreign `kind create
cluster` is left alone unless you pass `--include-unmanaged`. Add
`--delete-disks` to also remove every cluster's iSCSI targets, disk images, and
state dir; all target teardowns are batched into one privileged run, so the
sweep costs at most a single prompt (or none, with rooket's sudoers rule
installed).

### Shared OCI image cache

The CSI node plugins are DaemonSets, so every worker independently pulls the
same images. On an 11-node cluster that meant ~7.6 GB downloaded to deliver
~0.9 GB of distinct images — `quay.io/cephcsi/cephcsi` alone was fetched ten
times at 725 MB a copy, 2–3 minutes per node.

`rooket up` therefore starts **one** [zot](https://zotregistry.dev) container,
`rooket-cache`, on the kind network and points every node's containerd at it.
kind gives all clusters the same network, so a single cache serves every rooket
cluster on the host: an image is downloaded once per workstation, and the next
`up` — in this clone or any other — reuses it.

The cache is an optimisation, never a dependency. Its mirrors set no containerd
`server`, so if the cache is down or an image comes from a registry it does not
proxy, containerd pulls straight from upstream and the cluster comes up as it
always did — just slower.

Proxied registries are `quay.io`, `registry.k8s.io`, `docker.io`, `ghcr.io`,
and `gcr.io`. zot resolves an upstream from the repository prefix rather than
from containerd's `ns` parameter, so upstreams are enumerated rather than
matched by wildcard; one that is missing simply is not cached.

No teardown touches the cache unless asked, since surviving `down` is the whole
point:

```console
$ rooket down                  # cache preserved
$ rooket down --all            # cache preserved
$ rooket down --delete-cache   # cache container and its volume removed
```

`--delete-cache` is separate from `--delete-disks` because they reclaim
different things: `--delete-disks` removes one cluster's iSCSI targets and disk
images, while `--delete-cache` discards images shared by every cluster on the
host.

`rooket up` finds the rook source via `--dir`, `$ROOK_DIR`, or by walking up
from the current directory to the enclosing rook clone.

## Released Rook

To use rooket as a test harness for something that consumes Rook rather than
develops it, deploy a published release. No rook checkout is needed and
nothing is built:

```console
$ export ROOKET_NAME=rgw-test
$ rooket up --rook-version v1.20.7 --workers 1
```

The charts are pulled from `https://charts.rook.io/release` once per version
into `~/.cache/rooket/charts/` and reused by every cluster after that; the
images are the ones the release pins. The version is an exact tag, never a
range, and is recorded with the cluster, so a later `rooket deploy` or
`rooket values show` needs no `--rook-version` or `--config-dir` flag.
Passing a different `--rook-version` to `up` or `deploy` upgrades the
cluster to it.

Outside a rook clone the cluster name still comes from `--name` (on the
commands that take it) or `$ROOKET_NAME` — `values` has no `--name` flag, so
`$ROOKET_NAME` is its only way to reach such a cluster. Sticky configuration
lives in any directory laid out like `.rooket/` (`values/<chart>.yaml`,
`templates/`, `config.yaml`), named with `--config-dir` or
`$ROOKET_CONFIG_DIR` and recorded with the cluster; it must already exist,
since rooket refuses a missing one and never creates a named directory.
rooket never writes a `.gitignore` into it, so it can live in your own
repository.

A released cluster made outside a clone with no `--config-dir` records no
owner, so `rooket prune` always keeps it; remove it with `rooket down
--delete-disks`.

## Clusters and state

Each rook clone gets its own cluster. The cluster name is derived from the
clone's absolute path (`/home/me/github/rook3` → `home-me-github-rook3`), so
several clusters — one per checkout — can run concurrently; override with
`--name` or `$ROOKET_NAME`.

Per-cluster state lives in `~/.local/share/rooket/<name>/`:

- the OSD disk images (`*.img`, exported as iSCSI targets),
- the cluster's **kubeconfig** — rooket never touches `~/.kube/config`,
- the local registry's host port (auto-picked from 5001 up, persisted),
- the shape the cluster was created with (worker count, disks per worker, IQN
  date). Any of `--workers`, `--disk-count`, and `--iqn-date` you leave unset
  takes the recorded value. A flag you set that contradicts it is refused by
  `deploy`, `down`, and `block teardown`, and taken as the new shape by `up`,
  `cluster create`, and `block setup`,
- the Rook version it deploys, when released, and the configuration directory
  it was given, so later commands need neither flag.

Use the cluster from outside rooket with:

```console
$ rooket k <kubectl args>                            # kubectl wrapper ('k' = 'kubectl')
$ export KUBECONFIG="$(rooket kubeconfig --path)"    # or point your own tools at it
```

## Chart values and profiles

rooket composes the Helm values for each chart from layers, lowest first:

1. the chart's own `values.yaml`
2. rooket's generated base (image refs when built from a clone, OSD device
   pinning, dev-host cpu trims)
3. the configuration home's `values/<chart>.yaml` — sticky; a named
   `--config-dir` when one is recorded, else `<rook clone>/.rooket`
4. active profiles, in selection order

Nothing is locked: a values file can retarget the operator image or add to the
storage topology. Named lists such as `cephClusterSpec.storage.nodes` merge by
each entry's `name` rather than replacing the list, so naming an existing node
adds to its devices rather than swapping them, and naming a new one adds a
node alongside rooket's iSCSI-pinned ones. To actually replace such a list, set
it to `null` in a lower-priority layer and re-add it in a higher one — that
needs two layers, so it can't be done within a single sticky file.

The rook-ceph-cluster chart assumes three hosts. For a cluster of one or two
workers, the generated base also sets one mon and carries the chart's
`cephBlockPools`, `cephFileSystems`, and `cephObjectStores` whole, with every
pool fitted to the workers: replicated to at most the worker count, and
replicated rather than erasure-coded. Higher layers still merge onto those
entries by name, and `osd_pool_default_size` fits the pools Ceph creates on
its own, such as `.mgr`.

Every cluster's base sets `configOverride` to turn off the mon's free-space
check (`mon_data_avail_crit = 0`), since a kind node's `/var/lib/rook` sits on
the host's root filesystem. A `configOverride` in a higher layer replaces that
string rather than adding to it.

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

Switching off the `nfs` profile removes its CephNFS server at the same time as
its pod, so the pod can be left `Terminating` (and its PVC with it) until
kubelet gives up on the now-unreachable unmount — this is expected, not a bug.

A profile can also carry a values overlay for the operator or
ceph-csi-drivers chart, not just the cluster chart — `nfs` does, setting
`csi.nfs.enabled` for rook refs older than v1.20. `rooket deploy cluster`
only refreshes the cluster chart, so enabling or disabling such a profile
needs a full `rooket deploy` or `rooket up` to reach those other releases.

### Per-run chart values (integration tests)

A program that drives rooket, such as an integration test, can give it values
for each chart without editing the clone's `.rooket/` overrides or
`~/.config/rooket`: pass a profile directory by path to `--with` or
`--with-only`. A value containing `/` (or exactly `.` or `..`) is a path; a
relative one resolves against the directory rooket runs in.

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
another chart. These are errors: a values file named for no chart
(`values/cluster.yaml`) or not ending in `.yaml`/`.yml` (JSON content is
fine in a `.yaml` file); two different profiles with the same name, such as
`./rbd` alongside the built-in `rbd`, or two directories both named
`mytest`; a profile directory whose name starts with `_`; and a path in
`.rooket/config.yaml`'s `profiles:` list. `--with-only` replaces the
configuration home's sticky profile list, so that list is not read, but its
`values/` still applies.

## Commands

| Command | Purpose |
|---|---|
| `rooket up` / `rooket down` | full bring-up / teardown; `up --rook-version` deploys a released Rook with no checkout; `down --delete-disks` also removes targets, images, and state; `down --all` sweeps every cluster |
| `rooket block setup` / `teardown` | create/remove the iSCSI disk images and targets |
| `rooket cluster create` / `delete` | create/delete the kind cluster + registry |
| `rooket build` | `make` in the rook source, tag + push the image to the registry |
| `rooket deploy` | install the rook-ceph and rook-ceph-cluster charts |
| `rooket load <image>` | push any local image into the cluster's registry |
| `rooket kubectl` (`k`) | run kubectl with `KUBECONFIG` set for the cluster |
| `rooket helm` | run helm with the cluster's kubeconfig and isolated per-cluster helm config |
| `rooket kubeconfig` | print the cluster's kubeconfig (`--path` for its path) |
| `rooket list` | list clusters: live status, registry port, state dir |
| `rooket prune` | remove state dirs of clusters that no longer exist |
| `rooket config` | print the kind config that `create` would use |
| `rooket version` | print the version, commit, build time, and Go toolchain |

## Design

A standing design goal is that **every command exploits all available
concurrency to minimize wallclock time** — independent work runs in parallel,
and sequencing is used only where a real dependency, shared resource, or
terminal/prompt constraint requires it. For example, `rooket up` overlaps block
setup, the kind node-image pre-pull, and cluster create with the (long-pole)
rook `make` build. See [docs/design/concurrency.md](docs/design/concurrency.md)
for the dependency graph, the primitives, and the invariants that bound it.

## Tests

Unit tests: `go test ./...`. The end-to-end suite
(`go test -tags e2e ./test/e2e/ -timeout 60m`) needs existing block devices,
and either `ROOK_DIR` (a rook checkout) or `ROOKET_ROOK_VERSION` (a released
version, no checkout; specs that build or edit a checkout skip themselves in
that mode). It drives a real `rooket up`/`down` and asserts one OSD per
worker, no loop devices, a settled healthy cluster, RADOS I/O, CSI block-PVC
provisioning and reclaim, krbd-mounted RBD I/O, CephFS-PVC I/O, the
`list`/`kubectl`/`kubeconfig`/`prune` surfaces,
registry-port reuse across re-ups, `down --all` ownership scoping against a
foreign kind cluster, and clean teardown. CI runs the suite under docker on
every PR against rook master, release-1.20, and release-1.19 — covering both
the ceph-csi-drivers and rook-managed CSI flows. A released Rook v1.20.7
entry also runs with no checkout, on one worker, alongside a fast unit + vet
job.

## Upgrading from older rooket

Older versions used the fixed cluster name `rook` and wrote its context into
`~/.kube/config`. Tear such a cluster down with `rooket down --name rook`
before switching to per-clone names, and remove the stale `kind-rook`
context/cluster/user entries from `~/.kube/config` if kind left them behind.
