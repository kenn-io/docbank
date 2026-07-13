---
title: Backup
description: Docbank's JSONL-native Kit snapshot and restore architecture.
---

# Backup

`docbank backup init`, `backup create`, and `backup list` are implemented over
the authenticated daemon API; see the [Backup user guide](../usage/backup.md).
A coherent manual filesystem snapshot remains available by stopping the daemon
before copying the vault; see
[Vault Lifecycle](../usage/lifecycle.md#take-a-coherent-manual-snapshot).

The essential archive is the database plus `blobs/`. Configuration is useful
to retain when customized; logs, locks, and runtime records are not archive
state. A restored copy is not trusted until `docbank verify` succeeds.

## Kit integration status

The internal `backupapp` adapter supplies Kit v0.9.0 with Docbank's frozen logical
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

The daemon's create handler uses the same operation gate as mutations and
maintenance, but it does not hold the gate throughout backup I/O. Kit calls the
gate-backed freeze coordinator, Docbank pins the deferred JSONL transaction,
and Kit releases the gate before streaming metadata and blobs. The repository's
exclusive lock independently prevents concurrent writers to the same snapshot
repository.

!!! info "Planned — remaining Phase 4 commands"
    `docbank backup verify` and `docbank backup restore` have not landed. The
    restore adapter described above is internal until the recovery CLI/API
    defines target confinement, overwrite behavior, and operator output.

Backup reachability is intentionally broader than GC reachability: every
`blobs` row is captured, including a row that has become a GC candidate but has
not yet been reclaimed. This preserves the deletion pipeline's regret window
inside the snapshot.

## Boundary with packed storage

Backup and live packed storage share Kit's physical formats and verification
primitives, but docbank remains responsible for which catalog rows belong in a
snapshot. Kit does not infer application liveness or reach into docbank SQL.
