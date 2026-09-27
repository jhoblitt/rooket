# krbd device nodes in rooket

## The problem

**A kind node never sees the `/dev/rbdN` node the kernel creates when an RBD
image is mapped, so without help every krbd-mounted PVC fails to mount.**

A kind node is a privileged container running on the host's kernel. Its `/dev`
is not the host's devtmpfs but a tmpfs of its own, which the container engine
fills at container start with a node for every host device. ceph-csi's node
plugin, which hostPath-mounts the node's `/dev`, maps an RBD image with the
kernel client; the kernel publishes the new `/dev/rbdN` in devtmpfs — the
host's `/dev` — and nothing puts it in the node's.

udev is not the gap. The node runs no udevd, but ceph-csi maps krbd volumes
with `--options noudev`, which makes `rbd map` wait for the kernel's own
uevents instead of udev's, so the map itself completes. What fails is the check
`rbd map` runs straight afterwards (ceph's `src/krbd.cc`): it `stat`s the
device node in its own `/dev` and rejects the mapping if the node is missing —

```
rbd: mapping succeeded but /dev/rbd0 is not accessible, is host /dev mounted?
```

— or if the node's major:minor differ from the kernel's (`... does not match
expected <major>:<minor>`). The PVC provisions and binds; the pod that mounts
it never starts.

## What rooket does

**Node prep pre-creates `/dev/rbd0` through `/dev/rbd255` in every node, with
the device numbers the kernel will assign, before any image is mapped.**
`rbdNodeScript` (`internal/cluster/cluster.go`) renders the step and
`nodePrepScript` appends it after the device prune, so it rides the same single
exec per node as the rest of node prep ([concurrency.md](concurrency.md),
invariant 2). Node prep runs on every `up` and `cluster create`, a resumed
cluster included, so a node whose `/dev` was rebuilt by a restart gets its rbd
nodes back.

The step:

1. **Loads the module.** `modprobe rbd`, errors ignored. kind bind-mounts the
   host's `/lib/modules` read-only into every node, so the privileged node can
   load the host's module into the shared kernel, provided its image ships
   `modprobe`. ceph-csi's node plugin would load it too, but only when it
   starts, after deploy — too late for node prep to read the major.
2. **Reads the major.** The rbd major is allocated dynamically when the module
   loads, so the script reads it from `/proc/devices`
   (`awk '$2 == "rbd"'`) rather than assuming a number.
3. **Creates the nodes.** For each N below `rbdMaxDevices` (256),
   `mknod /dev/rbdN b <major> N<<4`, skipping any path that already exists.

### Why N<<4

With the module's `single_major` parameter on — the kernel's default, and what
`rbd map` asks for when it loads the module itself — the kernel registers one
block major named `rbd` and gives the device with ID N the name `rbdN` and
first minor N<<4, reserving 16 minors per device for partitions
(`rbd_dev_id_to_minor` and `RBD_SINGLE_MAJOR_PART_SHIFT` in the kernel's
`drivers/block/rbd.c`). The script never reads the `single_major` parameter,
and does not need to: without single-major the module registers a separate
major per device, named `rbd0`, `rbd1`, ..., so no plain `rbd` line appears in
`/proc/devices`, the script finds no major, and it creates nothing.

### Surviving the device prune

Node prep strips every device node in `/dev` that is neither on the
`allowedDevs` allowlist nor the node's own OSD disk. `allowedDevs` carries every
`/dev/rbdN` below `rbdMaxDevices`, generated from that one constant by
`rbdAllowlist`, for two reasons:

- A node inherits any `/dev/rbdN` the host already has mapped when the node
  container starts; the prune must not strip it.
- A re-run of node prep against a live cluster must not unlink a node a mounted
  volume is using. Membership is static, so the prune skips every rbd path on
  every run, and the `mknod` loop skips any path that exists: no re-run removes
  and recreates a live device node.

### Best-effort, unlike the rest of node prep

Every other node-prep operation that fails emits a `ROOKET_FAIL` marker, which
aborts cluster create. This step only prints `warning:` lines. A node without
rbd device nodes loses krbd mounts — the state every node was in before this
step existed — while its OSDs, CephFS, and RGW are unaffected; failing prep
would take all of those down for want of one feature. The device prune is the
opposite case: a host device left in reach is a safety problem, so its failure
is fatal.

## Alternatives rejected

- **Bind-mount the host's `/dev` into every node.** Rook's own CI kind config
  (`tests/config/kind-config.yaml` in rook) does this. For rooket it would undo
  the device prune: every host device would be back in reach of the node and
  its pods, and Rook's non-PVC inventory — a global ceph-volume scan — would
  again adopt every visible disk and pin all OSDs onto one node (see
  `PrepareNodes`).
- **Create each node after the map, from `/sys/bus/rbd/devices/<id>/`.** This
  would not depend on the N<<4 rule, but it comes too late: `rbd map` checks for
  the node within the same invocation, the moment the kernel reports the
  mapping, so the node must exist before the map starts. The rule it would
  avoid is the kernel's own definition, and `rbd map` verifies the numbers
  anyway.
- **16 nodes**, the first cut. The ceiling is host-wide, not per node: every
  kind node shares the host kernel, whose rbd device IDs come from one
  module-global allocator, so every node of every rooket cluster on the
  workstation, and any `rbd map` run on the host itself, draw from the same
  IDs. 256 costs 256 empty inodes in each node's tmpfs.

## Constraints

1. **Privileged nodes.** Creating and opening a block device node inside the
   node takes a privileged container. kind always runs nodes `--privileged`,
   and rooket never drives rootless podman (`engine.Resolve`). The same
   privilege is why the engine copies every host device into the node, which
   is why the prune exists.
2. **`/dev` is per node, and never bind-mounted whole.** rooket's kind config
   (`kindConfigTmpl`) bind-mounts only `/run/udev`, `/dev/disk`, and each
   worker's own OSD disk(s), and only into workers that have OSD disks.
   Anything krbd needs in `/dev` has
   to be created there, and has to be on the prune's allowlist.
3. **No udev in the node.** Nothing in the node reacts to a new device;
   ceph-csi's `noudev` is what lets the map finish without it.
4. **Writable `/sys`.** `rbd map` works by writing to
   `/sys/bus/rbd/add_single_major`. Node prep remounts the node's `/sys`
   read-write before anything is deployed, and that failure is fatal.
5. **Minor-number range.** Device ID N owns minors N<<4 through (N<<4)+15. The
   kernel allows IDs up to 65535 (`MINORBITS` is 20); rooket covers 0–255.

## Verification

- **Unit.** `TestNodePrepScriptRBD` (`internal/cluster/nodeprep_test.go`)
  asserts that the rendered script loads the module, reads the major from
  `/proc/devices`, computes the `i << 4` minor, and runs the `mknod`; that every
  `/dev/rbdN` below `rbdMaxDevices` is in `allowedDevs` and in the prune's
  keep-set; and that the rbd section carries no `ROOKET_FAIL` marker and no
  `rc=1`.
- **End to end.** `test/e2e/krbd_test.go` applies a 1Gi `ReadWriteOnce` PVC on
  the `ceph-block` class and a pod that writes a file to it, syncs, and reads it
  back. It waits for the PVC to bind and the pod to reach `Succeeded` — the pod
  cannot start until its image is mapped and mounted, so `Succeeded` covers the
  map, the mount, and the I/O — then deletes the pod and PVC and waits for the
  PVC to go. Ginkgo runs top-level containers in random order, so the spec's
  `BeforeAll` runs `rooket up` itself, rather than relying on the up/down
  suite's cluster, and waits for the cluster to settle. The rbd profile spec
  (`test/e2e/profiles_test.go`) depends on the same nodes: its pod must reach
  `Running`.
- **CI** runs the e2e suite under docker on every pull request and every push
  to main, against rook master, release-1.20, and release-1.19 builds and a
  released v1.20.7.

The N<<4 rule is checked against the kernel source, not at runtime: at
node-prep time nothing is mapped to compare against.

## Known limits

- **256 concurrent mappings per host.** The kernel hands out the lowest free
  device ID and frees it on unmap, so this bounds the mappings alive at once,
  not the mappings over time. A mapping past it gets an ID with no node and
  fails with the "not accessible" error.
- **No device isolation between nodes.** Every node carries the same
  major:minor pairs, all addressing the one host kernel, so a privileged pod on
  one node can open the device mapped for another node's volume, bypassing
  Kubernetes' single-attach semantics. That is acceptable for rooket — a single
  operator's local cluster, whose node containers are already privileged, run
  by someone who already has root on the host — and would not be for a
  multi-tenant cluster.
- **Silent until mount time.** When the step cannot do its job — the module
  could not be loaded, is not in single-major mode, or a `mknod` failed — its
  warning appears only in the `cluster create` output, and the failure surfaces
  later as the "not accessible" error on a pod's mount, with nothing pointing
  at rooket. If that error appears, look at node prep first. When the module
  was not loaded, loading it on the host and re-running `rooket up`, which
  re-runs node prep, creates the missing nodes; a module loaded without
  single-major has to be unloaded and loaded again with it first — though the
  step's warning calls both cases "not loaded".
- **The N<<4 rule is hard-coded.** A kernel that numbered rbd minors
  differently would fail every map with the "does not match expected" error,
  which at least names both pairs.
- **Coverage.** CI exercises docker only, not podman. The krbd spec's own `up`
  re-runs node prep, and whether that `up` is the cluster's first prep or a
  re-prep varies with the random order, so the spec proves the nodes are there
  when the pod mounts, not which prep created them. It maps one image on one
  node.
