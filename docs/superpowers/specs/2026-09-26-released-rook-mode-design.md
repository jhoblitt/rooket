# Released-Rook mode

Date: 2026-09-26
Status: approved, not yet implemented

## Problem

rooket deploys Rook only from a source checkout. `rooket up` locates a rook
clone, runs `make` in it, pushes the operator image to the cluster's local
registry, and installs the charts from the clone's `deploy/charts/`. The clone
also supplies the sticky configuration (`.rooket/`), the cluster's default
name, and the provenance `rooket prune` uses to tell a parked cluster from an
abandoned one.

A project that consumes Rook rather than developing it has no clone and needs
none. jhoblitt/rooket#58 (item 1) is the case that prompted this: rgw-go wants
rooket as its test harness, deploying a released Rook, and its CI has no rook
checkout. For such a consumer the build is pure cost — it is the long pole of
`rooket up` — and the charts it needs are published at
`https://charts.rook.io/release`.

## Goals

- `rooket up --rook-version v1.20.7` brings up a cluster from the released
  charts and images, with no rook clone anywhere and no build.
- Sticky configuration works without a clone, from a directory the consumer
  names and can keep under version control.
- Everything later in a cluster's life — `deploy`, `values`, `down`, `prune` —
  works from what the cluster recorded, without repeating the flags.
- Clone mode is unchanged for every user who never passes the new flags.

## Non-goals

- Version ranges (`v1.20.x`). The recorded version must say exactly what was
  deployed, and a range resolved anew on each deploy would drift under a
  cluster. Consumers pin an exact tag.
- A chart repository other than `https://charts.rook.io/release`, such as a
  mirror. Nothing asks for it yet.
- Switching a released cluster back to clone mode in place. `rooket down
  --delete-disks` removes the record, after which the next `up` starts over.
- Custom Ceph or radosgw images (#58 item 9) and a readiness wait (#58 item 7):
  separate changes.

## Design

The rook clone plays three roles today, and released mode separates them:
where the charts come from, where sticky configuration lives, and where the
operator image comes from.

### Charts

`--rook-version <tag>` on `up` and `deploy` selects released mode. The tag is
an exact released version, pre-releases included; anything that is not a
single version is rejected before any work starts.

rooket pulls the `rook-ceph` and `rook-ceph-cluster` charts at that version
into a host-wide cache under the user cache directory, one entry per version,
and installs from there. Every existing read of a chart's files then works
unchanged against the cache entry: the operator chart's `Chart.yaml`, whose
`ceph-csi-operator` dependency decides whether and at which version the
`ceph-csi-drivers` chart is installed, and the cluster chart's `values.yaml`,
whose pool lists a cluster of one or two workers gets fitted from. Released
v1.20.7 carries the same dependency declaration a clone's chart does, so the
drivers decision needs no second code path.

The alternative — handing helm `--repo` and `--version` and reading chart
files through `helm show` — needs a second path for every one of those reads
and the network for every deploy and every `values show`. The cache needs the
network only the first time a version is used.

Constraints on the cache:

- An entry is populated by pulling into a temporary directory beside it and
  renaming it into place, so a failed or interrupted pull never leaves a
  partial entry that later runs would trust. Two runs pulling the same version
  at once both succeed: the loser of the rename discards its copy and uses the
  winner's.
- A released chart ships its dependencies unpacked under its own `charts/`
  directory, not as the archives clone mode restores with `helm dependency
  build`. The restore step is skipped in released mode; run against a pulled
  chart it would try to rebuild dependencies the chart already has, and fail on
  the chart's `file://` library dependency.
- Nothing evicts entries. They are small, and an entry a cluster was deployed
  from is exactly what a later `deploy` of that cluster needs.

### Images

In released mode the operator image is the one the chart pins — for v1.20.7,
`docker.io/rook/ceph:v1.20.7` — and the Ceph image is the one the cluster chart
pins. rooket's generated base for the operator chart omits its image, digest
annotation, and pull policy, which exist to roll a mutable local tag; a
released tag does not move. The shared pull-through cache already proxies
`docker.io` and `quay.io`, so the images are fetched once per host.

`up` skips its build step and says so. `--force-build` with `--rook-version`
is rejected: there is nothing to build. `rooket build` itself is unchanged.

### Configuration home

`--config-dir <dir>`, or `$ROOKET_CONFIG_DIR`, names a directory with the
layout of `.rooket/`: `values/<chart>.yaml`, `templates/`, and `config.yaml`
for the profile list. The configuration home is resolved as: the flag, then
the environment variable, then the cluster's recorded directory, then the
`.rooket/` of the rook clone the command runs against or within, then none.
That last step is today's behavior, so a cluster that never names a directory
keeps its clone's configuration. The configuration home is independent of the
chart source, so a clone-mode cluster may name one too.

rooket never writes a `.gitignore` into a directory named this way. Clone mode
writes one containing `*` to keep `.rooket/` out of the rook checkout's `git
status`; a consumer's configuration directory is the opposite case — rgw-go
would keep it in its own repository — and that file would hide it from git.
Subdirectories are created only when rooket writes into one, which only
`values edit` does.

### What a cluster records

The rook version and an explicitly named configuration directory are recorded
in the cluster's state directory, beside its shape, and every later command
reads them:

- `deploy`, `values show`, `values edit`, and `values profiles` take the
  version and configuration home from the record unless given on the command
  line.
- An explicit `--rook-version` that differs from the record replaces it on
  `up` and `deploy`. Unlike a change of shape, a change of chart version is an
  ordinary `helm upgrade` of a running cluster, so there is nothing to refuse.
  Passing `--rook-version` to a clone-mode cluster moves it to released mode
  the same way.
- A recorded released version takes precedence over the enclosing clone: a
  released cluster stays released when a later command runs inside a clone.

### Naming

The name keeps its precedence: `--name`, `$ROOKET_NAME`, the enclosing clone's
path. Outside a clone, a command falls back to the fixed name `rook`. A command
given `--rook-version` refuses that fallback and asks for `--name` or
`$ROOKET_NAME`, because two unrelated consumers on one host would otherwise
share a cluster. Commands that only find released mode in a record already had
a name to find the record by, so they are unaffected.

### Prune

`prune` sweeps state directories of clusters that no longer exist, keeping
those parked by `rooket down`. It tells them apart by whether the cluster's
owner still exists, and counts a cluster with no recorded owner as abandoned,
so that state directories predating the provenance record stay sweepable.

A released cluster's owner is its recorded configuration directory, or the
clone it was created in when it has one: it is parked while either exists and
abandoned once both are gone. A released cluster with neither is parked,
kept until `rooket down --delete-disks` or `prune --include-parked`. Its record
is proof it is not a pre-provenance leftover, and nothing on disk will ever
disappear to say it was abandoned.

## Failure modes

- The chart repository is unreachable on a version's first use: `up` fails
  before creating anything, naming the version and the repository.
- The version does not exist: the same failure, reported from the pull.
- A cache entry exists but was damaged afterwards: `helm upgrade` fails on the
  chart, and the error names the entry to delete. rooket does not verify
  entries on every use.

## Testing

- Unit specs for the resolution order of the version and the configuration
  home, the record's round trip, population of a cache entry through an
  injected puller (including a concurrent loser and an interrupted pull), the
  skipped dependency restore, the operator base without an image, the naming
  refusal, prune's classification of released clusters, and that a named
  configuration directory never gets a `.gitignore`.
- An e2e CI job that runs `up --rook-version <pinned> --workers 1` with no rook
  checkout, followed by the suite's specs that do not need a clone.
- A local run of the same before the change is called done.
