---
title: Changelog
description: Release history.
---

# Changelog

This page records user-visible changes in every tagged release. Docbank remains
pre-1.0, so public interfaces may still evolve, but vaults created by v0.9.0 and
later are within the
[storage compatibility boundary](architecture/storage.md#released-upgrades).

## [v0.15.2](https://github.com/kenn-io/docbank/releases/tag/v0.15.2) — 2026-10-09

### New features

- Search transcripts for exact recordings through `/api/v1/search`. When
  recordings share the same audio, matches identify the selected recording so
  callers open the right message. Removed or changed recordings no longer crowd
  out valid results. See [exact-recording search](agents/integration.md#search-transcripts-for-exact-recordings).
- Anonymous usage reporting now includes active session duration in four
  buckets: under 1 minute, 1 to 5 minutes, 5 to 30 minutes, and over 30 minutes.
  Time spent in background browser tabs does not count.

### Improvements

- Resolve nested document paths faster in CLI and HTTP requests.
- Load collection member pages faster, with larger gains for documents stored
  at the root.
- Expanded and reranked document searches spend less time checking result paths.
- Import identical content under different filenames faster while keeping the
  documents distinct.
- Follow a new [configuration recipe for optional EmbeddingGemma 2 text retrieval](configuration.md#optional-embeddinggemma-2-text-recipe)
  with a separately provisioned model service. Actual model inference remains
  untested.

### Bug fixes

- Open document stores created with v0.15.0 again after the v0.15.1 upgrade
  error. The upgrade keeps a `.schema-v28.bak` recovery copy and preserves
  existing processing consent and pending work. Upgrades from v0.12.0 through
  v0.14.0 also retain pending storage moves and cleanup records. See
  [released upgrades](architecture/storage.md#released-upgrades).
- Backup and restore read existing packed content larger than 512 MiB on 64-bit
  systems without rewriting it.
- Format inventory keeps metadata extraction marked as qualified when Docbank
  is built with a newer Go toolchain. Tested parsers no longer lose that status
  solely because the Go version changes.

### Contributors

Thank you to [Rod Boev (@rodboev)](https://github.com/rodboev),
[Rusty Shackleford (@salmonumbrella)](https://github.com/salmonumbrella), and
[Wes McKinney (@wesm)](https://github.com/wesm) for their contributions to v0.15.2.
See the [full contribution history](https://github.com/kenn-io/docbank/compare/v0.15.1...v0.15.2).

## [v0.15.1](https://github.com/kenn-io/docbank/releases/tag/v0.15.1) — 2026-10-08

### New features

- Browse photos from the sidebar in a grid grouped by month or capture session.
  Select photos with clicks or touch checkboxes, adjust grid density, and keep
  your selection and scroll position when switching between Documents and Photos.
- Organize photos into albums through `docbank photos albums` and HTTP. Add
  selected photos or all photos matching a query, choose covers, star albums,
  and filter photos by album with `set:`. Removing photos from an album or
  deleting an album keeps the files.
- Match recording search results to the exact saved transcript after a
  replacement or retry. Transcript responses include optional IDs for the build
  and supplied input.

### Improvements

- Open collections and page through their files faster as your document store
  grows.
- Import new files faster with fewer disk waits.
- Re-import folders and repeat uploads faster for files smaller than 4 KiB under
  the default compression policy.
- Anonymous usage reporting counts each screen once per local document store
  per UTC day for each interface, including across restarts. The privacy dialog
  explains screen reporting in the browser and terminal UI.

### Bug fixes

- Restoring a backup keeps large originals compressed instead of leaving them
  uncompressed and inflating storage use. Existing backups need no conversion.
  Restore still requires temporary space for raw and compressed copies of each
  individual file.
- The public website loads its stylesheets and JavaScript without getting stuck
  in a redirect loop.

### Contributors

Thank you to [Rod Boev (@rodboev)](https://github.com/rodboev) and
[Wes McKinney (@wesm)](https://github.com/wesm) for their contributions to v0.15.1.
See the [full contribution history](https://github.com/kenn-io/docbank/compare/v0.15.0...v0.15.1).

## [v0.15.0](https://github.com/kenn-io/docbank/releases/tag/v0.15.0) — 2026-10-05

### New features

- Process selected document versions from the web app, TUI, CLI, HTTP API, or
  embedded Go API. Review what providers receive before granting consent, follow
  job progress, and read verified renditions, the extracted Markdown DocBank
  retains. Restored vaults require fresh consent for network processing.
- Inspect processing coverage and retained text without starting work. Use
  `docbank processing coverage` for an exact version and
  `docbank rendition window` to read text in bounded sections.
- Resolve saved citations to the exact retained text through HTTP or embedded
  Go. Reads refuse unavailable or mismatched references instead of substituting
  newer text.
- Search by meaning from the web app and TUI with a configured embedding
  service. Optional ZeroEntropy or Cohere reranking reorders results using
  disclosed excerpts. Find similar documents using stored embeddings without a
  provider call.
- Save complete queries and reusable highlight sets, validate field and Boolean
  expressions, and browse frozen query results. Rows, counts, and facets stay
  fixed while documents change.
- Select document ranges, apply tags to a selection atomically, and export
  checked rows as CSV. Keyboard shortcuts support browsing, selection, and tag
  assignment.
- Preview and export exact document versions as verified ZIP bundles. The web
  app supports selections, pages, and frozen queries; CLI and MCP exports
  support up to 1,000 selected originals, including historical versions.
  Explicitly release completed jobs to free export slots.
- Recover saved export plans and inspect missing outputs through CLI and MCP.
  Existing plans remain available without creating another preview.
- Create dated search-count reports for all documents, import collections, or
  exact selected versions. Review ambiguous dates and download CSV results with
  a frozen evidence ZIP. CLI and MCP support report inspection, history, and
  download; live reports expire after 30 minutes.
- Preflight and import load-file packages, which pair document files with review
  metadata and page maps. Directory and ZIP imports preserve supplied text,
  labels, family relationships, and custodian claims.
- Export frozen document selections as DAT packages with PDFs or images, or CSV
  packages with native files. Selected-page exports retain their original
  source-page numbers.
- Publish selected pages as Bates-stamped PDFs with sequential page labels.
  Preview labels before reserving a range, and download output checked against
  the selected pages. Abandoned numbers remain unavailable for reuse.
- Import MBOX and Google Takeout ZIP archives as individual emails with
  attachments as child documents. Interrupted imports resume after the last
  saved message, and repeated messages remain separate occurrences.
- Read archived email in the web inspector with decoded or raw headers and HTML
  or plain-text bodies. Follow attachments and parent messages at their exact
  versions, including nested emails.
- Retain email PDFs with headers, full body text, inline images, and attachment
  inventories. Rendering requires opt-in Linux setup; retained PDFs remain
  downloadable after restore and on macOS or Windows. Batch exports include
  retained PDFs and attachment occurrences.
- Retain supplied WAV and MP3 recordings, import transcripts, or transcribe
  audio through configured Docling services. Read exact-version transcripts with
  speaker labels and timing, and receive matched recording intervals in search
  results.
- Associate manually imported recordings with remote references. DocBank
  recognizes Cap Cloud, registered self-hosted Cap, and Loom links; automatic
  acquisition remains unavailable. Loom imports accept exported MP4 and SRT
  files. Check a recording-link submission by operation ID without resending its
  private link.
- Group matching RAW, JPEG, and XMP files as one photo during import. Ambiguous
  groups remain available for manual pairing. Browse photos by camera, lens,
  ISO, capture date, GPS, kind, or asset ID, with RAW/JPEG pairs shown once.
- Read photo and video metadata from exact originals, including supported camera
  RAW formats and large JPEG, TIFF, and MP4 files. Photo place-name lookup runs
  offline. Embedded applications can request retained previews at 512px, 2560px,
  or 4096px for supported formats.
- Create, rename, retire, merge, and split people through CLI and API, and read
  person records through MCP. Backups preserve people, custodian information,
  and decisions; restore rebuilds document–person links.
- Create, verify, restore, and clean up backups from embedded Go applications
  without a daemon. Include application-owned recovery files in the same
  snapshot, restore after losing the original vault, and select the SQLite
  driver.
- Back up and restore objects larger than 4 GiB, including embedded recovery
  files. New large-object snapshots require reader version 5; recovery files
  over 64 MiB require reader version 6. Upgrade shared repository readers before
  creating those snapshots.
- Choose import files with include and exclude glob patterns. Use
  `docbank add --replace` to update an existing document's history. Existing `--exclude`
  values now use glob matching.
- Inspect locally extracted source metadata and available MD5 fingerprints
  through document details. HTTP and embedded Go callers can append or correct
  provenance after import. Embedded callers can also make content replacement
  conditional on the revision they read.
- Connect local agents with `docbank mcp` to browse, search, and read exact
  versions through the daemon. Processing, report, export, and package writes
  require their respective opt-in flags.
- Extract EPUB text locally in the daemon. Go integrations also gain local and
  hosted extraction providers and embedding adapters for OpenAI, Cohere, Gemini,
  ZeroEntropy, and operator-run compatible services. Availability depends on the
  interface and configured provider.
- Process additional formats through Mistral in Go applications, including PPTX,
  fourteen text formats, and Office files converted locally to PDF. Replace v3
  capability manifests with freshly probed v4 manifests before processing any
  format. Text uploads use byte limits without a pre-upload page or spending
  bound.
- Verify Msgvault transfer directories, ZIPs, and legacy JSONL exports locally
  with `docbank transfer verify`.

### Improvements

- Navigate the web app through a labeled sidebar, clickable folder breadcrumbs,
  and a slide-out menu on narrow screens. File icons and readable type names
  replace raw media types in the list.
- Find related tags under shared slash-separated prefixes. Tags keep consistent
  colors across the catalog, filters, and assignments, including after renaming.
- Import large directories and upload batches faster, without imports slowing
  progressively as sibling counts grow. CLI commands also start faster.
- Search large filename sets and return large result pages faster. HTTP and
  embedded Go callers can set `content_first` to place document and transcript
  matches ahead of filename-only matches.
- Browse large directories and heavily used tags faster. Document details,
  downloads, vault information, and storage reports also take less time.
- Inspect format capabilities with `docbank formats`, including distinctions
  between recognition, retention, extraction, and provider qualification. On
  macOS, import preflight counts cloud placeholders and failed downloads explain
  how to make files available locally.
- Open vaults when the account home is unwritable by setting `DOCBANK_LOCK_DIR`
  to a writable absolute directory. Processes accessing overlapping vaults must
  share that setting.
- Report anonymous daemon activity and web app opens by default. Events exclude
  document content, filenames, paths, tags, and searches. Set
  `DOCBANK_TELEMETRY_ENABLED=0` and restart the daemon to opt out.
- Refuse private-file writes on Unix locations where DocBank cannot verify
  private permissions, including NFS and FUSE mounts. This affects restored
  configuration and web launch files.

### Bug fixes

- Identify JPEGs from their contents even when a Windows registry change assigns
  the `.jpg` extension the wrong type. YAML and TeX files also count as
  searchable text. Already stored files retain their existing types.
- Keep exclusively held files pending in Windows watched folders until they
  become readable, then wait for a fresh settle window. Retry when a checked
  file changes into a directory before opening.
- Keep `docbank info` and `docbank storage status` readable when packing removes
  loose files during a status scan.

### Contributors

Thank you to [Joi Ito (@Joi)](https://github.com/Joi),
[Marius van Niekerk (@mariusvniekerk)](https://github.com/mariusvniekerk),
[Rod Boev (@rodboev)](https://github.com/rodboev),
[Rusty Shackleford (@salmonumbrella)](https://github.com/salmonumbrella), and
[Wes McKinney (@wesm)](https://github.com/wesm) for their contributions to v0.15.0.

See the [full release contribution history](https://github.com/kenn-io/docbank/compare/v0.14.0...v0.15.0).

## [v0.14.0](https://github.com/kenn-io/docbank/releases/tag/v0.14.0) — 2026-08-23

### New features

- Add reusable embedding plans for Go applications.

### Improvements

- Adopt Go 1.27 and JSON v2, including the CI lint toolchain.
- Probe interleaved documents per format and mixed batches.
- Add media detection and fail-closed Voyage multimodal embedding contracts.
- Expose Mistral integration contracts.

## [v0.13.0](https://github.com/kenn-io/docbank/tree/v0.13.0) — 2026-08-18

### New features

- Add storage-neutral Go packages for deterministic document normalization,
  including normalized units, headings, spans, chunks, and checksums.
- Add bounded Mistral OCR processing with format detection, private staging,
  capability manifests, authenticated probes, retry handling, and fail-closed
  upload authorization.

### Breaking changes

- Move the public SQLite driver imports from `go.kenn.io/docbank/pkg/sqlite`
  to `go.kenn.io/docbank/sqlite`, including the `mattn` and `modernc`
  subpackages. Embedded applications must update these imports when upgrading;
  driver APIs and behavior are unchanged.

## [v0.12.0](https://github.com/kenn-io/docbank/tree/v0.12.0) — 2026-08-01

### New features

- Add verified multi-store storage with configurable local filesystem and
  S3-compatible blob placement.
- Upload new documents and download current or retained historical versions
  through the web application with integrity verification.
- Inspect document history, provenance, permanent audit records, background
  jobs, backups, and physical storage in the web application.
- Browse tags, assign them to documents, and manage tag definitions in the web
  application.
- Manage recoverable trash in the web application and TUI.
- Inspect tags, background jobs, backups, and physical storage in the TUI.

### Improvements

- Report packed storage left behind after a restore so operators can see what
  still awaits reclamation.

## [v0.11.0](https://github.com/kenn-io/docbank/tree/v0.11.0) — 2026-07-23

### New features

- Open and browse a vault in a local web application.
- Explore documents and audited history in a read-only analytical TUI.
- Schedule bounded automatic vault packing.
- Manage multiple permanent audit scopes.
- Inspect nodes, create virtual directories, and download documents from the
  CLI.
- View document provenance through the CLI and embedded Go API.
- Search by directory, tag, media type, and modification time.
- Access physical write receipts through the embedded Go API.

### Improvements

- Reset vaults fail closed when storage safety cannot be confirmed.
- Record shared tag changes across all applicable audit scopes.
- Report effective watched-inbox status.
- Delay watched-file ingestion until files are old enough to be stable.
- Publish verified downloads atomically without leaving partial files.
- Report maintenance lock contention immediately.
- Display concise human-readable timestamps.

## [v0.10.1](https://github.com/kenn-io/docbank/tree/v0.10.1) — 2026-07-22

### New features

- Browse permanent audit history across an entire scope.
- Reorganize multiple documents atomically, so every move succeeds or none do.
- View the currently selected vault with `docbank info`.

### Improvements

- Show physical byte totals in the loose-blob packing backlog.
- Preserve operator-approved Markdown in annotated release tags and make
  release creation an explicit, human-reviewed workflow.

## [v0.10.0](https://github.com/kenn-io/docbank/tree/v0.10.0) — 2026-07-21

### New features

- Add bounded embedded APIs for document ingestion, retrieval, traversal, and
  maintenance.
- Add immutable embedded content-addressed storage APIs.
- Accept stable node IDs as reusable CLI selectors across document operations.

### Improvements

- Compress worthwhile new loose blobs while preserving their logical SHA-256
  identities and verified-read contract.
- Bound `tree` output and report truncation so large vaults remain manageable
  for people and agents.

## [v0.9.0](https://github.com/kenn-io/docbank/tree/v0.9.0) — 2026-07-19

### New features

- Search inside verified UTF-8 plain text, Markdown, JSON, and JSONL content,
  not only document names and metadata.

### Project changes

- License Docbank under the Apache License 2.0.
- Establish v0.9.0 as the first released storage-compatibility boundary.

## [v0.8.1](https://github.com/kenn-io/docbank/tree/v0.8.1) — 2026-07-19

- Publish a patch-level conformance-bootstrap release from the same source as
  v0.8.0. This gave updater checks two releases that both implement
  `docbank version`, allowing the complete update path to be exercised.

## [v0.8.0](https://github.com/kenn-io/docbank/tree/v0.8.0) — 2026-07-19

- Make CLI and daemon version reporting explicit and consistent through
  `docbank version`, `docbank daemon version`, and compatible-daemon checks.

## [v0.7.0](https://github.com/kenn-io/docbank/tree/v0.7.0) — 2026-07-19

- Automatically ingest stable regular files from configured watched inboxes.
- Preserve source identity without modifying the watched files, and append
  later byte changes as immutable versions of the same document.
- Expose watcher lifecycle and failures through supervised daemon jobs.

## [v0.6.0](https://github.com/kenn-io/docbank/tree/v0.6.0) — 2026-07-19

### Permanent audited history

- Permanently enroll a directory tree in auditing with a preview-first,
  acknowledged workflow.
- Record and browse audited filesystem ingest, content replacement and revert,
  moves and renames, trash and restore, and tag creation, assignment, rename,
  and deletion.
- Independently replay protected authority with `docbank audit verify` and
  prove that current chains extend a separately recorded evidence report.
- Preserve and validate audited history through backup, JSONL export, and
  restore.

### Automation

- Return structured receipts for virtual-tree mutations and typed JSON from
  established read commands.
- Publish stable CLI exit codes for usage errors, missing objects, stale state,
  busy resources, and integrity findings.

## [v0.5.0](https://github.com/kenn-io/docbank/tree/v0.5.0) — 2026-07-17

- Expose the embedded Go API directly from the module root.
- Create audit authority through production enrollment.
- Record audited content replacements and inherited audited node creation.
- Classify unavailable embedded content separately during verification, and
  report missing or size-mismatched bytes as `ErrContentUnavailable`.

## [v0.4.0](https://github.com/kenn-io/docbank/tree/v0.4.0) — 2026-07-17

- Page immutable content history and open any historical version through the
  embedded API's verified-read contract.
- Persist the initial audit enrollment authority for durable integrity
  verification.
- Support portable JSON representations of audit authority.
- Stabilize provenance as audit authority with canonical record shapes and
  deterministic collection ordering.

## [v0.3.0](https://github.com/kenn-io/docbank/tree/v0.3.0) — 2026-07-16

### Document workflows

- Edit documents interactively with verified, versioned content replacement.
- List, replace, revert, and explicitly prune immutable content versions.
- Add, remove, list, and use stable user-facing tags.
- Find documents by authoritative content hash.
- Preflight large imports without opening content, stream ingest progress, and
  honor explicitly named cloud-storage roots.

### Embedded and daemon operation

- Embed independently rooted vaults in Go applications with selectable SQLite
  drivers, packing, and bounded child traversal.
- Preserve stable vault and document identities across ingest and metadata
  operations.
- Supervise daemon background jobs and expose their current state.

## [v0.2.1](https://github.com/kenn-io/docbank/tree/v0.2.1) — 2026-07-13

This recovery release carried the v0.2.0 capabilities through a validated
six-platform release build after v0.2.0's tag did not produce a published
GitHub release.

- Create, list, verify, and restore incremental backups with portable JSONL
  metadata.
- Inspect packed storage, pack loose objects, reclaim sparse packs, and recover
  safely from interrupted pack retirement.
- Upload remote content with digest checks and verify content identity over
  HTTP.
- Run the complete Docbank CLI and daemon lifecycle natively on Windows.
- Stream verified loose and packed content within separate resource limits.
- Install checksum-verified archives on Linux, macOS, and Windows, on both
  amd64 and arm64.
- Validate all release builders before tagging and allow a recovery release to
  span an intervening unpublished tag.

## [v0.2.0](https://github.com/kenn-io/docbank/tree/v0.2.0) — 2026-07-13

### Backup and packed storage

- Create daemon-owned incremental snapshots with authoritative JSONL metadata.
- Verify backup repositories and restore through a confined, exclusive
  recovery workflow.
- Inspect packed storage, pack loose objects, and reclaim sparse packs with
  explicit maintenance commands.
- Preserve recoverability when an obsolete pack cannot be retired immediately.

### Portable clients and releases

- Upload remote content with digest checking and retrieve content through
  verifiable, bounded streaming reads.
- Run the complete daemon, CLI, lock, backup, restore, and self-update lifecycle
  with native Windows behavior.
- Build precompiled archives and installers for Linux, macOS, and Windows on
  amd64 and arm64.

## [v0.1.0](https://github.com/kenn-io/docbank/tree/v0.1.0) — 2026-07-09

### Core vault

- Store documents in a virtual SQLite/FTS5 tree backed by a durable
  content-addressed blob store.
- Import directory trees idempotently, preserve provenance, and suffix genuine
  filename collisions without modifying source files.
- Browse, search, move, trash, restore, empty trash, garbage-collect, and
  verify documents from the CLI.
- Keep permanent deletion explicit: `trash empty` and `gc` preview unless the
  operator authorizes the mutation.

### Daemon-first operation

- Run every standalone CLI data command through an auto-started,
  authenticated, loopback-only daemon rather than opening the vault directly.
- Expose the versioned HTTP API for tree, content, search, ingest, mutation,
  reclamation, and verification workflows.
- Coordinate daemon discovery, idle shutdown, and exclusive vault ownership.
- Configure server and web behavior in `config.toml`, generate an offline
  OpenAPI document, and self-update from verified GitHub releases.
- Bound search with `--limit` and report when additional matches exist.
- Publish Linux amd64/arm64 and macOS arm64 archives with SHA-256 checksums.
