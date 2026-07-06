# docbank — Personal Document Archive: Design

Date: 2026-07-06
Status: Draft for review

## Purpose

docbank is an active document manager for the personal documents that
accumulate over a lifetime: PDFs, text files, spreadsheets, scans,
miscellaneous images, and other files. It is the third member of the
family alongside msgvault (communications archive) and fotobank
(photo/video archive).

The vault owns the bytes. Documents are ingested into content-addressed
storage; the organizing structure is a **virtual tree stored in SQLite**,
browsed by humans through a TUI and by agents through an HTTP API. Agents
are first-class: they can browse, retrieve, and reorganize the entire tree
through the API alone. Moves, renames, and reorganization are metadata
transactions that never touch bytes.

## Non-goals (v1)

- OCR of scanned documents (schema leaves room; v2)
- Embeddings, AI tagging, hybrid search (v2)
- Web UI
- At-rest encryption of the live store (the pack layer already supports
  encrypted backups)
- Importing attachments out of msgvault (natural future ingest source)
- Multi-user / sharing

## Relationship to msgvault (Phase 0: extraction)

docbank imports the msgvault Go module for its storage and backup
machinery. Before docbank work starts, one msgvault PR generalizes the
backup engine around application-provided hooks. This is a
**generalization, not a code move**: today `backup.CreateOptions` carries
msgvault-specific fields (`AttachmentsDir`, `MsgvaultVersion`), `Create`
calls schema-specific methods (`Stats`, `AttachmentRefs`,
`HasNonCanonicalAttachmentPaths`) on the frozen session, the manifest
embeds msgvault-typed `ManifestStats`/`ManifestAttachments`, and
`Restore`/`Verify` recompute those same stats as the fidelity proof.

### Extraction plan

- `internal/pack` → `pkg/pack`, unchanged. It already has zero msgvault
  imports.
- `internal/backup` → `pkg/backup`, generalized around small composable
  interfaces that the engine consumes at create, restore, AND verify time:

The hooks must serve all three engine operations — create, restore, and
verify. Today `AttachmentRefs` and `Stats` are methods on the frozen
session (a consistent read view), and restore hardwires the `msgvault.db`
filename, the `attachments/` directory, and msgvault-specific SQL that
re-derives content refs from the restored DB. The seam follows that
structure: create-side hooks operate against the frozen view; restore-side
hooks operate against the restored DB.

```go
// Freezer produces a consistent read view of the application database.
type Freezer interface {
    Freeze(ctx context.Context, dbPath string) (FrozenView, error)
}

// FrozenView is the create-side hook surface: content enumeration and
// stats are computed against the frozen snapshot, never the live DB.
type FrozenView interface {
    ContentRefs(ctx context.Context) ([]ContentRef, error)
    HasNonCanonicalPaths(ctx context.Context) (bool, error)
    Stats(ctx context.Context) (json.RawMessage, error) // opaque to engine
    Close() error
}

// RestoreLayout tells the engine how to materialize and prove a restore.
type RestoreLayout interface {
    DBFileName() string  // "msgvault.db" / "docbank.db"
    ContentDirName() string // "attachments" / "blobs"
    // ContentRefs re-derives content refs from the restored DB so the
    // engine can verify every referenced file was materialized.
    ContentRefs(ctx context.Context, db *sql.DB) ([]ContentRef, error)
    // Stats recomputes the app stats from the restored DB; the engine
    // byte-compares against the manifest's stats as the fidelity proof.
    Stats(ctx context.Context, db *sql.DB) (json.RawMessage, error)
}

// ExclusionProvider names live-archive paths a snapshot never captures.
type ExclusionProvider interface {
    ExcludedPaths() []string
}

// App composes the hooks. msgvault and docbank each implement it.
type App interface {
    Freezer
    RestoreLayout
    ExclusionProvider
    Version() string // recorded in the manifest (app name + version)
}
```

- **Format compatibility constraint:** existing msgvault backup repos
  (format v1) must remain readable and msgvault must keep writing
  manifests that today's readers accept. The manifest's `stats` block
  becomes app-defined JSON from the engine's perspective while msgvault's
  implementation emits the same keys it does today. The `msgvault_version`
  field generalizes to an app identity string with msgvault continuing to
  populate it identically. If any field cannot be preserved byte-for-byte,
  the PR bumps `FormatVersion`/`MinReaderVersion` per the existing
  versioning discipline in `docs/architecture/backup-format.md` — silent
  incompatibility is not an option. Note that snapshot IDs are recomputed
  by readers from the manifest's marshaled JSON, so field order and key
  names are load-bearing: turning `ManifestStats` into raw JSON must
  produce byte-identical marshaling for msgvault or the recompute check
  breaks on old readers.
