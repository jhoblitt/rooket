# Concurrency in rooket

## Design goal

**Every rooket command must exploit all available concurrency to minimize
wallclock time.** When two units of work share no data dependency and no
exclusive resource, they run concurrently — never in sequence. Sequencing is
justified only by a real constraint (a dependency, a shared exclusive resource,
or a terminal-prompt/output ordering requirement), and any such constraint is
documented at the call site.

This is a standing requirement, not a per-feature nicety: rooket is a
development-loop tool, and its wallclock time is paid by a human waiting to
iterate. New commands, and new steps within existing commands, are expected to
be structured as a dependency graph whose independent nodes are dispatched
concurrently, with the critical path as the only lower bound on runtime.

## Invariants that bound concurrency

Concurrency is maximized *subject to* these correctness invariants. They are
what a reviewer checks before signing off on a parallelized step:

1. **Respect data dependencies.** A step runs concurrently with another only
   when neither consumes the other's output. Example: `rooket build`'s `make`
   phase (compiles rook, produces the container image) shares nothing with
   block setup or cluster create, so it overlaps them; but the *push* phase
   needs the registry that cluster create stands up, so it waits.

2. **Respect exclusive resources.** Two steps that mutate the same resource are
   not overlapped. Example: node preparation and containerd mirror wiring
   both `exec` a script into the *same* node containers, so they are
   sequenced — two per-node passes must not run into one node at once — even
   though each pass internally fans out across nodes.

3. **One writer owns the terminal; everything else buffers.** Concurrent steps
   cannot all stream to the terminal or their output interleaves into
   nonsense. Exactly one long-running stream (the `make` build) writes to the
   terminal live; every concurrent sibling writes to a buffer that is flushed
   in a deterministic order once the terminal frees up. See *Primitives*.

4. **Never overlap an interactive prompt with another stream.** Block setup can
   fall back to a `pkexec` prompt on hosts without passwordless sudo. A prompt
   fighting a streaming `make` for the terminal is unusable, so block setup
   overlaps `make` *only* when a pre-flight check proves it will not prompt
   (`blockSetupPromptFree`: devices already present, or root, or a passwordless
   sudo grant). Otherwise it runs serially, in front, owning the terminal.

5. **Optimizations are best-effort and never gate correctness.** A speculative
   step (the node-image pre-pull) warns and continues on failure rather than
   aborting the run; if it does not help, the work it was racing simply happens
   later (kind pulls the image itself). The shared image cache is the same
   shape: it warns and clears a `cacheReady` flag, and the nodes it would have
   served just pull from upstream as they did before it existed.

