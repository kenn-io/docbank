---
title: "MCP search reports: author review"
description: "Author review of the frozen MCP search report design and its exact selection and date paging boundaries."
last_edited: 2026-10-01
---
# MCP search reports: author review

Reviewed design: [Frozen search reports through local MCP](2026-10-01-mcp-search-reports-design.md).

- Source revision: `3a23e5e61f26c0b8aef33582adaf2bbd7717f2a7`.
- Approved design at `ce27b956`, SHA-256: `5a7acf406816af99c29597801437ca292c064072a8dbfe0da36e88c03bee007e`.
- Relevant source, dependencies, generated clients, and Git index matched that
  revision during review. Only this design and review were added on disk.
  There are no Git submodules. The committed documents make this input
  reconstructible without a dirty source snapshot.
- Dependency versions read from `go.mod`: Go 1.27.0, Huma v2.38.0,
  MCP Go SDK v1.7.0, and jsonschema-go v0.4.3.

This records the author review and disposition of the subsequent adversarial
review before implementation. The feature is now implemented; the source claims
below describe the historical review baseline, not a current test report.
The design status banner was updated after implementation.

## Adversarial review disposition

Reviewed commit: `379160028215a0219d1ce6b610f98b30dfa1efd2`. Its design hash was
`e25918c3aa62ce3589fedd59d9c68730df93f2d2fa866f774a359e7cc8ec538e`.
That review used the same source baseline. The reviewer found no High issues and one Medium
clarification; the following dispositions apply to the revised design hash above.

- **Medium, shared pager: accepted.** `internal/reporting/cache.go:486` appends
  a member, then trims candidates. At `:500` or `:509`, failure to fit another
  candidate returns a limit error even when earlier entries could be returned.
  This is a source-backed path in the shared web/CLI/MCP pager. The design now
  requires the correction with omitted and zero `max_bytes`, and a default-size
  regression that returns earlier members with a usable continuation cursor.
  This review did not newly reproduce that path at runtime.
- **Low, revision message size: accepted as documentation.** Keep the existing
  per-field bounds and 1 MiB MCP envelope limit. Document caller-chosen batching,
  the additional handle consumed by each batch, unchanged expiry, and the
  existing 8 MiB HTTP/CLI alternative. Do not split a write automatically.
- **Low, more than 256 candidates: not accepted.** The text extractor has a
  256-candidate limit, but `internal/reporting/service.go:163` also checks the
  combined member list after metadata, fallback, and extracted candidates
  have been assembled. Lines 166–167 reject more than 256 before a frame is
  admitted to the cache. The design now identifies that combined limit.
- **Low, explicit release: deferred.** The eight-handle capacity and fixed
  expiry stay explicit. A report-release operation can be separately scoped
  if actual iterative use demonstrates the need; it is not included here.

The author findings below record the initial drafting pass. Their design line
numbers refer to `37916002`; source line numbers still match the baseline.

## Findings

### High

No unresolved High finding in this author review.

### Medium

No unresolved Medium finding in the final draft. The following issues were
resolved while writing it:

1. **Date pages need a byte budget at their existing owner.** Design lines
   159–183 now specify an optional `max_bytes` request field and a complete-page
   limit including the cursor. `internal/reporting/cache.go:436` currently
   limits members and candidates, then measures a page against 1 MiB before
   assigning its next cursor. `internal/mcp/read_tools.go:798` includes both
   structured content and JSON text. A smaller member limit alone does not
   address one large member. The proposed pager must return a partial page
   when the next candidate will not fit; the current loop can instead return
   a limit error after consuming zero candidates in a new item. This change
   belongs in the shared pager, with existing defaults retained.
2. **Terms alone cannot bound a full report summary.** Design lines 126–148
   now give create/revise a compact handle receipt and a separate bounded
   summary read. `internal/store/term_report_families.go:61` traverses connected
   families beyond selected membership; `:222` adds a warning for each shared
   child. `internal/reporting/cache.go:295` permits up to 8 MiB of summary JSON.
   Consequently, restricting selected members or expression length cannot
   ensure that the full summary fits MCP. No warning is silently truncated.
3. **Revisions consume capacity and do not renew the observation.** Design
   lines 209–222 explain eight shared API-key-owner slots and the possible
   initial-report-plus-seven-revisions limit. `cache.go:79` counts handles
   and pending work, `:145` starts another build for each revision, and `:220`
   derives expiration from the original observation. Waiting for expiry is
   explicit; this design adds no release mechanism or larger quota.
