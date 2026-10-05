# Search reports and evidence inspection

Search reports capture one observation for dated counts and offline evidence
verification. The [search-export guide](../usage/search-exports.md) owns request
fields, count definitions, limits, and recovery. The [MCP guide](../usage/mcp.md#frozen-search-reports)
owns tool inputs and transport limits; [document processing](../usage/document-processing.md#inspect-evidence-before-reporting)
owns the inspection workflow.

## Ownership

| Concern | Owner |
| --- | --- |
| Request normalization, dates, counts, and packet verification | `report/` |
| Current-version capture and retained family evidence | `internal/store/term_report_frame.go`, `term_report_families.go` |
| Captured text, date extraction, report lifetime and revisions | `internal/reporting/` |
| Durable request/summary receipts and metadata transfer | `internal/store/term_report_history.go` |
| HTTP ownership, admission, and history persistence | `internal/api/routes_term_reports.go` |
| Summary-bound download verification | `internal/daemonconn/term_report_download.go` |
| CLI recovery and local publication | `cmd/docbank/report*.go` |
| MCP receipts and host-local delivery | `internal/mcp/report*.go` |

Reports and [original-file export bundles](export-bundles.md) are separate
systems. Reporting uses retained evidence and starts no processing or provider
work.

## Admit selections in the captured observation

`report.NormalizeRequest` owns scope semantics without reading a vault.
Exactly one all-document, collection, or selected-document scope is active.
It copies nested selections and sorts identities by node ID without changing
term order, expressions, or date cutoffs. Frame, cache, and browser draft copies
must also own their nested input.

HTTP and CLI treat an omitted or null selected scope as absent. An empty
selected object is invalid. MCP's closed schemas reject explicit nulls and
apply smaller input limits. Keep schema errors distinct from report application
errors; evolving scope policy does not belong in SQLite constraints.

`MaterializeTermReportFrame` resolves the entire selected identity set in the
same lexical-generation and SQLite read snapshot used to capture matches and
evidence. A separate current-version preflight cannot replace that check.
Missing, trashed, or replaced versions reject the whole run as
`report_selection_changed`. Availability is checked across the whole set before
classifying a wrong hash on a matching current version as an invalid request.
Equal bytes under a new version ID still conflict.

Names, paths, tags, relationships, and processing evidence are observed during
capture. Metadata-only changes do not replace selected content identity.
The server records vault identity as provenance; selected requests need no
client vault-ID field. Browser drafts copy identities from selected rows;
refreshes cannot replace those identities. A history draft retains them but
clears reviewed date choices, and rerunning takes a new observation.

## Capture evidence without retaining source authority

`Service.Prepare` runs under `OperationGate.CaptureContext`. The preservation
gate protects physical evidence while the store captures metadata and the
service reads the exact retained text named by that capture. It does not
freeze ordinary metadata changes for the report's lifetime.

Preparation extracts date candidates, then discards full text. The combined
candidate limit applies after metadata, fallback, and text candidates have
been assembled. Counts, revisions, date pages, and artifacts use this captured
frame without consulting current source state. Later replacement, trash, or
pruning does not revoke captured evidence. Reports add no retention references
that prevent source deletion; fresh reports still require live current sources.

Date interpretation normalizes escaped punctuation in rendition text while
preserving raw tokens, quotes, byte offsets, and the retained text hash. Those
bindings let a verifier check the captured evidence without confusing rendered
Markdown with its stored bytes.

## Membership and families

Selected scope controls report members and counted documents. Family traversal
can pass through current related documents outside that population: an
unselected attachment can connect selected parents. Preserve those relationship
references without adding outside member rows, text bindings, dates, or hits.
Historical and trashed versions do not connect current family groups.

Coverage counts date-eligible members. In `available_only`, a member without a
usable date remains in the packet but contributes no scoped coverage. A date
outside every requested range also contributes no scoped coverage. Incomplete
family totals count eligible documents, not family groups. A truncated MIME
inventory can make family coverage incomplete even when all selected text is
searchable. Selection conflicts apply under either coverage policy.

Calculation and offline verification both require equality between selected
request identities and member identities. A subset check would miss an omitted
document. Collection witnesses remain specific to collection scope.

The `search-export-v1` packet retains request version 1. Verification checks
evidence, selected dates, counts, coverage, and digests. It proves internal
consistency, not source authenticity or external operator intent. The packet
contains neither complete source text nor enough evidence to rerun searches.

## Revisions, owners, and history

A revision shares its parent's frozen frame, retains unmentioned date choices,
and replaces choices by document identity. It gets a new handle and consumes
another owner slot. Every descendant retains the original observation's
30-minute expiry. Downloading neither renews expiry nor releases capacity.

CLI and MCP use the API-key owner; browser sessions use separate owners.
Live summaries, date cursors, and downloads enforce ownership and expiry.
Cursors bind the owner, report ID, member, and candidate; a parent's cursor
cannot continue a child's date pages. Restart loses live handles.

Vault-wide history stores requests and summaries with choices removed. Reading
or importing history validates receipt structure without requiring live sources.
History is neither an artifact archive nor evidence that a handle is available.
Restore preserves receipts but cannot restore their in-memory artifacts.

History retains 100 receipts. Its 16 MiB page threshold measures stored request
and summary JSON, not the complete HTTP envelope. Continue by returned item
count. Ordering is observation descending, then ID descending; concurrent
additions mean offset pages are not a stable snapshot.

## Bound transport output without losing evidence

MCP report writes require their own opt-in. Create and revise return compact
handle receipts because connected family warnings can make a full summary
large even for a small selection. Summary reads return `report_limit` rather
than dropping terms, counts, coverage, or warnings.

The shared date pager measures the complete page, including its cursor. When
another complete candidate will not fit, it returns the populated page and
resumes at that candidate. Only an item that cannot fit an empty page requires
a limit error. Quotes are never truncated. MCP requests a 256 KiB page because
its response includes both structured content and JSON text; the final encoded
result bound still applies.

MCP writes use one attempt. Transport, decode, cancellation, or post-write
validation failures can leave the outcome unknown. Preserve recognized daemon
problem codes, but do not turn an unusable successful reply into a known
admission rejection. There is no idempotency key or reliable way to identify a
lost create/revise reply through history.

## Verify delivery before publishing

`Connection.DownloadTermReportTo` reads the live summary, requires completed
counts, compares artifact size and digest with the stream, verifies transferred
bytes, and runs `report.VerifyBundle` on the staging file. It never publishes.
Callers own staging, budgets, destination rules, and cleanup. MCP downloads
share one verification budget with per-call child scopes.

Keep interrupted transport and local I/O distinct from integrity failures.
A different valid packet still fails the summary binding. Transfer or
verification failure must not publish partial output.

CLI accepts relative destinations; MCP requires an absolute destination outside
the data directory. Both require explicit overwrite permission. Sync and
cleanup errors can occur after publication: MCP reports publication, durability
uncertainty, and cleanup failure separately. CLI recovery advises inspecting
the destination when delivery is uncertain.

Once create/revise receives a validated summary, later CLI errors retain its
ID and wrapped cause. A revision names the child. A final status-write failure
after delivery says the file was saved. An unavailable handle needs a new
report; downloading again cannot recreate it. Never replay a write to repair
local delivery.

## Inspect current evidence separately

CLI coverage and rendition windows use the daemon's executable-profile
inventory. Reports can use retained evidence under a configured profile omitted
from that inventory. A profile lookup failure therefore does not prove that
retained evidence is missing or justify starting processing.

Coverage submits exactly one requested version and preserves every returned
class counter. Stale or unavailable coverage is successful diagnostic output;
it does not promise that a strict report will succeed. The CLI gets the vault
ID through unfiltered audit status, avoiding `VaultInfo` blob statistics;
audit status is not a constant-cost identity endpoint.

Window discovery resolves the live node, checks the explicit version, selects
the profile's active rendition, and compares returned build and profile. An
explicit attachment skips summary discovery at offset zero and on continuation.
The store checks current/live identity and active attachment in one read
snapshot; sequential CLI reads do not freeze the source.

Unpinned discovery inherits catalog depth, path-byte and filename-character
limits and scans the traversable live tree. Pinned calls bypass discovery,
not current-version or profile checks. Report catalog-limit errors as such;
refreshing an unchanged selection cannot cure them.

Window offsets count Unicode scalar values in stored Markdown. The reader
scans the prefix to reach the offset; bounded output does not mean constant
seek cost. The shared client accepts Huma's `$schema` field while rejecting
other unknown fields and checking identity, UTF-8, and bounds. A partial window
does not independently verify the full artifact checksum. Use the full
`rendition get` stream for that verification.
