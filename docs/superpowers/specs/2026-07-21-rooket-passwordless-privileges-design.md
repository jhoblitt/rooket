# Passwordless privileges for rooket's iSCSI operations

Date: 2026-07-21

## Problem

`rooket block setup`, `rooket block teardown`, and `rooket down --all` need root
to drive `targetcli` and `iscsiadm`. Today `runPrivilegedScript` (`cmd/block.go`)
generates a shell script and runs it as root: directly when already root,
otherwise `sudo -n sh` (never prompts, fails without passwordless sudo), then
`pkexec sh /tmp/rooket-iscsi-*.sh`, which prompts.

On a workstation without passwordless sudo, every cluster bring-up that needs
new iSCSI targets — and every `--delete-disks` teardown — costs an interactive
authentication prompt. The goal is a host configuration that removes the prompt
without abandoning the current behavior on hosts that have no such
configuration.

## Key finding: this grant is root-equivalent, and cannot be made otherwise

An earlier draft proposed a root-owned helper binary with a constrained verb
vocabulary, so that the passwordless grant could be narrowed to something
auditable. That approach was rejected because it cannot deliver the property it
exists to provide.

`targetcli /backstores/fileio create <name> <path> <size>` exposes whatever
`<path>` resolves to as a block device with root-level read/write. rooket's disk
images are owned and writable by the invoking user:

```
-rw-r--r-- 1 jhoblitt jhoblitt 10737418240 worker0-disk0.img
```

A helper that *derives* the path itself, never accepting one as an argument, is
still defeated by `rm worker0-disk0.img && ln -s /etc/shadow worker0-disk0.img`.
Deriving the path constrains the string, not the inode, and LIO follows the
symlink. `O_NOFOLLOW` validation in the helper does not close it either, because
targetcli reopens the path by name afterward.

Closing the hole would require moving the images into a directory the user
cannot write, which would break the unprivileged `truncate 0` → `truncate size`
zap that the loop-free teardown depends on.

Therefore: **passwordless iSCSI target creation over user-owned images grants
root, however it is packaged.** A helper would add a second build artifact, an
install step, a stale-binary failure mode, and a parallel code path in exchange
for no security gain.

What a narrow rule still buys, and all that it buys: blast radius for typos, not
granting `targetcli`'s interactive root REPL, and a self-documenting record of
what rooket touches. The generated file states this limitation in a comment so a
future reader does not mistake it for a privilege boundary.

## Design

One change in rooket, one file on the host, granted through sudoers only. polkit
is not used: it would require the same helper (pkexec keys on a program path,
and a `/tmp` script path cannot be whitelisted), and sudoers additionally works
headless and in CI.

### Step list replaces the generated script

`runPrivilegedScript(script string)` becomes a list of steps with two executors
driven from the same data:

```go
type privStep struct {
	argv      []string
	stdin     string // the initiatorname write
	ignoreErr bool   // today's "|| true"
}
```

`buildISCSIScript` and `buildISCSITeardownScript` return `[]privStep` instead of
a string. `renderScript([]privStep) string` reproduces today's shell text
exactly, and is needed regardless: `blockSetupRun` already prints the script in
its error path ("Run the following script manually with root privileges").

Execution order in `runPrivileged([]privStep)`:

