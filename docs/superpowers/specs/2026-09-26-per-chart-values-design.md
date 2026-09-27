# Per-chart values from a profile directory

Date: 2026-09-26
Status: approved, not yet implemented
Supersedes: layers 5 and 6 (`-f` and `--set`) of
[2026-07-21-chart-value-overrides-design.md](2026-07-21-chart-value-overrides-design.md)

## Problem

Programs that use rooket for integration testing need to set values for the
rook-ceph and rook-ceph-cluster charts on each run. The only per-run inputs
today are `-f` and `--set`, and both reach every release rooket installs:
rook-ceph, rook-ceph-cluster, and ceph-csi-drivers. A key meant for one chart
therefore lands in all three, and is harmless only while no other chart
happens to define it.

That is a gamble, not a property. `monitoring` is already a top-level key of
both rook charts. Nothing keeps the charts' key sets disjoint as they evolve,
and ceph-csi-drivers is published from a separate repository on its own
release cadence.

## Goals

- A caller supplies values for each chart separately, for one run, without
  writing into the rook clone or the user's config directory.
- Values meant for one chart can only reach that chart. A file that names no
  chart is an error, never silently ignored.
- The mechanism is documented with a worked example.

## Non-goals

- Chart-qualified flags (`--cluster-values`, `-f cluster=...`). Profiles
  already route values to a chart by filename; a second routing syntax would
  duplicate that.
- Suppressing the clone's `.rooket/values/<chart>.yaml` for a run.
  `--with-only` replaces the clone's profile list but not its sticky values;
  a CI checkout has none, and a developer's clone keeping them is intended.
- Accepting short chart names (`cluster.yaml`) as values filenames. Files on
  disk use full chart names, as the 2026-07-21 design established.

## Design

### Profiles by path

`--with` and `--with-only` accept a directory path anywhere they accept a
profile name — on `up`, `deploy`, and `values`. A value containing `/`, or
exactly `.` or `..`, is a path (`./mytest`, `/srv/tests/mytest`); any other
value is a profile name, resolved as today. A relative path resolves against the working directory
rooket was invoked from.

The directory has the layout every profile has:

```
mytest/
├── profile.yaml                  # description: ...
├── values/
│   ├── rook-ceph.yaml            # operator chart only
│   └── rook-ceph-cluster.yaml    # cluster chart only
└── templates/                    # optional; installed in rooket-profiles
    └── extra.yaml
```

A path profile is named for its directory's basename and is otherwise an
ordinary profile: it layers in selection order alongside named profiles, and
its templates install into the `rooket-profiles` release. Where a layer is
labelled for provenance (`values show --layers`), a path profile's label
carries the path as given, so two layers never share an ambiguous name.
`values profiles` lists an active path profile, marked active, alongside the
discoverable ones.

A test program's invocation is:

```console
$ rooket up --with-only ./mytest
```

Paths are accepted only on the command line. A `/` in the `profiles:` list of
`.rooket/config.yaml` is an error: a path there would resolve against
whatever directory rooket happened to be run from.

This changes the meaning of a value such as `./nfs`, which today silently
resolves to the built-in `nfs` profile; it becomes the `nfs` directory under
the working directory.

### Validation

Every rule below is an error that names the offending file or sources.

- A YAML file under a profile's `values/` must be named for one of the three
  charts: `rook-ceph`, `rook-ceph-cluster`, or `ceph-csi-drivers`. This holds
  for built-in, user, and path profiles alike. Today any other name is loaded
  and then never applied.
- Two active profiles with the same name must come from the same source. A
  path profile named `rbd` alongside the built-in `rbd`, or two directories
  both named `mytest`, is rejected. The generated chart already rejects two
  templates mapping to one filename; this rule also covers differently named
  templates and the values layers, which carry no such check.
- A path that does not exist, is not a directory, or has no `profile.yaml` is
  rejected, as is a directory whose basename is the reserved `local`.

### Removing `-f` and `--set`

`-f`/`--values` and `--set` are removed from `up`, `deploy`, and `values`.
Precedence, lowest first, becomes:

1. the chart's own `values.yaml`
2. rooket's generated base
3. `.rooket/values/<chart>.yaml` — sticky, this clone
4. active profiles, in selection order

This is a breaking change to the CLI. A caller that passed `-f extra.yaml`
moves its contents into `values/<chart>.yaml` of a profile directory, split by
the chart each key belongs to; a `--set` becomes a key in the same file.

## Testing

- Unit: telling a path from a name; resolving a relative path; each
  validation rule; a path profile's `values/rook-ceph-cluster.yaml` reaching
  the cluster chart's composed values and no other chart's.
- e2e: the profiles suite deploys with a path profile carrying a value for one
  chart and asserts, through each release's `helm get values`, that only that
  release received it.

## Documentation

The README's layer list drops `-f` and `--set`. A new section shows a profile
directory with per-chart values files, the `rooket up --with-only` invocation,
and `rooket values show <chart> --with-only ./dir --layers` as the way to
preview what each chart will receive.
