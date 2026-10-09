---
title: Capabilities
description: What Docbank can do today, grouped by task, with links to the guide for each.
---

# Capabilities

Docbank gives every document a stable ID, keeps its earlier versions, and
checks content whenever it reads or writes it. People use the CLI, web
application, or terminal browser. Agents use the local MCP server or the
authenticated HTTP API. Go applications can own separate vaults inside their
own process.

This page lists what Docbank does today. Each section links to the guide that
explains how.

## Collect and organize

- Import files and whole directory trees. Docbank never modifies the source.
- Choose which files to import with include and exclude globs. Replace
  existing content on purpose with `docbank add --replace`.
- Import MBOX and Google Takeout archives. Each occurrence of a message is kept
  separately, and attachments become documents of their own.
- Import load-file packages with their supplied text, page maps, labels,
  families, and custodian claims.
- Group matching RAW, JPEG, and XMP files as one photo. Ambiguous groups are
  left for you to pair by hand.
- Organize photos into albums through the CLI and HTTP API. Add selected photos
  or a complete query result, choose covers, and star albums. Removing members
  or deleting an album keeps the files.
- Upload a single document with a declared hash and size, which Docbank
  verifies.
- Arrange documents in folders. The folder tree is virtual, so moving a
  document does not move its stored bytes.
- Refer to a live document by path, or by a numeric node ID that stays the same
  through moves, renames, trash, and restore.
- Define tags and assign them regardless of folder. Renaming a tag changes its
  display name and nothing else. The web app groups slash-separated tag names
  and keeps each tag's color when its name changes.
- Move several nodes in one operation that either applies the whole plan or
  changes nothing.
- Watch local inbox folders and import files once they stop changing.

Start with [Importing documents](usage/importing.md) and
[Organizing and tagging](usage/organizing.md). For photos, see
[Photo assets and albums](usage/photos.md).

## Find and retrieve

- Browse directories and trees with sizes and modification times. Responses are
  limited in size.
- Search the names of live documents with ranked results, and filter by path,
  media type, modification time, or tag ID.
- Search the verified extracted text of UTF-8 text, Markdown, JSON, and related
  text formats.
- List documents by filter alone, without a text query.
- Save complete queries and reusable highlight sets in the web app or through
  the HTTP API.
- Write queries with field and Boolean expressions, which Docbank validates.
  Freeze a query's results so its rows, counts, and facets stay fixed.
- Search by meaning in the web app and TUI once embeddings are configured, or
  find similar documents locally from stored embeddings.
- Read archived email with decoded or raw headers, and follow each attachment
  to the version that was attached.
- Read imported or generated transcripts with speaker labels and timing, and
  match search evidence to the exact retained transcript build.
- Search photos in the web app and narrow results with filter counts. Group
  by month or capture session, adjust grid density, and keep your selection
  and place when switching workspaces.
- Browse photo assets over HTTP and filter by camera, lens, ISO, capture date,
  GPS, asset type, album, or quality scores. Use `unevaluated` to include photos
  whose quality signals are pending or unavailable. A RAW/JPEG pair appears once.
- Given a SHA-256 hash, find every node and version that still refers to it.
- Download current or earlier content and check its size, hash, and final
  verification result.
- Save a local download only after the complete temporary file verifies.

See [Photo assets](usage/photos.md), [Searching](usage/searching.md), the
[Web application](usage/web.md), and
the [Interactive terminal browser](usage/tui.md).

## Export a reviewed selection

- Preview and download verified ZIP bundles of the document versions you
  selected.
- Export checked rows in the web app as CSV, or apply a tag to the whole
  selection in one atomic step.
- Reopen a stored export plan and see which outputs are unavailable, through
  the CLI and MCP.
- Create search-count reports for a date range, as a CSV of results with a
  frozen evidence ZIP.
- Export frozen selections as DAT packages with PDFs or images, or as CSV
  packages with native files.
- Reserve Bates labels and export selected pages as stamped PDFs. Labels from
  an abandoned export are not reused.
- Keep PDF renderings of email, each with the complete body and an inventory of
  attachments. Rendering new PDFs needs an opt-in Linux setup. PDFs that are
  already stored can be downloaded on any platform.

