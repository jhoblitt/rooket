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
  removing targets needs root (`sudo -n`, then `pkexec`): the first
  `rooket block setup` and a `rooket down --delete-disks`. Day-to-day
  `up`/`down` cycles reuse the existing targets and never prompt.
- A Go toolchain (to build rooket) and a [rook](https://github.com/rook/rook)
  source checkout.

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
| `rooket up` / `rooket down` | full bring-up / teardown; `down --delete-disks` also removes targets, images, and state |
| `rooket block setup` / `teardown` | create/remove the iSCSI disk images and targets |
| `rooket cluster create` / `delete` | create/delete the kind cluster + registry |
| `rooket build` | `make` in the rook source, tag + push the image to the registry |
| `rooket deploy` | install the rook-ceph and rook-ceph-cluster charts |
| `rooket load <image>` | push any local image into the cluster's registry |
| `rooket kubectl` (`k`) | run kubectl with `KUBECONFIG` set for the cluster |
| `rooket kubeconfig` | print the cluster's kubeconfig (`--path` for its path) |
| `rooket list` | list clusters: live status, registry port, state dir |
| `rooket prune` | remove state dirs of clusters that no longer exist |
| `rooket config` | print the kind config that `create` would use |

## Tests

Unit tests: `go test ./...`. The end-to-end suite
(`go test -tags e2e ./test/e2e/ -timeout 60m`, needs `ROOK_DIR` and existing
block devices) drives a real `rooket up`/`down` and asserts one OSD per
worker, no loop devices, a settled healthy cluster, and clean teardown; CI
runs it under docker on every PR.

## Upgrading from older rooket

Older versions used the fixed cluster name `rook` and wrote its context into
`~/.kube/config`. Tear such a cluster down with `rooket down --name rook`
before switching to per-clone names, and remove the stale `kind-rook`
context/cluster/user entries from `~/.kube/config` if kind left them behind.