- Also exported for reuse: the scheduler (`internal/scheduler`), the
  config/home-dir mechanics (`internal/config` path handling, atomic
  save), and `internal/fileutil`. Exact export surface decided in the PR.
- msgvault behavior is unchanged; the PR is guarded in both directions:
  its existing backup tests, a round-trip test restoring a pre-extraction
  repo fixture (old writer → new reader), and a golden-manifest test
  proving a newly written msgvault snapshot is byte-compatible with the
  pre-extraction reader (new writer → old reader), covering the
  snapshot-ID recompute check.

## Architecture

Single Go binary, msgvault-shaped:

```
docbank/
├── cmd/docbank/            # Cobra CLI entrypoint
├── internal/
│   ├── store/              # SQLite schema + access (virtual tree)
│   ├── ingest/             # Ingest pipeline (CLI, watcher, API share it)
│   ├── extract/            # Text extraction workers
│   ├── api/                # Huma v2 HTTP API (agent surface)
│   ├── tui/                # Bubble Tea file browser
│   └── backupapp/          # backup.App implementation
└── go.mod                  # imports go.kenn.io/msgvault pkg/pack, pkg/backup
```

### Data layout

`~/.docbank/` (override with `DOCBANK_HOME`):

```
~/.docbank/
├── config.toml
├── docbank.db              # SQLite: virtual tree + metadata + FTS
├── blobs/<aa>/<sha256>     # loose content-addressed files (msgvault pattern)
└── logs/
```

Bytes are immutable and deduplicated by construction: the SHA-256 of the
content is the identity. Two ingested copies of the same file produce one
blob and two nodes.

## Virtual tree

### Schema (core tables)

```sql
nodes (
    id            INTEGER PRIMARY KEY,
    parent_id     INTEGER REFERENCES nodes(id),
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL,          -- 'dir' | 'file'
    blob_hash     TEXT,                   -- files only
    size          INTEGER,
    mime_type     TEXT,
    revision      INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    modified_at   TEXT NOT NULL,
    trashed_at    TEXT,                   -- NULL = live
    trash_parent  INTEGER,                -- original location, for restore
    trash_name    TEXT
)
blobs        (hash TEXT PRIMARY KEY, size INTEGER, created_at TEXT)
node_versions(node_id, blob_hash, size, replaced_at)  -- prior contents
ingests      (id, started_at, source_kind, source_desc)
provenance   (node_id, ingest_id, original_path, original_mtime)
tags         (id, name UNIQUE)
node_tags    (node_id, tag_id)
extracted_text (blob_hash, extractor, extractor_version, status,
                error, attempts, text, extracted_at,
                PRIMARY KEY (blob_hash, extractor))
-- one row per (blob_hash, extractor); re-extraction with a newer
-- extractor_version replaces the row. status: 'ok' | 'failed'.
-- FTS5 external-content table over extracted_text + node names
```

### Tree invariants

These rules are enforced in the store layer and matter immediately for
agent-driven batch reorganization:

- Exactly one root node (`parent_id IS NULL`, kind `dir`). Because
  SQLite treats NULLs as distinct in unique indexes, this needs its own
  enforcement: a partial unique index on a constant expression
  (`CREATE UNIQUE INDEX one_root ON nodes((1)) WHERE parent_id IS NULL`).
- `UNIQUE(parent_id, name)` among **live** nodes (partial index
  `WHERE trashed_at IS NULL`). Trashed nodes do not block reuse of a name.
- Names are stored as given, Unicode NFC-normalized, compared
  case-sensitively. Forbidden: empty, `.`, `..`, or containing `/` or NUL.
- Paths are a display and convenience concept; **node IDs are canonical**.
  Every API response includes IDs.
- Moves cannot create cycles (a node cannot move under its own
  descendant); the store checks ancestry in the move transaction.
- On bulk import, a name collision within the destination directory
  auto-suffixes: `report.pdf` → `report (2).pdf`. The provenance row
  preserves the original path regardless.
- Trash preserves `trash_parent`/`trash_name`; restore returns the node
  to its original location, re-suffixing on conflict. Trashing a
  directory trashes its subtree as a unit.
- Every mutation bumps the node's `revision`. Directory content changes
  (child added/removed/renamed) bump the directory's revision too.

### Versions

