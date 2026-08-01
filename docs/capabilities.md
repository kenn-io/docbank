---
title: Capabilities
description: What Docbank does for people, agents, applications, recovery, and physical storage.
---

# Capabilities

Docbank is one document authority with several ways to use it. People can work
through the CLI, web application, or terminal browser. Agents and external
applications use the authenticated HTTP contract. Go applications can embed
separately rooted vaults without a sidecar process. Every surface resolves the
same stable nodes, immutable versions, and verified content.

## Collect and organize

- Recursively import files and directories without modifying their sources.
- Upload one document through a verified streaming contract.
- Arrange documents in a virtual tree without moving stored bytes.
- Address live documents by path or keep stable numeric node IDs across moves,
  renames, trash, and restore.
- Define stable tags, rename their display names, and assign them independently
  of folder placement.
- Perform an all-or-nothing batch reorganization with exact final coordinates.
- Configure watched inboxes for settled, repeatable server-side ingestion.

Start with [Importing Documents](usage/importing.md) and
[Organizing & Tagging](usage/organizing.md).

## Find and retrieve

- Browse bounded directory and tree projections with sizes and modification
  times.
- Search live names with FTS5 ranking and filter by path, media type,
  modification time, or stable tag identity.
- Search verified extracted text for supported UTF-8 text, Markdown, JSON, and
  related textual formats.
- Resolve every retained reference to a known SHA-256 digest.
- Stream current or historical content while checking size, digest, and
  terminal verification evidence.
- Publish a durable local download only after the complete staged file verifies.

See [Searching](usage/searching.md), the [Web Application](usage/web.md), and
the [Interactive Terminal Browser](usage/tui.md).

## Change without erasing history

- Keep every content edit as an immutable version by default.
- Replace the current bytes under a revision precondition so a stale editor or
  agent cannot overwrite a newer decision.
- Revert by creating a new head that records its historical source rather than
  rewinding the version graph.
- Retrieve any retained version by its stable UUID after the document moves.
- Preview and deliberately prune unwanted non-current history when unlimited
  retention is not appropriate.
- Record immutable provenance facts and later corrections without rewriting
  the prior observation.

See [Editing & Versions](architecture/editing-and-versions.md).

## Recover and verify

- Move nodes to recoverable trash without reclaiming content.
- Restore the same stable node with explicit collision and missing-parent
  behavior.
- Permanently empty selected trash only through a preview-first operation.
- Reclaim unreachable loose content with explicit garbage collection, then
  compact dead packed payload separately.
- Re-hash every authorized object and distinguish missing, corrupt, fenced,
  unavailable, and malformed-metadata findings.
- Create incremental snapshot repositories, verify their structure and bytes,
  and restore into a separately coordinated vault before publication.

See [Trash, GC, Repack & Verify](usage/trash-and-gc.md),
[Backup & Restore](usage/backup.md), and [Vault Lifecycle](usage/lifecycle.md).

## Retain permanent evidence

- Preview exactly what a permanent audited scope will protect before enabling
  an irreversible retention promise.
- Keep enrolled history sticky through moves and trash, with inherited
  protection for new descendants.
- Record supported content, topology, tag, and provenance changes in canonical
  append-only scope chains.
- Browse typed node or scope history and independently replay the complete
  authority.
- Compare current terminal heads with evidence recorded outside the vault.
- Preserve audited history through deterministic backup and restore.

See [Permanent Audited History](usage/audited-history.md).

## Place physical content deliberately

- Ingest into a fixed local filesystem primary.
- Store loose content raw or zstd-compressed and combine eligible small objects
  into sealed immutable packs.
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
- Use stable IDs, ETags, revisions, bounded pages, typed problem codes, and
  digest receipts instead of scraping human output.
- Follow NDJSON progress to one terminal result for long-running work.
- Inspect durable job IDs after an uncertain client outcome or daemon restart.
- Embed a vault through `go.kenn.io/docbank`, choosing CGO or pure-Go SQLite,
  while preserving the same exclusive ownership and content authority rules.

See [Docbank for Agents](agents.md), the
[Agent Integration Guide](agents/integration.md), and [Embed in Go](embedding.md).

## Deliberate boundaries

Docbank does not synchronize a mutable folder between devices, create public
share links, provide collaborative editing, or encrypt live secondary stores.
Text extraction is intentionally limited to formats Docbank can verify and
decode directly; OCR and semantic embeddings are not part of the current
retrieval contract. The [Roadmap](roadmap.md) describes product direction
without turning these capability pages into an implementation tracker.
