# Overlapping `deploy`'s cluster-chart prep with the operator phase

A proposal, not yet implemented, against rooket's `main`. It follows up
[Concurrency in rooket](concurrency.md): that document's design goal,
numbered invariants, and "Adding concurrency to new work" checklist bind this
change, and the invariant numbers below are its.

## Where `deploy` stands

`rooket deploy` installs four helm releases in order: `rook-ceph` (the
operator), `ceph-csi-drivers`, `rook-ceph-cluster`, and the generated
`rooket-profiles`. The ceph-csi-drivers install runs from inside the operator
install, and only for a rook whose operator chart gates its ceph-csi-operator
dependency on `csi.installCsiOperator` (v1.20 onward); an older rook's
operator manages CSI itself, and the step is skipped.

Ahead of all four, `deploySetup` resolves the cluster, takes the cluster lock,
settles where the charts come from — a rook clone, or a released version's
charts pulled into the chart cache — resolves the active profile set once for
the whole deploy, and, while it holds the lock, records the source for later
commands when it differs from the cluster's record. The installs only read
what it settles.

concurrency.md's *Concurrency in `deploy`* section records the edges between
the releases: the operator precedes ceph-csi-drivers (which needs the
csi.ceph.io CRDs) and rook-ceph-cluster (whose CRs need the ceph.rook.io CRDs),
and rook-ceph-cluster precedes rooket-profiles (whose resources reference
cluster-chart resources). Each is a real data dependency (invariant 1),
commented at its call site, and none of them changes here.

It also records a narrower rule: the operator and cluster installs' restores
share the cluster's "make" purpose helm home, whose config and cache files are
non-atomic (see `helmEnv`), so the two must never run concurrently with each
other (invariant 2). The rule binds only for a clone, because
`restoreChartDeps` skips a released Rook, and only when a restore has to
fetch, because only then does it run helm. Today call order keeps the two
apart; both call sites carry the rule, and the operator's credits call order
with enforcing it.

## The opportunity

Before its `helm upgrade --install`, the cluster install does four things,
none of which reads anything the operator phase produces:

1. restores the cluster chart's dependency archives (`restoreChartDeps`);
2. when the cluster has disks, resolves each worker's iSCSI disks to device
   paths (`clusterStorageNodes`, via `waitForISCSIDevice`);
3. builds the generated base layer from the chart's own defaults
   (`clusterBase`);
4. composes that base with the configuration home's sticky values and the
   active profiles' layers, and writes the result into the cluster's state
   dir (`writeComposed`).

Only the install itself needs the operator. Yet all four wait behind the
operator install and the ceph-csi-drivers install — which fetches its chart
from a remote repository and makes up to five attempts, 5 s apart, while the
csi.ceph.io CRDs become established. Nothing but code structure puts them
there.

## What the overlap buys

The gain is bounded by how long that prep takes, and on the paths that reach
`deploy` normally it is local file and symlink I/O:

- **Dependency restore.** A released Rook's charts ship their dependencies
  unpacked, so for one the restore does nothing. For a clone it reads the
  chart's `Chart.yaml`, and runs helm — reaching the network — only when a
  dependency fetched over http(s) has no archive on disk. rook's
  rook-ceph-cluster chart declares only its in-tree `file://../library`
  dependency (on release-1.19, release-1.20, and master alike), so for it the
  restore never runs helm.
- **Device resolution.** One symlink read per disk when the disk's by-path
  link exists; otherwise a 200 ms poll for up to 10 s, one disk at a time.
  `block setup` waits out that same poll for every disk before it returns,
  and `cluster create` resolves every disk again before it creates the kind
  cluster, so by the time `deploy` runs each link has normally resolved
  already.
- **Base and composition.** Reads of the chart's `values.yaml` and the sticky
  values file, an in-memory merge, and one file written.

So on the fast path the overlap saves little. It pays where the prep is slow:
a disk whose link is late to appear, or a rook ref whose cluster chart
declares a fetchable dependency with its archive missing. The case for doing
it regardless is the design goal — sequencing is justified only by a real
constraint, and this sequencing has none — and a before/after wallclock (see
*Verification*) is what shows whether the restructuring pays for itself.

## Proposal

Restore both charts' dependencies first, one after the other, then fork the
rest into two lanes that join before the cluster install:

```
 deploySetup (cluster, lock, source, profiles)
        │
 restore rook-ceph deps → restore rook-ceph-cluster deps     (invariant 2)
        │
        ├─ lane A: operator install → ceph-csi-drivers install ───┐
        │                                                         │
        └─ lane B: resolve devices → cluster base → compose ──────┤
                                                                  ▼
                                    cluster install → profiles install
```

