# CLI report recovery: author review

Reviewed design: [Inspect and recover search reports from the CLI](2026-10-01-cli-report-recovery-design.md).

- Source revision: `b04894976863bfb92c9211216e50f2fad0aff3e6`.
- Reviewed design SHA-256: `adb6732f72754e1f0871383d0263eb48d2795e7bc2ec679220a4e748d37f587d`.
- Current design SHA-256 after implementation and unavailable-handle advice clarification:
  `45b36e673663917421a5320239ec11d0d015e85eef04e6b1b3d58644e1c2dd5e`.
- During this review the only working-tree additions were this design and
  review. Product source, generated clients, dependencies, and existing tests
  matched the clean baseline. There are no Git submodules.
- Relevant versions in `go.mod`: Go 1.27.0, Cobra v1.10.2, Huma v2.38.0,
  generated-client library v3.75.15, and Kit v0.29.0. This proposal does not
  change dependencies or rely on new third-party behavior.

The design is implemented. This document preserves the author's pre-implementation
source check and disposition of the subsequent adversarial review. Test references
in that historical review were source inspections, not execution evidence.
The current design hash includes implementation status, baseline wording, and
the PR review clarification: unavailable handles advise creating a new report.
The original error code and exit status remain unchanged.

## Adversarial review disposition

The supplied review covered clean commit `a3d4183a`, with no High or Medium
findings, and found the design ready for implementation planning. At that
review, product source matched the recorded baseline. Its three Low notes resolve as follows:

- **Post-publication cleanup: accept conservative wording.**
  `cmd/docbank/report.go:93` joins staging cleanup into the return error after
  `publishGetFile` can have succeeded. `cmd/docbank/get_publish.go:23` names
  publication on a directory-sync failure, but a cleanup error need not do so.
  Use neutral recovery wording for every publication-helper error: delivery did
  not finish cleanly; inspect the destination before retrying. Preserve the
  underlying cause and any existing published-file message. A later status-write
  failure after a nil download result can positively say the file was saved.
  Do not add a publication-state API for this slice.
- **Data-directory destination restriction: defer.**
  `cmd/docbank/get.go:183` has no such check, so existing create/revise and the
  proposed download share that behavior. Native export checks the parent through
  `home.Layout.ContainsDirectory` in `cmd/docbank/export_download.go:27`; MCP's
  `validateExportDestination` also checks it. A consistent restriction would
  change existing CLI behavior and is not part of this approved recovery scope.
- **Existing dates ID preflight: defer.**
  `cmd/docbank/report.go:216` acquires the daemon before calling the client,
  whose `TermReportDates` validates the ID. The new commands validate first and
  return usage exit 2 as specified. Changing the existing dates command's exit
  behavior is optional adjacent work, not required for the new commands.

These dispositions clarify the reviewed implementation boundary without changing
the design's behavior. The earlier author findings and their design line numbers
below remain tied to `a3d4183a`.

## Findings

### High

No unresolved High finding in this author review.

### Medium

No unresolved Medium finding in this author review. The draft makes four
otherwise ambiguous boundaries explicit:

1. **History is not live availability** (design lines 81–112).
   `internal/api/routes_term_reports.go:121` reads vault-wide stored history;
   `internal/reporting/cache.go:386` rejects missing, expired, and differently
   owned handles. The proposed commands preserve that distinction rather than
   presenting a recorded `complete` state as a promise of a download.
2. **Continuation uses the number returned** (design lines 98–107).
   `internal/store/term_report_history.go:138` can stop before the requested
   limit. The byte threshold sums stored JSON, not the whole HTTP envelope.
   The CLI must not skip rows by advancing by the requested limit.
3. **A known report survives a local-delivery failure** (design lines 144–162).
   In `cmd/docbank/report.go:140` and `:249`, create and revise receive a
   validated summary before printing coverage and downloading. Later errors
   currently omit its ID. Preserving that known ID needs no new daemon state;
   identifying a run after an unusable write response is a different problem
   and is deliberately not promised here.
4. **Publication can precede an error** (design lines 137–156).
   `cmd/docbank/get_publish.go:23` reports directory-sync failure after making
   the file visible. `cmd/docbank/report.go:93` joins a deferred cleanup error
   after publication. The draft does not promise destination absence on every
   error or require a new publication framework to classify an uncertain case.

### Low

The CLI deliberately returns full stored requests in history JSON, including
large selections. The existing storage threshold and explicit paging keep this
bounded; a count-only history API would be separate scope. Explicit CLI limit
zero is rejected even though HTTP uses it as a default, and the draft calls out
that choice rather than attributing it to the daemon.

## Verified source contracts