4. **Post-write validation is not an admission failure.** Design lines
   230–247 require report-specific unknown-outcome handling for unusable write
   replies. `internal/daemonconn/term_reports.go:52` and `:100` can reject a
   summary after the HTTP operation returns. The current single-attempt helper
   handles transport/decode/cancellation, but its generic sanitizer does not
   retain an arbitrary local validation error's identity. Implementation must
   make that classification explicit, including compact-result encoding errors.

### Low

The design intentionally leaves large summary inspection to HTTP when the
complete MCP result would exceed 1 MiB. Dates, revisions, and ZIP delivery
still work with that handle. Adding paged summary warnings would be a separate
contract; it is not implied by this specification.

## Verified

Twenty-four claim groups checked against the baseline. Proposed additions are
identified as changes rather than existing capabilities.

| Claim | Source evidence |
| --- | --- |
| Report tools and their opt-in are additions; native exports already exist | `internal/mcp/tools.go:153` builds the catalog from current options; `internal/mcp/export_tools.go:21` defines export writes. No report definitions are present. |
| Both transports receive one options-based catalog | `cmd/docbank/mcp.go:49` passes options before choosing stdio or HTTP; `internal/mcp/protocol.go:36` defines options and `:64` registers catalog/middleware. No new positional bool chain is needed. |
| Closed schemas and validation before handlers are established patterns | `internal/mcp/schemas.go:38` closes objects, `:84`/`:91` own UUID/hash shapes; `internal/mcp/tools.go:224` validates arguments before dispatch. The proposed scalar/object schemas exclude null. |
| Complete results have a separate limit from payloads | `internal/mcp/read_tools.go:798` emits structured content plus JSON text and checks their encoding; `internal/mcp/protocol.go:95` adds metadata and reserves SDK result-type space. `internal/mcp/schemas.go:22` sets 1 MiB. |
| Current identity discovery and hash lookup already exist | `internal/mcp/read_tools.go:532` requires node/version for `get_document`; `:604` implements version paging and maps `BlobHash`. List/search results supply the initial node/version pair. No original download is needed to obtain hashes. |
| Request normalization owns exact scope and term semantics | `report/request.go:29` validates version, scope, profile, timezones, terms, and choices; `:127` validates selected identities and sorts a clone by node. The proposed 1,000-member MCP creation ceiling is smaller than the engine's 50,000 limit. |
| Choice IDs and allowed actions have existing owners | `report/date_fields.go:130` and `report/content_dates.go:218` produce lowercase SHA-256 candidate IDs. `report/request.go:166` validates choice shape, reason bytes, action-specific fields, and reviewed roles. |
| Stale selection differs from a bad hash | `internal/store/term_report_frame.go:75` checks availability for the whole selected set before hashes; `internal/api/routes_term_reports.go:17` maps changed selection to conflict and invalid selection to request error. |
| Capture and final calculation do not recapture during revision | `internal/reporting/service.go:41` prepares the frozen frame; `:178` calculates using captured evidence. `internal/reporting/cache.go:145` shares the parent's frame and merges decisions by document. |
| Coverage is not packet membership | `report/counts.go:125` skips unusable dates before counting coverage; `:136` applies row date ranges. `service.go:178` distinguishes ambiguity, missing dates, and strict search/family coverage failures. |
| Compact write receipts avoid a known response-size boundary | `internal/store/term_report_families.go:61` traverses connected families; `:222` emits per-child warnings. `report/types.go:278` includes warnings via coverage in Summary; `cache.go:295` allows an 8 MiB summary. The compact receipt is a new MCP projection. |
| Report IDs, cursor bindings, and live reads exist | `internal/reporting/cache.go:311` generates 24 random bytes encoded as 48 hex characters; the cursor codec binds owner, report ID, member, and candidate. `internal/daemonconn/term_reports.go:67` and `:82` read summary/date pages. |
| The pager needs an additive change, not a new endpoint | `report/types.go:297` currently has only cursor and limit. `cache.go:436` pages up to 100 members and 1,000 candidates, with candidate continuation. `internal/api/routes_term_reports.go:155` accepts that type on the existing dates route. `internal/apiclient/client.gen.go:22433` aliases the shared Go type. |
| An omitted byte limit can remain optional without a nullable pointer | Huma v2.38.0 `schema.go:880`–`:897`, read from the module cache, makes a JSON `omitempty` field optional. Range validation belongs in the shared pager and connection so `ErrReportLimit` reaches the existing 413 mapper; no schema range tag is required. |
| CLI/MCP ownership is shared, browser ownership is separate | `internal/api/middleware.go:204` authenticates API-key calls and assigns the same master report owner; the browser path assigns its session owner. `routes_term_reports.go:69` retrieves that owner for cache operations. |
| Capacity includes revisions and pending builds | `internal/reporting/cache.go:79` enforces two builders globally, 64 entries/builders globally, and eight owner entries/pending builds. `:145` admits revision as another build. These limits differ from native export job slots. |
| Expiration and history do not preserve artifacts | `cache.go:220` anchors all descendants to observation plus 30 minutes; `:555` acquires existing in-memory artifacts. `internal/store/term_report_history.go:77` clears reviewed choices and keeps request/summary records, capped at 100; `:108` provides history pages bounded at 16 MiB. |
| Retry helpers distinguish reads and writes | `internal/mcp/daemon.go:223` retries a read only before response start; `:271` attempts a write once. `:296` sanitizes daemon failures and preserves recognized problem facts, not arbitrary local causes. |
| Existing client operations validate replies after writing | `internal/daemonconn/term_reports.go:52` creates, `:100` revises, and `:30` validates summaries. The new tool must distinguish those post-write errors from invalid arguments. No idempotency identity is supplied by these operations. |
| Existing stream verification checks length/hash, but does not compare a summary | `internal/daemonconn/term_reports.go:125` limits the copy to size plus one and checks its hash; `:140` opens authenticated CSV/bundle streams. `cmd/docbank/report.go:106` verifies the downloaded packet. The new shared helper adds the summary comparison and preserves error categories. |
| Packet verification already accounts for in-memory bytes | `report/verify.go:230` reserves and reads the archive, accounts for decoded payloads, and returns internally-consistent true/source-verified false at `:359`. `report/budget.go:53` creates a shared budget; child scopes own reservations and release them on Close. Per-server sharing is a proposed MCP addition. |
| Destination checks and publication already have the needed outcomes | `internal/mcp/bates_tool.go:374` validates the destination; `internal/filepublish/publish.go:26` returns published true on later directory-sync failure, `:60` creates a private stage, and `:91` reports cleanup errors. `internal/mcp/export_download.go:43` demonstrates the local-file flow and cause-preserving errors. |
| Report downloads use ordinary API authentication and different time budgets | `internal/api/routes_term_reports.go:254` registers direct CSV/bundle GETs; browser ticket issuance is separate. `internal/api/middleware.go:52` exempts these GETs from its 60-second timeout. `internal/mcp/http.go:34` supplies the two-minute HTTP request deadline. `report/budget.go:14` owns the 60-second report-build limit. |
| Domain classification and local logging need explicit report additions | `internal/api/routes_term_reports.go:17` owns report problem codes; `internal/mcp/tools.go:372` currently lacks those mappings. `:565` logs local causes only for the existing export I/O category. Reusing the message text alone would omit report causes. |