The alternative keeps each restore in its own lane and makes lane B's restore
wait for lane A's to finish. That is a cross-lane ordering needing its own
synchronization, bought to overlap a step that, for rook's cluster chart,
reads one file. Restoring both up front instead:

- keeps invariant 2 enforced the way it is today, by plain sequential code;
- leaves lane B with no helm run and no helm home, so beyond the terminal the
  lanes share only state they read and the directory each writes its own
  chart's values file into;
- moves a failed cluster-chart restore ahead of the operator install, so that
  failure no longer arrives after the operator and ceph-csi-drivers releases
  are already installed.

The restores stay on the critical path, where they are today.

## Constraints on the implementation

- **Invariant 2, by construction.** In a full deploy both restores run before
  the fork and in neither lane. The operator-side comment credits call order
  — the whole operator install completes before the cluster install starts —
  so both call sites' comments must name the new mechanism instead.
  `deploy operator` and `deploy cluster` each restore only their own chart,
  so they are unaffected.

- **Invariant 3: one writer owns the terminal.** Lane A is the long stream —
  two helm installs, the second possibly retrying — so it keeps the terminal
  and streams live, while lane B writes to a buffer flushed after the join —
  a deliberate choice over the `switchWriter` concurrency.md names for this
  job, since lane B's output is short and nothing needs it before the join.
  `runConcurrent` would also satisfy invariant 3, but its cost, per its own
  comment, is that a branch's output appears only once every branch
  finishes; here that would hold back the operator phase's helm output until
  the operator phase is over. The installers print straight to stdout today
  (`run.Printf`, `run.CmdWithEnv`), so lane B's share of the cluster install
  must write to the buffer it is handed instead (`run.Fprintf`).

- **Invariant 4 does not constrain this.** Nothing in either lane runs
  privileged — device resolution reads world-readable by-path symlinks, and
  the rest is helm, git, a query to the local registry, and file I/O — so no
  prompt can compete for the terminal.

- **Both lanes' errors surface.** As in `upCreateAndBuild`, neither lane
  cancels the other; a lane that fails prints an immediate one-line notice,
  and both errors are returned, joined, after the join. A lane-B failure — a
  disk that never resolves — alongside a lane A that succeeds then leaves the
  cluster where it does today: operator and ceph-csi-drivers installed,
  cluster chart not.

- **Package variables are settled before the fork.** `up` assigns the
  `deploy*` package variables and then calls `deployCmd.RunE` directly, and
  `deploySetup` writes more of them (among them the name, the recorded shape,
  the kube context, the helm environment, the registry port). Both lanes read
  them, so every write must land before the fork and none after it. CI runs
  the unit tests under `-race`, so a test that drives both lanes checks it.

- **Three entry points, one overlap.** Only `deploy` has both phases.
  `deploy operator` and `deploy cluster` keep their sequential flows, and the
  profiles release stays after the cluster install in both `deploy` and
  `deploy cluster`. The cluster install's prep therefore has to be callable
  apart from its helm install without changing the single-chart path.

- **The cluster lock is unchanged.** Both lanes run inside the lock
  `deploySetup` takes, which the command releases when it returns.

## Out of scope: the ceph-csi-drivers → rook-ceph-cluster order

The join keeps the cluster install after ceph-csi-drivers, as today.
concurrency.md's deploy section records that order as call structure rather
than a data dependency — it falls out of `installCephCsiDrivers` being called
from inside the operator install — and names overlapping the two installs as
a candidate of its own. That overlap would move a helm install, not just local
prep, into the concurrent region, under constraints that section lists. That
is a separate change.

## Updating `concurrency.md`

Its *Concurrency in `deploy`* section and its `deploy` row under
*Per-command status* both describe deploy as sequential, with the cluster
prep's wait named as a candidate overlap, and the section credits the chain's
call order with keeping the restores apart. Both change when this lands: the
cluster prep overlaps the operator phase, and the restores are kept apart by
running ahead of the fork.

## Verification

- `go build ./...`, `go vet ./...`, `go vet -tags e2e ./test/e2e/`, and
  `go test -race ./...`.
- Unit tests that pin the schedule: the restores finish before either lane
  starts, lane B's output reaches the terminal only after the join and in one
  piece, and a failure in either lane surfaces alongside the other's.
- The e2e suite (`test/e2e`, run by the integration workflow). Its matrix
  gives lane A a different shape per entry, and each must pass: release-1.19
  (no ceph-csi-drivers install), release-1.20 and master (with it), and
  released v1.20.7 (charts from the chart cache, no restores).
- A wallclock comparison: `rooket deploy` timed before and after against the
  same cluster. Given *What the overlap buys*, expect a small difference on
  the fast path; the number is what justifies the restructuring.
