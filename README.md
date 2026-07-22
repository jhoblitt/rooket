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
- A Go toolchain (to build rooket) and a [rook](https://github.com/rook/rook)
  source checkout.

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

## Quick start

```console
$ go build -o ~/bin/rooket .
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

`rooket up` finds the rook source via `--dir`, `$ROOK_DIR`, or by walking up
from the current directory to the enclosing rook clone.

## Clusters and state

Each rook clone gets its own cluster. The cluster name is derived from the
clone's absolute path (`/home/me/github/rook3` → `home-me-github-rook3`), so
several clusters — one per checkout — can run concurrently; override with
`--name` or `$ROOKET_NAME`.

Per-cluster state lives in `~/.local/share/rooket/<name>/`:

- the OSD disk images (`*.img`, exported as iSCSI targets),
- the cluster's **kubeconfig** — rooket never touches `~/.kube/config`,
- the local registry's host port (auto-picked from 5001 up, persisted).

Use the cluster from outside rooket with:

```console
$ rooket k <kubectl args>                            # kubectl wrapper ('k' = 'kubectl')
$ export KUBECONFIG="$(rooket kubeconfig --path)"    # or point your own tools at it
```

## Commands

| Command | Purpose |
|---|---|
| `rooket up` / `rooket down` | full bring-up / teardown; `down --delete-disks` also removes targets, images, and state; `down --all` sweeps every cluster |
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
(`go test -tags e2e ./test/e2e/ -timeout 60m`, needs `ROOK_DIR` and existing
block devices) drives a real `rooket up`/`down` and asserts one OSD per
worker, no loop devices, a settled healthy cluster, RADOS I/O, CSI block-PVC
provisioning and CephFS-PVC I/O (krbd mounts need udev-created device nodes,
which kind nodes lack), the `list`/`kubectl`/`kubeconfig`/`prune` surfaces,
registry-port reuse across re-ups, `down --all` ownership scoping against a
foreign kind cluster, and clean teardown. CI runs the suite under docker on
every PR against rook master, release-1.20, and release-1.19 — covering both
the ceph-csi-drivers and rook-managed CSI flows — alongside a fast unit + vet
job.

## Upgrading from older rooket

Older versions used the fixed cluster name `rook` and wrote its context into
`~/.kube/config`. Tear such a cluster down with `rooket down --name rook`
before switching to per-clone names, and remove the stale `kind-rook`
context/cluster/user entries from `~/.kube/config` if kind left them behind.