1. euid 0 → run each step directly.
2. `sudo -n cat /etc/sudoers.d/rooket` succeeds, OR `sudo -n true` succeeds
   (some other passwordless grant is available — e.g. a workstation's blanket
   `%wheel ALL=(ALL) NOPASSWD: ALL`, or a CI runner's) → run each step as
   `sudo -n <argv>`, no prompts. If a step is denied anyway — a rule stale in
   a way the probe can't see, e.g. a distro upgrade relocating `targetcli` —
   fall back to step 3 rather than surfacing the denial: every step is
   idempotent by construction, so retrying the whole list through pkexec costs
   one extra prompt instead of failing.
3. Otherwise → `renderScript` → temp file → `pkexec sh`, one prompt.

Step 2's first probe is the same call `sudoers status` uses. Both probes are
side-effect free, unlike `targetcli --version`, which takes
`/var/run/targetcli.lock`. `sudo -n true` only detects a grant broader than
rooket's own — `true` is deliberately excluded from `privilegedCommands`, so a
host holding nothing but rooket's narrow rule fails this second probe, keeping
it distinguishable from the first even though runPrivileged treats a pass on
either one the same way.

### Single command vocabulary

Both the sudoers renderer and the step validator derive from one declaration, so
the rule and the code cannot drift:

```go
var privilegedCommands = []privCommand{
	{name: "targetcli", anyArgs: true, denyBare: true},
	{name: "iscsiadm",  anyArgs: true},
	{name: "systemctl", exactArgs: []string{"start", "iscsid"}},
	{name: "systemctl", exactArgs: []string{"restart", "iscsid"}},
	{name: "tee",       exactArgs: []string{"/etc/iscsi/initiatorname.iscsi"}},
	{name: "cat",       exactArgs: []string{"/etc/sudoers.d/rooket"}},
}
```

`renderSudoers` emits the `Cmnd_Alias` from this list. `validateSteps` asserts
every generated step is covered by it. A new privileged command that is not
added to the vocabulary fails a unit test rather than silently producing a rule
that prompts at runtime.

The check is deliberately one-directional — every step must be in the
vocabulary, but not every vocabulary entry need appear as a step. `cat` is
granted for `status` and the executor probe and is never a step; `tee` and
`systemctl restart iscsid` appear only when the initiator name needs writing
(below). All of them must stay in the vocabulary regardless, because a first
run on a fresh host does emit them.

### Conditional initiatorname write

`buildISCSIScript` currently emits, unconditionally:

```sh
printf 'InitiatorName=%s\n' '<iqn>' | tee /etc/iscsi/initiatorname.iscsi > /dev/null
...
systemctl restart iscsid && sleep 1
```

The initiator name is a constant — `iqn.<date>.local.rooket:initiator`, not
per-cluster — so on any host that has run `block setup` before, this rewrites
the file with the bytes it already contains and then restarts iscsid to pick up
a change that did not happen.

The restart is the problem. `iscsid` is host-global and shared by every rooket
cluster's live iSCSI sessions, and rooket supports per-clone clusters that
coexist (`rook`, `rook3`, …). Bringing up cluster B therefore bounces the daemon
underneath cluster A's in-use OSD disks, for no benefit. Both steps are dropped
when the file already declares the wanted name:

```go
// blockSetupRun, before building the steps
writeInitiator := !initiatorNameCurrent(initIQN)
steps := buildISCSIScript(initIQN, disks, sizeGB, writeInitiator)
```

`initiatorNameCurrent` reads `/etc/iscsi/initiatorname.iscsi`, which is
world-readable (`-rw-r--r-- root root`), so the check needs no privileges. It
compares the file's `InitiatorName=` assignment against the wanted IQN, ignoring
comments and blank lines, and returns false when the file is absent, unreadable,
or declares a different or additional name. The write itself keeps today's
semantics: `tee` truncates, so a stale file is replaced rather than appended to.

The `systemctl restart iscsid` step is emitted **if and only if** the write is,
and the `sleep 1` that follows it goes with it. `systemctl start iscsid` stays
unconditional: it is idempotent, and on hosts where `iscsid.socket` is not
enabled it is what makes the daemon available at all.

The I/O stays out of the builder — `blockSetupRun` reads the file and passes a
bool — so the step builders remain pure functions over their arguments and stay
directly testable.

This is a correctness fix rather than a prompt reduction. It saves no prompts:
`blockSetupRun` already skips the whole privileged block when
`allISCSIDevicesPresent`, and when a target *is* missing the run needs
`targetcli` anyway.

### Generated file

`/etc/sudoers.d/rooket`, mode 0440, owned root:root:

```sudoers
# Managed by rooket. Do not edit; regenerate with `rooket sudoers install`.
#
# Grants passwordless root for the iSCSI target operations performed by
# `rooket block setup`, `rooket block teardown`, and `rooket down --all`.
#
# SECURITY: this grant is root-equivalent. targetcli can expose any file as a
# fileio backstore and rooket's disk images are user-writable, so anyone
# holding this grant can obtain root. It is a convenience for a single-user
# development workstation, not a privilege boundary.

Cmnd_Alias ROOKET_ISCSI = /usr/bin/targetcli, \
                          /usr/sbin/iscsiadm, \
                          /usr/bin/systemctl start iscsid, \
                          /usr/bin/systemctl restart iscsid, \
                          /usr/bin/tee /etc/iscsi/initiatorname.iscsi, \
                          /usr/bin/cat /etc/sudoers.d/rooket

jhoblitt ALL=(root) NOPASSWD: ROOKET_ISCSI, !/usr/bin/targetcli ""
```

Rationale for each constraint:

- `systemctl` is pinned to two exact verb+unit pairs. An unrestricted
  `systemctl` grant is unconditional root via `systemctl link`.
- `tee` is pinned to the single file it writes.
- `!/usr/bin/targetcli ""` denies the no-argument form, which is targetcli's
  interactive root configuration shell. Bare `/usr/bin/targetcli` in the alias
  permits any arguments; sudoers is last-match-wins, so the trailing negation
  removes only the REPL.
- `cat` is pinned to rooket's own file. This is the one grant here that is *not*
  root-equivalent: `/etc/sudoers.d` is mode 0750 root:root, so the user cannot
  replace the file with a symlink to another path, and the content is a policy
  declaration rather than a secret.

Binary paths are resolved at install time and baked in absolute, as sudoers
requires.

### Why `cat`, and not file permissions

`/etc/sudoers.d` is mode **0750 root:root**. The directory denies traversal to
non-root regardless of the file's own mode, so relaxing the file to 0444 would
not make it readable; only loosening the directory would, which is strictly
worse than granting one pinned `cat`. The `cat` grant is the only way for rooket
to read its own installed rule without prompting.

### `rooket sudoers` subcommand

| Command | Root needed | Purpose |
|---|---|---|
| `rooket sudoers print` | no | Render the file to stdout for inspection or manual installation. |
| `rooket sudoers status` | no | Report whether the grant is live and current. |
| `rooket sudoers install` | yes (one prompt) | Validate, then atomically install the file. |
| `rooket sudoers uninstall` | yes (one prompt) | Remove the file. |

`--user` selects the granted account, defaulting to `$SUDO_USER` when set (so
`sudo rooket sudoers install` grants the real user, not root) and otherwise the
current user.

Installing the no-prompt rule costs exactly one authenticated prompt, once. This
is inherent, not a defect.

#### Install procedure

A malformed file in `/etc/sudoers.d/` breaks `sudo` for the entire machine, so
the sequence is:

1. Resolve each binary with `LookPath`, then **reject any resolution that is not
   root-owned or that is user-writable** before baking it into the file. `$PATH`
   is user-controlled; without this check `install` would write
   `/home/jhoblitt/evil/targetcli` into sudoers.
2. Write to `/etc/sudoers.d/rooket.tmp`. Sudo's `#includedir` ignores filenames
   containing a dot, so the staging file is inert even mid-write.
3. Run `visudo -cf /etc/sudoers.d/rooket.tmp`. On failure, unlink and abort with
   visudo's diagnostics.
4. `chmod 0440`, `chown root:root`, then `rename(2)` onto
   `/etc/sudoers.d/rooket` — atomic, same filesystem.

#### `status` states

`sudo -n cat /etc/sudoers.d/rooket`:

- **exit 0** → grant is live. Compare the output byte-for-byte against a fresh
  render: `up to date`, or `stale — run rooket sudoers install`.
- **non-zero** → not installed, or installed by a rooket predating a vocabulary
  change. The remedy is the same in both cases, so `status` does not distinguish
  them; it reports `not installed — run rooket sudoers install`.

The comparison is byte-for-byte over the whole file, comments included, because
the file declares itself unmanageable by hand. It catches the failure mode worth
catching: a distro upgrade relocating `targetcli` renders differently from what
is installed, and `status` flags it instead of leaving a mysterious prompt to
appear one day.

Execution degrades correctly in the stale case: `block setup` falls back to the
`pkexec` path, which is exactly today's behavior.

## Testing

Three layers. Only the third needs new CI infrastructure.

GitHub runners have blanket `NOPASSWD: ALL` — the existing e2e job depends on it
to run `rooket block setup` unattended. A test asserting "the passwordless path
worked" therefore passes vacuously on a runner; it would pass with the sudoers
file deleted. Policy assertions must run where the test user holds *only*
rooket's rule, which means a container.

### 1. Unit tests — existing `unit` job

`cmd/block_test.go`, `cmd/sudoers_test.go`:

- `renderScript(steps)` reproduces today's script text, so the existing quoting
  tests (`TestBuildISCSIScriptsQuoteOperands`) retain their meaning for the
  pkexec fallback.
- Every step produced by the builders is covered by `privilegedCommands`, with
  `writeInitiator` both true and false, so neither variant can introduce an
  ungranted command.
- `renderSudoers` golden output.
- `LookPath` validation rejects a user-writable or non-root-owned resolution.
- `initiatorNameCurrent` against a `t.TempDir()` file: exact match; different
  IQN; absent file; unreadable file; leading/trailing whitespace; comment lines;
  a second `InitiatorName=` assignment (must return false — the file must
  declare the wanted name and nothing else).
- `buildISCSIScript` with `writeInitiator` false omits **both** the `tee` step
  and `systemctl restart iscsid`, and retains `systemctl start iscsid`; with it
  true, emits both, with `restart` ordered after the write. Asserting the pairing
  directly is what keeps a later edit from reintroducing an unconditional
  restart.

### 2. Executor tests with a stub `sudo` — existing `unit` job

No root, no iSCSI. A stub `sudo` on `$PATH` records its argv and exits 0 →
assert the exact itemized invocations, in order, with stdin routed to the `tee`
step. A second stub exiting non-zero → assert rooket renders the script and
invokes `pkexec`.

This is the only coverage the **fallback path** receives; neither the runner nor
the container exercises it.

### 3. New `sudoers-policy` CI job

A Fedora container with a test user holding no sudo rights. Install via
`rooket sudoers install --user tester` (running as root in the container, so no
prompt), then assert a permit/deny matrix with `sudo -l -U tester <cmd>`, which
reports permission **without executing anything**:

| Must be permitted | Must be denied |
|---|---|
| `targetcli /iscsi create iqn.2003-01.local.rooket:x` | `sh -c id` |
| `iscsiadm -m node --login` | `targetcli` (bare — the root REPL) |
| `systemctl restart iscsid` | `systemctl restart sshd` |
| `tee /etc/iscsi/initiatorname.iscsi` | `tee /etc/passwd` |
| `cat /etc/sudoers.d/rooket` | `cat /etc/shadow`, `cat /etc/sudoers` |

The job also asserts `visudo -cf` accepts the generated file, that the installed
file is 0440 root:root, that `sudoers status` reports `up to date` immediately
after install, and that `uninstall` removes it and returns `status` to
`not installed`.

The deny half is what earns this job its keep: it catches a rule that
accidentally grants `systemctl` or `cat` wholesale. It is hermetic and fast — no
kind, no LIO, no rook.

### Incidental coverage

The runner has no rooket rule installed, so `sudoersGrantLive`'s `cat` probe
never succeeds there; it's the runner's blanket NOPASSWD making the second,
`sudo -n true` probe succeed that routes the existing e2e matrix onto the
itemized `sudo -n <argv>` path against real targetcli and iscsiadm — coverage
that does not exist by the `cat` probe alone. A wrong argv in any step fails
e2e loudly.

The e2e job also covers both initiatorname branches for free: the runner starts
with no `/etc/iscsi/initiatorname.iscsi`, so the workflow's `Set up iSCSI OSD
disks` step takes the write branch, and every subsequent `rooket up` in the
suite takes the skip branch against a real iscsid.

## Non-goals

- **polkit.** Rejected above; sudoers alone carries the grant.
- **A root-owned helper binary.** Rejected above.
- **rooket enabling `target.service`.** `target.service` is disabled on the
  reference workstation, so LIO configuration is never restored at boot and
  every reboot forces a fresh privileged setup. Enabling it is the single
  largest reduction in how often the grant is exercised, but it makes targets
  from deleted clusters survive reboots until pruned. That trade-off belongs to
  the host owner, so it is documented in the README as a recommendation rather
  than performed by rooket.
- **Making the grant non-root-equivalent.** Established above as impossible
  without moving the disk images out of user-writable storage.
- **Removing `systemctl` from the grant entirely.** `systemctl start iscsid`
  stays unconditional, so the pinned `start`/`restart` pair remains in the
  sudoers file even though `restart` is rarely emitted.
