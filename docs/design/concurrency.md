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
   not overlapped. Example: node preparation and containerd-registry wiring
   both `exec` a script into the *same* worker containers, so they are
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
   later (kind pulls the image itself).

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
 resolve rookDir / cluster name / registry port   (fast, must precede all)
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
will not run (the rook tree is unchanged since the last push) there is nothing
to overlap, so the infra lane runs serially in front. This decision is a
scheduling hint only — the authoritative build-skip gate re-runs after the join
— so a wrong guess costs sequential speed, never a wrong image.

## Concurrency inside `cluster create`

After the kind cluster exists (which also creates the "kind" network), these
steps share only the cluster and otherwise touch disjoint subsystems, so they
run as one `runConcurrent` group:

- **prepare nodes** — `exec` into workers: remount `/sys`, install lvm2 and
  cryptsetup, mask host devices (the long pole here — apt over the network).
- **create registry** — a host-side container on the kind network.
- **apply registry ConfigMap** — to the apiserver (`kube-public`).
- **install prometheus-operator CRDs** — a helm install to the apiserver.

Containerd-registry wiring runs *after* this group: it needs the registry to
exist (data dependency) and it `exec`s into the same nodes as node preparation
(exclusive resource — invariant 2), so it cannot join the group.

## Concurrency in `down`

Teardown's dependency graph is tighter than bring-up's, and the invariants
(especially 1 and 2) do most of the shaping:

- **Across clusters (`down --all`) — parallel.** Different clusters share no
  kind cluster, registry, or disk, so every cluster's delete (kind delete →
  registry delete → confirm-gone → zap preserved disks) runs concurrently.
  With N clusters this collapses N sequential deletes to roughly one delete's
  wallclock. The concurrent deletes are the group; the batched iSCSI target
  teardown that follows is a **barrier** (invariant 1): it must see every
  cluster confirmed gone before it removes any target, and it is deliberately a
  single privileged run so the whole sweep costs at most one prompt.

- **Within one cluster (`cluster delete` / plain `down`) — mostly sequential,
  by invariant.** The disk zap truncates the OSD images, which corrupts a live
  cluster, so it must wait for a **confirmed** kind delete (invariant 1). And
  single-cluster delete aborts the whole teardown if the kind delete fails,
  leaving the registry intact — so its registry removal cannot be hoisted to
  run concurrently the way `--all`'s best-effort registry removal can. This is
  a case where the invariants legitimately preclude overlap; the design goal is
  satisfied by *not* manufacturing unsafe concurrency, and by documenting why.

- **`down` → `block teardown` — sequential.** Tearing down the iSCSI targets
  logs out sessions the kind nodes hold, so it waits for the cluster to be gone
  (invariant 1).

Concurrent teardown output follows invariant 3: each cluster's delete writes to
its own `runConcurrent` buffer (including its zap lines, which is why
`cluster.ZapISCSIDisks` takes an `io.Writer`), flushed in cluster-name order.

## Per-command status

| Command | Concurrency exploited |
| --- | --- |
| `up` | infra lane `(block ∥ pre-pull → create)` overlaps the `make` build lane; deploy follows on the join |
| `cluster create` | node prep ∥ registry ∥ ConfigMap ∥ prometheus CRDs, then containerd wiring |
| `build` | `make` overlaps cluster create (via `up`); push follows |
| node operations | every per-node script fans out across nodes via `forEachNode` |
| `down --all` | every cluster deleted concurrently, then one batched iSCSI target teardown as the barrier |
| `cluster delete` / `down` | sequential by invariant (zap needs a confirmed delete; registry stays intact on a failed delete) |

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