6. **Host-wide singletons must tolerate concurrent *processes*, not just
   concurrent goroutines.** Invariants 1–3 bound concurrency *within* one
   command. Some resources are shared across simultaneous `rooket` invocations
   — one cluster per rook clone is an explicitly supported workflow, so two
   `up` runs overlap routinely. Anything host-wide rather than per-cluster (the
   shared image cache container and its volume, the released-chart cache) is
   reached by both, and an exists-then-create sequence across two processes is
   a race no in-process lock can close.

   The rule is to make such a step **idempotent and race-absorbing** rather
   than to serialize it: let an arbiter that already exists settle the race
   (the container engine enforces unique names; a directory rename either
   lands or finds the winner's entry in place), and on failure re-check
   whether the winner produced what was wanted. `cache.Create` does exactly
   this — losing the race is the expected path and reports success — and
   `chartcache.Ensure` pulls into a temporary directory and, when its rename
   loses, uses the winner's entry. The alternative, treating a lost race as an
   error, would silently degrade the loser's cluster.

   Where nothing arbitrates in time, the step is serialized instead, under a
   cross-process file lock. Registry host-port allocation (`LockPorts`) is
   one: two clusters coming up together would both probe the same free port
   and record it, and only the loser's registry bind — after its record is
   written — would notice. A rook clone's build (`lockBuildCache`) is another:
   two clusters built from one clone write the same `make` output tag. And
   commands on one *cluster* never overlap at all: every command that mutates
   a cluster holds that cluster's lock (`LockCluster`) for its whole run, since
   two runs against one cluster are a mistake, not a workflow. The sweeps over
   many clusters, `down --all` and `prune`, hold it the same way for each
   cluster they touch (`lockSweep`): they try every affected cluster's lock
   before removing anything of any of them, tear down only the clusters they
   hold, and keep those locks through the batched privileged teardown and the
   state-dir removals; a cluster another rooket holds is skipped and reported.
   Holding many locks at once cannot deadlock, because a cluster lock is only
   ever tried, never waited for: a run that finds one held gives up at once, so
   no run ever waits on a cluster lock and no cycle of waits can pass through
   one.

## Primitives

- **`runConcurrent(out, fns...)`** (`cmd/concurrent.go`) — the default building
  block. Runs each `fn` concurrently, each writing to its own buffer, then
  flushes the buffers to `out` in call order and returns the joined errors.
  Ordered buffering satisfies invariant 3; per-branch buffers need no locking.
  Reach for this whenever a step has independent sub-steps.

- **`switchWriter`** (`cmd/switchwriter.go`) — buffers writes until `Promote`
  hands it a live destination, then flushes the backlog and streams the rest
  live. It is what lets the infra+create side run concurrently with `make`:
  `make` owns the terminal while it runs; the other side's output appears — in
  order, then live — the moment `make` stops streaming.

- **`cluster.forEachNode`** (`internal/cluster/cluster.go`) — runs a per-node
  script across all nodes concurrently, buffering each node's output and
  flushing in node order. The node-level analogue of `runConcurrent`.

## The `up` dependency graph

`rooket up` is the command with the most concurrency to exploit. Its steps and
their true dependencies:

```
 resolve name + lock, shape, registry port, source   (fast, must precede all)
        │
        ├─ block setup ─┐                              node-image pre-pull
        │  (iSCSI OSD    │  (best-effort, concurrent    with block setup)
        │   devices)     │
        │                ▼
        │        cluster create ──► registry ──► push (also needs make)
        │        (needs the iSCSI                      │
        │         devices to bind-mount)               │
        │                                              │
        └─ make (compile rook + build image) ──────────┘
           (the long pole; shares nothing with the infra side)
                                                       │
                                                       ▼
                                                    deploy
                        (needs cluster + pushed image + devices)
```

Hard edges (the only sequencing that is forced):

| Edge | Reason |
| --- | --- |
| block setup → cluster create | create bind-mounts the resolved `/dev/sdX` iSCSI devices into the kind config |
| node-image pre-pull → cluster create | create must find the image already present to skip its own pull |
| cluster create (registry) → build push | push needs the registry to publish into |
| make → build push | push tags what make built |
| cluster create + push → deploy | deploy needs the cluster and the image in the registry |
| block setup → deploy | deploy resolves the devices for per-node OSD pinning |

Everything else runs concurrently. Concretely, `up` overlaps two lanes:

- **Infra lane:** `(block setup ∥ node-image pre-pull)` → `cluster create`.
- **Build lane:** `make`.

`make` is the long pole (a rook build is minutes; block setup + create is tens
of seconds), so folding block setup and the pre-pull into the infra lane — where
they overlap `make` — removes them from the critical path entirely on a cold
build. The critical path becomes `make → push → deploy` instead of
`block → create → make → push → deploy`.

The overlap is chosen only when it is safe (invariant 4) and useful: if `make`
will not run there is nothing to overlap, so the infra lane runs serially in
front. `--skip-build` never runs it, and neither does a released Rook, which
has nothing to build: the graph above is a clone's. (A released Rook's first
step instead pulls its charts into the chart cache when the cache lacks them,
so an unreachable chart repository fails before anything is stood up.)
Otherwise a pre-create probe skips `make` when the rook tree is unchanged
since the last push or the clone's build cache already holds a build of it.
That probe is a scheduling hint only: when it
says `make` can be skipped, the authoritative build-skip gate runs after the
join, against the registry and port create may have just repaired, so a wrong
guess costs sequential speed, never a wrong image.

## Concurrency inside `cluster create`

After the kind cluster exists (which also creates the "kind" network), these
steps share only the cluster and otherwise touch disjoint subsystems, so they
run as one `runConcurrent` group:

- **prepare nodes** — one script `exec`'d into every node, control plane
  included: remount `/sys` read-write, raise systemd's `DefaultTasksMax`,
  install lvm2 and cryptsetup, prune `/dev` to an allowlist plus the node's
  own OSD disk(s), then pre-create `/dev/rbd0`–`/dev/rbd255` for krbd (see
  [krbd device nodes](krbd-device-nodes.md)). On a fresh node the apt install
  is the long pole here — it goes to the network; a reused node already
  carries both packages and skips it.
- **create registry** — a host-side container on the kind network.
- **start the shared image cache** — likewise a host-side container on the kind
  network, but host-wide rather than per-cluster (invariant 6).
