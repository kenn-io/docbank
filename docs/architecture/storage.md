---
title: Storage
description: The SQLite schema, blob store layout, durability discipline, and enforced invariants.
---

# Storage

Everything lives under `~/.docbank/`: one SQLite database and one blob
directory. The pair is the archive — copy both and you've moved the
vault; `docbank verify` proves the copy is intact.

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
[Concurrency & Locking](locking.md)).

## Database schema

Core tables (`internal/store/schema.sql`):

```sql
nodes (
    id            INTEGER PRIMARY KEY,
    parent_id     INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL,          -- 'dir' | 'file'
    blob_hash     TEXT REFERENCES blobs(hash),   -- files only
    size          INTEGER,
    mime_type     TEXT,
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
node_versions  (node_id, blob_hash, size, replaced_at)   -- reserved; no writer yet
ingests        (id, started_at, source_kind, source_desc)
provenance     (node_id, ingest_id, original_path, original_mtime)
tags           (id, name UNIQUE)          -- reserved; no API/CLI surface yet
node_tags      (node_id, tag_id)
extracted_text (blob_hash, extractor, extractor_version, status,
                error, attempts, text, extracted_at)      -- reserved; no workers yet
nodes_fts      -- FTS5 external-content index over live node names
```

!!! info "Planned — version and audit schema"
    The reserved `node_versions` shape is not yet a compatibility promise.
    Before the first editing writer lands, it will gain stable version identity
    as a canonical UUIDv4 with a uniqueness constraint and the fields needed by
    [Audited History](audited-history.md). UUID identity avoids a portable
    allocator whose rollback or reuse could retarget stale version references.
    Audit scopes, sticky memberships, mutation records, and chain state will be
    current-schema metadata included in deterministic JSONL rather than
    inferred from paths or pack layout. A daemon-exclusive bootstrap will give
    every existing file a stable initial version and `current_version_id`, and
    create the stable vault ID, before editing or audit capabilities become
    available. Audited trash origins will retain immutable parent IDs and names
    outside nullable live foreign-key semantics, so deleting an unaudited origin
    cannot rewrite protected history. The existing `trash_parent` becomes a
    non-authoritative locator excluded from audit hashing and reconciliation.
    Audit baselines and replay also cover `node_tags` with their `tags` records
    and `provenance` with referenced `ingests`; FTS and `extracted_text` remain
    rebuildable derived data outside audit authority. Ingest and provenance
    records become immutable facts, with database guards preventing ordinary
    update or deletion when an audited member roots them.
    Tag IDs likewise become canonical non-reusable UUIDv4 values; tag names may
    change or be recreated without retargeting historical assignments. Once any
    scope exists, node insertion and parent/name/trash changes require a guarded
    audit transaction whose precomputed membership, baseline, descendant-event,
    lineage, and scope-head effects must match exactly before commit.

## Invariants enforced in the schema

The important rules hold at the SQL layer, so they bind every writer:

- **Exactly one root.** SQLite treats NULLs as distinct in unique
  indexes, so a partial unique index on a constant expression does it:
  `CREATE UNIQUE INDEX one_root ON nodes((1)) WHERE parent_id IS NULL`.
- **Live-sibling name uniqueness.**
  `UNIQUE(parent_id, name) WHERE trashed_at IS NULL` — trashed nodes
  never block a name.
- **Kind/content consistency.** A CHECK constraint ties
  `kind = 'file'` to `blob_hash IS NOT NULL` and directories to NULL.
- **Referential integrity.** `blob_hash` references `blobs`; a blob row
  can't be deleted while anything points at it, which is what makes GC's
  reachability query trustworthy.

The store layer adds the rules SQL can't express: name validation
(reject empty, `.`, `..`, `/`, NUL) with Unicode NFC normalization,
cycle prevention on moves (ancestry walk inside the move transaction),
revision bumps on every mutation, and size consistency between a node
and its blob row.

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
