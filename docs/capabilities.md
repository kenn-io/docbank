---
title: Capabilities
description: What Docbank does for people, agents, applications, recovery, and physical storage.
---

# Capabilities

Docbank stores documents with stable IDs, keeps prior versions, and checks
content when it reads or writes it. People use the CLI, web application, or
terminal browser. Agents and external applications use the authenticated HTTP
API. Go applications can own separate vaults inside their own process.

## Collect and organize

- Recursively import files and directories without modifying their sources.
- Upload one document with a declared hash and size that Docbank verifies.
- Arrange documents in a virtual tree without moving stored bytes.
- Address live documents by path or keep stable numeric node IDs across moves,
  renames, trash, and restore.
- Define stable tags, rename their display names, and assign them independently
  of folder placement. The web app groups slash-separated names and keeps each
  tag's color when its name changes.
- Move several nodes in one operation that either applies the whole plan or
  changes nothing.
- Watch local inboxes and import files after they stop changing.

Start with [Importing Documents](usage/importing.md) and
[Organizing & Tagging](usage/organizing.md).

## Find and retrieve

- Browse directories and trees with response limits, sizes, and modification
  times.
- Search live names with ranked results and filter by path, media type,
  modification time, or tag ID.
- Search verified extracted text for supported UTF-8 text, Markdown, JSON, and
  related textual formats.
- List documents using bounded filters without a text query.
- Save named queries and reusable highlight sets through the HTTP API.
- Find every retained node and version that refers to a known SHA-256 hash.
- Download current or historical content and verify its size, hash, and final
  verification result.
- Save a local download only after the complete temporary file verifies.

See [Searching](usage/searching.md), the [Web Application](usage/web.md), and
the [Interactive Terminal Browser](usage/tui.md).

## Change without erasing history

- Keep every content edit as an immutable version by default.
- Replace content only if the node still has the revision the writer inspected.
- Revert by creating a new current version that records which prior version
  supplied its content.
- Retrieve any retained version by its stable UUID after the document moves.
- Preview and deliberately prune unwanted non-current history when unlimited
  retention is not appropriate.
- Record where content came from. Keep the original facts when adding a correction.

See [Editing & Versions](architecture/editing-and-versions.md).

## Recover and verify

- Move nodes to recoverable trash without reclaiming content.
- Restore the same node ID even when its former name is taken or its parent
  is missing.
- Permanently empty selected trash only through a preview-first operation.
- Reclaim unreachable loose content with explicit garbage collection, then
  compact dead packed payload separately.
- Recompute content hashes and report missing or corrupt bytes separately
  from inaccessible stores and invalid metadata.
- Create incremental backups and verify their structure and bytes.
- Restore into a separate vault and verify it before making it available.
- Use the embedded Go API to preview removal of old backup snapshots and
  reclaim repository data that no surviving snapshot needs.

See [Trash, GC, Repack & Verify](usage/trash-and-gc.md),
[Backup & Restore](usage/backup.md), and [Vault Lifecycle](usage/lifecycle.md).

## Retain permanent evidence

- Preview exactly what a permanent audited scope will protect before enabling
  an irreversible retention promise.
- Keep enrolled history protected after moves and trash.
- Extend protection to new descendants of an enrolled directory.
- Record supported content, tree, tag, and source-history changes in an
  ordered history that cannot be edited through Docbank.
- Browse node or scope history and verify it by replaying the recorded events.
- Compare the latest history hashes with evidence saved outside the vault.
- Preserve audited history through deterministic backup and restore.

See [Permanent Audited History](usage/audited-history.md).

## Place physical content deliberately

- Import into a fixed local primary store.
- Store content as separate raw or zstd-compressed objects, then combine
  eligible small objects into packs that Docbank seals against further writes.
- Attach fenced secondary filesystem or HTTPS S3-compatible namespaces without
  placing their deployment coordinates or credentials in portable metadata.
- Preview copy or move costs, verification reads, egress, scratch space,
  shared-reference constraints, audit pins, and pack-level reclamation.
- Repair a damaged location from another verified copy, salvage uniquely held
  content from a fenced store, and evacuate a secondary before unregistering it.
- Keep backups complete across remote-only placement and restore either into a
  fresh local primary or an explicitly mapped target topology.

Placement is capacity management, not synchronization, sharing, encryption, or
backup. See [Multi-store Storage](usage/storage.md) for the operational and
trust boundaries.

## Build agent and application workflows

- Discover the current daemon and authenticate over loopback.
- Generate the live OpenAPI contract offline or retrieve it from the daemon.
- Use stable IDs and revisions to act on inspected documents.
- Follow progress as one JSON record per line through a final result.
- Inspect durable job IDs after an uncertain client outcome or daemon restart.
- Embed a vault through `go.kenn.io/docbank`, choosing CGO or pure-Go SQLite,
  with the same exclusive ownership and content checks.

See [Docbank for Agents](agents.md), the
[Agent Integration Guide](agents/integration.md), and [Embed in Go](embedding.md).

## Process documents in Go

Go applications can use the [document packages](document-understanding.md) to
prepare text and Markdown renditions, call OCR and embedding providers, and
keep results tied to the original content. The embedded vault API also reads
source metadata and generates [visual previews](architecture/visual-previews.md).

The vault retains processing profiles, disclosure consent, derived results, and
independent embedding sets. Its internal retrieval components combine lexical
and vector matches, with optional query expansion, reranking, and QMD retrieval.
These components are described in
[Document Processing](architecture/document-processing.md).

The daemon registers plain-text extraction and two kinds of configured
[embedding runtime](configuration.md#embedding-workers-and-credentials).
Installing a provider package does not register it with the daemon or prepare
an import for semantic search. The CLI, web app, and terminal browser expose
lexical search; they do not expose the internal hybrid retrieval pipeline.

## Deliberate boundaries

Docbank does not synchronize a mutable folder between devices, create public
share links, provide collaborative editing, or encrypt live secondary stores.
See the [Roadmap](roadmap.md) for planned product work and the linked guides
for each interface's current limits.
