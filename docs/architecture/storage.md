---
title: Storage
description: The SQLite schema, blob store layout, durability discipline, and enforced invariants.
---

# Storage

A standalone vault normally lives under `~/.docbank/`: one SQLite database and
one blob directory. The pair is the archive, but they must come from one
coherent point in time. Do not copy a running vault as a backup; use
`docbank backup create`, or stop the daemon before taking a manual filesystem
snapshot. `docbank verify` proves a completed copy is internally consistent.

## Blob store

```
blobs/
├── tmp/                      # in-flight writes
├── <aa>/<sha256>             # loose content; aa = first two hash chars
└── packs/<aa>/<pack>.mvpack  # sealed immutable packs
```

Blobs are immutable and deduplicated by SHA-256. New content is first
published loose; the shared Kit engine supports moving eligible content into
sealed packs without changing its identity. Reads consult the SQLite catalog
and transparently use the current loose or packed representation. Existing
loose-only vaults open without conversion.

**Durability discipline.** `go.kenn.io/kit/packstore` streams every write to
`blobs/tmp/`, fsyncs the file, renames it into place, then fsyncs the shard
directory — including on the
deduplication fast path, so a reference is never handed out for a
directory entry that could vanish on power loss. The database
transaction that references a blob commits only after the blob is
durable. A crash between the two leaves an *orphan blob*: harmless,
invisible, reclaimed by `gc`.

Stale `tmp/` files from interrupted writes are cleaned at startup — but
only when no other docbank process holds the vault (see
[Ownership & Concurrency](locking.md)).

## Database schema

Core tables (`internal/store/schema.sql`):

```sql
nodes (
    id            INTEGER PRIMARY KEY,
    parent_id     INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL,          -- 'dir' | 'file'
    current_version_id TEXT,                -- files only
    revision      INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    modified_at   TEXT NOT NULL,
    trashed_at    TEXT,                   -- NULL = live
    trash_parent  INTEGER,                -- original location, for restore
    trash_name    TEXT
)
blobs          (hash PRIMARY KEY, size, created_at)
blob_packs     (pack_id PRIMARY KEY, entry_count, stored_bytes, created_at)
blob_pack_index(blob_hash PRIMARY KEY, pack_id, pack_offset,
                stored_len, raw_len, flags, crc32c)
content_versions(version_id UUID PRIMARY KEY, node_id, blob_hash, size,
                 mime_type, recorded_at, node_revision,
                 introduced_operation_id, transition_kind, source_version_id)
ingests        (id, started_at, source_kind, source_desc)
provenance     (identity SHA-256 PRIMARY KEY, node_id, ingest_id,
                original_path, original_mtime, supersedes)
watch_sources  (watch_name, source_ref, node_id, last blob_hash and size)
tags           (id UUID PRIMARY KEY, name UNIQUE, revision)
node_tags      (node_id, tag_id)
audit_records  (digest PRIMARY KEY, kind, operation/event/node indexes, record_json)
audit_authority(lineage_id, operation high-water, allocation count/head)
audit_scopes   (scope_id, target_node_id, enable operation, count/head)
audit_baselines(digest, scope_id, target_node_id, operation_id)
audit_memberships(scope_id, node_id, baseline_digest)
extracted_text (blob_hash, extractor, extractor_version, status,
                error, attempts, text, extracted_at)      -- versioned derived cache
text_extraction_queue (blob_hash, next_attempt_at)         -- derived daemon work
text_searchable_versions (version_id)                      -- Go-derived MIME eligibility
content_fts    -- derived FTS5 index over successful extraction rows
nodes_fts      -- FTS5 external-content index over live node names
```

File nodes and content versions cross-reference one another: a file must have a
current version belonging to that node, while directories cannot carry one.
Version UUIDs and their introducing operation UUIDs are random, canonical
UUIDv4 values. `(node_id, node_revision)` and
`(node_id, introduced_operation_id)` are unique. See
[Editing & Versions](editing-and-versions.md) for the read and retention
contract.

