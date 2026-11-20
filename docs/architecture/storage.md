---
title: Storage
description: The SQLite schema, blob store layout, durability discipline, and enforced invariants.
---

# Storage

Docbank stores document metadata in SQLite and document bytes in immutable
content storage. A standalone vault normally keeps its database and built-in
primary blob directory under `~/.docbank/`, and the catalog can also authorize
copies in secondary stores.

To capture content across all stores, run `docbank backup create`. Manually
copying the database and primary directory produces a complete archive only
when the primary holds every retained blob. If you make a manual copy, stop the
daemon first, and run `docbank vault verify` before relying on the result. See
[Backup and recovery](backup.md) for the full capture and restore contract.

Hidden-photo backups preserve credentials, failed attempts, and lockout expiry.
Unlock sessions stay in memory and expire when you restart or restore.

This page owns the on-disk layout, schema relationships, and upgrade rules. For
the high-level document model, start with [How Docbank works](overview.md).

## Blob store

```
blobs/
├── tmp/                      # in-flight writes
├── <aa>/<sha256>[.zst]       # raw or zstd loose content; aa = first two hash chars
└── packs/<aa>/<pack>.mvpack  # sealed immutable packs
```

Blobs are immutable and deduplicated by the SHA-256 digest of their decoded
bytes. Docbank first publishes new content as loose files in the fixed local
primary. Objects of at least 4 KiB use zstd compression only when it saves at
least 10%, while smaller or incompressible objects stay raw. The shared Kit
engine can move either loose encoding into sealed packs without changing
content identity. When you read a document, Docbank checks the SQLite catalog
and reads raw loose, compressed loose, or packed content from an authorized
filesystem or S3-compatible location. If a released database schema requires an
incompatible upgrade, Docbank rebuilds the SQLite catalog through deterministic
JSONL and translates the existing physical authority without rewriting any
content bytes.

`go.kenn.io/kit/packstore` publishes each new write in four steps:

1. Stream the bytes into `blobs/tmp/`.
2. Sync the completed file to durable storage with `fsync`.
3. Rename the file into its canonical location.
4. Sync the containing shard directory.

Kit also syncs the shard directory on the deduplication fast path. Docbank
commits a database reference only after the blob is durable on disk. If a crash
happens before that commit, it can leave an **orphan blob** on disk with no
catalog authority. Normal reads cannot see orphan blobs, and `gc` can reclaim
them later.

At startup, Docbank removes stale `tmp/` files left behind by interrupted
writes, as long as no other Docbank process holds the vault (see
[Ownership and concurrency](locking.md)).

## Database schema

Core tables in `internal/store/schema.sql`:

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
blob_stores    (store_id UUID PRIMARY KEY, name UNIQUE, kind, role, lifecycle,
                binding profile, ownership epoch, created_at)
blob_locations(blob_hash, store_id, generation, loose/pack kind, encoding,
                stored_size, pack eligibility,
                PRIMARY KEY (blob_hash, store_id))
blob_packs     (store_id, pack_id, entry_count, stored_bytes, created_at,
                bounded-maintenance summary fields,
                PRIMARY KEY (store_id, pack_id))
blob_pack_entries(blob_hash, store_id, pack_id, pack_offset,
                  stored_len, raw_len, flags, crc32c,
                  PRIMARY KEY (blob_hash, store_id))
storage_operations(operation_id UUID PRIMARY KEY, kind, source store,
                   versioned request/plan, progress, state, error, receipt,
                   retention)
content_versions(version_id UUID PRIMARY KEY, node_id, blob_hash, size,
                 mime_type, recorded_at, node_revision,
                 introduced_operation_id, transition_kind, source_version_id)
ingests        (id, started_at, source_kind, source_desc)
provenance     (identity SHA-256 PRIMARY KEY, node_id, ingest_id,
                original_path, original_mtime, supersedes)
watch_sources  (watch_name, source_ref, node_id, last blob_hash and size)
push_sources   (push_name, source_ref, node_id, provenance identity,
                last blob_hash and size, accepted_at)
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

The `blobs` table tracks logical membership, meaning each row states that
Docbank retains that content identity. The `blob_locations` table tracks
physical authority, where each row records that a specific store holds a
verified copy that can satisfy reads. Docbank observes runtime health
separately and never rewrites those durable rows. Pack identity is scoped to a
store, so the same immutable pack can exist in more than one store.

Store bindings live in machine-local `config.toml` profiles rather than
portable authority. The catalog stores only the profile name and a fenced
ownership epoch. See [Multi-store storage](../usage/storage.md) for how to
configure and operate stores, and the sections below for the complete authority
boundary.