| Claim group | Evidence at the source baseline |
| --- | --- |
| CLI gap and existing helpers | `cmd/docbank/report.go:118` registers create, verify, csv, dates, and revise; `:274` adds that set. `:19`, `:84`, and `:106` define coverage, publication, and verified download helpers. |
| Live summary read and ID shape | `internal/daemonconn/term_reports.go:19` checks 48 lowercase hex characters; `:32` validates live summary authority; `:68` reads and matches the requested ID. The proposed exported ID validator does not exist yet. |
| Ordered counts and receipt fields | `report/types.go:254` has five count fields; `:278` defines Summary with terms, coverage, dates, IDs, and artifact authority. No request or source text is needed for the proposed show output. |
| Generated history response | `internal/apiclient/client.gen.go:9695` exposes `ListTermReportHistory`; `:21164` has offset/limit query pointers; `:22069` aliases the response to `store.TermReportHistoryPage`. |
| HTTP page bounds | `internal/api/routes_term_reports.go:121` permits offset 0–100 and limit 0–50; limit zero becomes 20. No route change is needed. |
| Durable record scope | `internal/store/term_report_history.go:16` stores request and summary; `:39` sets 100 records and the 16 MiB threshold; `:77` clears date choices before saving. `:119` and `:138` own ordering and shortened pages. |
| Historical validation is distinct | `internal/store/term_report_history.go:42` checks receipt/request consistency without requiring live artifact authority. `internal/daemonconn/term_reports.go:32` additionally requires complete counts and artifact metadata. |
| Owners and unavailable handles | `internal/api/middleware.go:217` gives API-key callers the master owner; `:226` assigns the browser session owner. `internal/reporting/cache.go:386` enforces ownership and expiry; `internal/api/routes_term_reports.go:32` maps unavailable to HTTP 410. |
| Capacity and observation lifetime | `internal/reporting/cache.go:91` counts builders, entries, and owner pending work; `:145` admits revisions as new builds; `:220` derives expiry from observation plus 30 minutes. Summary and artifact acquisition do not start builds. |
| Shared download verification | `internal/daemonconn/term_report_download.go:23` reads the summary, refuses review state, compares the stream, verifies transfer bytes, then calls `report.VerifyBundle`. `internal/daemonconn/term_reports.go:136` caps the copy at advertised size plus one and preserves read errors. |
| File publication and output errors | `cmd/docbank/report.go:84` syncs and closes before `publishGetFile`; `cmd/docbank/get.go:183` resolves and validates the destination. `cmd/docbank/get_publish.go:14` owns replacement and no-replace publication; its sync failure explicitly reports publication. |
| Existing exit classes | `cmd/docbank/exit.go:46` preserves typed classifications and maps `daemonconn.ErrIntegrity` to 6. The problem-code map in `internal/daemonconn/receipts.go:367` has no special unavailable-report mapping, so it falls through to runtime exit 1. |
| JSON command convention | `cmd/docbank/json.go:10` writes through the JSON text encoder and wraps output errors. Existing commands use this helper; the proposed commands reuse it. |
| Isolated CLI test setup | `cmd/docbank/cli_test.go:39` runs the real Cobra root; `:71` selects a temporary Docbank home and starts the real daemon. `cmd/docbank/report_test.go:38` exercises create, offline verification, and CSV extraction. |
| Relevant existing regressions | `internal/store/term_report_history_test.go:14`, `:80`, and `:115` cover metadata round-trip, retention, and a shortened page. `internal/daemonconn/term_report_download_test.go:71` covers summary/packet mismatch and local failure categories. `internal/mcp/report_download_test.go:109` covers interrupted transfer and explicit retry. `cmd/docbank/get_test.go:102` covers concurrent no-replace publication. |

## Roadmap assessment

#726, #740, #749, and #753 now provide selected reports, native export CLI,
native export MCP, and report MCP respectively. The current CLI gap is visible
in its registered commands; closing it does not require reviving a closed PR
stack or designing another export engine.

`internal/mcp/report_workflow_test.go:19` connects exact selection, date review,
frozen report verification, and native export using synthetic text documents.
It checks that later source changes leave captured report bytes unchanged.
The history tests separately cover metadata export/import. These are useful
existing checks; they do not demonstrate a complete PDF processing, backup,
restore, and maintenance scenario. The umbrella's broader acceptance assessment
therefore remains open after this proposed CLI slice.

## Adversarial review prompt

Review the design at the hash and source revision above. Treat this ledger as
claims to challenge. Return severity-ranked findings with design line numbers
and source evidence; do not implement. In particular:

1. Does show ever imply that a recorded receipt is a live handle, or that a
   browser report belongs to the API-key owner? Is history pagination correct
   when its byte threshold shortens a page?
2. Can create/revise lose the known child ID or the original error category
   after receiving a validated summary? Does any recovery path accidentally
   repeat a write or claim that a lost response can be identified reliably?
3. Does the download reuse preserve summary binding, offline verification,
   interrupted-stream classification, overwrite rules, and truthful messages
   when publication precedes an error?
4. Is the proposed command/test scope sufficient for this operator outcome
   without adding another response type, endpoint, renderer, or file-delivery
   framework? Are any existing-capability claims incorrect?

## Verdict

Ready for implementation planning after adversarial review. No High or Medium
finding remains. The commands are not implemented, and the umbrella's broader
workflow acceptance assessment remains open.
