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

Keep literal expected counts and independent byte comparisons alongside packet
and archive verification. Internal consistency does not establish source
authenticity. Fixtures use isolated synthetic vaults and close setup stores
before daemon ownership. These tests do not qualify recursive attachment export,
browser workflows, or family maintenance and restore. Local execution provides
no evidence of an untested operating system.

## Rendered photos

Photo source resolution extends `ListPhotoAssets`, including its complete query population and Hidden unlock. Preparation captures the exact display versions, asset/file/node revisions, authored decisions, and keyword values. The renderer reads verified originals sequentially, shares image inspection and RAW preview selection with visual previews, and writes temporary outputs before entering publication. Sealing checks those inputs again and publishes durable blob receipts and existing export role roots in one short transaction. A concurrent edit refuses the plan.

`photo_rendered` recipes bind the source, frozen input, output dimensions, embedded-preview origin, and profile. The ZIP worker and independent archive verifier consume those receipts through the existing export flow. JPEG and PNG outputs use a bounded TIFF directory writer and an XMP merge. Rebuilding reachable EXIF directories removes GPS payloads and stale thumbnails. Authored XMP values overwrite source decisions, including empty strings and zero ratings. Exported orientations normalize to 1 after pixel transforms.

Rendered copies are temporary export artifacts. Their role roots protect them from garbage collection while retained, but authority backups exclude the photo plan and its generated output. Ordinary source versions retain their existing backup authority.
