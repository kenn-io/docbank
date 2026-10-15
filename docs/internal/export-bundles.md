# Export bundle design

Native exports freeze exact document versions and output receipts before a
worker writes an archive. The [export guide](../usage/export-bundles.md) owns
commands, HTTP operations, formats, limits, and recovery. The
[MCP guide](../usage/mcp.md#native-export-jobs) owns tool arguments and delivery
results. This page explains implementation boundaries and their rationale.

## Ownership

| Component | Responsibility |
| --- | --- |
| `document/bundle` | Wire types, limits, archive structure, and independent verification |
| `internal/store/export_*` | Frozen sources and plans, admission, ownership, replay, and retention |
| `internal/exporter` | Worker claims, private archives, leases, cleanup, and explicit release |
| `internal/api/routes_exports.go` | Authenticated operations and domain-error mapping |
| `internal/daemonconn` | Job-bound ticket download and archive verification |
| `cmd/docbank`, `internal/mcp` | Client input, output, and local publication policy |
| `internal/filepublish` | Private staging and platform-specific publication |

CLI and MCP use the daemon connection and generated client, never direct vault
access. API-key callers share the `master` export owner; browser sessions have
separate owners. Restarting an MCP process grants no independent allowance.

## Exact selection and preview

MCP previews create an explicit source, then an originals-only plan. CLI previews also accept a photo rendering profile and a Photos scope.
They accept up to 1,000 node/version pairs without expanding folders,
attachments, or families. Multiple retained versions of one node are valid;
duplicate pairs are not. Historical versions need not be current, but the node
must be live and untrashed at source sealing and plan creation. Hash, size, and
any positive revision precondition must agree at both boundaries.

Plans freeze current names and paths beside the selected version. They neither
reconstruct historical names nor substitute newer content. A published
attachment is an ordinary document when explicitly selected. Omitting it
excludes its separate output; an original EML still preserves every MIME part
inside its bytes. Export is not redaction.

Client validation prevents malformed identities, duplicates, negative values,
and excessive total bytes from reserving source records. Live identity checks
remain authoritative in the daemon; failed checks can retain a failed source
reservation until cleanup. CLI's bounded JSON-v2 reader permits omitted or null
numeric fields to decode as zero. MCP requires `size` and rejects explicit
nulls through closed schemas. Shared value checks must preserve that distinction.

MCP gets original hashes through `list_document_versions`; catalog projections
do not contain them. Selection requires a version-list call per node. Preview
does not hide that cost with automatic per-member reads.

## Replay and frozen authority

Source and plan creation are separate transactions. A plan failure can leave a
sealed source for bounded replay or cleanup. Clients do not compensate by
deleting it or inventing replacement operation IDs.

Replay compares canonical requests, including member-array order. Reordering
the same members under one source operation ID conflicts. A failed or resolving
source cannot be replayed into success. Source and plan replay retain their
original admission deadlines.

Job admission checks for a matching retained job before checking new-plan
admission. Clients send the reviewed fingerprint without refreshing it or
rejecting replay using their own clock. After an uncertain start, inspect the
known job ID or explicitly repeat the identical start request. Preview's replay
window can expire while the job remains accessible.

Preview restrictions do not govern every accessible plan or job. Lifecycle
commands and retained-plan reads accept richer same-owner plans with additional
roles, larger selections, and volumes.

## Admission, retention, and release

The canonical plan's `expires_at` is its admission deadline. Separate stored
retention controls reads and protects required authority. Jobs can extend
retention without changing the header or fingerprint. Reads extend neither
deadline; a readable plan need not admit a new job.

Every retained job counts toward admission, including terminal jobs. CLI, MCP,
and other API-key callers share two owner slots. Sources and plans have a
separate global allowance; releasing a job does not immediately remove its
original admission records.

Explicit release preserves repeatable downloads until the operator reclaims
finished work. Automatic release would discard recovery authority when local
publication or response delivery is uncertain. Raising owner capacity alone
would leave global capacity and archive cleanup unresolved.

`Worker.Release` takes the worker mutex before the operation gate, matching
cleanup's lock order. The store authorizes the exact owner and terminal job
before archive removal. The mutex excludes new leases while release checks
existing leases and completes its transaction. Unused tickets and active
downloads prevent release with `export_retained`; release neither revokes them
nor waits for them. A completed download can briefly retain its server lease.

Release removes the archive before deleting the row and reducing source/plan
retention. A filesystem failure leaves the row. A later transaction failure
can leave a row without an archive; removal tolerates that state on retry.
Download then returns `export_expired` with a release-retry hint. Retention
reduction preserves original admission deadlines and other jobs' needs.

Release also deletes the job's replay authority. Do not reuse its operation
ID: an eligible plan could admit fresh work. Local downloads and original
documents are unaffected.

## Retained-plan inspection

Plan reads return the stored header. Problem reads scan frozen document rows,
validate them, count all problems, and return one page. They do not inspect
current heads or processing coverage.

`after` counts skipped problems in one immutable plan. Each call returns at
most 50 items; `next == 0` is terminal. One document can have several unavailable
roles, and an incomplete attachment inventory adds a problem for each requested
attachment role. Each read can scan the complete plan; bounded output does not
mean constant-time paging. The complete encoded page must fit within 64 KiB.
The store refuses an oversized page rather than shortening it. HTTP, CLI, and
MCP cannot request a smaller page.

MCP inspection uses the full retained-header schema with optional fields, not
preview's originals-only schema. Query fingerprints include the `sha256:`
prefix. Real query, saved-query, and snapshot fixtures protect this distinction.
Shared MCP export errors are operation-neutral because the dispatcher receives
no tool name. Recovery advice belongs beside each operation: a plan read does
not imply a job exists, and release cannot shrink an oversized problem page.

## Verified local publication

`Connection.DownloadExportArchiveTo` binds the requested job, completed job
receipt, ticket receipt, and independently verified archive receipt. The
expected fingerprint comes from the job, outside the downloaded ZIP.

The daemon holds an archive lease while reverifying the archive, then issues
a download ticket. The client uses the same daemon connection, accepts only
the existing relative ticket route, bounds
the transfer by receipt size plus one byte, and runs `bundle.Verify`. It compares
the complete verified receipt before returning. Ticket URLs never become result
fields or URL-wrapped diagnostics.

Adapters write to a private seekable stage beside the destination, then sync,
close, and publish through `filepublish`. Before publication, failure preserves
the destination. Without overwrite permission, a destination created during
transfer also prevents publication.

Publication and later durability, cleanup, or response delivery are separate
outcomes. Preserve the publication boolean on error; never remove a published
verified file as recovery. MCP returns `published` or
`published_durability_unknown` with the receipt and a separate cleanup-failure
flag. A failed response can still leave a complete file visible.

Downloads never cancel, release, or recreate jobs. A deliberate retry gets a
fresh ticket and transfers the complete archive again.

## MCP execution boundaries

Preview retains authority and can prevent pruning, so it requires the separate
`--allow-export-writes` startup opt-in. Inspection remains available without it.
The flag enables local filesystem writes and export work for that process; it
does not establish separate per-client ownership.

Reads use `daemonRead`, which retries once only after transport failure before
a response begins. Writes use the single-attempt helper and translate uncertain
outcomes to the export domain. A stale connection after daemon restart can
produce that result without proving dispatch occurred. Download has its own
single-attempt connection and publication path.

Classify integrity, unfinished-job conflicts, and file errors before generic
daemon sanitization loses their causes. Client messages stay bounded; detailed
local causes belong in the operator log. MCP HTTP's two-minute deadline covers
daemon verification, transfer, and local verification. The archive ceiling does
not guarantee delivery within it; CLI or stdio can access the same retained job.

## Qualification boundaries

The real-daemon CLI/MCP suites exercise exact originals, historical versions,
inspection paging, replay, shared capacity, release, and verified publication.
Store and worker tests cover retention clocks, lease races, and partial release.

`TestReportPDFWorkflow` and `TestReportPDFRestore` compose selected reports,
date review, original export, and restore before and after selected documents change.
They use synthetic PDFs with published evidence; they do not qualify fresh
extraction or provider accuracy. Reports require current selected versions;
exports can use retained historical versions after replacement. Restore does
not restore daemon processing configuration.

`TestReportFamilyExplicitOriginalExports` exercises each client's own admission,
download, and release with explicit parent/child selections. It compares exact
identities and original bytes, including omitted parts inside EMLs. Related
family tests cover [selected report counts and relationships](search-reports.md#membership-and-families).

`frontend/screenshots/report-export.screenshot.ts` adds one real-browser case
that drives the actual embedded web application instead of mocked state.
`make report-export-browser-test` (`frontend/screenshots/report-export.run.mjs`)
builds separate CGO and `CGO_ENABLED=0` branch binaries, launches each through
`DOCBANK_SCREENSHOT_BINARY` with no fallback to an installed binary, and
requires exactly one passing, unskipped Chromium run per mode. The case creates
a synthetic vault and four single-document fixtures through the branch CLI,
waits for search readiness, then uses real UI controls to: get a strict
`incomplete_coverage` refusal for a missing-text-only selection with no history
row added; export exactly three selected original versions and verify the
downloaded ZIP's size and SHA-256 against the completed job receipt; create a
`needs_review` report over all three documents, resolve the one ambiguous
content date through **Review dates**, and read the completed child's
unchanged parent live through a same-origin fetch carrying
`X-Docbank-Web-Session`; and confirm the completed report's evidence ZIP and
CSV survive a later source replacement by byte equality, while reusing that
selection as a draft after replacement returns HTTP 409
`report_selection_changed` with no new history row. Independent Python
standard-library readers re-verify the ZIP/JSON/JSONL/CSV evidence and the
native bundle's checksums, member hashes, and plan fingerprint both before and
after stopping the daemon, and confirm the stopped database, opened read-only
and immutable, has zero `rendition_jobs` and `embedding_jobs` rows.
`DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR` gates and captures the case so ordinary
documentation screenshot generation skips it, and a dedicated
`report-export-browser` Linux Chromium job in `.github/workflows/ci.yml` runs
it. This Linux-only job does not establish native macOS/Windows browser
coverage, family recovery through backup and maintenance, or a general
browser-test framework; those remain separate qualification boundaries.

Keep literal expected counts and independent byte comparisons alongside packet
and archive verification. Internal consistency does not establish source
authenticity. Fixtures use isolated synthetic vaults and close setup stores
before daemon ownership. These tests do not qualify recursive attachment
export or family maintenance and restore. Local execution provides no evidence
of an untested operating system.

## Rendered photos

Photo source resolution extends `ListPhotoAssets`, including its complete query population and Hidden unlock. Preparation captures the exact display versions, asset/file/node revisions, authored decisions, and keyword values. The renderer reads verified originals sequentially, collects container inspection and metadata once and shares EXIF interpretation and RAW preview selection with visual previews, and writes temporary outputs before entering publication. Publication writes up to 1 GiB of staged output into blob storage under the mutation lease. Sealing then rechecks those inputs and records durable blob receipts and existing export role roots in a database transaction. A concurrent edit refuses the plan.

`photo_rendered` recipes bind the source, output dimensions, embedded-preview origin, and profile. The ZIP worker and independent archive verifier consume those receipts through the existing export flow. JPEG and PNG outputs use a bounded TIFF directory writer and an XMP merge. Rebuilding reachable EXIF directories removes GPS payloads and stale thumbnails. Only confirmed authored fields overwrite source decisions, including empty strings and zero ratings. Catalog tags replace embedded keywords only when at least one tag exists. Otherwise, a bounded IPTC reader transfers Photoshop resource 0x0404 keywords from JPEG APP13 and TIFF 0x8649, plus TIFF 0x83bb keywords into XMP, combining them with existing plain subjects while preserving unrelated and hierarchical properties. IPTC supports undeclared ASCII, declared UTF-8, and declared Latin-1; malformed records or unsupported keyword encodings refuse metadata-enabled preparation with a metadata-off remedy. Preparation refuses extended XMP when metadata is enabled; turn image metadata off to export that photo. Full preparation inputs stay internal; sealing compares their canonical JSON with current authority. ICC profiles travel with pixels, including metadata-off exports. Rendering runs outside the mutation gate; admission caps members, source bytes, pixels per image and output bytes before publication. Exported orientations normalize to 1 after pixel transforms.

Rendered copies are temporary export artifacts. Their role roots protect them from garbage collection while retained. Portable backups include retained plans and their generated output.

Preparation admits one photo export per vault, up to 16 photos, 100 million pixels per image, 512 MiB of source bytes, and 1 GiB of encoded output. The output limit counts delivered bytes. At 100 million pixels, a 16-bit decoded image can occupy 800 MB, with up to 400 MB each for resize, orientation, and JPEG matte buffers, alongside source bytes, encoded output, and metadata rewrite buffers. Peak process memory can exceed several GiB; the 1 GiB output cap is not a heap limit.