See [Verified export bundles](usage/export-bundles.md),
[Search exports](usage/search-exports.md),
[Web exports](usage/web.md#export-a-verified-zip), and
[Email PDFs](usage/email-pdf.md).

## Edit without losing history

- By default, every content edit is kept as an immutable version.
- Replace content only if the node still has the revision the writer inspected.
- Revert by creating a new current version that records which earlier version
  supplied its content.
- Retrieve any kept version by its UUID, even after the document moves.
- Preview and prune older versions when you do not want to keep them all.
- Record where content came from. A correction is added next to the original
  record and does not replace it.

See [Editing and versions](architecture/editing-and-versions.md).

## Recover and verify

- Move nodes to trash without reclaiming their content.
- Restore a node with the same ID, even when its former name is taken or its
  parent is missing.
- Empty selected trash permanently, always through a preview first.
- Reclaim content that nothing refers to by running garbage collection.
  Compacting dead space inside packs is a separate step.
- Recompute content hashes. The report separates missing or corrupt bytes from
  inaccessible stores and invalid metadata.
- Create incremental backups and verify their structure and bytes.
- Restore into a separate vault and verify it before putting it to use.
- Create, verify, restore, and clean up backups from embedded Go applications,
  including recovery files the application owns.
- Keep people, custodian information, and decisions through a restore.
- Back up objects larger than 4 GiB. New large-object snapshots require reader
  version 5, and recovery files over 64 MiB require reader version 6. Upgrade
  every reader of a shared repository before creating these snapshots.

See [Trash, garbage collection, and repack](usage/trash-and-gc.md),
[Backup and restore](usage/backup.md), and
[Vault lifecycle](usage/lifecycle.md).

## Keep permanent audited history

- Preview what a permanent audit scope will protect before you enable it.
  Enabling a scope cannot be undone.
- Keep enrolled history protected after moves and trash.
- Extend protection to new descendants of an enrolled directory.
- Record supported content, tree, tag, and source-history changes in an ordered
  history that cannot be edited through Docbank.
- Browse the history of a node or a scope, and verify it by replaying the
  recorded events.
- Compare the latest history hashes with evidence saved outside the vault.
- Keep audited history through backup and restore, which reproduce it
  deterministically.

See [Permanent audited history](usage/audited-history.md).

## Choose where content is stored

- Import into a local primary store, whose location is fixed.
- Store content as separate raw or zstd-compressed objects. Docbank then
  combines eligible small objects into packs and seals each pack against
  further writes.
- Attach secondary stores: filesystem directories or HTTPS S3-compatible
  namespaces, each fenced by an ownership marker. Their locations and
  credentials stay out of the vault's portable metadata.
- Before copying or moving content, preview the cost: verification reads,
  egress, scratch space, shared-reference constraints, audit pins, and
  pack-level reclamation.
- Repair a damaged location from another verified copy, salvage content held
  only in a fenced store, and evacuate a secondary store before unregistering
  it.
- Keep backups complete when some content exists only in a remote store.
  Restore into a fresh local primary store, or into a target layout you map
  explicitly.

Placing content in another store adds capacity. It does not synchronize, share,
encrypt, or back up that content. See [Multi-store storage](usage/storage.md)
for how to operate stores and what each one is trusted with.

## Build agent and application workflows

- Discover the running daemon and authenticate over loopback.
- Connect local agents with `docbank mcp`. Processing, report, export, and
  package writes are each off until you pass the matching opt-in flag.
- Resolve a saved citation to the exact text it cited, through HTTP or Go. A
  reference that is unavailable or no longer matches fails. Docbank does not
  substitute newer text.
- Create, rename, retire, merge, and split people through the CLI or API, and
  read person records through MCP.
- Generate the OpenAPI contract offline, or retrieve it from the daemon.
- Act on a document you inspected by using its stable ID and revision.
- Follow progress as one JSON record per line, ending with a final result.
- Look up a job by its durable ID after a client loses track of it or the
  daemon restarts.
- Embed a vault through `go.kenn.io/docbank` with CGO or pure-Go SQLite. An
  embedded vault has the same exclusive ownership and content checks.

See [Docbank for agents](agents.md), the
[Agent integration guide](agents/integration.md), and
[Embed in Go](embedding.md).

## Process documents and search the results

You can [configure a processing profile](usage/configuration.md), review what
each provider will receive, grant consent, and run the profile against one
version of a document. The CLI, web app, and TUI show processing plans, job
status, and the verified Markdown that processing produced. See the
[processing workflow](usage/document-processing.md) and the
[CLI commands](cli-reference.md#docbank-processing).

[Processing search](usage/search.md) has lexical, semantic, hybrid, and auto
modes. It searches only the source versions you have authorized. Semantic and
hybrid search send query text to a provider, so they need active consent.
Ordinary name and text search is separate from processing search.

In the web app and TUI, Auto selects Hybrid when an embedding binding is
available. The processing HTTP API and CLI use lexical retrieval for `auto`.

Reranking is optional and off unless you ask for it. It reorders the first
results, after Docbank has revalidated them, through a configured ZeroEntropy
or Cohere adapter. Reranking has its own consent grant, a 4,096-byte excerpt
ceiling, and a receipt that reads `applied`, `degraded`, or `skipped`.

Go applications can use the [document packages](document-understanding.md) to
prepare text and Markdown renditions, call OCR and embedding providers, and
keep each result tied to the content it came from. The embedded vault API also
reads source metadata and generates
[visual previews](architecture/visual-previews.md).

The vault stores processing profiles, disclosure consent, derived results, and
independent embedding sets. Internally, retrieval combines lexical and vector
matches, with optional query expansion, reranking, and QMD retrieval.
[Document processing](architecture/document-processing.md) describes these
components.

The daemon can extract plain text and EPUB locally. With configuration, it can
also transcribe audio through Docling and run
[embedding runtimes](configuration.md#embedding-workers-and-credentials).
Installing a provider package does not register it with the daemon, and does
not prepare an import for semantic search. The default configuration has no
processing profiles, and new imports do not run this workflow automatically.

## What Docbank does not do

Docbank does not synchronize a working folder between devices, create public
share links, provide collaborative editing, or encrypt live secondary stores.
The [roadmap](roadmap.md) lists planned work, and each linked guide lists the
current limits of its interface.