Replacing a file node's content records the prior `(blob_hash, size)` in
`node_versions` and points the node at the new blob. Version history is
nearly free in a CAS. v1 exposes listing and retrieving prior versions;
no diffing.

## Ingest

One pipeline behind all entry points:

1. Hash the source file (SHA-256, streaming).
2. If the blob does not exist: write to `blobs/tmp/`, fsync the file,
   rename to `blobs/<aa>/<hash>`, then fsync the `<aa>` directory (and
   `blobs/` itself when `<aa>` was just created) so the directory entry
   is durable before the DB row commits — otherwise power loss can leave
   a committed row pointing at a vanished blob. `pkg/pack` already
   implements this discipline (`SyncDir` is exported; the Phase 0 PR
   also exports the currently-internal mkdir-synced helper); reuse it.
   Rename onto an existing blob is success (idempotent).
3. In one DB transaction: insert/refresh `blobs` row, create the node,
   write provenance, auto-suffix on name collision.
4. Queue text extraction for the blob (skipped when the blob already
   has a completed record at the current extractor version — dedup
   means each unique content is extracted once per extractor version).

### Crash and retry semantics

- Blob writes are idempotent; retrying an interrupted ingest converges.
- The DB transaction commits only after the blob is durable. A crash
  between blob write and commit leaves an **orphan blob**, which is
  harmless and reclaimed by `docbank gc`.
- `blobs/tmp/` is cleared on startup.
- Ingest never deletes or modifies source files. (A `--remove-source`
  flag may come later, after trust is established — mirroring msgvault's
  archive-first-delete-later posture.)

### Entry points

- **CLI:** `docbank add <path>... [--dest /virtual/path]`. Directories
  import recursively, preserving relative structure as the initial
  virtual tree. This is the bulk-migration path for 20 years of
  accumulated Documents/Dropbox/old-drive trees; it must be resumable
  with a concrete idempotency rule: for each source file, the suffix
  resolver walks the candidate names in the destination directory
  (`report.pdf`, `report (2).pdf`, `report (3).pdf`, …); if any live
  candidate has the same `blob_hash`, the file is skipped (already
  imported — including files a prior run imported under a suffix). If
  all candidates exist with different content, the next free suffix is
  used; otherwise the first free candidate name is taken. Re-running an
  interrupted import therefore converges without duplicating nodes, and
  needs no provenance-table uniqueness.
- **Watched inboxes:** the daemon watches configured directories (scanner
  output, a "To File" folder). A file is imported only after a stability
  window — size and mtime unchanged for a configurable settle period
  (default 5s) — so partially copied files are never swallowed. Hidden
  files and known partial-download extensions are skipped. Imports land
  under `/inbox/<YYYY-MM-DD>/` awaiting filing.
- **HTTP upload:** multipart upload with target virtual path.

### Text extraction

Background workers (daemon or on-demand after CLI ingest) extract text
per blob, one record per `(blob_hash, extractor)` with the
`extractor_version` that produced it:

- PDF text layer, plain text/markdown, and office formats in v1.
- Renames/moves never trigger re-extraction; only new blobs do.
- A blob is extraction-eligible when it has no record for the extractor
  or its record's `extractor_version` is older than the current one;
  re-extraction replaces the record (stale text from old extractor
  versions is never kept). `docbank extract --missing` backfills.
- Failures are recorded in the same record (`status='failed'`, error
  text, attempt count) — an error, not silence — and are retryable; a
  corrupt PDF must not wedge the queue.

FTS5 indexes extracted text plus node names/paths and tags.

## HTTP API (the agent surface)

Served by `docbank serve`, reusing msgvault's daemon patterns: Huma v2
with a **typed OpenAPI contract from day one**, API-key auth, generated
client. Design test: an agent must be able to do everything the TUI can
through this API alone.

Filesystem-shaped endpoints (sketch; exact shapes in the OpenAPI spec):

- `GET /nodes/{id}` and `GET /path/{path}` — stat (both return IDs)
- `GET /nodes/{id}/children` — list, paginated
- `GET /nodes/{id}/content` — bytes; `GET .../versions` — history
- `GET /search?q=...` — FTS + filters (tag, MIME, date, path prefix), paginated
- `POST /nodes` — mkdir / upload
- `PATCH /nodes/{id}` — rename / move / retag, requires `If-Match: <revision>`
- `POST /batch/move` — bulk reorganization; `dry_run` mode validates the
  whole batch (collisions, cycles, missing IDs) and reports the outcome
  without applying; execution is all-or-nothing in one transaction
