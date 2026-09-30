# CLI exports of selected original files

Status: implemented in this branch. The maintainer selected
explicit release and inline execution on 2026-09-29. This extends the existing export
workflow in [#719](https://github.com/kenn-io/docbank/issues/719).

Source baseline: `0af2361e6ed8d79ef7997a778389c19936d36126`, after #726.
The [native export guide](../../usage/export-bundles.md) describes the existing
engine; this document records the approved CLI and release contract.

## Outcome and scope

A script can name exact retained document versions, freeze an export plan,
review its size and fingerprint, start a job, inspect or cancel it, download
a locally verified ZIP, and explicitly release a finished job. The CLI uses the
authenticated daemon throughout.

The preview command creates a flat `docbank-bundle-v1` plan containing original
files for 1–1,000 explicitly supplied node/version pairs. It adds no attachments,
folder descendants, renditions, or current-version substitutions. Originals
retain their original contents; redactions are not applied.

The lifecycle commands also accept existing plans and jobs accessible to the
CLI's daemon identity. They do not impose an original-only restriction on an
already created plan. The existing verifier supports those bundles, including
volumes. This does not add CLI controls for creating other plan kinds.

The approved capacity policy adds one release endpoint and CLI command,
described below. There are no new storage record types,
archive formats, dependencies, or ownership rules. Query selection, chunk uploads above 1,000 members,
rendition selection, volume configuration, MCP delivery, job listing, progress
subscriptions, automatic polling, resumable downloads, and a standalone offline
verification command are outside this scope. Existing package/load-file exports
remain a separate workflow.

## Command contract

```text
docbank export preview --request selection.json [--json]
docbank export start <plan-id> --fingerprint <sha256> --operation-id <job-id> [--json]
docbank export status <job-id> [--json]
docbank export cancel <job-id> [--json]
docbank export download <job-id> <local-file> [--overwrite] [--json]
docbank export release <job-id> [--json]
```

These commands are noninteractive. They use `daemonconn.Ensure`, the generated
client through `Connection.API()`, and the existing command context. The CLI
does not open a vault or adopt a browser session. Non-browser API clients share
the existing `master` export owner and its retained-job allowance.

With the current engine, two completed CLI/API-key exports occupy both shared
slots for 24 hours from their respective completion times. A third new job
returns `413 export_limit` until a slot is cleaned up. Download does not free a
slot, cancel refuses completed jobs, and another script can occupy the same
allowance. The proposed release command supplies the missing way to reclaim a
slot without increasing resource ceilings or making download destructive.

Each successful command emits one result to stdout. `--json` emits only the
JSON result, using `writeCLIJSON`; errors go to stderr. A successful `start`
means the daemon returned a job, not that the archive is ready. Reading a failed
job with `status` is successful and exposes its failure. No command waits for
the next job state.

### Preview: freeze explicit membership

The request is a small CLI-owned envelope, with `members` using the existing
`bundle.Member` fields:

```json
{
  "source_operation_id": "11111111-1111-4111-8111-111111111111",
  "plan_operation_id": "22222222-2222-4222-8222-222222222222",
  "members": [
    {
      "node_id": 12,
      "version_id": "33333333-3333-4333-8333-333333333333",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "size": 5
    }
  ]
}
```

These are synthetic identity examples, not an importable request. Callers supply
both operation IDs before the first request and retain the file for retries.
The CLI never generates replacement IDs after an error. A new deliberate
preview uses new IDs.

Read at most 1 MiB plus one sentinel byte before decoding; reject a larger file.
Use JSON v2 with unknown-member rejection, as the report CLI does, with this
command's own 1 MiB limit. Reject missing operation IDs and a missing, null,
empty, or over-1,000 `members` list as usage errors before daemon work. Reject
malformed JSON and wrong field types the same way. Do not add a second parser
just to distinguish null from omitted scalar values: ordinary decoding applies.

Before contacting the daemon, check both operation IDs and all decoded members
for the value rules below, duplicate node/version pairs, and combined original
bytes above `bundle.MaxRoleBytes`. Report a usage error naming the first invalid
field, including the zero-based member index where applicable. Use the existing
`daemonconn.IsCanonicalUUIDv4` check and
`canonical.IsSHA256Hex`; do not add another hex parser or use a page-source
validator with different size rules. These cheap checks prevent malformed
requests from reserving a failed source record. Live identity and revision
checks remain in the daemon; no per-member HTTP lookups are needed.

The daemon remains authoritative for member validation. Operation and version
IDs must be canonical UUIDv4 strings, node IDs positive, hashes lowercase
64-character hexadecimal strings, sizes nonnegative, and revisions nonnegative.
An omitted or null size decodes as zero and can match an empty original. An
omitted, null, or zero revision adds no revision precondition. A nonzero revision
must match the node when the source is sealed and when the plan is created.

Each `(node_id, version_id)` pair occurs once. Distinct retained versions of
the same node are allowed and count separately. The node must exist and be
untrashed at both source sealing and plan creation, and the supplied version,
hash, and size must agree. A retained historical version is valid even when a
newer version is current. The plan freezes the node's name and path at plan
creation, not its historical name and path. The CLI does not refresh identities,
drop invalid members, or normalize a changed request into a new selection.

Preview performs exactly these two existing mutations:

1. `CreateExportSource` with `operation_id = source_operation_id`,
   `kind = "explicit"`, and the supplied members.
2. `CreateExportPlan` with `operation_id = plan_operation_id`, the returned
   source ID and member hash, and `roles = [{"role":"original"}]`. Leave all
   other policies unset.

It returns the existing `bundle.Plan` as JSON. Human output shows the plan ID,
source ID, member hash, plan fingerprint, member count, original-file bytes,
metadata bytes, and plan admission expiry. Label the byte counts as planned
contents, not final ZIP size. Explain that original contents are included.
This is a summary; the supplied request is the explicit membership list. No new
per-file preview endpoint or duplicated role-summary result is needed.

Preview creates retained source/plan authority but does not start a job. It can
therefore temporarily prevent pruning content, like the existing web preview.

### Start, status, and cancel

`start` sends `bundle.JobRequest` with the explicit operation ID, plan ID, and
reviewed fingerprint. It returns `bundle.Job`. The operation ID becomes the
job ID. Supplying a different fingerprint never causes the CLI to fetch a new
fingerprint and retry. Send directly to job admission so its replay-first
behavior governs. Do not add a CLI check of the original plan's admission
deadline; that would wrongly block a still-retained job replay.

`status` makes one `GetExportJob` call and returns `bundle.Job`. Human output
includes ID, plan ID, fingerprint, state, completed roles/bytes, deadline,
retention expiry, and any failure or final receipt. Progress is an observation,
not a promise about the next state.

`cancel` makes one `CancelExportJob` call. JSON success is
`{"job_id":"<id>","accepted":true}`; human output says the cancellation
request was accepted. It does not claim every accepted request changed the
state: already canceled or failed jobs are successful no-ops. A completed job
returns the existing conflict. There is no follow-up GET required for success.

An interrupted or failed `start` may already have admitted a job. Retry the same
start request or inspect its known job ID with `status`. Do not recover it by
replaying `preview`: the source's original ten-minute replay deadline can expire
while its job remains retained. Exiting the CLI does not cancel the daemon job;
cancellation is an explicit command.

### Download and local publication

`download` returns `bundle.Receipt` as JSON only after successful verification
and local publication. Human output also names the destination. It never prints
or stores the one-use ticket URL in a result or diagnostic.

The operation has one publication boundary:

1. Resolve the destination with `prepareGetDestination`. Require its parent
   to exist, reject a directory destination, and require `--overwrite` for an
   existing file. Apply the package-export command's existing
   `home.Resolve`/`ContainsDirectory` check to keep output outside Docbank's data
   directory. These are destination policy checks, not a new filesystem API.
2. Use `filepublish.CreateStage` to create a private file beneath the destination
   directory, with a command-specific prefix. Arrange cleanup for every return
   path. It must be seekable and readable with `ReadAt`, not a bytes buffer.
3. Read the requested job through `GetExportJob`. Require the returned job ID
   to match, its state to be `completed`, and a receipt. Check the receipt's
   format, canonical fingerprint/hash, positive size bounded by
   `bundle.MaxArchiveBytes`, and positive entry count. Its plan fingerprint must
   match the job's fingerprint. A non-completed job is an error, not a wait.
4. Call `DownloadExportArchive` with an empty `bundle.DownloadRequest`. Compare
   its entire receipt with the job receipt before downloading. Use its relative
   ticket URL only on the same ownership-proven daemon connection. The URL must
   name the existing `/api/daemon/web-download/file` path and contain a ticket;
   reject another origin/path or a fragment. Do not follow redirects or expose
   the token through a wrapped HTTP error. The daemon's existing lease already
   reverifies the retained archive before it issues this ticket.
5. Stream an HTTP 200 body into the stage, bounded by the receipt size plus one
   byte. Require exactly the declared byte count. Close the response on success,
   error, or interruption. Do not load the ZIP into memory or extract it.
6. Run `bundle.Verify(ctx, file, size, job.Fingerprint)` on the completed stage
   and compare the entire returned receipt with the job/ticket receipt. This
   independently checks the archive structure, contents, fingerprint, and
   whole-ZIP hash. A separate streaming hash is unnecessary because this verifier
   computes it. The expected fingerprint comes from the job, not the ZIP itself.
7. Sync and close the stage, then call `filepublish.Publish` with the explicit
   overwrite policy. Only then emit the receipt. Preserve the helper's
   `published` boolean when reporting an error.

Before publication, any transfer, integrity, sync, or cancellation failure leaves
the destination untouched. Attempt stage cleanup on every return and report any
cleanup failure. A file that appears concurrently also blocks publication without
`--overwrite`. With overwrite, replacement happens only after full verification; no partial archive
is visible at the destination.

An error after publication, including directory sync, cleanup, or stdout failure,
can leave the complete verified file in place. Say so with its destination;
do not promise rollback or remove the published file. A process crash can leave
a private stage; reuse `CreateStage`'s existing stale-stage cleanup policy.

A failed download does not cancel or recreate the job. A later invocation issues
a fresh ticket and starts the transfer again. A ticket issued but never consumed
expires under the existing two-minute policy; this CLI does not introduce a
ticket-cancel protocol. Streaming and verification honor the command context.

### Release: reclaim finished jobs explicitly

Approved policy: keep the two-job allowance and 24-hour retention by default,
and let callers release a finished job when they no longer need its retained
archive or status. Download never releases automatically. A script can run
`download` and then `release` after checking success; it can instead keep the job
for repeated downloads. Human download output identifies the job and explains
that release frees its shared slot. The JSON receipt shape stays unchanged.

Add `DELETE /api/v1/exports/jobs/{id}`, operation ID `releaseExportJob`, with no
request body. Use the existing authenticated export owner and ordinary operation
gate. The route calls an exporter operation that owns its locking and gate;
do not wrap the route in a second acquisition of that same gate.

The release contract is:

- Only the caller's `completed`, `failed`, or `canceled` jobs are releasable,
  including expired rows awaiting cleanup. Queued/running jobs return
  `409 export_conflict`; callers must cancel them first. Unknown or other-owner
  IDs return the existing not-found response.
- An outstanding ticket or active download holds an archive lease. Return
  `409 export_retained` while that job has any lease, without changing the job.
  Do not revoke tickets, interrupt downloads, wait for lease expiry, or poll.
  An unused ticket can block release for its existing two-minute lifetime.
- Authorize the exact job before inspecting its lease count or deleting its
  archive. Serialize lease acquisition, cleanup, and release with the worker's
  existing mutex; use the same mutex-before-gate order as `Worker.Cleanup`.
- With no lease, remove only that job's private archive, treating an already
  absent archive as removed. Then delete the terminal job row and reduce the
  plan/source retention in one store transaction. Recompute retention from the
  original admission deadlines and other retained jobs, using
  `reduceExportRetention`; do not delete source documents or other jobs.
- Return HTTP 204 only after archive removal and the store transaction succeed.
  The job no longer counts toward either admission limit. Its status, download,
  and repeated release then return not found. The CLI prints a release receipt,
  `{"job_id":"<id>","released":true}`, only for that HTTP success.

Filesystem removal and the store transaction are not atomic. If removal fails,
leave the row and its retention in place. If the subsequent transaction fails,
the archive may already be gone while the row still occupies a slot: report the
error and let the caller retry release. Download of that missing archive returns
`410 export_expired` with a retry-release hint. The retry tolerates the missing archive.
If the success response is lost, status/not-found can establish that the job is
no longer accessible; repeated release does not recreate it. Do not introduce
tombstones or a second job state machine for release retries.

Release deliberately gives up that job's download, status, and replay authority.
Do not reuse its job operation ID: replay after deletion can admit a fresh job
if its plan is still eligible. The downloaded local file is unaffected. Source
and plan records keep their original admission deadlines and any retention
needed by other jobs; normal cleanup removes them when eligible.

This policy is preferred to merely raising the master allowance: that would
still leave the eight-job global ceiling and retained archives to manage.
It is preferred to automatic release after download because the caller may need
another copy or recovery after a local publication/output error.

## Retries, limits, and errors

Source creation and plan creation are not one transaction. A plan failure can
leave a sealed source until cleanup. Replay the unchanged request with the same
IDs; do not delete the source as compensation. Existing canonical request
digests define sameness, including member-array order. Reordering the request
with the same source ID conflicts even though the eventual member set is equal.

Replay succeeds only while the matching record remains eligible. A source left
resolving or failed returns a conflict; this command does not restart it. A plan
replay still requires its original admission deadline. A job replay can return
the retained job after that deadline because it checks the job first. Conflicting
reuse of an ID is an error. No promise of permanent deduplication survives record
cleanup, and the CLI performs no automatic retry or silent replanning.

Preserve the existing ten-minute source/plan admission, two-hour job deadline,
24-hour completed-job retention unless explicitly released, 32 global combined
source/plan records, and eight global retained jobs with two per owner. The CLI
does not extend these limits. After two completions, release at least one job
before starting another; otherwise wait for expiry and cleanup. The
1,000-member preview ceiling is the existing direct-source ceiling; larger API
jobs may still be inspected and downloaded. Archive transfer remains bounded by
the existing 52 GiB archive limit. Retention and restart behavior belong to the
existing engine, not new CLI state files.

The source/plan allowance is shared across all owners, including web sessions.
A source reserved before a live identity check fails remains a failed record
for its original ten-minute lifetime and cannot be retried into success. Local
format checks avoid simple typos consuming that allowance; they cannot prevent
live identity conflicts. Existing daemon errors do not identify the failing
member. Record cleanup can lag expiry while protected by a lease or maintenance.
Releasing a job frees its job slot, not all source/plan slots. For example, 16
distinct fresh previews each consume a source and a plan and can exhaust the
global 32-record allowance within ten minutes even if their jobs are released.
Other owners can also fill the eight-job global allowance. These capacity errors
remain visible; do not silently evict another owner's records or reset IDs.

Keep the current HTTP problem responses and shared CLI exit classification.
For example, export conflicts use `409 export_conflict`, expired authority
uses `410 export_expired`, and capacity limits use `413 export_limit`.
Do not invent a selected-report error mapping for exports. Local request-shape
errors are usage errors. Receipt or archive mismatches are integrity errors
through the existing `daemonconn.ErrIntegrity`/CLI classification. Transport,
disk, cancellation, and daemon errors remain distinguishable from corruption.
`--json` changes success formatting, not the existing stderr error contract.

## Ownership of the change

Command parsing and human output belong in `cmd/docbank`. JSON output reuses
`writeCLIJSON`. Generated API methods already cover the source, plan, and job
operations except the proposed release. Generate its client from the new route
through the repository's normal API-generation command. The verified streaming
download needs a small `daemonconn` method that uses that connection's HTTP client
and the existing bundle verifier. Release adds a focused exporter/store operation
beside existing lease, cleanup, and retention code.
Publication remains in the CLI and reuses `internal/filepublish`.

Follow `Connection.CreatePackageExportTo` in
`internal/daemonconn/package_export.go:65` for ticket acquisition, the same
connection's HTTP client, its `X-Api-Key` header, bounded streaming, and independent
file verification. Its package verifier and receipt type do not apply to native
bundles. Do not copy its raw return of the `c.hc.Do` error: an HTTP `*url.Error`
includes the ticket URL in its message. Remove that URL-bearing wrapper before
adding safe operation context, preserving the underlying cancellation/transport
error. This is a rule for the new method, not a refactor of package downloads.

Follow `exportBatesFile` in `internal/mcp/bates_tool.go:300` for `CreateStage`,
sync, close, and `Publish`, including its published flag. Retain the CLI's error
and cleanup contract above rather than importing MCP response types or logging
policy. A transport-error example must show that stderr contains no ticket URL
while cancellation remains identifiable.

Do not generalize report/package readers, rebuild the API catalogue, change
generated clients by hand, or refactor other download commands as part of this
work. Explicit early release is the only job-retention policy change; source,
plan admission, archive, and verifier contracts remain unchanged. The public
export guide and [CLI reference](../../cli-reference.md) should describe the
commands when implemented; until then the export guide's statement that no CLI
bundle-export command exists remains correct. Actionable work and status belong
in kata.

## Behavioral examples for implementation review

These describe required observations, not a new test framework or a demand to
duplicate existing store/verifier coverage. CLI success cases use a real daemon
and a temporary synthetic vault. Controlled transport faults belong at the
download boundary; assertions inspect files and returned results.

1. Select one of two documents. Preview creates no job and reports one original.
   Start and status expose the same plan/fingerprint. Download contains that
   version's original bytes and excludes the unselected document and attachments.
2. Replace a document, retain its old version, then explicitly select the old
   version. Its old bytes export successfully. Selecting both retained versions
   produces two members. Missing/trashed nodes, duplicate pairs, incorrect hash
   or size, and a nonzero stale revision fail without a partial plan.
3. Reject malformed, unknown-field, empty/null-member, over-1-MiB,
   1,001-member, and locally invalid value requests before creating a source.
   A malformed version/hash or negative size/revision identifies the member
   field and reserves no source record. Null/omitted scalar handling follows the
   decoding rules above; no special raw-JSON presence scan appears.
4. Reissue unchanged preview/start requests after discarding their first
   responses: the eligible IDs are reused. A changed request with a retained ID
   conflicts. Exercise the gap between successful source creation and failed
   plan creation without automatically generating another source ID.
5. Starting with the wrong fingerprint fails. Replaying an admitted job after
   the plan deadline recovers that job. A status read of a failed job succeeds;
   canceling it does not report a fabricated canceled state. Canceling a
   completed job conflicts. Preview replay after its source deadline fails even
   when status still returns its retained job. No command enters a polling loop.
6. Headless CLI download uses the real master-owner ticket route. A truncated,
   oversized, or altered response fails. A different valid bundle also fails,
   even if its internal checksums agree. A malformed or disagreeing receipt
   fails before publication. Integrity checks do not depend on trusting headers.
7. Existing destinations and a destination created during transfer survive
   without overwrite. Overwrite replaces only with a fully verified archive.
   Interrupted transfer, verifier failure, disk failure, and ordinary cleanup
   leave no partial destination. A post-publication directory-sync or output
   failure reports that the verified file is already present.
8. An interrupted transfer leaves the job available for another download. The
   next command obtains a new ticket rather than reusing the consumed one.
   An issued but unused ticket follows existing expiry and lease release.
9. Complete two jobs as the master owner. A third new job fails with
   `export_limit`, even after downloading the first two. Release one and admit
   the third with a fresh job ID and an unexpired plan. Another API-key caller
   observes the same allowance. Release leaves original document bytes intact.
10. Release rejects an active job, another owner's job, and a job with a ticket
    or download lease. Once the lease ends, release succeeds, removes its archive
    and row, and shortens unused extended retention without shortening another
    job's retention. Repeated release returns not found. A filesystem failure
    preserves the row; a transaction failure after file removal permits a retry.
    Exercise lease/release races through the actual worker boundary.

Implementation verification must cover the real CLI/HTTP path and publication
behavior on supported platforms, retain both SQLite modes, and use existing
bundle verification tests for format details. No real developer corpus is needed.
