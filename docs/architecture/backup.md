---
title: Backup & Recovery
description: Docbank's JSONL-native Kit snapshot and restore architecture.
---

# Backup and recovery

Standalone `docbank backup init`, `backup create`, `backup list`, `backup
verify`, and `backup restore` use the authenticated daemon API; see the
[Backup user guide](../usage/backup.md). Applications that own an embedded
vault use `BackupRepository`, `Vault.CreateBackup`, and `Vault.RestoreBackup`
directly; see [Embedding Docbank](../embedding.md#back-up-and-restore-an-embedded-vault).
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

## Kit integration status

The internal `backupapp` adapter supplies Kit with Docbank's frozen logical
view: every authoritative `blobs` row, representation-neutral fidelity stats,
and mixed loose/packed content reads. A short operation-owner freeze opens and
pins one deferred SQLite read transaction; the freeze then ends, writers resume
into the WAL, and metadata, content membership, and fidelity statistics
continue to see the same point-in-time state.

The same pinned transaction emits a separate deterministic
`docbank-placement-v1` artifact. It names source store UUIDs, display names,
backend kinds, roles, per-hash store UUIDs, and aggregate counts and bytes.
Deployment bindings, paths, endpoints, bucket coordinates, credentials,
ownership epochs, encodings, and pack coordinates are excluded. Placement
authority changes take the preservation side exclusively for their short
catalog commit, so the logical membership and placement artifact cannot
describe different authority handoffs.

Docbank also has a deterministic, versioned JSONL representation of its logical
metadata, identified in manifests as `docbank-metadata-jsonl-v1`. It contains
the complete virtual directory tree and file records,
including stable IDs, content hashes, timestamps, trash coordinates, prior
versions, ingest provenance, watched-source cursors, tags, and extracted text.
It intentionally omits FTS rows and physical pack mappings: search indexes are
rebuilt by importing nodes, while restore grants physical authority only after
content has been verified and published. Import targets must be fresh
current-schema databases;
a malformed or referentially incomplete stream leaves the pristine target
unchanged. Capture makes two deterministic passes over the same pinned
transaction: the first establishes the exact artifact size and the second
streams the bytes into Kit without materializing a second database or a JSONL
temporary file. The header also preserves the node-ID allocation high-water
mark, including IDs whose rows were later deleted, so restore never reuses a
value that an external reference may remember.

Capture reads raw loose, zstd loose, and packed blobs through Kit's
bounded-memory stream. The physical source encoding is not copied into backup
metadata: Kit decodes and verifies the logical bytes before repository
publication. The archive may grant authority to copied bytes only after
terminal EOF verifies
their stored framing, decoded length, and SHA-256 identity; opening a stream or
closing it early is not a successful copy.

Restore constructs a fresh current-schema database from the verified JSONL
artifact inside Kit's private staging area. It then checkpoints the database,
publishes verified content, grants fresh catalog authority to the chosen loose
or packed representation, reproduces the recorded fidelity statistics, and
only then publishes the staged vault. Source pack rows never enter the JSONL
artifact. Docbank's restore wrapper owns both the metadata restorer and packed
target so callers cannot accidentally separate those policies. Integration
coverage proves logical JSONL equality, loose and packed source capture,
packed publication, large loose-object fallback, and reads every restored blob
through the same mixed store used by a live vault.

Default restore treats the placement artifact as informational and collapses
all content into a fresh fixed primary. Explicit store mapping first restores
and proves a complete local copy, then claims fresh target store identities,
publishes or adopts immutable destination objects, reads every object back,
and records mapped authority while target-tree coordination remains held.
A remote-only database is published without primary catalog authority, but
restore leaves the now-untracked primary files intact until publication
succeeds; ordinary garbage collection may reclaim them afterward. A failed
restore therefore cannot remove bytes still owned by the database it was meant
to replace. Audit-protected bytes retain primary authority unless the mapping
includes the explicit remote-only acknowledgement.

The restored database and its built-in primary marker change ownership as one
recoverable publication. Before replacing the database, Docbank durably records
the prior database fingerprint and both marker identities, installs and reads
back the restored marker, and rolls it back if publication fails. If the process
stops between those steps, the next restore compares the visible database with
that fingerprint without opening unknown files for mutation; a normal vault
open reconciles its validated catalog identity. Storage access begins only
after the marker agrees with the database that actually became visible.

Earlier development snapshots used Kit's SQLite page-map metadata.
They remain restorable through the same wrapper. New captures always use JSONL;
the legacy path is a reader compatibility boundary, not an alternative format
for new snapshots.

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
content streaming. Credential-bearing extras retain Kit's sensitivity marker;
the current plaintext repository refuses them unless the embedding application
explicitly permits plaintext secret capture for that backup.

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

Standalone restore is daemon-mediated; embedded restore is invoked through the
open vault but never mutates that running store. Before Kit receives a target,
Docbank canonicalizes its existing path prefix and
rejects any parent, descendant, or symlink alias overlapping the live vault or
repository, plus any additional roots protected by an embedding application.
Filesystem identity supplements those lexical checks for case- or
normalization-equivalent aliases. Kit then opens the target without following a
final symlink and passes that same held `os.Root` to Docbank's coordinator
before cleanup or publication. The coordinator repeats the identity, overlap,
and empty-target checks against the held directory and locks its entire ancestor
hierarchy. This excludes overlapping restores and daemon roots; replacing the
pathname afterward cannot redirect Kit's descriptor-relative writes. The local
lock file is retained after success or failure so every contender locks the
same inode; it does not count as payload for empty-target policy.
Compatible
packs are verified and published before one staged catalog replacement;
incompatible selections fall back to verified loose content. The restored
database remains private until content verification, SQLite integrity, and
manifest-stat proofs all pass. The streaming API exposes those stages and a
terminal typed proof, with the SQLite scan and manifest-stat comparison
reported separately; the non-streaming endpoint keeps agent output to one JSON
document.

Backup reachability is intentionally broader than GC reachability: every
`blobs` row is captured, including a row that has become a GC candidate but has
not yet been reclaimed. This preserves the deletion pipeline's regret window
inside the snapshot.

The current JSONL authority round-trips the node allocator high-water mark,
blobs, tree and trash state, content versions and current pointers, ingests,
provenance, tags, and extraction records. Its relational validation runs before
a restored database is published. Every disjoint permanent audit scope, its
membership, and its independent chain are preserved; the complete
audited-history backup and restore contract is described in
[Audited History](audited-history.md).

## Boundary with packed storage

Backup and live packed storage share Kit's physical formats and verification
primitives, but docbank remains responsible for which catalog rows belong in a
snapshot. Kit does not infer application liveness or reach into docbank SQL.