Existing checks inspected as implementation references:
`internal/api/routes_term_reports_test.go`,
`internal/daemonconn/term_reports_test.go`,
`internal/reporting/cache_test.go`,
`internal/mcp/export_tools_test.go`, and
`internal/mcp/export_download_test.go`. They are not evidence that the proposed
MCP report tools already work. No product source or tests were changed for this
specification.

## Adversarial review prompt

Review the design at the exact hash and source revision above. Treat this
ledger as a set of claims to challenge. Return severity-ranked findings with
design line numbers and direct source evidence; do not implement or broaden
the scope. In particular:

1. Does every field needed for current-version selection and reviewed choices
   come from the existing MCP/daemon surfaces? Are report IDs, candidate IDs,
   document versions, and parent/child cursors kept distinct?
2. Does the proposed byte-aware pager preserve every candidate, make progress,
   include cursor overhead, and fit the full escaped MCP response? Are default
   HTTP/CLI behavior and invalid-limit error codes stated accurately?
3. Can a valid large family graph prevent delivery of a newly created handle?
   Does compact receipt projection solve that without hiding summary evidence
   or promising that all summaries fit MCP?
4. Can a revision renew expiration, replace the parent's choices, or evade
   the shared eight-handle limit? Are capacity and unrecoverable lost-response
   identities documented honestly without adding a hidden release subsystem?
5. Does any write replay after reconnect/cancellation, or classify a reply
   validation failure as a known admission failure? Does the proposed reuse
   account for what generic daemon sanitization discards?
6. Can a different valid ZIP, malformed packet, publication race, cancellation,
   or cleanup failure yield an incorrect saved-file claim? Does the shared
   verification budget have bounded lifetime on every exit?
7. Is any proposed wrapper, endpoint, storage change, schema restriction, or
   test unnecessary for this one local operator workflow? Identify the smaller
   existing mechanism if so.

## Verdict

Ready for implementation planning after the accepted pager clarification.
No unresolved High or Medium item remains from the supplied adversarial review.
The rejected candidate-limit note has direct source evidence above.
