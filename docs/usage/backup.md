---
title: Backup
description: Create incremental, verifiable snapshots in an immutable repository.
---

# Backup

Docbank snapshot repositories are append-only directories of immutable,
checksummed files. A snapshot contains the deterministic JSONL description of
the virtual tree plus every catalog-authorized document blob, whether its live
representation is loose or packed. Unchanged metadata and content are reused
across snapshots, so repeated captures add only new repository objects and a
new manifest. Metadata is a complete logical JSONL description per snapshot,
not a row-level delta: an unchanged description is reused by hash, while any
logical change stores one new compressed metadata object.

The command surface initializes repositories, creates incremental snapshots,
lists and verifies them, and restores a proved vault into a separate target.

!!! warning "Repositories are not encrypted"
    Snapshot metadata and document content are compressed but not encrypted.
    Protect the repository with filesystem permissions and encrypted storage,
    especially before placing it on removable or cloud-synchronized media.
    Snapshot pruning and repository retention commands are also not exposed yet.

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

Set a default repository to omit `--repo`:

```toml
# $DOCBANK_HOME/config.toml
[backup]
repo = "~/Backups/docbank"
```

Restart the daemon after changing `config.toml`; configuration is read only at
daemon startup.

## Initialize a repository

```bash
docbank backup init [--repo DIR] [--json]
```

Initialization creates the repository layout and its random identity. It
refuses an existing non-empty or already initialized destination rather than
silently adopting unrelated files. The repository must be configured or
supplied explicitly. A relative CLI `--repo` is resolved from the invoking
shell's working directory before it is sent to the daemon.

## Create a snapshot

```bash
docbank backup create [--repo DIR] [--tag LABEL] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

Creation runs inside the one vault-owning daemon. It briefly pauses mutations
while opening a deferred SQLite read transaction, then normal writes resume
into the WAL while JSONL and document content stream from the pinned
point-in-time view. GC, trash empty, verification, and packed-storage
maintenance queue until capture finishes so they cannot remove content the
snapshot still requires. Every loose or packed content stream must reach
verified EOF before its bytes are accepted into the repository. A failure never
publishes a partial snapshot manifest; rerun the command after addressing the
error.

During an interactive run, `--progress auto` draws an in-place bar for each
stage (freeze, metadata, attachments, and seal), including item and byte
counts when available. Redirected output uses throttled, newline-terminated
progress instead, so logs remain readable. Force either behavior with
`--progress bar` or `--progress plain`. Progress goes to stderr; `--json`
suppresses it and writes one snapshot object to stdout for automation.

`--jobs 1` serializes blob readers for repositories on spinning disks or NAS
storage. Zero uses Kit's CPU-based default. `--tag` is a free-form label shown
by `backup list`. `--force-unlock` is recovery for a known-dead repository lock,
not a way to override another running backup.

## List snapshots

```bash
docbank backup list [--repo DIR] [--json]
```

The table reports immutable snapshot ID, creation time, logical file/blob
counts, bytes newly added to the repository, and tag. `--json` returns the same
typed snapshot summaries as `GET /api/v1/backup/snapshots`, including the
metadata format and parent snapshot ID.

## Verify repository integrity

```bash
docbank backup verify [SNAPSHOT] [--repo DIR] [--all] [--quick] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

With no snapshot argument, verification proves the latest snapshot. Pass an
immutable snapshot ID to prove one historical snapshot, or `--all` to prove
every manifest. A full verification resolves repository indexes and pack
footers, materializes the snapshot's JSONL metadata, reads and SHA-256 verifies
every referenced content object, and checks Docbank's recorded logical totals.
Content shared by several selected snapshots is read only once during one
verification pass. Every finding is reported with the affected snapshot, and
the command exits non-zero if any problems were found.

`--quick` checks manifests, indexes, pack structure, metadata, and logical
references without reading document content. Its `bytes_read` therefore still
includes metadata bytes. It is useful after each capture,
but it does not prove the storage medium has retained every content byte; run
full verification regularly. `--jobs 1` avoids concurrent reads on spinning
disks and latency-sensitive network storage. The progress and JSON contracts
match `backup create`: progress is written to stderr, and `--json` suppresses
progress so stdout contains one typed report.

