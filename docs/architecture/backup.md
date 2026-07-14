---
title: Backup
description: Docbank's JSONL-native Kit snapshot and restore architecture.
---

# Backup

`docbank backup init`, `backup create`, `backup list`, `backup verify`, and
`backup restore` are implemented over the authenticated daemon API; see the
[Backup user guide](../usage/backup.md).
A coherent manual filesystem snapshot remains available by stopping the daemon
before copying the vault; see
[Vault Lifecycle](../usage/lifecycle.md#take-a-coherent-manual-snapshot).

The essential archive is the database plus `blobs/`. Configuration is useful
to retain when customized; logs, locks, and runtime records are not archive
state. A restored copy is not trusted until `docbank verify` succeeds.

## Kit integration status

The internal `backupapp` adapter supplies Kit v0.9.2 with Docbank's frozen logical
view: every authoritative `blobs` row, representation-neutral fidelity stats,
and mixed loose/packed content reads. A short daemon freeze opens and pins one
deferred SQLite read transaction; the freeze then ends, writers resume into the
WAL, and metadata, content membership, and fidelity statistics continue to see
the same point-in-time state.

Docbank also has a deterministic, versioned JSONL representation of its logical
metadata, identified in manifests as `docbank-metadata-jsonl-v1`. It contains
the complete virtual directory tree and file records,
including stable IDs, content hashes, timestamps, trash coordinates, prior
versions, ingest provenance, tags, and extracted text. It intentionally omits
FTS rows and physical pack mappings: search indexes are rebuilt by importing
nodes, while restore grants physical authority only after content has been
verified and published. Import targets must be fresh current-schema databases;
a malformed or referentially incomplete stream leaves the pristine target
unchanged. Capture makes two deterministic passes over the same pinned
transaction: the first establishes the exact artifact size and the second
streams the bytes into Kit without materializing a second database or a JSONL
temporary file. The header also preserves the node-ID allocation high-water
mark, including IDs whose rows were later deleted, so restore never reuses a
value that an external reference may remember.

Capture reads loose and packed blobs through Kit's bounded-memory stream. The
archive may grant authority to copied bytes only after terminal EOF verifies
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

Snapshots created before this cutover used Kit's SQLite page-map metadata.
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

Kit's structured progress events remain structured across the daemon boundary.
The streaming create endpoint emits NDJSON stage updates followed by one
terminal result or error; the typed client validates that sequence before
reporting success. The human CLI renders the same events as terminal bars or
plain log lines. Machine-readable CLI output uses the non-streaming endpoint so
stdout remains one JSON document.

Repository verification is daemon-mediated even though it reads the backup
repository rather than the live vault. The authenticated JSON endpoint returns
one complete typed report; the NDJSON endpoint carries Kit's verification
progress followed by exactly one terminal report or error. Quick mode proves
structure and references without reading document content. Full mode reads and
hash-verifies referenced content, deduplicating shared objects across selected
snapshots, and returns every finding rather than stopping at the first damaged
object. Kit's shared repository lock permits concurrent verifies and restores
while excluding repository writers.

Restore is likewise daemon-mediated but never mutates the running store. Before
Kit receives a target, Docbank canonicalizes its existing path prefix and
rejects any parent, descendant, or symlink alias overlapping the live vault or
repository. Filesystem identity supplements those lexical checks for case- or
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

!!! info "Planned — audited-history authority"
    Full audit will extend the deterministic JSONL with scopes, sticky
    memberships, canonical enrollment baselines and digests, mutation records,
    per-scope chain entries/heads, a stable vault ID, both allocator high-water
    marks, a vault-wide allocation lineage, and stable content versions. Every
    protected historical blob becomes snapshot content. Import, verification,
    and restore must recompute baseline digests from immutable enrollment
    snapshots, verify later mutations and allocation lineage separately,
    restore the allocators at the verified lineage tail, and reject an
    internally missing, malformed, reordered, truncated, or hash-inconsistent
    stream before publishing the database. Each canonical mutation is bound to
    exactly one allocation-lineage entry by operation ID and mutation hash.
    Snapshot manifests carry an evidence bundle containing the stable vault ID,
    every scope count/head, and allocation-lineage count/head. When independently
    trusted, that manifest or bundle is rollback evidence; a fresh import without
    such an external reference cannot detect a coherently rewritten set of
    chains.

    Before overwriting an existing audited target, restore inspects it under
    the target lock. The snapshot must have the same vault ID and prove every
    existing scope head and the vault-wide allocation-lineage head as prefixes
    at their existing counts; missing, shorter, divergent, pre-audit, or
    unrelated history is refused before cleanup. Because every authoritative
    operation carries a random operation ID in that lineage, independently
    mutated copies cannot pass merely by consuming equal numeric allocator
    values. The complete contract is in
    [Audited History](audited-history.md).

## Boundary with packed storage

Backup and live packed storage share Kit's physical formats and verification
primitives, but docbank remains responsible for which catalog rows belong in a
snapshot. Kit does not infer application liveness or reach into docbank SQL.