- **apply registry ConfigMap** — to the apiserver (`kube-public`).
- **install prometheus-operator CRDs** — a helm install to the apiserver.

Containerd mirror wiring runs *after* this group: it needs both the registry
and the cache to exist (data dependency) and it `exec`s into the same nodes as
node preparation (exclusive resource — invariant 2), so it cannot join the
group.

That wiring is **one** per-node pass, not two. The registry mirror and the
cache mirrors are separate concerns but mutate the same exclusive resource, so
invariant 2 forbids running them as concurrent siblings; composing both into a
single `containerdScript` is strictly better than sequencing two passes,
because it halves the number of `exec`s into each node. When the cache failed
to start, the same script is rendered with no cache mirrors and the registry
wiring proceeds unchanged (invariant 5).

## Concurrency in `down`

Teardown's dependency graph is tighter than bring-up's, and the invariants
(especially 1 and 2) do most of the shaping:

- **Across clusters (`down --all`) — parallel.** Different clusters share no
  kind cluster, registry, or disk, so every cluster's delete (kind delete →
  registry delete → confirm-gone → zap preserved disks) runs concurrently.
  Before any delete starts, the sweep tries the lock of every cluster it will
  touch and keeps the ones it gets until it is done with them (invariant 6); a
  cluster another rooket holds is skipped and left intact rather than waited
  for. With N clusters this collapses N sequential deletes to roughly one
  delete's wallclock. The concurrent deletes are the group; with
  `--delete-disks`, the batched iSCSI target teardown that follows is a
  **barrier** (invariant 1): it must see every cluster confirmed gone before
  it removes any target, it covers only the clusters the sweep holds, and it
  is deliberately a single privileged run so the whole sweep costs at most one
  prompt.

- **`prune` — sequential.** prune tears down every orphaned and stranded
  cluster's iSCSI targets in one batched privileged run, then, only once that
  succeeds, removes the orphans' state dirs. Like `down --all`, it first tries
  the lock of every cluster it will touch and keeps the ones it gets through
  both steps (invariant 6): a cluster that looks abandoned may be an `up` that
  has set up its targets but not yet created its kind cluster, and its lock is
  the only sign of that. A cluster another rooket holds keeps its targets and
  its state, and is reported.

- **Within one cluster (`cluster delete` / plain `down`) — mostly sequential,
  by invariant.** The disk zap truncates the OSD images, which corrupts a live
  cluster, so it must wait for a **confirmed** kind delete (invariant 1). And
  single-cluster delete aborts the whole teardown if the kind delete fails,
  leaving the registry intact — so its registry removal cannot be hoisted to
  run concurrently the way `--all`'s best-effort registry removal can. This is
  a case where the invariants legitimately preclude overlap; the design goal is
  satisfied by *not* manufacturing unsafe concurrency, and by documenting why.

- **`down` → `block teardown` — sequential.** With `--delete-disks`, tearing
  down the iSCSI targets logs out sessions the kind nodes hold, so it waits for
  the cluster to be gone (invariant 1).

Concurrent teardown output follows invariant 3: each cluster's delete writes to
its own `runConcurrent` buffer (including its zap lines, which is why
`cluster.ZapISCSIDisks` takes an `io.Writer`), flushed in cluster-name order.

## Concurrency in `deploy`

`deploy` installs up to four helm releases, one after another: rook-ceph (the
operator), ceph-csi-drivers, rook-ceph-cluster, and rooket-profiles. The
ceph-csi-drivers install runs from inside the operator install, and only for a
rook whose operator chart gates its ceph-csi-operator dependency on
`csi.installCsiOperator` (v1.20 onward); an older rook's operator manages CSI
itself, and the step is skipped. `deploy operator` runs the first two, and
`deploy cluster` the last two.

Three of the edges are real data dependencies (invariant 1):

- **operator → ceph-csi-drivers.** ceph-csi-drivers needs the csi.ceph.io CRDs
  the operator chart's ceph-csi-operator subchart installs; they may not be
  established the instant the operator install returns, which is why
  `installCephCsiDrivers` retries up to five times rather than assuming they
  are ready.
- **operator → rook-ceph-cluster.** The cluster chart's CRs (CephCluster,
  pools, object store, ...) are instances of CRDs the operator chart installs,
  which helm needs served before it can create them, and they need the
  operator running to reconcile them.
- **rook-ceph-cluster → rooket-profiles.** Profile resources reference
  cluster-chart resources — a CephObjectStoreUser's object store, a
  StorageClass a PVC binds to — so they cannot be applied first.

