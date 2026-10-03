---
title: "Frozen search reports through local MCP"
description: "Implemented local MCP contract for frozen search reports, dated results, evidence retrieval, and release."
last_edited: 2026-10-01
---
# Frozen search reports through local MCP

Status: implemented. The corrected date pager applies to every caller.
The owning [MCP guide](../../usage/mcp.md#frozen-search-reports) describes usage
and the retained limits.

Source baseline: `3a23e5e61f26c0b8aef33582adaf2bbd7717f2a7`, after
[native MCP exports](https://github.com/kenn-io/docbank/pull/749).
This is the next bounded outcome from
[the contribution reset](https://github.com/kenn-io/docbank/issues/719).
The [selected-report design](2026-09-29-exact-document-reports-design.md)
owns the existing frozen-evidence contract. The
[native MCP export design](2026-09-30-mcp-native-exports-design.md) supplies
the local-file delivery pattern.

## Outcome and boundary

A local operator can use MCP to report on exact current document versions,
inspect ambiguous date evidence, record reviewed date choices, and save an
independently verifiable evidence ZIP on the machine running MCP. The same
selected versions can feed the existing originals-only native export tools.
These are separate artifacts: the report ZIP contains evidence and counts;
the native export contains original files.

Reuse the daemon report engine, frozen frames, date decisions, packet format,
ownership, history, and expiration. Reporting starts no provider work. Capture
must reject a changed selection; later source changes do not invalidate the
captured report. Missing evidence stays visible through existing coverage and
date-review behavior. Do not silently switch to available-only coverage.

Add five tools and a separate report-write flag. One additive HTTP request
field is needed to fit date evidence into MCP replies. No new endpoint,
storage schema, packet format, retention policy, dependency, background job,
or operation registry is part of this design.

History browsing, standalone CLI download/show commands, whole-vault or
collection creation through MCP, chunk handles, remote artifact delivery,
separate CSV proof, automatic processing, and production/redaction workflows
remain outside this increment. CSV is derived from the verified ZIP with the
existing `docbank search-export csv` command. Closed PRs #456 and #507 are
requirements references, not code to import as cumulative branches.

## Tools and operator control

Add `docbank mcp --allow-report-writes`, default false, and
`ServerOptions.AllowReportWrites`. Apply it to stdio and HTTP through the
existing options-based catalog. Other write flags do not enable report writes;
this flag does not enable native exports. The full report/originals workflow
requires both report and export opt-ins.

| Tool | Input | Success payload before common cache fields | Default |
| --- | --- | --- | --- |
| `get_report_summary` | `report_id` | `{ "summary": <report.Summary> }` | Available |
| `get_report_dates` | `report_id`, optional `cursor`, `limit` | `{ "report_id": "…", "page": <report.DatePage> }` | Available |
| `create_report` | `request` | Compact report receipt described below | Disabled |
| `revise_report` | `report_id`, `choices` | Compact report receipt described below | Disabled |
| `download_report` | `report_id`, `destination_path`, optional `overwrite` | Local-file receipt described below | Disabled |

Creation and revision retain cache entries and durable history, even when the
result needs review. They are writes, with false destructive and idempotence
hints. Download is a write with a true destructive hint because overwrite can
replace a file; its idempotence hint is false. The two inspection tools use the
existing read annotations. Disabled tools are absent and cannot be called.

Use the existing structured-content plus JSON-text result convention and
top-level `ttlMs: 0`, `cacheScope: "private"`. Every input and output object
has a closed schema. Preserve existing nested report field names and omission
rules. Tool descriptions identify the host-local destination, ask the client
to review scope/date choices with its operator, and distinguish a frozen
report from current vault state. No consent-token or per-client authorization
system is added.

## Creating an exact report

The `request` object uses `report.Request` and `report.NormalizeRequest`.
It requires `version: 1`, `selected_documents`, `timezone`, and `terms`.
`all_documents` may be omitted or false; true is invalid. `collection_ids`
and `date_choices` are not accepted in this creation tool. Reviewed choices
belong in `revise_report`, after the caller has seen this observation's evidence.

`selected_documents.documents` is required and contains 1–1,000 whole-document
identities. Each requires a positive signed 64-bit `node_id`, canonical
lowercase UUIDv4 `version_id`, and 64-character lowercase hexadecimal `sha256`.
Only one version per node is allowed. Reuse the existing UUID/hash schema
helpers and request normalizer; do not add a second identity parser. The
normalizer's node ordering is intentional. Preserve term order and content.

`terms` retains the engine's 1–128 rows and 8,192-Unicode-character expression
limit. Each row requires unique positive `number`, `expression`, `syntax`
(`simple` or `advanced`), and `dates.start`/`dates.end` as literal inclusive
`YYYY-MM-DD` cutoffs. The complete MCP request must fit the existing 1 MiB
transport limit; the HTTP and CLI report limits remain unchanged.

`profile`, `source_timezone`, `numeric_date_order`, and `coverage_mode` remain
optional with their existing meanings. Omitted coverage is `strict`;
`available_only` must be selected explicitly. Timezone strings are capped at
128 UTF-8 bytes in MCP, then checked by the existing normalizer. Profile keeps
its existing 128-byte limit. Date syntax, timezone existence, duplicate rows,
scope, and request semantics remain owned by `NormalizeRequest` and the daemon.
MCP schema/type errors and local normalization failures return invalid arguments
before contacting the daemon. Present nulls are invalid MCP arguments, including
null selections; the more permissive HTTP/CLI omission-or-null contract is
unchanged.

The caller discovers a node/current-version pair with `list_documents` or the
existing search results, then obtains its `blob_hash` from
`list_document_versions`. `get_document` inspects an already selected pair;
it requires both IDs. Match the exact version ID rather than assuming the
first version-list item is current.
Map `content_version_id` to `version_id` and `blob_hash` to `sha256`. This
currently needs a version-list call per selected node. No catalog projection
or automatic per-node lookup is added. Native export additionally needs size;
its allowance for historical versions does not apply to new reports.

The daemon remains authoritative for current, live, untrashed identity checks.
Missing, replaced, or trashed selections return `report_selection_changed`;
a wrong hash on an otherwise current selection returns `invalid_report_request`.
Never substitute a head, omit a member, widen scope, or add attachments.

Creation returns either `complete` or `needs_review` as a successful receipt.
`needs_review` has no downloadable counts. Other strict coverage failures keep
their existing errors. Under `available_only`, a member without a usable date
remains in the packet but can be absent from counted coverage. Input selection
size and coverage `scoped` are different quantities.

Create and revise return `report_id`, optional `parent_id`, `state`,
`observed_at`, `expires_at`, and `unresolved_dates`, plus the common cache
fields. Complete receipts also include `bundle_bytes` and `bundle_sha256`;
needs-review receipts omit those two fields. These values are copied from the
validated daemon summary, with `summary.id` mapped to `report_id`. Counts,
terms, and coverage are read separately through `get_report_summary`.

This compact receipt is deliberate. Even a small selected set can connect to
a large family graph, whose warnings enlarge the summary. Do not make delivery
of a new handle depend on the complete summary fitting in an MCP reply. There
is no additional compact-summary endpoint or durable receipt format.

## Inspection, date decisions, and paging

Report IDs are exactly 48 lowercase hexadecimal characters, not UUIDs.
Use `Connection.GetTermReport` and `Connection.TermReportDates` for reads.
Inspection can read any handle owned by this daemon connection, including a
CLI-created whole-vault or collection report. It does not impose the 1,000-node
creation bound on those handles. If the full summary cannot fit the existing
MCP result limit, return `report_limit`; read that summary through the existing
HTTP API. The handle remains usable for dates, revision, and download. Do not
truncate terms, counts, coverage, or warnings to make a summary fit. Measure
the complete result with space for the existing SDK metadata; the final
middleware size guard remains in place.

`get_report_dates` takes an optional opaque cursor, at most 4,096 bytes, and
an optional limit of 1–100 members, default 50. Omitted or empty cursor starts
at the beginning. Return the daemon cursor unchanged. A member can span pages;
`candidates_complete` distinguishes a partial candidate list. Callers retain
earlier candidates for that member and continue until `next_cursor` is absent.
Candidate IDs, evidence hashes, locators, rejected values, selections, and
existing choices must be preserved without quote truncation or interpretation.

The existing date endpoint bounds its page near 1 MiB before MCP wraps it;
MCP emits the payload twice and caps the complete result at 1 MiB. Merely
lowering the member count is insufficient: one member can have 256 candidates.
This is the combined limit enforced by `Service.Prepare` after adding metadata,
fallback, and extracted-text candidates, not just the text extractor's limit.
Extend `report.DatePageRequest` with `MaxBytes int` encoded as
`max_bytes,omitempty`. Omission or zero keeps the existing 1 MiB default.
Positive values must be 64 KiB–1 MiB; reject other values with
`report.ErrReportLimit`, mapped by the route to 413 `report_limit`. This is
an optional field, not required by Huma. Regenerate the existing API artifacts.
The daemon connection validates the same range before sending it.

MCP always supplies 256 KiB; callers cannot override it through the tool.
The bound applies to the encoded full `report.DatePage`, including its next
cursor. Adapt the existing cache pager rather than adding an MCP cursor store.
If a next member/candidate would exceed the budget, return the entries already
fitted and a cursor to the first unreturned candidate. Do not drop evidence,
advance past an unreturned candidate, return an empty nonterminal page, or
raise a limit error merely because the remaining room is insufficient. If a
single required member/candidate cannot fit an otherwise empty page, return
`report_limit`. The corrected pager applies to every caller, including web and
CLI requests that omit `max_bytes` or send zero. Those requests keep the 1 MiB
default ceiling, not the existing bug that fails an already populated page
when the next candidate does not fit. Return the populated page and its
continuation cursor instead. Page boundaries need not be byte-for-byte stable.

The 256 KiB payload bound leaves room for both MCP representations and metadata.
The existing final result-size guard remains authoritative. Verify the full
wire result, including escaping and metadata, rather than measuring only the
Go payload or assuming the copy doubles its size exactly.

`revise_report.choices` is a required nonempty array of at most 1,000 existing
`report.DateChoice` values. Require document identity, candidate ID, evidence
SHA-256, nonblank reason (at most 4,096 UTF-8 bytes), and action. Document
identities use the creation input's canonical forms. Candidate IDs and evidence
hashes are 64 lowercase hexadecimal characters. Action is `select`, `interpret`,
or `reclassify`; optional reviewed date, timezone, and role fields retain the
engine's existing action-dependent rules. Reviewed timezone uses the MCP
128-byte bound. Reviewed roles are `document_date`, `created`, `authored`,
`sent`, `captured`, `signed`, `effective`, or `expiry`. No new date inference or
free-form replacement evidence is introduced. Schema and cheap shape checks
precede sending; the daemon validates the full choice semantics and evidence.

The 1,000-choice count and 4,096-byte reason length are individual ceilings,
not a promise that their simultaneous maxima fit a request. The complete MCP
message, including JSON escaping and the protocol envelope, must fit within
1 MiB. Larger reviews need shorter reasons where appropriate, or caller-chosen
batches applied to each newly returned child. The adapter never splits a
revision automatically. Each batch consumes another shared handle and keeps
the original expiry; an initial report with no other owner handles permits
at most seven such revisions before capacity is full. For a review that cannot
fit those limits, use the existing HTTP or CLI revise path with its 8 MiB
request ceiling to submit a larger batch. That still consumes a handle and
does not bypass the shared capacity limit.

Make one revision request against the original supplied ID. A parent with a
large summary can still be revised because the result is a compact receipt.
The daemon combines parent choices with incoming choices, replacing only
matching document choices.
Unmentioned choices remain. It returns a new child ID and `parent_id`; the
parent is unchanged. Empty choices are not a way to clear decisions. Cursor
bindings are per handle: restart paging on the child, not with a parent cursor.
Choices with stale or absent evidence keep `stale_evidence` or
`invalid_report_choice`, rather than refreshing a source or guessing a date.

## Capacity, lifetime, and uncertain outcomes

CLI and MCP use the same API-key report owner. Browser sessions have separate
owners. Keep the existing limits: two concurrent builds globally, 64 cached
or pending handles globally, eight per owner, 60 seconds per build, and the
daemon's shared 1 GiB accounted report budget. Revision consumes another handle
even though its frozen frame is shared. A report plus seven revisions can fill
all eight slots; unrelated CLI/MCP calls contend for those slots too.

Every descendant expires 30 minutes after the original observation, not
30 minutes after revision or download. Successful download frees no handle.
There is no report-release endpoint in this scope. When capacity is full,
wait for existing handles to expire; batch reviewed choices when practical.
This design retains that throughput limit. Do not raise ceilings, evict another
report, release implicitly, or restart a daemon
to reclaim capacity. Explain the limit in tool instructions and the guide.

Daemon restart loses live handles and date pages. Durable history keeps only
the existing bounded request/summary receipts, not report ZIPs or reviewed
choices; backup/restore does not resurrect them. Download before expiry.
After expiry or restart, create a fresh observation from a freshly checked
selection; do not present it as regeneration of the old report.

Reads use `daemonRead`, which may reconnect and repeat a read once before any
response starts. Creation and revision use the existing single-attempt write
pattern (`daemonProcessingStart`), with report-specific outcome wording at the
call site. Do not place writes inside a read retry callback. A lost, canceled,
or undecodable write response returns `report_outcome_unknown`. A reply that
arrives but fails summary validation or MCP result encoding after a write is
also an unknown outcome; do not hide it as a known admission failure. Preserve
recognized daemon problem codes when the server actually returns one.

Report IDs are generated by the daemon. There is no caller operation ID,
idempotent replay, or reliable MCP lookup for a lost create/revise response.
The error must say the call may have created a report and must not be retried
automatically. The operator can inspect existing history receipts through the
web/HTTP interface, but matching a receipt does not prove a lost call's identity
or grant another owner's artifact access. A deliberate retry creates another
observation or child and can consume another slot. After daemon restart, the
first write on a stale connection may be outcome-unknown even if it was never
sent; this matches the existing conservative write behavior.

## Verified local delivery

`download_report` saves only the evidence ZIP. `destination_path` is required;
`overwrite` defaults false. Reuse `validateExportDestination`: an absolute clean
path, existing resolved parent outside the Docbank data directory, and any
existing destination must be a regular file. Use the existing path schema
limit. Parent symlinks are resolved by that helper, not categorically forbidden.
Write to a private stage created by `filepublish.CreateStage` in the destination
directory; do not open the destination for streaming.

Read the summary for the requested ID and require `complete`. Bind its
`bundle_bytes` and `bundle_sha256` to the stream returned by
`Connection.OpenTermReport(id, "bundle")`. Use `TermReportStream.CopyVerified`
to check transferred length and SHA-256; retain the existing 512 MiB ceiling.
Then rewind and call `report.VerifyBundle` on the staged bytes before publication.
A different valid packet must fail the summary/stream digest comparison just
as a corrupt packet fails verification. A `needs_review` report returns
`date_review_required` and creates no published output.

Extract this summary/stream/file verification into a small daemon-connection
helper and use it from the CLI's existing `downloadReportPacket` too. Keep
destination policy and publication in each caller. The helper writes into a
caller-owned seekable staging file, closes the HTTP response, accepts the
caller's verification budget, and returns byte count, digest, and verification
result. It never publishes. Distinguish read/write I/O errors, integrity
failures, budget failures, cancellation, and daemon problems before generic MCP
sanitization can lose those causes. Use existing digest/identity helpers.

`VerifyBundle` reads and accounts for packet bytes in memory. The MCP server
therefore shares one `report.NewBudget(report.DefaultBudgetBytes)` across its
report downloads, with a child scope closed on every exit. Do not create an
independent 1 GiB allowance for each concurrent tool call. This uses existing
budget accounting, not a new scheduler or spool service. It bounds accounted
verification allocations, not total process memory. Budget exhaustion returns
`report_limit`; no output is published.

After verification, sync and close the staging file, check cancellation, and
call `filepublish.Publish`. Preserve its published boolean. Once publication
succeeds, a later directory-sync or staging-cleanup failure must not become a
generic failed download that hides the saved file. Return:

```json
{
  "report_id": "<48 lowercase hex characters>",
  "destination_path": "<absolute local output path>",
  "bytes": 12345,
  "sha256": "<64 lowercase hex characters>",
  "verification": { "internally_consistent": true, "source_verified": false },
  "state": "published",
  "cleanup_failed": false,
  "ttlMs": 0,
  "cacheScope": "private"
}
```

The verification object is an explicit MCP projection of `report.Verification`;
it does not inherit that Go type's untagged field names. State is `published`
or `published_durability_unknown` when publication succeeded but the final
directory sync failed. `cleanup_failed` is independent of that state. Log local
causes, including cleanup errors, for the operator; clients get fixed messages.
An early destination error must not claim a stage exists. A competing destination
without overwrite must survive unchanged. No automatic download retry occurs
after streaming or publication. If the MCP reply itself is lost, inspect the
destination and verify the ZIP before deciding to repeat the operation.

Downloads use the existing authenticated report stream, not browser tickets.
The report GET is exempt from the daemon's ordinary 60-second request timeout.
MCP HTTP still has its existing two-minute deadline covering reads, transfer,
verification, and publication. Stdio has no equivalent HTTP deadline. Read
`bundle_bytes` first when choosing a transport; large ZIPs are not guaranteed
to finish over MCP HTTP. Use stdio for the same live handle, or the existing
HTTP download followed by `docbank search-export verify`. The CLI has no
standalone download-by-ID command in this scope.

## Error contract

Malformed schemas, unknown keys, explicit nulls, local input bounds, and
destination-policy failures use the existing invalid-arguments RPC result.
Transport-wide oversized-frame handling remains unchanged.
Otherwise use the existing bounded domain-error result with fixed messages:

| Code | Meaning and caller action |
| --- | --- |
| `invalid_report_request`, `invalid_query`, `invalid_profile`, `invalid_report_scope` | Correct the request; preserve daemon classification without echoing expressions or paths. |
| `report_selection_changed` | Refresh and explicitly reselect current versions; never retry with substituted heads. |
| `invalid_report_choice`, `stale_evidence` | Inspect the specified report's evidence and correct the reviewed decision. |
| `incomplete_coverage`, `incomplete_date_coverage` | Evidence is insufficient for strict reporting; do not silently change coverage mode. |
| `date_review_required` | Inspect dates and revise before downloading counts. |
| `report_unavailable` | The handle is expired, unknown, or belongs to another owner, or reporting is unavailable. No artifact restoration is promised. |
| `report_capacity` | Wait for the shared build/handle capacity to become available. |
| `report_limit` | Narrow the request or use the existing larger HTTP/CLI boundary where applicable; a local verification budget may also be exhausted. |
| `report_timeout` | The daemon's report build deadline was reached. Do not widen the deadline automatically. |
| `report_outcome_unknown` | Create/revise may have succeeded without a usable reply; do not automatically repeat. |
| `report_integrity` | Summary, download, or packet evidence disagreed; no file was published. |
| `report_local_io` | Local file work failed before publication; inspect the operator log for the cause. |

Keep existing daemon-unavailable and cancellation behavior for read/download
operations. Map local integrity and file errors before generic daemon-error
sanitization. The fixed domain result need not copy the HTTP query-position
payload or a list of conflicting members. Original error details belong in
appropriate local diagnostics, not MCP text.

## Evidence required of the implementation

Use existing daemon/MCP test harnesses with temporary synthetic vaults. Cover
these behaviors at their owning boundary rather than adding parallel parsers,
mock report engines, or tests that search implementation text:

1. Both transports expose reads by default and enforce the independent report
   opt-in on actual tool calls. Other flags do not enable report writes.
2. A real MCP workflow obtains exact current identities, creates a report,
   inspects ambiguous dates, revises, and downloads a verified packet. Check
   known counts and exact packet membership independently. Export those same
   originals with the merged tools and compare the original file hashes.
3. Replacing a selected version before creation yields a conflict. Replacing
   or trashing it after capture leaves summary, evidence, and downloaded counts
   frozen. Missing text and unusable dates follow strict/available-only rules;
   no provider is invoked. Excluded documents remain excluded.
4. Large date evidence spans the new byte-bounded pages without loss,
   duplication, partial-candidate truncation, empty-page loops, or oversized MCP
   wire results. Include one member spanning pages, heavily escaped strings,
   and the case where the next candidate cannot fit the remainder. Include a
   regression with `max_bytes` omitted at the default 1 MiB ceiling: earlier
   members fill the page, the next member's first candidate does not fit,
   and the response succeeds with the earlier members and a cursor that resumes
   at that candidate. Exercise explicit zero through the same default path.
   Check the too-large single-item error separately.
5. A child retains unmentioned parent choices and the original expiration;
   parent cursors do not work on it. Capacity includes children and other
   master-owner reports. Expiration/restart loses handles, not durable history.
   Reuse existing engine coverage for unchanged storage/restore guarantees.
6. Creation/revision never replay automatically after canceled transport or
   undecodable/invalid replies. Check complete and needs-review receipt schemas
   and full wire size. A large family-warning summary must not prevent delivery
   of its new handle; summary inspection reports the size limit, while dates
   and download remain usable. Treat injected transport faults as tests of
   handling, not evidence of their frequency.
7. Download rejects changed bytes, a different valid packet, and short/long
   streams before publication. Cover destination races, cancellation before
   publication, sync/cleanup failures, truthful saved-file state, and observable
   local error causes. Shared verification reservations are released on success
   and failure. The CLI continues using the same verification contract.

Run the affected reporting, daemon connection, API, MCP, and CLI checks in both
SQLite modes, plus repository-required lint/generation/documentation checks.
Supported native platform checks remain required for changed publication paths;
compile-only substitutes are insufficient. No new web interface is proposed.
Update the owning MCP guide and report/HTTP references
when implemented; keep this proposal outside public navigation and change its
status banner when the feature ships.
