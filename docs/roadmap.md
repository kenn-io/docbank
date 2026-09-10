---
title: Roadmap
description: Current capabilities, current limits, and planned product direction.
---

# Roadmap

Docbank stores, organizes, versions, verifies, and backs up documents today.
This page separates those capabilities from planned work. For instructions and
exact limits, follow the linked guides.

| Phase | Scope | Status |
|-------|-------|--------|
| 0 | Shared storage and backup engines in `go.kenn.io/kit` | Implemented |
| 1 | Core document store, import, search, and CLI | Implemented |
| 2a | Daemon, HTTP API, self-update, and releases | Implemented |
| 2b | Versions, audit history, tags, watched inboxes, and document processing | Partly implemented; see below |
| 3 | Web application and terminal browser | Available; broader workflows remain planned |
| 4 | Backup and restore | Implemented; further hardening remains |

## Implemented (Phase 1)

- Import files and directory trees without changing their sources.
- Organize documents in a virtual tree with stable node IDs.
- Search names and verified UTF-8 text with ranked results.
- Recover trashed documents before permanent deletion.
- Preview garbage collection with `gc` and check content with `verify`.
- Keep one process in control of a vault through operating-system locks.

The [Capabilities](capabilities.md) page summarizes current behavior.
The [CLI Reference](cli-reference.md) owns commands, flags, and error behavior.
The core commands include `add`, `provenance`, `mkdir`, `ls`, `tree`, `cat`,
`mv`, `rm`, `restore`, `search`, `trash`, `gc`, and `verify`.

## Phase 2a — Infrastructure (implemented)

The CLI and external clients use one authenticated daemon. The daemon handles
vault ownership, background work, and maintenance. It starts automatically for
data commands and supports explicit `run`, `start`, `status`, `restart`, and
`stop` operations.

- [Daemon](architecture/daemon.md) explains discovery and lifecycle.
- [HTTP API](architecture/http-api.md) describes the shared API.
- [Ownership & Concurrency](architecture/locking.md) explains exclusive access.
- [Configuration](configuration.md) owns daemon and background-job settings.

`docbank update` installs published releases and coordinates daemon restart.
`docbank openapi` produces the API description without opening a vault.
Release archives and `SHA256SUMS` cover Linux, macOS, and Windows on amd64 and
arm64.

Physical storage supports separate files and immutable packs. Operators use
`docbank storage status`, `docbank storage pack`, and `docbank storage repack`
to inspect and maintain them. Ordinary imports write separate files; startup
does not convert existing content. Kit's unpack operation remains internal.
See [Trash, GC, Repack & Verify](usage/trash-and-gc.md).

## Phase 2b — Features (in progress)

### What is available?

- [Content versions](architecture/editing-and-versions.md) preserve prior bytes
  when users replace or revert a document.
- [Tags and batch moves](usage/organizing.md) support stable labels and
  reorganizations that apply as one transaction.
- [Provenance](usage/importing.md) records where imported content came from.
- [Watched inboxes](configuration.md#watched-inboxes) import settled files and
  append later source changes to the same stable node.
- [Permanent audited history](usage/audited-history.md) protects disjoint
  directory scopes and verifies their recorded changes.
- [Search](usage/searching.md) indexes bounded, verified text formats.
- [Source metadata](architecture/source-metadata.md) records typed facts from
  original bytes, including auxiliary MD5 checksums.
- [Embedded visual previews](embedding.md#read-canonical-visual-previews)
  produce canonical JPEG previews through the Go API.
- [Document packages](document-understanding.md) provide Go applications with
  normalization, OCR adapters, media checks, and embedding inputs.
- [Embedding workers](configuration.md#embedding-workers-and-credentials)
  execute retained jobs when matching profiles, runtime configuration, input
  generations, and disclosure consent are present.

Docbank also retains a catalog of derived document results, text-search
projections, processing profiles, and consent. These components do not make
new imports an automatic end-to-end OCR or semantic-search workflow.

### What remains planned?

- Overlapping audit scopes.
- Automatic extraction of PDF text layers and Office documents into a vault.
- Broader daemon document-processing and semantic-search workflows.
- A defined retention contract for external references to Docbank nodes.

Embedded `Create` already accepts a source kind, description, opaque reference,
and optional modification time. Those facts record origin; they do not keep a
node from being permanently deleted. Standalone local import records a
filesystem source path. See [Embed in Go](embedding.md).

## Phase 3 — Human applications

### What is available?

The [web application](usage/web.md) browses and searches the vault. Users can
inspect versions, source history, tags, and permanent audit history. They can
upload files, download verified current or historical content, manage tags,
trash documents, and restore them. The portal also shows background jobs,
backup recovery points, and storage health.

The [terminal browser](usage/tui.md) provides tree navigation, search, document
and version details, a node audit timeline, and recoverable trash and restore.
Its operations views report daemon jobs, storage inventory, and configured
backup recovery points.

Both clients use the authenticated API and have no privileged route into the
vault. Storage placement remains an operator or agent API workflow; its web
and terminal views are read-only.

### What remains planned?

- Broader metadata inspection and version comparison in the web application.
- Import progress and broader metadata and evidence in the terminal browser.
- Reusable tree, timeline, comparison, evidence, and job components for other
  applications.

Rich document comparison belongs in the web application or external tools.

## Phase 4 — Backup (implemented)

`docbank backup init|create|list|verify|restore` creates and manages recovery
points through the shared Kit engine. Backups preserve the virtual tree,
stable IDs, content versions, trash, provenance, tags, and extraction state.
Restore builds and checks a fresh database and content store before making the
vault available. Older SQLite page-map snapshots remain restorable.

[Backup & Restore](usage/backup.md) owns the operating procedure.
[Backup Architecture](architecture/backup.md) explains the format and checks.
Further work covers representative-corpus hardening and retention policy.

## Multi-store placement (implemented)

New content starts in one local primary store. Operators can attach secondary
filesystem or S3-compatible stores, preview content placement, and execute
verified copies or moves. They can also repair damaged copies, recover bytes
from a store that fails its ownership check, and evacuate a store. Durable job
IDs let clients inspect operations whose outcome was uncertain.

Backup includes content held only in a secondary store. Restore uses a fresh
local primary by default, or an explicitly mapped set of stores. See
[Multi-store Storage](usage/storage.md) for exact ownership checks, deployment
bindings, preview requirements, and recovery behavior.

Automatic replication targets, primary-store replacement, lifecycle policies,
and web or terminal placement controls remain planned.

## Deferred beyond v1

- Encryption for live storage and backup repositories.
- Importing attachments from msgvault.
- Multi-user access and sharing.

An MCP server and broader document-processing, embedding, and semantic-search
workflows remain in progress. The available worker components are described
above; they are not a complete public retrieval workflow.