The fourth, **ceph-csi-drivers → rook-ceph-cluster**, is call structure, not
an invariant: the cluster install comes after ceph-csi-drivers only because
`installCephCsiDrivers` is called at the end of the operator install. Nothing
the cluster chart creates needs what ceph-csi-drivers installs — Driver and
OperatorConfig CRs, and the driver pods' ServiceAccounts and RBAC — and rook's
operator reads none of it: the csi.ceph.io CRDs come with the operator chart,
and the CephConnection and ClientProfile CRs that point ceph-csi at the Ceph
cluster are written by rook's operator itself. The one link is a name. The
cluster chart's StorageClasses name the drivers as their provisioners, and
rooket's generated ceph-csi-drivers values name the drivers to match; but a
StorageClass carries its provisioner as a string, so it can exist before the
driver does, and a PVC against it waits until the driver runs. That is the
order a rook v1.19 cluster comes up in, since its operator starts the CSI
driver only once a CephCluster exists.

That makes ceph-csi-drivers ∥ rook-ceph-cluster, both after the operator
install, a candidate overlap — not implemented. An overlap has to settle what
the call order settles today: both installs run helm against the cluster's
"rooket" purpose helm home, whose files `helmEnv` calls non-atomic (invariant
2); both stream helm output (invariant 3); and the cluster install would start
straight after the operator install, facing the same not-yet-established CRD
window `installCephCsiDrivers` retries through, where today the whole
ceph-csi-drivers install sits in between. The cluster install's own prep —
restoring its chart's dependencies, resolving its iSCSI devices, composing its
values — likewise waits behind the whole operator phase for no reason but
code structure; [Overlapping `deploy`'s cluster-chart prep with the operator
phase](deploy-concurrency-followup.md) proposes overlapping it.

A second, narrower rule sits inside the chain: the operator and cluster
installs each restore their chart's dependency archives first
(`restoreChartDeps`, which runs `ensureChartDeps` for a clone and skips a
released Rook, whose charts ship their dependencies unpacked). A restore that
finds an archive missing runs helm in the cluster's "make" purpose helm home
(`helmEnv`'s `HELM_CACHE_HOME` / `HELM_REPOSITORY_CONFIG`, non-atomic per that
function's own comment), so the two restores must never run concurrently with
each other (invariant 2). The chain's sequencing already keeps them apart —
the whole operator install, including ceph-csi-drivers, completes before the
cluster install's restore starts — so no extra synchronization is needed to
enforce it. The same home serves the helm runs inside rook's `make`, which
`up` finishes before it deploys; a standalone `build` and `deploy` of one
cluster are kept apart by the cluster lock (invariant 6).

So `deploy` runs wholly in sequence today, but not wholly by invariant: three
edges and the restore rule justify their sequencing, while the
ceph-csi-drivers → rook-ceph-cluster order and the cluster prep's wait are open
work against the design goal.

## Per-command status

| Command | Concurrency exploited |
| --- | --- |
| `up` | infra lane `(block ∥ pre-pull → create)` overlaps the `make` build lane; deploy follows on the join |
| `cluster create` | node prep ∥ registry ∥ image cache ∥ ConfigMap ∥ prometheus CRDs, then one combined containerd mirror pass |
| `build` | `make` overlaps cluster create (via `up`); push follows |
| node operations | every per-node script fans out across nodes via `forEachNode` |
| `down --all` | every cluster deleted concurrently, then (with `--delete-disks`) one batched iSCSI target teardown as the barrier |
| `cluster delete` / `down` | sequential by invariant (zap needs a confirmed delete; registry stays intact on a failed delete) |
| `deploy` | sequential: operator → ceph-csi-drivers, operator → cluster, and cluster → profiles are data dependencies; ceph-csi-drivers → cluster and the cluster prep's wait are call structure, candidate overlaps; the two `restoreChartDeps` calls share a helm home and must stay apart |

## Adding concurrency to new work

When you add a command or a step:

1. Write down its sub-steps and the data dependency between each pair.
2. Identify shared exclusive resources (the same node's `exec`, a single
   terminal, an interactive prompt).
3. Dispatch every independent node with `runConcurrent` (or `forEachNode` for
   per-node work); sequence only across a real edge from step 1 or 2.
4. Give exactly one long-running stream the terminal; route every concurrent
   sibling through a buffer (`runConcurrent`) or a `switchWriter`.
5. Document each sequencing decision at the call site with the reason, so the
   next reader can tell a real constraint from an accident.