The API key protects access to the daemon, not direct access to physical
storage. Docbank encodes and content-verifies loose objects and packs in
secondary filesystem and S3 namespaces, but it does not encrypt them. Anyone
who can read the raw store sits inside your deployment trust boundary.
Filesystem or provider encryption and access control remain your
responsibility, and native live-store encryption is deferred product scope.

## Released upgrades

v0.9.0 is the first storage compatibility boundary. Every newer database stores
an explicit, monotonically increasing storage-schema version. When you open a
supported older vault with a newer incompatible schema, Docbank runs a logical
cutover instead of applying in-place SQL migrations:

1. Checkpoint and read the released database without modifying its schema.
2. Export and validate deterministic metadata-v1 JSONL.
3. Import that stream into a fresh current-schema database.
4. Restore loose and packed physical authority, then validate and checkpoint.
5. Keep a version-identified source recovery copy and atomically publish the
   new database.

Upgrades from schema v3 and later copy storage operations along with their
store and cleanup records unchanged, so queued or interrupted placements,
evacuations, and photo imports resume after the upgrade. Upgrades from v0.15.0
and later also copy pending loose-blob retirements and pending derivative
purges. They preserve the source processing incarnation, so existing processing
consent and queued rendition jobs stay authorized. Because an upgrade stays
within the same vault, it also keeps state that a restore deliberately resets:

- Export sources, plans, and jobs with their owners, so export handles still
  resolve, interrupted exports resume, and completed archives remain intact.
- Embedding jobs, so failed or retry-exhausted jobs do not re-queue with a
  fresh retry budget.
- Media receipts, so an admitted request without a rendition job yet still
  resumes.
- Document event and people rebuild receipts with the epochs they count
  against, so you can look up or replay a rebuild by its operation ID and
  unfinished rebuilds complete.
- Unexpired package preflights and in-progress mailbox uploads with their
  accepted chunks, so you can continue within the 24-hour session instead of
  starting over.

If a copied source table has columns that differ from the current schema,
Docbank stops the upgrade before changing the vault. Restoring from a backup
still requires fresh consent before provider work resumes.

Every released generation shares the same cutover driver, paired with a small
source adapter that knows how to export that generation's logical authority and
restore its physical blob catalog. The v0.9.0 adapter recognizes the only
released database that predates the explicit version marker, while later
generations are matched only by their stored version. An older binary
refuses a database from a newer generation instead of trying to interpret it.
The current physical identity column also deliberately differs from the mandatory v0.9
startup query, so an unversioned v0.9 binary fails closed rather than silently
writing with obsolete storage rules.

The recovery copy is named `<database>.v0.9.0.bak` for a v0.9.0 source and
`<database>.schema-v28.bak` for a v0.15.0 source. If v0.15.1 refused your vault
with `schema version 28 has no supported JSONL cutover`, open it with v0.15.2
or later to complete the upgrade.

The recovery copy holds private vault metadata and inherits the vault's
owner-private boundary. Keep it until you verify both the upgraded vault and a
fresh backup. After that, you can remove it while the daemon is stopped. The
cutover neither duplicates nor recompresses blob files.

File nodes and content versions cross-reference each other. Every file must
have a current version belonging to that node, and directories cannot carry
one. Version UUIDs and their introducing operation UUIDs are random, canonical
UUIDv4 values, and both `(node_id, node_revision)` and
`(node_id, introduced_operation_id)` are unique. See
[Editing and versions](editing-and-versions.md) for the read and retention
contract.

Each provenance fact has a SHA-256 identity derived from its immutable node,
ingest, original-path, mtime, and optional predecessor fields. Current ingest
creates an unsuperseded fact, and SQL prevents rewriting ingest or provenance
rows. JSONL preserves both the identity and the optional `supersedes` edge,
rejecting any dangling, cross-node, branching, or cyclic graph during import.

`watch_sources` is a small operational cursor rather than a second provenance
graph. Its primary key is `(watch_name, source_ref)`, where `source_ref` is the
current source location, and it tracks the stable node along with the last
accepted source bytes. When a live daemon watch confirms a rename, it rebinds
only that current location inside a storage transaction. The original source
path remains in immutable provenance, and the archived node keeps its own
virtual placement and selected version. Go code owns watch decisions. Unchanged
source bytes never overwrite an independently edited or reverted node head, and
a node or destination key already owned by push cannot be rebound.
Native identity and birth evidence stay process-local and never enter the
schema or metadata export. The in-memory tuple binds the node, native object
identity, birth evidence, and handle-derived file information only after a
watched ingest succeeds; unaccepted observations cannot transfer that
ownership.

