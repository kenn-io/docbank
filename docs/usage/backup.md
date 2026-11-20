---
title: Backup and restore
description: Create incremental snapshots in a backup repository, verify them, and restore a vault from them.
---

# Backup and restore

Create a backup snapshot to recover your vault later, independently of its
live storage. Each snapshot includes the folder tree, retained document
versions, and their retained derivatives, whether stored individually or in a
pack, and excludes unreferenced bytes waiting for garbage collection. Before
publishing a usable vault at a separate target, restore verifies the snapshot.

On 64-bit systems, backup and restore can read existing packed content larger
than 512 MiB without rewriting the packs. Reading older compressed content can
require memory proportional to the full uncompressed file size.

A repository is a directory of backup files that Docbank adds to without
changing existing files. Each snapshot has a manifest that records which files
belong to it, and repeated captures reuse unchanged content by hash.

Each snapshot also includes a complete metadata description in JSONL, with one
JSON record per line. Docbank reuses this description when it is unchanged. A
metadata change stores one new compressed description rather than a set of
individual row changes. See [Backup architecture](../architecture/backup.md)
for the repository format.

!!! warning "Repositories are not encrypted"
    Snapshot metadata and document content are compressed but not encrypted.
    Protect the repository with filesystem permissions and encrypted storage,
    especially before placing it on removable or cloud-synchronized media.

