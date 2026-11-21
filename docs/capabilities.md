---
title: Capabilities
description: What Docbank can do today, grouped by task, with links to the guide for each.
---

# Capabilities

Docbank gives every document a stable ID, retains its earlier versions, and
verifies content on every read and write. You can work through the CLI, web
application, or terminal browser, while agents connect through the local MCP
server or authenticated HTTP API, and Go applications can own separate vaults
in-process.

## Collect and organize

- Import individual files or whole directory trees without modifying the
  originals.
- Import from a [URL](usage/importing.md#import-from-a-url) while keeping the
  requested URL, redirect destination, fetch time, and response headers.
- Filter imports with include and exclude globs, or replace existing content on
  purpose with `docbank document add --replace`.
- Import MBOX and Google Takeout archives. Docbank keeps each occurrence of a
  message separately and stores attachments as their own documents.
- Import load-file packages along with their supplied text, page maps, labels,
  families, and custodian claims.
- Group matching RAW, JPEG, and XMP files as one photo. When a match is
  ambiguous, Docbank leaves the files for you to pair by hand.
- Organize photos into albums in the web app, CLI, or HTTP API. You can add
  selected photos or a full query result, pick album covers, and star albums.
  Removing members or deleting an album leaves the underlying files untouched.
- Upload a single document with a declared hash and size for Docbank to verify.
- Arrange documents in virtual folders. Because the tree is virtual, moving a
  document leaves its stored bytes in place.
- Address live documents by path or by a numeric node ID that stays the same
  through moves, renames, trash, and restore.
- Create tags and assign them across folders. Renaming a tag updates only its
  display name, and the web app groups slash-separated tag names while keeping
  each tag's color when you rename it.
- Move several nodes in one atomic operation that either applies the whole plan
  or changes nothing.
- Watch local inbox folders and import files once they stop changing.

Start with [Importing documents](usage/importing.md) and
[Organizing and tagging](usage/organizing.md). For photos, see
[Photo assets and albums](usage/photos.md).

## Find and retrieve

- Browse directories and trees with sizes and modification times in
  size-limited responses.
- Search live document names with ranked results, and filter by path, media
  type, modification time, or tag ID.
- Search verified extracted text across UTF-8 text, Markdown, JSON, related
  text formats, and the text layer of PDFs.
- List documents using filters alone, without entering a text query.
- Save complete queries and reusable highlight sets in the web app or through
  the HTTP API.
- Write queries with validated field and Boolean expressions, and freeze a
  query's results so its rows, counts, and facets stay fixed.
- Search by meaning in the web app and TUI once you configure embeddings, or
  find similar documents locally from stored embeddings.
- Read archived email with decoded or raw headers, and follow any attachment to
  the exact version that arrived with the message.
- Read imported or generated transcripts with speaker labels and timing, and
  match search evidence to the exact retained transcript build.
- [Search transcripts for selected recordings](agents/integration.md#search-transcripts-for-exact-recordings)
  over HTTP while keeping each recording's identity even when multiple
  recordings share the same audio.
- Search photos in the web app and narrow results with filter counts. You can
  group by month or capture session, adjust grid density, and keep your
  selection and place when switching workspaces.
- Browse photo assets over HTTP and filter by camera, lens, ISO, capture date,
  GPS, asset type, album, or quality scores. Pass `unevaluated` to include
  photos whose quality signals are pending or unavailable. A RAW/JPEG pair
  appears once.
- Look up every node and version that still refers to a given SHA-256 hash.
- Download current or earlier content and check its size, hash, and final
  verification result.
- Save a local download only after the complete temporary file passes
  verification.

See [Photo assets](usage/photos.md), [Searching](usage/searching.md), the
[Web application](usage/web.md), and the
[Interactive terminal browser](usage/tui.md).

## Export a reviewed selection

- Preview and download verified ZIP bundles containing the exact document
  versions you selected.
- Render up to 16 photos as JPEG or PNG with size, image metadata, and GPS
  options, up to 100 million pixels per image.
- Export checked rows in the web app as CSV, or tag the whole selection in one
  atomic step.
- Reopen a stored export plan through the CLI or MCP to see which outputs are
  unavailable.
- Build search-count reports for a date range as a CSV of results paired with a
  frozen evidence ZIP.
- Export frozen selections as DAT packages with PDFs or images, or as CSV
  packages with native files.
- Reserve Bates labels and export selected pages as stamped PDFs. Labels from
  an abandoned export are never reused.
- Store PDF renderings of email with the full body and an attachment inventory.
  Rendering new PDFs requires an opt-in Linux setup, while stored PDFs can be
  downloaded on any platform.

See [Verified export bundles](usage/export-bundles.md),
[Search exports](usage/search-exports.md),
[Web exports](usage/web.md#export-a-verified-zip), and
[Email PDFs](usage/email-pdf.md).

## Edit without losing history

- Keep every content edit as an immutable version by default.
- Replace content only when the node still matches the revision you inspected.
- Revert by creating a new current version that records which earlier version
  supplied its content.
- Retrieve any kept version by its UUID, even after the document moves.
- Preview and prune older versions when you do not want to keep all of them.
- Record where content came from. Adding a correction appends it next to the
  original record instead of replacing it.

See [Editing and versions](architecture/editing-and-versions.md).

## Recover and verify

- Move nodes to trash without reclaiming their stored content.
- Restore a node with its original ID, even if its former name is taken or its
  parent directory is missing.
- Empty selected trash permanently, always starting with a preview.
- Reclaim unreferenced content with garbage collection, then compact dead space
  inside packs as a separate step.
- Recompute content hashes with a report that separates missing or corrupt
  bytes from inaccessible stores and invalid metadata.
- Create incremental backups and verify both their structure and their bytes.
- Restore into a separate vault and verify it before putting it into use.
- Create, verify, restore, and clean up backups from embedded Go applications,
  including recovery files the application owns.
- Preserve people, custodian information, and decisions across a restore.
- Back up objects larger than 4 GiB. New large-object snapshots require reader
  version 5, and recovery files over 64 MiB require reader version 6. Upgrade
  every reader of a shared repository before creating those snapshots.

See [Trash, garbage collection, and repack](usage/trash-and-gc.md),
[Backup and restore](usage/backup.md), and
[Vault lifecycle](usage/lifecycle.md).

## Keep permanent audited history

- Preview what a permanent audit scope protects before turning it on. Enabling
  a scope cannot be undone.
- Keep enrolled history protected through moves and trash.
- Extend protection automatically to new descendants of an enrolled directory.
- Record supported content, tree, tag, and source-history changes in an ordered
  history that cannot be edited through Docbank.
- Browse the history of a node or scope and verify it by replaying the recorded
  events.
- Compare the latest history hashes against evidence saved outside the vault.
- Keep audited history through backup and restore, which reproduce it
  deterministically.

See [Permanent audited history](usage/audited-history.md).

## Choose where content is stored

- Import into a local primary store at a fixed location.
- Store content as separate raw or zstd-compressed objects. Docbank combines
  eligible small objects into packs and seals each pack against further writes.
- Attach secondary stores: filesystem directories or HTTPS S3-compatible
  namespaces, each fenced by an ownership marker. Store locations and
  credentials stay out of the vault's portable metadata.
- Preview the cost before copying or moving content, including verification
  reads, egress, scratch space, shared-reference constraints, audit pins, and
  pack-level reclamation.
- Repair a damaged location from another verified copy, salvage content held
  only in a fenced store, and evacuate a secondary store before unregistering
  it.
- Keep backups complete even when some content lives only in a remote store,
  and restore into a fresh local primary store or an explicit target layout.

Adding another store expands capacity, but it does not synchronize, share,
encrypt, or back up your content. See [Multi-store storage](usage/storage.md)
for how to operate stores and what each one is trusted with.

## Build agent and application workflows

- Discover the running daemon and authenticate over loopback.
- Connect local agents with `docbank mcp`. Processing, report, export, and
  package writes stay off until you pass the matching opt-in flag.
- Resolve a saved citation to the exact text it cited through HTTP or Go. If a
  reference is unavailable or no longer matches, Docbank fails instead of
  substituting newer text.
- Check through MCP whether numbers and identifiers in an answer appear in its
  saved citations. A match does not prove the answer is true.
- Create, rename, retire, merge, and split people through the CLI or API, and
  read person records through MCP.
- Generate the OpenAPI contract offline or fetch it from the running daemon.
- Act on a document you inspected using its stable ID and revision.
- Follow progress as one JSON record per line, ending with a final result.
- Look up a job by its durable ID if a client loses track of it or the daemon
  restarts.
- Embed a vault through `go.kenn.io/docbank/docbank` with CGO or pure-Go
  SQLite, keeping the same exclusive ownership and content checks.

See [Docbank for agents](agents.md), the
[Agent integration guide](agents/integration.md), and
[Embed in Go](embedding.md).

## Process documents and search the results

You can [configure a processing profile](usage/configuration.md), review what
each provider receives, grant consent, and run the profile against one version
of a document. The CLI, web app, and TUI show processing plans, job status, and
the verified Markdown that processing produced. See the
[processing workflow](usage/document-processing.md) and the
[CLI commands](cli-reference.md#docbank-processing).

[Automatic and processing search](usage/search.md) supports lexical, semantic,
hybrid, and auto modes. Explicit processing search uses only the source
versions you authorize. Because semantic and hybrid search send query text to a
provider, they require active consent. Plain CLI search automatically tries
hybrid retrieval over current live file versions when you configure a query
embedding binding, and reports a degradation reason if it falls back to
ordinary lexical search. Explicit processing search keeps the profile and
source-version fence you supply.

In the web app and TUI, **Auto** selects **Hybrid** when an embedding binding
is available. The processing HTTP API and explicit CLI `--mode auto` use
lexical retrieval.

Reranking is optional and off unless you ask for it. After Docbank revalidates
the first results, reranking reorders them through a configured ZeroEntropy or
Cohere adapter. It has its own consent grant, a 4,096-byte excerpt ceiling, and
a receipt that reads `applied`, `degraded`, or `skipped`.

Go applications can use the [document packages](document-understanding.md) to
prepare text and Markdown renditions, call OCR and embedding providers, and
keep each result tied to its source content. The embedded vault API also reads
source metadata and generates
[visual previews](architecture/visual-previews.md).

The vault stores processing profiles, disclosure consent, derived results, and
independent embedding sets. Internally, retrieval combines lexical and vector
matches, with optional query expansion, reranking, and QMD retrieval. See
[Document processing](architecture/document-processing.md) for how these pieces
fit together.

The daemon extracts plain text, EPUB, and PDF text layers locally, and indexes
new PDFs for ordinary search as they are added. With configuration, it can also
transcribe audio and convert documents through Docling, run Mistral OCR, and
use [embedding runtimes](configuration.md#embedding-workers-and-credentials).
Installing a provider package does not register it with the daemon or prepare
imports for semantic search. Other processing runs on new imports only after
you turn it on, for example with local text search, face suggestions, or a
watch that routes files to a profile.

## What Docbank does not do

Docbank does not synchronize a working folder between devices, create public
share links, provide collaborative editing, or encrypt live secondary stores.
See the [roadmap](roadmap.md) for planned work, and check each linked guide for
the current limits of its interface.