`push_sources` tracks the same source digest authority for client-owned
folders. Its `(push_name, source_ref)` key survives even if you prune the
content version that was current when the source was accepted. Because the
cursor stores a digest and size rather than physical-byte authority, it does
not block pruning or add old bytes to a backup. Like a watch cursor, each push
cursor owns its node.

The schema and metadata-v1 codec can persist a complete first audit enrollment,
covering topology and attached-metadata genesis, a shared baseline, sticky
membership, an enrollment event, scope chain, and allocation lineage. During
import, Docbank recomputes every canonical digest and reconciles the protected
closure against the restored current state before accepting it. You create a
vault's first audit scope with `docbank audit enable`. Until you enroll a
scope, this authority stays dormant. Once audit authority exists, the Go store
rejects logical mutation classes that do not yet record an audit transition.

Supported audit transitions cover filesystem ingest, content replacement and
reversion, in-scope moves and renames, reversible trash and restore, and tag
creation, assignment, and rename. Each commits in the same metadata transaction
as the change it records. Every authority change advances the allocation
lineage, content operations add immutable versions, and changes with scoped
effects also record events and scope-chain entries. Pack layout and backup
reads stay maintainable throughout. See [Audited history](audited-history.md)
for the full mutation and maintenance contract.

## Structural invariants enforced in the schema

SQL enforces relational shape, uniqueness, foreign keys, and basic scalar
checks. Go business logic owns append-only audit semantics, mutation
sequencing, canonical construction, and replay, so the same rules can work with
another metadata backend:

- **Exactly one root.** Because SQLite treats NULLs as distinct in unique
  indexes, Docbank uses a partial unique index on a constant expression:
  `CREATE UNIQUE INDEX one_root ON nodes((1)) WHERE parent_id IS NULL`.
- **Live-sibling name uniqueness.**
  `UNIQUE(parent_id, name) WHERE trashed_at IS NULL` keeps names unique among
  live siblings. Trashed nodes never block a name.
- **Kind/content consistency.** A CHECK constraint requires `kind = 'file'`
  rows to have `current_version_id IS NOT NULL` and directories to have NULL.
- **Referential integrity.** Content-version `blob_hash` values reference
  `blobs`, so a blob row cannot be deleted while any retained version still
  points to it. That constraint keeps GC's reachability query trustworthy.

The Go store layer enforces the remaining rules:

- Normalize names to Unicode NFC and reject empty names, `.`, `..`, `/`, and NUL.
- Walk ancestry inside a move transaction to reject cycles.
- Advance revisions on every mutation.
- Require a node's size to match its blob row.

Tag IDs are random UUIDv4 values, and tag names are mutable, NFC-normalized
text. Assignments point to the stable tag ID, and each tag revision covers both
the tag's name and its full set of assignments. Real assignment changes bump
both the tag and the directly affected node. Renaming a tag bumps the tag and
every assigned node once in the same transaction. Deleting a tag checks its
revision before cascading through assignments without deleting nodes, and
emptying tagged trash advances each affected tag once before its assignments
cascade away.

## Timestamps and identity

- All timestamps use UTC RFC 3339 text.
- **Node IDs are canonical.** Docbank derives paths for display. Every CLI
  listing includes node IDs, and ID-based operations such as `restore` survive
  any amount of renaming.

## Trash representation

When you trash a node, Docbank stamps `trashed_at` across the whole subtree in
one transaction and records `trash_parent` and `trash_name` on the trash root
so restore can put it back. Docbank also reparents the trash root under `/` at
trash time. Because `parent_id` cascades on delete, reparenting keeps an
independently trashed subtree alive even if its original parent is later
permanently deleted. Every node trashed in the same operation shares the same
`trashed_at` timestamp. Only trash roots carry `trash_name`, while descendants
stay part of their root's subtree.

Paginated trash reads group photo roots by asset and keep each ordinary root
separate. Docbank counts those grouping keys directly for the total, and
selects a display representative and latest member trash time only for the
requested page. Both reads run in one transaction so the total and page see the
same snapshot.

## Concurrent first-open

Docbank applies the schema and creates the root node inside one
`BEGIN IMMEDIATE` transaction with a bounded busy-retry. If two processes race
to create the same fresh vault, they serialize instead of failing on SQLite's
WAL-conversion and DDL lock upgrades, and both end up with the same single root
through an atomic `INSERT ... SELECT ... WHERE NOT EXISTS`.