Use `backup forget` to remove selected recovery points and `backup prune` to
reclaim their storage. Embedded applications can use the
[same cleanup operations](../embedding.md#remove-recovery-points-and-reclaim-storage).

## Quick start

```bash
# One time
docbank backup init --repo ~/Backups/docbank

# Capture the live vault through its daemon
docbank backup create --repo ~/Backups/docbank --tag before-reorganization

# Inspect the snapshot history
docbank backup list --repo ~/Backups/docbank

# Prove the latest snapshot, including every referenced content byte
docbank backup verify --repo ~/Backups/docbank

# Recover and prove it without touching the running vault
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

To omit `--repo`, set a default repository:

```toml
# $DOCBANK_HOME/config.toml
[backup]
repo = "~/Backups/docbank"
```

Restart the daemon after changing `config.toml`. Docbank reads configuration
only at daemon startup.

## Schedule backups

Initialize the repository, then add a schedule in `config.toml`:

```toml
[backup]
repo = "~/Backups/docbank"
interval = "24h"
keep_last = 14
```

Restart the daemon to apply the schedule. The daemon captures immediately when
no scheduled snapshot exists or the newest one is due, and later captures run
at the configured interval. An overdue capture starts within one minute after
your computer wakes, and an enabled schedule keeps the background daemon
running through idle periods. The repository must already be initialized. If a
capture fails, the daemon retries after the shorter of the interval and five
minutes.

The schedule runs only while the daemon is running. After a reboot, the daemon
starts again when a Docbank command or the web app next needs it, and an
overdue capture begins then. To keep captures current across reboots, run
`docbank daemon start` from a login item or a launchd or systemd user service.

You can override the interval and retention without restarting:

```sh
docbank backup schedule set interval 1h
docbank backup schedule set keep_last 5
docbank backup schedule reset interval
docbank backup schedule show
```

`show` prints the active overrides beside the interval and retention in force.
Interval changes apply immediately, `keep_last` applies at the next capture's
retention, and resetting inherits `config.toml`. An in-flight capture finishes
with the retention count it started with. Overrides use the vault settings
revision and travel with backup and restore; a restored vault's schedule
applies after `docbank backup claim`, while the repository path stays in
`config.toml`. When the effective schedule cannot run, for example because a
restored vault carries an interval override to a home without `[backup] repo`,
Jobs reports the problem and the daemon can still stop when idle.

After each capture, retention keeps the newest `keep_last` scheduled points,
including the new one, and forgets older points. It prunes unused storage on
the first capture after the daemon starts and after it forgets a point, while
manual snapshots stay available. If cleanup fails, the new point stays
available and cleanup retries after the next scheduled capture.

Scheduled points use `docbank:scheduled:<vault-id>`, and retention selects only
this vault's tag. Manual captures through the CLI, HTTP API, and embedded API
reject the reserved `docbank:scheduled:` prefix, though the plain tag
`docbank:scheduled` remains an ordinary manual tag.

`docbank jobs` shows the schedule's latest error. When the newest scheduled
point is older than twice the interval, Jobs reports it as stale and the
daemon logs one warning per stale episode. This check reads snapshot times
after a restart. A future timestamp caps the initial wait at one interval, and
restarting resets that wait. A backward clock correction cannot extend an
active wait.

To disable scheduling, set `interval = "0"`. Enabling it requires `repo`, a
positive `keep_last`, and an interval of at least one minute.

After upgrading, [claim existing history](#adopt-an-existing-repository) before
resuming manual or scheduled captures.

## Initialize a repository

```bash
docbank backup init [--repo DIR] [--json]
```

Initialization creates the repository layout and its random identity, then
assigns it to the current vault installation. Each repository belongs to one
vault, and captures from another vault are refused. Moving or upgrading a
vault keeps its installation. Restoring a vault creates a new installation, so
the restored copy must [claim the repository](#adopt-an-existing-repository)
before it captures. The source-independent embedded initializer leaves an
empty repository unassigned until its first vault capture.

Initialization refuses a destination that is already initialized as a
repository. An empty unmarked repository binds to this vault on its first
capture. You must configure the repository or supply it explicitly. The CLI
resolves a relative `--repo` path from your invoking shell's working directory
before sending it to the daemon.

## Adopt an existing repository

When a repository has history but no `docbank-owner` marker, as after
upgrading from a release without ownership, or when another installation of
this vault owns it, as after a restore, claim it before Docbank adds snapshots
or uses its history for scheduling:

```bash
docbank backup claim --repo ~/Backups/docbank
```

The command prints the repository identity, snapshot count, and newest
snapshot time. When another installation of this vault owns the repository,
the output says so; claiming moves capture access to this installation, and
the other installation's captures then require a claim. Review the history,
then answer `y` or `yes` in a terminal to assign it to the selected vault, or
pass `--yes` for automation. Passing `--json` prints the final claim object
with `owned` true or false, while previews and prompts go to stderr.

Your confirmation applies only to the history you reviewed. If the repository
changes between the preview and the confirmation, the claim fails with
`backup_claim_changed` and exits `4`; run the command again to review the new
history.

A claim preserves existing snapshots and cannot take a repository from another
vault. Scheduled captures report the claim requirement in Jobs and leave
repository history untouched. Captures into a repository owned by another
vault return HTTP `409` with `backup_repository_owner_conflict` and exit `2`
in the CLI, while history this installation has not claimed returns `409` with
`backup_repository_claim_required` and exits `7`. Claims require the
operator's API credential. Embedded callers use
[`Vault.PreviewBackupClaim` and `Vault.ClaimBackupRepository`](../embedding.md#back-up-and-restore-an-embedded-vault).

Docbank releases without ownership can still list, verify, and restore owned
repositories. Because they do not check the owner marker, they can also add
manual snapshots to them, and scheduled retention never removes those
snapshots.

## Create a snapshot

```bash
docbank backup create [--repo DIR] [--tag LABEL] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

The daemon captures one consistent view of the vault. It briefly pauses
changes while opening a SQLite read transaction, and normal writes then resume
into SQLite's write-ahead log (WAL) while backup reads the earlier view.

GC, trash empty, verification, and packed-storage maintenance wait until the
capture finishes so they do not remove content needed by the snapshot. The
daemon accepts a document into the repository only after reading and verifying
all of its bytes. If capture fails, it publishes no partial snapshot manifest;
address the error, then rerun the command.

During an interactive run, `--progress auto` draws an in-place bar for each
stage (freeze, metadata, attachments, and seal), including item and byte
counts when available. Redirected output uses throttled, newline-terminated
progress instead, so logs remain readable. To force either behavior, pass
`--progress bar` or `--progress plain`. Progress goes to stderr, and `--json`
suppresses it to write one snapshot object to stdout for automation.

For repositories on spinning disks or NAS storage, pass `--jobs 1` to
serialize blob readers; zero uses Kit's CPU-based default. `--tag` sets a
free-form label shown by `backup list`. `--force-unlock` is recovery for a
known-dead repository lock, not a way to override another running backup.

## List snapshots

```bash
docbank backup list [--repo DIR] [--json]
```

The table reports each snapshot's ID, creation time, logical file/blob counts,
total blob bytes, bytes newly added to the repository, recorded coverage, and
tag. Passing `--json` returns the same typed snapshot summaries as
`GET /api/v1/backup/snapshots`, including the metadata format and parent
snapshot ID.

## What each snapshot contains

Create and list output include recorded coverage: photo assets and files,
distinct verified photo bytes, albums and members, hidden and trashed photos,
checkout definitions and entries, and retained change receipts. Browser
sessions receive snapshot lists without coverage. JSON summaries expose these
under `coverage`, with `version: 1`, `included`, and `excluded`, while
top-level `blob_bytes` records all verified blob bytes. `backup list` shows
one row per snapshot and one shared exclusions line. Older snapshots show
`unrecorded` in the coverage columns and omit the JSON field. Snapshots with
a newer coverage version show `unsupported` and an ID-specific warning, and
JSON includes `coverage_warning` while omitting `coverage`; their base
statistics and IDs remain available for cleanup. Before publication, restore
recounts version-1 coverage, preserves the recorded exclusions, and rejects
unsupported versions with upgrade guidance. Verification checks manifest
consistency and physical content and reports unsupported coverage versions; it
does not recount photo coverage.

Upgrade Docbank before restoring snapshots that record coverage version 1 with
a binary predating that coverage format. Older binaries refuse publication
with a statistics mismatch, and the manifest's physical reader version does
not express this application requirement.

Coverage excludes browser sessions, hidden unlock sessions, transient storage
operations, checkout destination bindings, and rebuildable search indexes,
photo timelines, quality signals, `photo_technical_metadata`, vector indexes,
and pending photo suggestion text. Suggestion dismissals and accepted-field
decisions are backed up but not counted in coverage, and restore describes the
undecided fields again after fresh consent. Hidden passcodes, lockout
decisions, ingest provenance, saved-query runs, and term-report history remain
included. The trashed-photo count includes any asset with at least one trashed
managed file, even when another member remains live. Restored checkouts stay
unbound until you explicitly rebind them.

## Remove recovery points and reclaim storage

```bash
docbank backup forget SNAPSHOT [SNAPSHOT...] [--repo DIR] [--dry-run] [--allow-empty] [--force-unlock] [--json]
docbank backup prune [--repo DIR] [--dry-run] [--force-unlock] [--json]
```

Pass exact IDs from `backup list`; unknown or misspelled IDs return 404.
Forget refuses the last recovery point unless you pass `--allow-empty`, and
refuses a parent required by a retained incremental snapshot. Forget removes
manifests, while prune separately removes unused packs and rewrites packs with
less than half their encoded content still live. Dry runs take the same
repository lock and report the planned work, with text output starting with
`dry run: nothing removed`.

Ownership checks govern captures and automatic retention, while explicit
`forget` and `prune` operate on the repository selected by the operator,
independently of its source vault.

These commands require the master daemon credential, and browser sessions
cannot run them. When cleanup errors occur with selected, planned, or
completed work, the API problem includes partial reports in its
`backup_forget` or `backup_prune` field. The CLI prints the report in text or
JSON before returning the cleanup error and exiting nonzero. Text output lists
selected and forgotten snapshot IDs, planned pack removals and rewrites, and
removed pack names. Items absent from completed lists remain pending or
unconfirmed, and bytes written alone do not prove a rewrite completed. Planned
bytes are not net savings, and cleanup does not securely erase content.

## Recover on a fresh installation

To recover on a fresh installation, use a temporary fresh home to run the
recovery daemon and restore into a separate directory. The target must be
disjoint from that home and the backup repository.

```bash
DOCBANK_HOME=~/Recovery/control docbank daemon start
DOCBANK_HOME=~/Recovery/control docbank backup restore --repo ~/Backups/docbank --target ~/Recovery/vault
DOCBANK_HOME=~/Recovery/control docbank daemon stop
DOCBANK_HOME=~/Recovery/vault docbank vault info
```

Before publishing the database, restore validates photo relationships and
recorded coverage, verifies managed content, clears sessions, and leaves
checkouts unbound. Keep the backup repository available until you have checked
the restored vault.

## Verify repository integrity

```bash
docbank backup verify [<snapshot>] [--repo DIR] [--all] [--quick] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

With no snapshot argument, Docbank verifies the latest snapshot. Pass a
snapshot ID to check a historical snapshot, or pass `--all` to check every
manifest. A full verification performs these checks:

1. Read the repository indexes and pack footers.
2. Reconstruct the snapshot's JSONL metadata.
3. Read every referenced content object and verify its SHA-256 hash.
4. Check recorded totals against manifest attachment totals and report unsupported coverage versions. Restore separately recounts version-1 coverage from the staged database before publication.

Docbank reads content shared by several selected snapshots only once during
one verification pass. Every finding is reported with the affected snapshot,
and the command exits non-zero if any problems were found.

Passing `--quick` checks manifest statistics and coverage versions, indexes,
pack structure, metadata, and logical references without reading document
content, so its `bytes_read` still includes metadata bytes. It is useful after
each capture, though it does not prove the storage medium still holds every
content byte; run full verification regularly. Pass `--jobs 1` to avoid
concurrent reads on spinning disks and latency-sensitive network storage. The
progress and JSON contracts match `backup create`: progress goes to stderr,
and `--json` suppresses progress so stdout contains one typed report.

Snapshots include the folder tree, trash state, retained versions, ingest and
provenance records, watched-source cursors, tags, saved queries, and
extraction records, as well as source metadata, renditions, visual previews,
and stored embedding results. Restore rebuilds and validates these records
without calling processing providers. See
[Backup architecture](../architecture/backup.md#which-retained-records-must-round-trip)
for retained records and rebuildable indexes.

Snapshots keep photo suggestion dismissals and accepted-field decisions, plus
a marker for an unconfirmed rotation proposal, and omit generated text and
rotation angles. After restore and fresh processing consent, Docbank describes
only undecided fields again. A dismissed caption stays dismissed while its
pending rotation regenerates, and a confirmed rotation needs no regeneration.

## Restore and prove a snapshot

```bash
docbank backup restore [<snapshot>] --target DIR [--repo DIR] [--overwrite]
                       [--jobs N] [--force-unlock]
                       [--store-map OWNER_PRIVATE_FILE]
                       [--progress auto|bar|plain] [--json]
```

By default, restore selects the latest snapshot; pass a snapshot ID to recover
a historical point. The CLI resolves `--target` from its working directory
before sending an absolute server path to the daemon. The target must be
separate from both the running vault and the repository, and Docbank rejects
direct paths, parents, descendants, and symlink aliases that overlap either
one. Filesystem-identity checks also reject differently cased or
Unicode-normalized spellings that identify the same tree on filesystems where
those names are equivalent.

### How does restore protect the target?

Before writing, restore pins the target directory and takes its vault-tree
lock, including for a fresh or empty target. That lock excludes a second
restore, a restore to any ancestor or descendant, and a daemon rooted anywhere
in the same tree. Replacing the target pathname while restore is running
cannot redirect publication. A successful restore leaves the ordinary
`vault.lock` as part of the usable vault, and a failed restore also leaves
that advisory file in place after releasing it. Retries ignore `vault.lock`
when deciding whether the target contains payload, and keeping the pathname
avoids split-lock races between old and newly created lock files.

A new or empty target needs no destructive flag, while a non-empty target is
refused unless you pass `--overwrite`. Overwrite is a merge, and files absent
from the snapshot remain in place. The old database and SQLite sidecars stay
intact until Docbank has read and verified all repository content, the
replacement database passes `integrity_check`, and its logical statistics
match the manifest. Only then does restore publish the database. Failure or
cancellation before publication leaves an existing database intact and creates
no `docbank.db` for a new target. If cleanup fails after publication, restore
reports an error and keeps the new database and its ownership marker, whereas
earlier failures restore the prior marker. When you open the vault or retry
the restore, Docbank reconciles an interrupted handoff against a durable
fingerprint of the prior database and the validated identity of whichever
database was published. It never opens an unrelated file named `docbank.db`
for mutation to decide which side won.

### Where does restored content go?

By default, restore copies compatible repository packs, verifies them, writes
them durably, and records them as readable locations in the catalog. If a pack
or object exceeds Docbank's current storage policy, restore writes it as a
verified individual blob file instead, and the result reports the loose count
and grouped fallback reasons. This is a representation choice, not an
integrity failure.

Individual files use the standalone daemon's compression policy, including
when restoring through the embedded API: files of at least 4 KiB use zstd
when it saves at least 10%, including large files reconstructed from backup
chunks. The restored database and host extras remain ordinary files. Restoring
an individual file temporarily needs space for both its uncompressed bytes and
its compressed candidate, and Docbank removes the unused copy when that file
finishes. An overwrite restore reuses existing verified files in their current
encoding.

Snapshots also carry a non-secret `docbank-placement-v1` description of source
stores and the hashes each store holds. Default restore places all verified
content in a fresh local primary store and gives that store a new ID and
ownership epoch, the value used to distinguish one store owner from the next.
It does not inherit source paths, endpoints, credentials, buckets, bindings,
or ownership epochs.

`--store-map` maps source store IDs to binding profiles already loaded by the
daemon performing the restore. The TOML file must be an owner-private regular
file and may select a new empty namespace or a takeover. Docbank reads mapped
bytes back independently before recording target authority, while unmapped
bytes stay local. A `remote_only` restore revokes primary catalog authority in
the staged database but leaves its physical staging files for garbage
collection after that database is published, so a failed overwrite cannot
damage the existing vault. Audited bytes also retain primary authority unless
the mapping selects both `remote_only` and `allow_audited_remote_only`. See
[Multi-store storage](storage.md#backup-and-restore) for the file format and
trust boundary.

The CLI turns a relative `--store-map` path into an absolute path from its
working directory, and the daemon opens and reads that path on the daemon
host, not on the caller's host. Mapping-file read and parse errors never
include the file contents.

Because the daemon opens and writes those server-local paths, the restore API
and explicit `repo` paths accept only loopback clients. A network client of a
daemon bound to a non-loopback address can create, list, and verify snapshots
in the configured `[backup] repo`; run restores on the daemon host, for
example with `docker exec`.

Backups exclude `config.toml`. Reconfigure processing profiles and other local
settings on the restored target before using them, and preserve the target's
storage bindings described below.

Restore also verifies that mapped filesystem stores do not overlap the live
source vault or backup repository. Before publication, it gives a target that
has no `config.toml` a minimal owner-private `config.toml` containing the
mapped binding profiles, so the restored vault can prove and read remote-only
authority after restart. An existing target configuration is preserved and
must already define the same mappings exactly.

### What does the restore report mean?

Interactive restore shows `metadata`, `attachments`, `extras`, `integrity`,
and `statistics` progress as separate stages. Passing `--json` suppresses
progress and returns one report containing physical layout counts and the
`content_verified`, `sqlite_integrity`, and `manifest_stats` proof fields. The
final report also inspects the restored vault's physical inventory while its
target-tree coordination is still held. It reports loose files, live packed
blobs, pack count, and any logically dead packed bytes that still occupy
immutable pack files. Those bytes have not been reclaimed. Inspect and repack
the restored target, not the currently running source vault:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank storage status
DOCBANK_HOME=~/Restores/docbank-test docbank storage repack
```

Reading this inventory does not run maintenance. If restore succeeds but the
inventory read fails, the report still records a successful restore and adds
a `storage_warning`.

A successful report means the target is a complete vault. Restore does not
start it or replace the running vault with it. Inspect it under its own home
first:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank vault verify
DOCBANK_HOME=~/Restores/docbank-test docbank document tree /
DOCBANK_HOME=~/Restores/docbank-test docbank daemon stop
```

## Backups move forward only

Restore with the release that wrote the backup or a newer one. Older releases
reject snapshots containing record kinds they do not understand instead of
restoring a vault without those records.

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Because its files are write-once, a completed
repository is suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes, and never
edit repository packs, indexes, manifests, locks, or configuration by hand.