!!! info "Planned — editing and audited-history fidelity"
    The editing/identity bootstrap will use `docbank-metadata-jsonl-v2`;
    today's v1 remains the pre-bootstrap format. Zero-scope v2 will preserve the
    stable vault ID, node-ID allocator high-water mark, content versions and
    current references, and non-reusable tag and ingest identities without audit
    genesis or lineage. Enabling the first audit scope will extend that same v2
    authority with audit scopes,
    sticky memberships, shared enrollment-baseline batches and digests,
    mutation records, per-scope
    chain heads, a complete vault-topology genesis snapshot, canonical
    unknown-origin records, baseline ancestor-spine witness generations,
    witness-change lists, atomic topology deltas, net path-effect commitments, a
    stable vault ID, both allocator high-water marks, a vault-wide allocation
    lineage, and authoritative
    tag/provenance attachments with the tag and ingest records they reference.
    Capture will include every protected historical blob. Verify and restore
    will recompute baseline digests from frozen enrollment records, replay and
    reconcile attached metadata, replay the vault topology from genesis, derive
    each enrollment's exact trash-origin closure, and derive each topology
    delta's net descendant and witness sets from the prior projections. They
    verify later mutations and allocation lineage separately, restore the
    allocators at the verified lineage tail, and reject internally missing,
    reordered, truncated, or hash-invalid authority rather than restoring only
    the current tree.
    Canonical mutations and allocation entries will be cross-bound by operation
    ID and mutation hash. Every post-audit topology delta will also be bound to
    its allocation entry by operation ID and delta digest, including deltas with
    no scoped effect; witness-change lists will be bound by count and digest.
    Each snapshot manifest will carry the stable vault ID, every scope
    count/head, and allocation-lineage count/head as one evidence bundle.
    Rollback detection requires that complete bundle from a trusted
    prior snapshot or external record; a fresh import cannot identify a
    coherently rewritten set of chains.

    Overwriting an existing audited target is forward-only: under the target
    lock, the snapshot must prove the same vault ID and preserve or extend every
    existing scope chain and the vault-wide allocation lineage. Independently
    mutated copies diverge in that lineage even when they consume the same node
    IDs or operation sequences. Older, pre-audit, divergent, or unrelated
    snapshots can restore to a fresh directory but cannot replace the audited
    target through the normal command. See the
    [Audited History](../architecture/audited-history.md) contract.

## Restore and prove a snapshot

```bash
docbank backup restore [SNAPSHOT] --target DIR [--repo DIR] [--overwrite]
                       [--jobs N] [--force-unlock]
                       [--progress auto|bar|plain] [--json]
```

Restore selects the latest snapshot by default; pass an immutable snapshot ID
to recover a historical point. The CLI resolves `--target` from its working
directory before sending an absolute server path to the daemon. The target
must be separate from both the running vault and the immutable repository.
Direct paths, parents, descendants, and symlink aliases that overlap either
one are rejected. Filesystem-identity checks also reject differently cased or
Unicode-normalized spellings that identify the same tree on filesystems where
those names are equivalent.

Every restore pins the target directory and takes its vault-tree lock before
writing, including for a fresh or empty target. That excludes a second restore,
a restore to any ancestor or descendant, and a daemon rooted anywhere in the
same tree. Replacing the target pathname while restore is running cannot
redirect publication. A successful restore leaves the ordinary `vault.lock` as
part of the usable vault. A failed restore also retains that stable advisory
file after releasing it: retries ignore `vault.lock` when deciding whether the
target contains payload, and retaining the pathname avoids split-lock races
between old and newly created lock files.

A new or empty target needs no destructive flag. A non-empty target is refused
unless `--overwrite` is explicit. Overwrite is a merge: files absent from the
snapshot remain in place. The old database and SQLite sidecars remain intact
until all repository content has been read and verified, the replacement
database passes `integrity_check`, and its logical statistics match the
manifest. Only then is the database published. A failed or cancelled restore
does not publish `docbank.db` for a new target and does not replace an existing
database.

Compatible repository packs are copied, verified, durably published, and
granted catalog authority by default. A pack or object that exceeds Docbank's
current storage policy is restored as a verified loose blob instead; the
result reports the loose count and grouped fallback reasons. This is a
representation choice, not an integrity failure.

Interactive restore shows metadata, document, extras, SQLite integrity, and
manifest-statistics progress as separate stages.
`--json` suppresses progress and returns one report containing physical layout
counts and explicit `content_verified`, `sqlite_integrity`, and
`manifest_stats` proof fields. A successful report means the target is a
complete vault, but it does not automatically replace or start it. Inspect it
under its own home first:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank verify
DOCBANK_HOME=~/Restores/docbank-test docbank tree /
DOCBANK_HOME=~/Restores/docbank-test docbank daemon stop
```

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Its files are write-once, making a completed
repository suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes; never edit
repository packs, indexes, manifests, locks, or configuration by hand.