- `POST /nodes/{id}/trash`, `POST /nodes/{id}/restore` — with `If-Match`
- `GET /tags`, tag CRUD

Concurrency model: per-node `revision` with `If-Match` preconditions on
all mutations (a global tree ETag would invalidate every agent's work on
any unrelated change). `412 Precondition Failed` tells an agent to
re-read and retry. SQLite serializes writes; preconditions exist to
catch lost updates across agent read-modify-write turns, not to lock.

## TUI

Bubble Tea file-manager over the virtual tree, following msgvault's TUI
patterns: navigate/drill down, FTS search, tag and metadata display,
rename/move/trash/restore, version list, and "open" (materialize blob to
a temp file, hand to the default app). The TUI talks to the same store
layer as the API — no privileged operations.

## Backup

Direct reuse of the extracted engine. `internal/backupapp` implements
`backup.App`: freeze = SQLite WAL-checkpoint + pinned read transaction
(engine-provided helper); content refs = all rows in `blobs`; stats =
node/blob/tag counts + date range as JSON; exclusions = `logs/`, `tmp/`.

Backup reachability is deliberately broader than GC reachability: every
`blobs` row is captured, including blobs that are GC candidates (e.g.
after emptying the trash but before `docbank gc --run`). Backups
preserve GC candidates until GC actually reclaims them; only then do
they age out of new snapshots.

Commands mirror msgvault 0.17: `docbank backup init|create|list|verify|restore`
against a NAS-mounted (or any) repo path. Incremental by construction:
page-diffed DB snapshots + only-new blobs into sealed packs.

## Garbage collection

Deletion is soft and blobs are never reclaimed implicitly. v1 ships
`docbank gc`:

- Reachable = blobs referenced by live nodes ∪ trashed nodes ∪
  `node_versions`.
- Unreachable blobs are deleted from `blobs/`, their `blobs` and
  `extracted_text` rows removed, in that order (a crash mid-GC leaves
  rows without files; GC re-run reconciles, and `docbank verify` reports
  the discrepancy).
- Dry-run by default: `docbank gc` lists candidates and bytes
  reclaimable; `docbank gc --run` executes.
- Emptying the trash (`docbank trash empty [--older-than 30d]`) deletes
  trashed nodes; their blobs become GC candidates unless referenced
  elsewhere.

## Error handling principles

- Ingest is idempotent and resumable; failures name the file and reason.
- The store rejects invariant violations (name collisions, cycles) with
  typed errors the API maps to 409/412 and the TUI to messages.
- `docbank verify` checks blob files against recorded hashes (sampled or
  full) — corruption is detected, not discovered at retrieval time.
- Background workers (extraction, watcher) log failures per item and
  continue; they never crash the daemon on bad input.

## Testing

- testify, table-driven, per project convention.
- Store: tree-invariant tests (collisions, cycles, trash/restore,
  revision bumps) exercised through the public store API.
- Ingest: real fixture trees (synthetic files only — no real PII);
  crash-simulation tests for blob-write/commit ordering; watcher
  stability-window tests with slowly-written files.
- Extraction: real sample PDFs/office files; a corrupt-input test.
- API: httptest against the real store; If-Match conflict tests;
  batch-move dry-run vs execute; pagination.
- Backup: full round-trip (create → verify → restore → prove stats) on a
  populated store.
- Phase 0: msgvault's existing backup tests must pass unchanged, plus
  both compatibility directions: a fixture round-trip proving
  pre-extraction repos still restore (old writer → new reader), and the
  golden-manifest test proving newly written snapshots remain
  byte-compatible with the pre-extraction reader (new writer → old
  reader).

## Build order

Each phase is independently useful:

0. **msgvault extraction PR** — `pkg/pack` + generalized `pkg/backup`
   (its own spec in the msgvault repo when work starts).
1. **Core** — store (schema + invariants), ingest pipeline, CLI:
   `add`, `ls`, `tree`, `mv`, `rm` (trash), `restore`, `cat`, `search`,
   `gc`, `verify`.
2. **Daemon + API** — `serve`, Huma routes, watched inboxes, extraction
   workers.
3. **TUI** — file browser.
4. **Backup** — `backupapp` + backup commands.

## Open questions (deferred, not blocking)

- Go module path and repo hosting for docbank.
- Whether the scheduler export happens in Phase 0 or when docbank's
  daemon needs it (Phase 2).
- MCP server wrapping the API (post-v1; the OpenAPI contract makes it
  mechanical).
