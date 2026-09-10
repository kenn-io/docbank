---
title: Roadmap
description: Current capabilities, current limits, and planned product direction.
---

# Roadmap

Docbank stores, organizes, versions, verifies, and backs up documents today.
The CLI, web app, and terminal browser use one authenticated local daemon.
Go applications can also own separate vaults and use the document-processing
packages directly.

This page describes product direction. The [capability guide](capabilities.md)
summarizes available features; the linked guides own instructions and limits.
The [changelog](changelog.md) records published releases.

## What can I use now?

| Reader task | Current capability | Guide |
| --- | --- | --- |
| Collect documents | Import files and trees, select files with glob patterns, replace existing content deliberately, and watch local inboxes | [Importing](usage/importing.md) |
| Organize documents | Stable IDs, folders, tags, and atomic batch moves; the web app groups and colors tags | [Organizing](usage/organizing.md) |
| Find documents | Ranked name and text search, bounded filters without a query, and API storage for named queries and highlight sets | [Searching](usage/searching.md) |
| Keep earlier content | Immutable versions, revision checks, reversion, and deliberate history pruning | [Editing and versions](architecture/editing-and-versions.md) |
| Record origin and evidence | Append-only provenance and permanent audited directory scopes | [Importing](usage/importing.md) and [audited history](usage/audited-history.md) |
| Recover documents | Recoverable trash, explicit permanent deletion, garbage collection, and pack reclamation | [Trash and GC](usage/trash-and-gc.md) |
| Recover a vault | Incremental backups, verified restore into a separate target, and snapshot removal through the embedded API | [Backup and restore](usage/backup.md) |
| Add physical storage | Verified placement, repair, salvage, and evacuation across filesystem and S3-compatible stores | [Multi-store storage](usage/storage.md) |
| Integrate an application | Authenticated HTTP API, offline OpenAPI generation, and separately rooted embedded Go vaults | [Agent integration](agents/integration.md) and [Embed in Go](embedding.md) |

Release archives cover Linux, macOS, and Windows on amd64 and arm64.
`docbank update` installs a published release and coordinates daemon restart.

## What document processing is available?

Go applications can extract source metadata, generate visual previews, and use
local or hosted providers to produce text, Markdown, and embeddings. An
embedding represents content as numbers for comparison. The
[Go processing guide](document-understanding.md) maps the provider packages and
their requirements.

The vault retains derived results with their source versions, processing
profiles, disclosure consent, and independent embedding sets. It has
rebuildable vector indexes and internal hybrid retrieval, which combines
lexical and vector matches. Optional internal components expand queries,
rerank results, and retrieve from operator-hosted QMD. See
[Document Processing](architecture/document-processing.md) for the flow and
trust boundaries.

The daemon automatically extracts supported plain text. Its configured
[embedding workers](configuration.md#embedding-workers-and-credentials) can
execute retained jobs for the registered runtime contracts. Those jobs still
need prepared input generations, matching profiles, and current consent.

These pieces do not yet form an automatic OCR-to-semantic-search workflow for
new imports. Public CLI, web, and TUI search remains lexical. Saving a query
with hybrid-search settings does not execute internal retrieval.

## What can the human interfaces do?

The [web app](usage/web.md) browses and searches the vault. Users can upload
files, download current or historical content, manage tags, and trash or
restore documents. They can inspect versions, source history, audit evidence,
background jobs, backup recovery points, and storage health.

The [terminal browser](usage/tui.md) provides tree navigation, search, document
and version details, a node audit timeline, and recoverable trash and restore.
Its operations views show daemon jobs, storage inventory, and configured
backup recovery points.

Storage placement controls remain an operator or agent API workflow. The web
and terminal storage views are read-only. Saved queries and highlight sets
have HTTP APIs; neither client has a management screen for them.

## What remains planned?

- Automatic PDF and Office extraction for new vault imports, broader daemon
  processing workflows, and public semantic search.
- An MCP server for agent integrations.
- Overlapping permanent audit scopes.
- A retention contract for external references to Docbank nodes. Embedded
  source references currently record origin; they do not prevent deletion.
- Broader metadata inspection and version comparison in the web app.
- Import progress and broader metadata and evidence in the terminal browser.
- Reusable tree, timeline, comparison, evidence, and job components for other
  applications.
- Automatic replication targets, primary-store replacement, lifecycle
  policies, and web or terminal storage-placement controls.
- Broader backup retention policy and representative-corpus hardening.

Rich document comparison belongs in the web app or external tools.

## Deferred beyond v1

- Encryption for live storage and backup repositories.
- Importing attachments from msgvault.
- Multi-user access and sharing.
