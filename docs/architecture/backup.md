---
title: Backup & Recovery
description: Docbank's JSONL-native Kit snapshot and restore architecture.
---

# Backup and recovery

A Docbank snapshot captures the logical vault and a verified copy of every
retained content blob. Restore rebuilds the metadata in a fresh database and
publishes the vault only after its checks pass. It does not require the source
vault's physical storage layout.

This page owns the snapshot and restore architecture. Use the
[Backup user guide](../usage/backup.md) for commands and the
[embedded backup guide](../embedding.md#back-up-and-restore-an-embedded-vault)
for Go integration.

## Which entry point owns the operation?

Standalone `docbank backup init`, `backup create`, `backup list`, `backup
verify`, and `backup restore` use the authenticated daemon API; see the
[Backup user guide](../usage/backup.md). Applications that own an embedded
vault use `BackupRepository`, `Vault.CreateBackup`, and `Vault.RestoreBackup`
directly; see [Embedding Docbank](../embedding.md#back-up-and-restore-an-embedded-vault).
`BackupRepository.Restore` works without a source vault and uses the build's
default SQLite driver. `Vault.RestoreBackup` supplies its configured driver and
adds the source vault root to the protected set. Repository callers must
explicitly declare any offline storage that restore must preserve.

Both methods use the same restore path. It excludes the repository and protected
roots, locks the target hierarchy, restores host files, and verifies the result
before publication.
A coherent local-state filesystem snapshot remains available by stopping the
daemon before copying the vault, but it is not a topology-independent backup;
see [Vault Lifecycle](../usage/lifecycle.md#take-a-coherent-backup).

The database plus built-in `blobs/` directory is a complete manual archive only
while every retained blob has primary authority. A vault may deliberately keep
its sole verified copy in a secondary store, so the built-in snapshot workflow
is the topology-independent recovery authority: it reads one verified
candidate for every logical blob or fails without publishing a partial
snapshot. Configuration is useful to retain when customized; logs, locks, and
runtime records are not archive state. A restored copy is not trusted until
`docbank verify` succeeds.

## How does capture keep one consistent view?

**Kit** is the shared library that manages the snapshot repository and physical
content transfer. Docbank supplies the logical records and decides which bytes
the snapshot must retain.

The internal `backupapp` adapter supplies three things to Kit: the complete
set of blobs authorized for backup, statistics for checking the restored
logical state, and readers for loose or packed content.

Capture establishes one consistent view:

1. The vault owner briefly blocks mutations and pins a deferred SQLite read
   transaction.
2. The owner releases that block. Writers resume into SQLite's write-ahead log
   (WAL).
3. Capture reads metadata, content membership, and statistics through the pinned
   transaction. All three continue to describe the same point in time.

The same pinned transaction emits a separate deterministic
`docbank-placement-v1` artifact. It names source store UUIDs, display names,
backend kinds, roles, per-hash store UUIDs, and aggregate counts and bytes.
Deployment bindings, paths, endpoints, bucket coordinates, credentials,
ownership epochs, encodings, and pack coordinates are excluded. Placement
authority changes take the preservation side exclusively for their short
catalog commit, so the logical membership and placement artifact cannot
describe different authority handoffs.

## What metadata does a snapshot retain?

Docbank exports logical metadata as deterministic JSONL: one JSON record per
line, with stable record and field ordering. Snapshot manifests identify this
artifact as `docbank-metadata-jsonl-v1`. It contains the complete virtual
directory tree and file records,
including stable IDs, content hashes, timestamps, trash coordinates, prior
versions, ingest provenance, watched-source cursors, tags, and extracted text.
It omits rebuildable full-text and vector indexes and physical pack mappings.
Restore rebuilds full-text search and vector indexes from retained records.
When a vector source cannot be rebuilt locally, restore records that missing
coverage instead of calling an external provider. Restore grants physical
authority only after content has been verified and published.

Import targets must be fresh
current-schema databases;
a malformed or referentially incomplete stream leaves the pristine target
unchanged.

Capture makes two deterministic passes over the same pinned
transaction: the first establishes the exact artifact size and the second
streams the bytes into Kit without materializing a second database or a JSONL
temporary file.

The header also preserves the node-ID allocation high-water
mark, including IDs whose rows were later deleted, so restore never reuses a
value that an external reference may remember.

## When does copied content become trusted?

Capture reads raw loose, zstd loose, and packed blobs through Kit's
bounded-memory stream. The physical source encoding is not copied into backup
metadata: Kit decodes and verifies the logical bytes before repository
publication. The archive may grant authority to copied bytes only after
terminal EOF verifies
their stored framing, decoded length, and SHA-256 identity; opening a stream or
closing it early is not a successful copy.

## How does restore publish a complete vault?

Restore performs these steps in Kit's private staging area:

1. Build a fresh current-schema database from verified JSONL.
2. Checkpoint the database.
3. Publish verified content.
4. Grant fresh catalog authority to the chosen loose or packed representation.
5. Check that the restored logical state matches the recorded statistics.
6. Publish the staged vault.

Source pack rows never enter the JSONL artifact. Docbank's restore wrapper owns
both the metadata restorer and packed target, keeping their policies together.
Integration coverage proves logical JSONL equality, loose and packed source capture,
packed publication, large loose-object fallback, and reads every restored blob
through the same mixed store used by a live vault.

### How are multiple stores restored?

Default restore treats the placement artifact as informational and places all
content in a fresh fixed primary. Explicit store mapping first restores
and proves a complete local copy, then claims fresh target store identities,
publishes or adopts immutable destination objects, reads every object back,
and records mapped authority while target-tree coordination remains held.
A remote-only database is published without primary catalog authority, but
restore leaves the now-untracked primary files intact until publication
succeeds; ordinary garbage collection may reclaim them afterward. A failed
restore therefore cannot remove bytes still owned by the database it was meant
to replace. Audit-protected bytes retain primary authority unless the mapping
includes the explicit remote-only acknowledgement.

### What happens if publication stops midway?

The restored database and its built-in primary marker change ownership as one
recoverable publication. Before replacing the database, Docbank durably records
the prior database fingerprint and both marker identities, installs and reads
back the restored marker, and rolls it back if publication fails. If the process
stops between those steps, the next restore compares the visible database with
that fingerprint without opening unknown files for mutation; a normal vault
open reconciles its validated catalog identity. Storage access begins only
after the marker agrees with the database that actually became visible.

!!! info "Historical snapshot format"
    Earlier development snapshots used Kit's SQLite page-map metadata. The
    restore wrapper still reads them. Every new capture uses JSONL; callers
    cannot select the historical format for a new snapshot.

## Which operations may run during capture?

The daemon's create handler uses two sides of the operation gate. Kit's freeze
coordinator briefly takes the mutation-exclusive side while Docbank pins the
deferred JSONL transaction, then releases it so writers can resume before
metadata and blob streaming finishes. A separate shared preservation side is
held for the complete capture. Maintenance takes that side exclusively, so GC,
trash empty, verification, pack, and repack cannot remove or replace content
authority still named by the pinned snapshot. The repository's exclusive lock
independently prevents concurrent writers to the same snapshot repository.

Embedded owners may run one host preparation callback inside the same short
mutation freeze and declare immutable host files for Kit to capture as extras.
This lets one manifest bind an application's catalog snapshot to Docbank's
logical snapshot without extending the freeze across repository preparation or
content streaming.

Credential-bearing extras retain Kit's sensitivity marker;
the current plaintext repository refuses them unless the embedding application
explicitly permits plaintext secret capture for that backup.

## How do clients know capture or verification succeeded?

Kit's structured progress events remain structured across the daemon boundary.
The streaming create endpoint emits NDJSON stage updates followed by one
terminal result or error; the typed client validates that sequence before
reporting success. The human CLI renders the same events as terminal bars or
plain log lines. Machine-readable CLI output uses the non-streaming endpoint so
stdout remains one JSON document.

Standalone repository verification is daemon-mediated, while embedded owners
call `BackupRepository.Verify` directly. Quick mode proves structure and
references without reading document content. Full mode reads and hash-verifies
referenced content, deduplicating shared objects across selected snapshots, and
returns every finding rather than stopping at the first damaged object. Kit's
shared repository lock permits concurrent verifies and restores while excluding
repository writers. The daemon's authenticated JSON endpoint returns one
complete typed report; its NDJSON endpoint carries progress followed by exactly
one terminal report or error.

## How does restore protect other directories?

The daemon handles standalone restore. Embedded callers can restore through an
open vault without changing that running store.

Before cleanup or publication, Docbank and Kit perform these checks:

1. Docbank canonicalizes the target's existing path prefix. It rejects parents,
   descendants, and symlink aliases that overlap the live vault, repository, or
   additional roots protected by the embedding application. Filesystem identity
   checks also detect case- or normalization-equivalent aliases.
2. Kit opens the target without following a final symlink. It passes that held
   `os.Root` to Docbank's coordinator.
3. The coordinator repeats identity, overlap, and empty-target checks against
   the held directory. It locks the directory's entire ancestor hierarchy.
4. Kit performs writes relative to that held directory. Replacing the pathname
   cannot redirect those writes.

The locks exclude overlapping restores and daemon roots. The local lock file
remains after success or failure so contenders lock the same inode. It does
not count as payload when restore checks whether the target is empty.

Restore verifies and publishes compatible packs before one staged catalog
replacement. Incompatible selections fall back to verified loose content. The
database remains private until content verification, SQLite integrity, and
manifest-stat proofs all pass.

The streaming API reports these stages and a terminal typed proof. It reports
the SQLite scan separately from the manifest-stat comparison. The non-streaming
endpoint returns one JSON document for agents.

## Which retained records must round-trip?

Backup captures every blob still referenced by a retained content version,
rendition build or job, rendition artifact, visual preview, or stored embedding
input or vector set. Temporary holds used only to delay garbage collection do
not make staged bytes part of a backup.

The current JSONL authority round-trips the node allocator high-water mark,
blobs, tree and trash state, content versions and current pointers, ingests,
provenance, tags, saved queries and highlight sets, and extraction records. It
also retains auxiliary MD5 fingerprints, source-metadata generations and heads,
rendition evidence and artifacts, lexical generations, processing profiles and
consent, visual-preview generations and heads, and durable embedding results.
Restore needs no provider call to recover those stored results. Rebuildable
vector indexes, their build jobs, and reader leases are excluded.

Manifest statistics distinguish retained derivative artifacts from rebuildable
lexical projections. Capture and restore use the store's shared artifact-role
registry to order those statistics consistently.

Relational validation runs before a restored database is published. Every
disjoint permanent audit scope, its membership, and its independent chain are
preserved; the complete
audited-history backup and restore contract is described in
[Audited History](audited-history.md).

## Who can remove snapshots and reclaim repository space?

Embedded applications remove selected recovery points with
`BackupRepository.Forget` and reclaim unused repository space with
`BackupRepository.Prune`. Forget supplies exact snapshot IDs to Kit and preserves
its last-recovery-point and incremental-parent errors. `BackupRepository.Prune`
supplies Docbank's existing `backupapp` adapter so Kit traces the same metadata,
content, auxiliary artifacts, and host-file references used by verification
and restore. Docbank never enumerates or deletes Kit repository files itself.
Both methods work without a source vault and return partial reports with errors.

Kit serializes cleanup with its exclusive repository lock. Pruning publishes
replacement packs and a live index before retiring old indexes and then old
packs; retry after interruption recomputes live references and cleanup candidates.
Only wholly dead packs and packs below half-live encoded payload are reclaimed or rewritten.
This leaves mostly-live packs partly unused. Removing snapshot records alone
does not reclaim their stored bytes, and neither operation promises secure
erasure. Scheduling and recovery-point selection belong to the embedding host;
the daemon and CLI do not expose these cleanup operations.

Backup and live packed storage share Kit's physical formats and verification
primitives, but docbank remains responsible for which catalog rows belong in a
snapshot. Kit does not infer application liveness or reach into docbank SQL.
