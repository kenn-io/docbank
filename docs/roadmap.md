---
last_edited: 2026-11-07
title: Roadmap
description: What Docbank does today, what it does not do yet, and what is planned.
---

# Roadmap

Today, Docbank stores, organizes, versions, verifies, and backs up documents
through a shared authenticated local daemon used by the CLI, web app, and
terminal browser. Go applications can also own separate vaults and use the
document-processing packages directly.

For details on current features, see the [capability guide](capabilities.md),
or check the [changelog](changelog.md) for published releases. Instructions and
limits live in the guides linked below. The
[review-to-delivery roadmap](https://github.com/kenn-io/docbank/issues/719)
connects the original document-workflow proposals to merged outcomes, work under
review, and the next scope decisions.

## What works today

| Task | What works today | Guide |
| --- | --- | --- |
| Collect documents | Import files and trees, select files with glob patterns, replace existing content on request, and watch local inbox folders | [Importing](usage/importing.md) |
| Organize documents | Stable IDs, folders, tags, and atomic batch moves. The web app groups and colors tags | [Organizing](usage/organizing.md) |
| Find documents | Ranked name and text search, filtering without a query, and saved queries and highlight sets in the web app and API | [Searching](usage/searching.md) |
| Review and export | Frozen queries, verified ZIP bundles, dated search reports, load-file packages, and Bates-stamped selected pages | [Web application](usage/web.md) and [export bundles](usage/export-bundles.md) |
| Redact and number one PDF | Review explicit masks, render and verify a redacted PDF, and retain a numbered delivery for recovery through the browser, CLI and HTTP API | [Native PDF review and delivery](usage/document-processing.md#review-and-render-a-native-pdf) |
| Read email and recordings | Mailbox import, navigation between messages and their attachments, stored email PDFs, imported recordings, and timed transcripts | [Web application](usage/web.md#read-archived-email) and [processing](usage/document-processing.md) |
| Organize photos | RAW/JPEG/XMP grouping, a web grid grouped by month or capture session, albums in the web app, CLI, or HTTP API, and metadata and album filters | [Photo assets](usage/photos.md) |
| Keep earlier content | Immutable versions, revision checks, reversion, and history pruning on request | [Editing and versions](architecture/editing-and-versions.md) |
| Record origin and evidence | Append-only provenance and permanent audited directory scopes | [Importing](usage/importing.md) and [audited history](usage/audited-history.md) |
| Recover documents | Trash with restore, permanent deletion as a separate step, garbage collection, and pack reclamation | [Trash and GC](usage/trash-and-gc.md) |
| Recover a vault | Incremental backups, verified restore into a separate target, and snapshot removal through the embedded API | [Backup and restore](usage/backup.md) |
| Add physical storage | Verified placement, repair, salvage, and evacuation across filesystem and S3-compatible stores | [Multi-store storage](usage/storage.md) |
| Integrate an application | Authenticated HTTP API, local MCP server, offline OpenAPI generation, and embedded Go vaults with their own roots | [Agent integration](agents/integration.md), [MCP](usage/mcp.md), and [Embed in Go](embedding.md) |

Release archives cover Linux, macOS, and Windows on amd64 and arm64. Running
`docbank update` installs a published release and coordinates the daemon
restart.

Native PDF preparation and rendering require the configured Linux amd64 page
runtime and supported source text. Reserving numbers explicitly retains the
unredacted original indefinitely. See the linked guide for limits and the
difference between recovering recorded bytes and verifying them against a
review.

## Document processing today

Go applications can extract source metadata, generate visual previews, and call
local or hosted providers to produce text, Markdown, and embeddings. An
embedding represents content as numbers so similar content can be compared. See
the [Go processing guide](document-understanding.md) for the provider packages
and their requirements.

The vault stores each derived result alongside the source version it came from,
along with processing profiles, disclosure consent, and independent embedding
sets. Vector indexes can be rebuilt. Hybrid retrieval combines lexical and
vector matches and stays limited to the source versions a search is authorized
for. Optional internal components handle query expansion, result reranking, and
retrieval from an operator-hosted QMD. See
[Document processing](architecture/document-processing.md) for the flow and
trust boundaries.

The daemon extracts supported plain text automatically. Configured
[embedding workers](configuration.md#embedding-workers-and-credentials) can run
stored jobs for registered runtime contracts, provided the jobs have prepared
input generations, matching profiles, and current consent.

You can [preview and run configured processing profiles](usage/document-processing.md),
inspect jobs, and read stored sanitized renditions through the CLI, HTTP API,
web app, and TUI. [Automatic and processing search](usage/search.md) supports
lexical, semantic, hybrid, and auto modes over only the source versions you
have authorized.

HTTP and MCP clients can [save exact text citations](usage/mcp.md#saved-text-citations)
and reopen retained quotations even after a source or rendition is replaced.
Citations do not prevent pruning or purge.

New imports do not run OCR or prepare semantic search automatically. The
default configuration includes no processing profiles, and without them search
uses file names and locally extracted text. With a configured embedding
binding, **Auto** search in the web app and TUI uses **Hybrid**. Saving a query
with hybrid-search settings does not run it. Frozen query snapshots in
**Documents** support lexical mode, while **Photos** snapshots also support
semantic and hybrid search.

## Web app and terminal browser

In the [web app](usage/web.md), you can browse and search the vault, upload
files, download current or earlier content, manage tags, and trash or restore
documents. You can also inspect versions, source history, audit evidence,
background jobs, backup recovery points, and storage health.

The [terminal browser](usage/tui.md) gives you tree navigation, search,
document and version details, a node audit timeline, and trash with restore.
Its operations views display daemon jobs, storage inventory, and configured
backup recovery points.

Storage placement still happens through the CLI or the API, and the storage
views in the web app and terminal browser are read-only. You can
[manage saved queries and highlight sets](usage/web.md#saved-queries-and-highlights)
in the web app and HTTP API, while the terminal browser has no screen for
managing them.

## Planned work

- Automatic Office extraction for new vault imports and broader processing
  workflows.
- Overlapping permanent audit scopes.
- A retention contract for external references to Docbank nodes. Embedded
  source references currently record origin and do not prevent deletion.
- Broader metadata inspection and version comparison in the web app.
- Import progress and broader metadata and evidence in the terminal browser.
- Tree, timeline, comparison, evidence, and job components that other
  applications can reuse.
- Automatic replication targets, primary-store replacement, lifecycle policies,
  and web or terminal storage-placement controls.
- Broader backup retention policy, and hardening against representative
  document collections.

Rich document comparison belongs in the web app or external tools.

## Deferred beyond v1

- Encryption for live storage and backup repositories.
- Importing attachments from msgvault.
- Multi-user access and sharing.
