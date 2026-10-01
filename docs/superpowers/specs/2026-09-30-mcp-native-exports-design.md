# Native exports through local MCP

Status: implemented. Adversarial
review of `b7a70cc3` found no High or Medium findings. The contract below includes
the accepted Low refinements and the reason for deferring batch catalog hashes.

Source baseline: `5f36ee4ec6245a1e558cac2de36218b52700a66f`, after
[CLI exports and explicit release](https://github.com/kenn-io/docbank/pull/740).
This is the next bounded increment of
[the contribution reset](https://github.com/kenn-io/docbank/issues/719).
The [CLI export design](2026-09-29-cli-native-exports-design.md) supplies the
existing engine contract; this document specifies the MCP adapter.

## Outcome and scope

A local operator can ask an MCP client to select exact document versions,
review an originals-only export, start it, inspect its progress, and save a
verified ZIP on the machine running Docbank. The operator explicitly releases
finished jobs to reclaim capacity. The client can do this entirely through MCP.

Use the existing daemon, native bundle format, job ownership, verifier, and
file publication helpers. Selection is 1–1,000 explicit original versions,
including retained historical versions. A preview neither starts processing
nor adds renditions, attachments, or other related documents. Original contents
are exported without redaction or sanitization; tool descriptions say so.

This increment adds six tools, one startup flag, and the missing original hash
in the existing version-list result. It does not add report tools, chunked
selection uploads, per-file plan inspection, subscriptions, remote destinations,
MCP archive resources, base64/chunk streaming, provider work, or a new registry.
No database, archive, daemon HTTP API, dependency, or retention-policy change is
required. Original bytes travel from the daemon to a local staging file, never
inside an MCP response.

## Tools and operator control

Add `--allow-export-writes` to `docbank mcp`, default false, and corresponding
`ServerOptions.AllowExportWrites`. Apply it identically to stdio and HTTP.
The catalog is fixed at startup. Neither `--allow-package-writes` nor
`--allow-processing` nor `--allow-photo-edits` enables these export writes.

| Tool | Input | Success payload, before common cache fields | Available by default |
| --- | --- | --- | --- |
| `preview_export` | `source_operation_id`, `plan_operation_id`, `members` | `{ "plan": <bundle.Plan> }` | No |
| `start_export` | `operation_id`, `plan_id`, `fingerprint` | `{ "job": <bundle.Job> }` | No |
| `get_export_status` | `job_id` | `{ "job": <bundle.Job> }` | Yes |
| `cancel_export` | `job_id` | `{ "job_id": "…", "accepted": true }` | No |
| `download_export` | `job_id`, `destination_path`, optional `overwrite` | `{ "job_id": "…", "destination_path": "…", "receipt": <bundle.Receipt>, "state": "published", "cleanup_failed": false }` | No |
| `release_export` | `job_id` | `{ "job_id": "…", "released": true }` | No |

Preview creates retained source and plan records and can prevent pruning, so it
is a write despite its name. A disabled tool is absent from discovery and cannot
be called by name. Read-only status does not acquire a download ticket.
Use read-only annotations only for status; mark cancel, download, and release
destructive because they end active work, can replace a destination, or discard
a retained archive. Preview and start are non-destructive.
Use conservative false idempotence hints for the new
writes; describe explicit replay separately rather than promising indefinite
deduplication. Status retains the existing read-tool annotations.

Every successful result uses the existing bounded structured-content plus JSON
text convention, with top-level `ttlMs: 0` and `cacheScope: "private"`. Nested
plan, job, and receipt values use the existing bundle JSON field names and
omission rules, with closed output schemas; do not return member arrays or ZIP
bytes. A failed job is a successful status observation containing that job's
state and failure code, not a tool execution error. No tool polls or waits for
job completion.

The process flag is the operator's opt-in to retained export work and local
filesystem writes. Tool descriptions tell the client to review membership and
destination with its operator before starting or downloading. There is no new
consent token, private session, or claimed per-client authorization boundary.

## Obtaining an exact selection

The current MCP document and version results omit original-file hashes.
`list_document_versions` already fetches `api.ContentVersion`, which has
`BlobHash`; add required `blob_hash` (64 lowercase hex characters) to each
version item and its output schema. Preserve existing fields, ordering, paging,
and the 250-item page limit. This addition is available without write flags.
Do not add another metadata endpoint or fetch original bytes to compute hashes.

This means one version-list call per selected node, including current versions
(and further pages if needed). A 500-node selection needs 500 such calls.
Batch catalog hashes are deferred: both `store.DocumentSummary` and
`api.DocumentSummary` omit them, so adding hashes to `list_documents` also changes
the store projection and HTTP catalog contract. Keep this adapter increment
bounded; do not hide that cost with automatic per-item lookups inside a tool.

An agent discovers nodes using existing list/search tools, then lists versions
for each chosen node. For preview it copies `node_id`, maps
`content_version_id` to `version_id`, maps `blob_hash` to `sha256`, and copies
`size`. The version's historical `NodeRevision` is not a current node revision
precondition. Do not expose or silently use it for that purpose.

All new tool inputs are closed objects. IDs are canonical lowercase UUIDv4s and
hashes are 64 lowercase hexadecimal characters. Schema validation and existing
strict argument decoding reject unknown fields and wrong types before daemon
work. Preview requires both operation IDs and a non-null `members` array of
1–1,000 non-null objects, each with required `node_id`, `version_id`, `sha256`,
and `size`; `revision` is optional. Node IDs are positive signed 64-bit integers;
size and revision are nonnegative signed 64-bit integers. Omitted revision is
zero and adds no precondition. Explicit null scalar values are invalid MCP
arguments. Unlike the CLI JSON reader, MCP requires size to be present.

Reject duplicate `(node_id, version_id)` pairs and a combined original size over
50 GiB locally, using overflow-safe arithmetic. Distinct retained versions of
one node are allowed and count separately. Reuse `uuidSchema`, `sha256Schema`,
`daemonconn.IsCanonicalUUIDv4`, and `canonical.IsSHA256Hex` where applicable;
do not add another UUID or hash parser. Local checks avoid simple malformed
requests reserving global source slots; the daemon remains authoritative for
live identity and availability checks. Sharing the CLI's value validation is
permitted as a small extraction, without changing its decoding/usage contract.

The daemon checks that each node is live and untrashed and that the retained
version, hash, size, and any positive revision agree at source sealing and plan
creation. It does not require that version to remain current. It freezes the
node's name and path at plan creation, not the version's historical location.
The MCP adapter never refreshes a selected identity, drops a failed member,
sorts the submitted array, or substitutes another selection.

## Preview, execution, and explicit release

`preview_export` follows the existing CLI's two calls on one acquired daemon
connection: `CreateExportSource` with `kind: "explicit"`, the source operation
ID and exact members; then `CreateExportPlan` with the plan operation ID,
returned source ID/member hash, and `roles: [{"role":"original"}]`.
Leave other policies unset. Return the full bounded `bundle.Plan` header.
Its totals and byte counts describe planned contents, not final ZIP size.
The input is the member list; this tool is not a per-document receipt listing.

Source creation and plan creation are separate transactions. If plan creation
fails, keep the source for ordinary cleanup or explicit replay. Do not compensate
by deleting authority or inventing new IDs. The caller supplies all three
operation IDs (source, plan, job) before making the corresponding requests.

`start_export` sends the supplied `bundle.JobRequest` directly to
`CreateExportJob`. The operation ID becomes the job ID. Do not refresh the
fingerprint or reject a replay locally because the plan deadline passed:
the daemon checks for a retained matching job before new admission.
`get_export_status` reads that ID once through `GetExportJob`.

`cancel_export` calls `CancelExportJob` once. Success means the request was
accepted: failed/already canceled jobs are successful no-ops, completed jobs
conflict, and an active job is canceled by the engine. Do not fabricate a new
state or require a follow-up GET for success.

`release_export` calls the merged `ReleaseExportJob` operation once. Only the
caller's completed, failed, or canceled jobs, including expired rows awaiting
cleanup, can be released. Active jobs conflict; unknown or other-owner IDs are
not found. Any ticket or active download lease yields `export_retained`, without
waiting or revoking it. Even a release immediately after a successful download
may need retry while the server finishes closing its lease. An unused ticket
can retain the archive for its existing two-minute lifetime.

Release removes the archive and job, frees its retained-job slot, and reduces
plan/source retention according to the existing engine. It does not remove
originals or local downloaded files, or promise immediate source/plan deletion.
Repeated release returns not found. A transaction failure after archive removal
can leave a job row: retry release; downloading the missing archive reports
`export_expired`. Download never releases automatically, including after local
publication or response failures.

Lifecycle tools may operate on any accessible existing plan/job, including ones
created by the CLI or HTTP API with larger selections or additional roles.
The originals-only and 1,000-member limits govern this preview tool, not all jobs
it can inspect or download. The supplied plan fingerprint is always required to
start; a known job ID is sufficient for status, cancellation, download, release.

## Replay, ownership, and capacity

MCP uses the existing daemon API-key owner, `master`, shared with the CLI and
other non-browser API-key callers. An MCP process restart does not create a new
owner or recover another owner's browser jobs. IDs and results contain no
ownership token; daemon authorization governs every operation.

Eligible preview replays use the exact same request and IDs. Array order affects
the canonical source request digest, so reordering with the same ID conflicts.
A resolving/failed source cannot be replayed into success. Source and plan replay
expire after their original ten-minute admission windows even when a job extends
their retention. After a delayed/lost start response, recover through the known
job ID or the same start request, not by replaying preview.

No write callback is retried automatically. Reuse `daemonProcessingStart` for
preview, start, cancel, and release; translate `errProcessingOutcomeUnknown` to
an export-specific error at the call site, as the existing Bates tools do.
Its processing-specific text must not become an export response. Do not extract
or rename the shared helper for this increment.
Do not wrap preview, start, cancel, release, or download in `daemonRead`,
which can replay a callback after an initial transport failure. Status can use
that existing read helper. Discard failed connections under the existing lease
rules; do not rerun the operation after reconnecting.

For preview, start, cancel, or release, when a daemon response is lost through
cancellation, transport failure, or response-decode failure after an attempt,
report `export_outcome_unknown` if an
MCP response can still be delivered. The message instructs the caller to inspect
its known IDs. For preview, unchanged replay is bounded by source/plan expiry;
for start, use status or identical start; for cancel, inspect status; for release,
not-found establishes only that the job is no longer accessible. A released job
ID must never be reused: its deleted replay authority can permit a fresh job
while the plan remains eligible. These are explicit caller recovery actions.
The first write through a cached connection after a daemon restart can report
unknown outcome even when no request reached the new daemon. The helper does
not distinguish that case or reconnect and replay the write. Explain this
conservative result in the MCP guide and use the same ID-based recovery.

Keep all engine limits: 32 global combined source/plan records, eight global
retained jobs, two retained jobs per owner regardless of state, ten-minute
source/plan admission, two-hour job deadline, 24-hour completed-job retention,
and ten-minute failed/canceled retention. Limits can remain occupied beyond
expiry until cleanup. Two completed master jobs block a third admission until
one is released or cleaned up; downloading alone does not free capacity.

Release frees job slots, not the separate global source/plan allowance. Sixteen
fresh previews can consume all 32 records in ten minutes even if each job is
released. A failed live identity check can retain a failed source for ten
minutes; local format checks cannot prevent that. Other web/API users share
global ceilings. Surface capacity errors rather than evicting records, resetting
IDs, or raising allowances.

## Verified delivery to a local file

`destination_path` refers to the machine running the MCP process, not a remote
client's machine. It is a nonempty absolute clean path of at most 16,384
characters, using the existing `maxPathCharacters` bound for export tools.
`overwrite` defaults to false and, when supplied, must be a
boolean. Reuse `validateExportDestination`: the parent must exist; its resolved
location must be outside Docbank's data directory; an existing destination must
be a regular file and requires overwrite. This does not add a ban on symlinks in
parent paths. A ZIP extension is not required.

Use `filepublish.CreateStage` beneath the destination directory and
`Connection.DownloadExportArchiveTo(ctx, jobID, stage.File)` on one acquired
connection. This merged helper binds the job identity and completed receipt,
compares the ticket receipt, validates the ticket URL, bounds streaming by size
plus one byte, runs `bundle.Verify` against the job fingerprint, and compares the
full verified receipt. The daemon separately verifies before issuing its lease.
Do not copy the ticket logic, build a second verifier, buffer or extract the ZIP,
or send a ticket URL in MCP results or diagnostics.

Sync and close the verified stage, check cancellation before publication, then
call `filepublish.Publish` with the explicit overwrite policy. Preserve its
`published` boolean. Before publication, errors leave the destination unchanged;
a destination that appears concurrently also blocks no-overwrite publication.
Ordinary interrupted work attempts stage cleanup on every return path.

When Publish reports publication, return the receipt and destination with
`state: "published"`; if its later directory sync fails, return
`state: "published_durability_unknown"`. Both report an already visible verified
file and are successful tool results with different durability guarantees.
Run stage cleanup before constructing the result. A cleanup failure after
publication sets `cleanup_failed: true` without discarding the receipt or
pretending the destination was rolled back. Before publication, local I/O or
cleanup failures are tool errors; their fixed message says a private stage may
remain. If cleanup also fails after a primary error, preserve the primary error
code and record the cleanup failure through bounded diagnostics. Never remove
the published destination as error recovery.

Cancellation is not atomic with publication. A lost MCP response or a deadline
after publication can leave the verified file visible even if the caller sees
an error. Documentation tells callers to inspect that destination, not blindly
retry with overwrite. A process crash may leave a private stage; retain
CreateStage's existing stale-stage cleanup policy. No persistent MCP download
task or automatic resumed transfer is introduced.

A failed transfer keeps the daemon job unless its existing lifecycle expires it.
A later call obtains a fresh ticket and retransfers the file. Ticket acquisition
or download errors before publication do not cancel, release, or recreate the
job. Preserve integrity-error classification before generic daemon sanitization:
the current daemon boundary helper does not retain `ErrIntegrity` as a cause.
Likewise, map the helper's local `bundle.ErrConflict` for an unfinished job to
`export_conflict` rather than a generic internal error.

## Transport bounds and errors

Keep the 1 MiB inbound message limit for stdio and HTTP, including the MCP
envelope, and the existing 1 MiB complete tool-result cap. Do not echo preview
members in responses; the plan is already a compact header. Keep the HTTP
request deadline of two minutes, including daemon verification, streaming,
local verification, and response delivery. Stdio honors the caller's context
and cancellation; it does not acquire that HTTP-specific deadline.

The engine's 52 GiB archive ceiling is not a promise that every archive can be
downloaded through MCP HTTP within two minutes. The deadline covers server-side
archive verification, transfer, and client-side verification. Tell callers to
inspect `get_export_status` and its completed `job.receipt.size` before choosing
HTTP delivery. Size is a planning aid, not a guaranteed time threshold; storage
and transfer speed also matter. Do not add a hard size cutoff or extra automatic
status call to download. For a job that cannot finish
within that deadline, callers use `docbank export download <job-id> <path>`
against the same selected vault, or a suitable stdio invocation. Do not extend
timeouts, run a detached transfer, or weaken verification. Cancellation before
publication cleans the stage when possible; cancellation after publication has
the visible-file caveat above. Status/start remain short asynchronous operations.

Malformed arguments use existing JSON-RPC invalid-params responses. New export
domain failures use existing bounded `{code,message}` tool errors with
`IsError: true`, adding fixed safe messages for these codes:

| Code | Meaning and recovery |
| --- | --- |
| `not_found` | This identity is unavailable to the caller. |
| `validation` | The daemon rejected export input; correct it before retrying. |
| `export_conflict` | A live precondition, reused ID, fingerprint, or job state conflicts; inspect the request/job. |
| `export_expired` | Admission or retained bytes expired; a missing archive may still require explicit release. |
| `export_limit` | A selection/resource ceiling was reached; narrow the request, release an eligible job, or wait for cleanup. |
| `export_retained` | A ticket/download still holds the archive; retry release after it ends. |
| `export_role_unavailable` | A required role cannot be exported. |
| `export_timeout`, `export_canceled`, `export_failed` | Preserve an explicit daemon problem code; no automatic retry or implication that the whole multi-call preview rolled back. |
| `export_outcome_unknown` | An attempted daemon write has no conclusive response; recover by the caller-supplied IDs. |
| `export_integrity` | Receipt, archive identity, or verification mismatch; nothing was published. |
| `export_local_io` | Staging, sync, close, cleanup, or pre-publication file installation failed; a private stage may remain. |

Map only known daemon problem facts and local typed errors; never forward raw
daemon messages, filesystem errors, ticket URLs, or error causes. Existing
daemon-unavailable and sanitized RPC errors cover other transport/internal
failures. A canceled transport may prevent any response at all. Filesystem
publication state is represented only by the download success contract above;
an ambiguous network reply must not be reported as proof that no file exists.

## Ownership and behavioral review

Catalog, schemas, bounded outputs, and adapter functions belong in `internal/mcp`;
the startup flag belongs in `cmd/docbank/mcp.go`. Reuse generated API operations
and the merged daemonconn download method. Keep engine, store, verifier, and
publication semantics with their existing owners. Any shared-helper extraction
for request value validation must serve this concrete adapter and preserve
other callers; the daemon write helper remains unchanged. Update the MCP
guide and link to the owning native export guide for shared limits/release rules
for this capability. Actionable work and status remain in kata.

These examples define observable behavior, not a requirement to duplicate the
existing engine/verifier test suites:

1. Default stdio and HTTP catalogs expose status and version hashes; all five
   export writes are absent and direct calls fail. The export flag enables them
   without enabling other write families, and unrelated flags do not enable them.
   Discovery marks cancellation, download, and release destructive.
2. Through a real MCP client and daemon using a synthetic vault, list a node's
   versions, construct members solely from those results, preview an old version,
   start, inspect completion, download, independently verify its original bytes,
   and release. A second unselected document and attachments stay out of the ZIP.
   Selecting both retained versions of one node produces two members.
3. Null/missing required inputs, unknown fields, malformed IDs/hashes, duplicates,
   negative numbers, overflowing totals, and 1,001 members fail before a source
   is reserved. A valid but stale identity fails at the daemon without silently
   changing membership. Required-size and optional-revision behavior match this
   MCP schema, not assumptions about the CLI decoder.
4. An eligible exact preview/start replay recovers retained authority. Reordered
   members or a changed fingerprint conflict. Preview expiry does not prevent
   status or matching job replay. Observe no automatic second write when a daemon
   request has an ambiguous outcome; assert the export-specific error/recovery.
5. Two master jobs occupy the same slots from CLI and MCP. An explicit MCP release
   permits another start; downloads alone do not. A real leased archive blocks
   release, then permits it after the lease ends. Repeated release is not-found;
   active jobs conflict and another owner's job remains inaccessible.
6. Download rejects an unfinished job, a mismatching receipt, and damaged or
   substituted bytes before publication. Assert the adapter keeps integrity and
   conflict error codes rather than losing them in generic sanitization. Reuse
   download/verifier coverage for the underlying ZIP invariants.
7. Existing destinations survive without overwrite; overwrite publishes only
   verified bytes. Interrupted transfer leaves no partial destination. Directory
   sync/cleanup failures after publication preserve the receipt and report the
   exact state/cleanup flag. No error or result exposes the one-use ticket.
8. A bounded HTTP deadline cancels an in-flight download; the job remains usable
   and the destination follows the publication boundary. A later download gets
   a fresh ticket. Use controlled deadlines and synthetic small files, not a
   52 GiB fixture or a change to production timeout policy.

Verification must exercise actual MCP discovery/calls and output schema checks,
the real daemon path, and local file results. Retain both SQLite modes and
supported platforms; no real developer corpus is needed. This specification
preserves design-review evidence; it is not an implementation review.