Each provenance fact has a SHA-256 identity derived from its immutable node,
ingest, original-path, mtime, and optional predecessor fields. Current ingest
creates an unsuperseded fact; SQL prevents rewriting ingest or provenance rows.
JSONL preserves the identity and optional `supersedes` edge and rejects a
dangling, cross-node, branching, or cyclic graph during import.

`watch_sources` is a small operational cursor, not a second provenance graph.
Its primary key is `(watch_name, source_ref)`, and it records both the stable
node and the last source bytes accepted. Watch decisions live in Go: unchanged
source bytes never replace an independently edited or reverted node head.

The schema and metadata-v1 codec can persist one complete first audit
enrollment: topology and attached-metadata genesis, a shared baseline, sticky
membership, an enrollment event, scope chain, and allocation lineage. Import
recomputes every canonical digest and reconciles the protected closure with the
restored current state before accepting it. A vault's first audit scope is
created through the public enrollment workflow (`docbank audit enable`);
until a scope is enrolled, this authority remains dormant. Once audit
authority exists, the Go store rejects logical mutation classes that do not yet
record an audit transition. Supported transitions — filesystem ingest, content
replacement and reversion, in-scope moves and renames, reversible trash and
restore, and tag creation, assignment, and rename — commit in the same
metadata transaction as the change they record: every authority change
advances the allocation lineage, content operations add immutable versions,
and changes with scoped effects additionally record events and scope-chain
entries. Pack layout and backup reads
remain maintainable. The mutation and maintenance contract is maintained
in [Audited History](audited-history.md).

## Structural invariants enforced in the schema

SQL owns relational shape, uniqueness, foreign keys, and basic scalar checks.
Append-only audit semantics, mutation sequencing, canonical construction, and
replay are Go business logic so the same rules can serve another metadata
backend:

- **Exactly one root.** SQLite treats NULLs as distinct in unique
  indexes, so a partial unique index on a constant expression does it:
  `CREATE UNIQUE INDEX one_root ON nodes((1)) WHERE parent_id IS NULL`.
- **Live-sibling name uniqueness.**
  `UNIQUE(parent_id, name) WHERE trashed_at IS NULL` — trashed nodes
  never block a name.
- **Kind/content consistency.** A CHECK constraint ties
  `kind = 'file'` to `current_version_id IS NOT NULL` and directories to NULL.
- **Referential integrity.** Content-version `blob_hash` values reference
  `blobs`; a blob row can't be deleted while any retained version points at it,
  which is what makes GC's reachability query trustworthy.

The store layer adds the rules SQL can't express: name validation
(reject empty, `.`, `..`, `/`, NUL) with Unicode NFC normalization,
cycle prevention on moves (ancestry walk inside the move transaction),
revision bumps on every mutation, and size consistency between a node
and its blob row.

Tag IDs are random UUIDv4 values and names are NFC-normalized, mutable text.
Assignments refer to the stable ID. Each tag revision covers its name and
complete assignment set. Real assignment changes bump both the tag and directly
affected node; renaming bumps the tag and every assigned node once in the same
transaction. Delete checks the tag revision before cascading through
assignments, not nodes. Emptying tagged trash advances each affected tag once
before its assignments cascade away.

## Timestamps and identity

- All timestamps are UTC RFC 3339 text.
- **Node IDs are canonical**; paths are derived for display. Every CLI
  listing includes IDs, and ID-based operations (`restore`) survive any
  amount of renaming.

## Trash representation

Trashing stamps `trashed_at` on the whole subtree in one transaction and
records `trash_parent`/`trash_name` on the trash root so restore can put
it back. The trash root is reparented under `/` at trash time: since
`parent_id` cascades on delete, this keeps an independently-trashed
subtree alive even if its original parent is later permanently deleted.
All nodes trashed in one operation share the same `trashed_at` stamp,
which is how `trash list` distinguishes trash roots from members of a
trashed subtree.

## Concurrent first-open

The schema is applied and the root node created inside a single
`BEGIN IMMEDIATE` transaction with a bounded busy-retry. Two processes
racing to create the same fresh vault serialize instead of tripping over
SQLite's WAL-conversion and DDL lock upgrades, and both arrive at the
same single root (the root insert is an atomic
`INSERT ... SELECT ... WHERE NOT EXISTS`).
