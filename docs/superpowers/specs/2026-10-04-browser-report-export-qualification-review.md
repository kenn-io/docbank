# Author source review: browser report and export qualification

Status: implemented and locally qualified after external approval of `3337924c`.
Both SQLite modes executed one passing Chromium case with no skips. The new
CI job has not run; the main-pinned dispatcher activates it after merge.

Design: `2026-10-04-browser-report-export-qualification-design.md`.
Design SHA-256:
`8c1e2ecbf19662da8aec3ba11e414350bb13a3bee66964e68520f8a1143b165d`.
Source baseline: `eec4756eeedf751e0f48af9e3d56c9ac7ea98ba8`, merged #791.
This slice changes no product code, dependencies, or generated clients. The
branch also incorporates newer main changes; final qualification uses that
combined source, with its exact commit recorded in the PR handoff.
The initial author pass reviewed the two then-untracked specification documents.
The 2026-10-05 recheck compared against `f5ba5c8a`; only these two documents
changed, and the source baseline remains unchanged.
The repository has no gitlinks. Playwright is pinned to 1.61.1 in
`frontend/package.json`; no package change is proposed.

## Scope decision

The remaining issue #719 acceptance work includes browser integration and
family recovery through maintenance. The existing browser cases already cover
important report and native-export behavior separately; this proposal adds
one small combined case and makes it repeatable in PR CI. It is not a new
feature program or a claim that all umbrella acceptance evidence is complete.

The four-file fixture uses existing native text extraction, avoiding a second
cross-language rendition publisher. PDF rendition behavior and email families
retain their existing Go qualifications. Family backup/restore and maintenance
remain a separate decision, as does minimal PDF redaction.

## Findings

The external review found one Medium that the original author pass missed:
the new CI job cannot run on the implementing PR because the dispatcher uses
the workflow on main. Direct inspection of `.github/workflows/ci-pr.yml:13`
confirms the pinned reference; `.github/workflows/ci.yml:5` supplies the main
push trigger. The design now requires local runs in both SQLite modes before
merge and separates that evidence from CI activation after merge. Watching the
first main run still requires an explicit maintainer request.

The three Low clarifications are also resolved:

- Live parent reads use a same-origin browser fetch with the existing session
  header. History comparisons use the CLI because history is vault-wide.
- `DOCBANK_SCREENSHOT_BINARY` names the absolute branch binary, and
  `DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR` enables the new case and names its
  capture directory. An enabled case cannot silently use another binary.
- The stopped database is read through Python's standard-library `sqlite3`
  using a read-only, immutable URI, without a Go helper or dependency.

No unresolved High or Medium source-contract findings remain in this recheck.
The source-review findings were resolved before implementation. Local
browser evidence is recorded below.

Two ambiguities were removed during drafting:

- Export runs before report creation. The final report drawer can then stay
  open during replacement and a second download. Closing it would discard the
  active handle; history's **Use as draft** creates a new observation.
- `families.jsonl` contains relationship edges. Three unrelated members do
  not imply three rows in that file; the contract requires an empty edge list
  and singleton grouping in the member evidence.

## Verified source claims

| Claim | Source evidence at the baseline |
| --- | --- |
| Selected-report browser proof is opt-in and already verifies a frozen download and stale rerun. | `frontend/screenshots/search-export.screenshot.ts:13`, `:77`, `:88`, `:99` |
| Native-export browser proof is opt-in, uses 1,001 members, and checks downloaded ZIP contents independently. | `frontend/screenshots/export-drawer.screenshot.ts:13`, `:17`, `:111` |
| The frontend check type-checks screenshot code without running these application workflows. | `Makefile:59`; `frontend/package.json` scripts; `.github/workflows/ci.yml:67` |
| PRs call the main-pinned workflow; the new job first takes effect after merge. | `.github/workflows/ci-pr.yml:13`; `.github/workflows/ci.yml:5` |
| The existing screenshot runner builds and launches branch code, and the Playwright configuration has one worker and disables telemetry. | `frontend/screenshots/run.mjs`; `frontend/screenshots/playwright.config.ts` |
| An existing case accepts `DOCBANK_SCREENSHOT_BINARY`; the new gate is a proposed addition. | `frontend/screenshots/similar-documents.screenshot.ts:15`; `frontend/screenshots/search-export.screenshot.ts:12` |
| `make build` hard-codes CGO on. | `Makefile:26` |
| Live summaries require the owning browser token; history is not filtered by owner. | `frontend/src/api-transport.ts:32`; `internal/api/routes_term_reports.go:121`, `:149`; `internal/store/term_report_history.go` |
| The selection dock exposes both report and export actions. | `frontend/src/SelectionDock.svelte`; `frontend/src/App.svelte:1468`, `:1481` |
| The report draft copies selected identities and defaults to UTC and dates from 2000 through today. | `frontend/src/TermReportDrawer.svelte:20`, `:23`, `:26` |
| Review choices are made against candidate IDs, document identities, and evidence hashes; a reason is required. | `frontend/src/TermReportDrawer.svelte:211`, `:225` |
| The review UI identifies a document by node ID, not filename. | `frontend/src/TermReportDrawer.svelte:344` |
| Review-pending reports withhold counts and download buttons; the completed report has browser CSV and ZIP controls. | `frontend/src/TermReportDrawer.svelte:328`, `:336` |
| The export drawer defaults to original required, other roles omitted, flat packaging, and duplicate preservation. | `frontend/src/ExportDrawer.svelte:17` |
| Two different equally preferred content dates require review; the content document date outranks vault addition. | `report/dates.go:38`, `:73`, `:116`, `:124` |
| Native text is scanned by the date extractor; no configured profile selects the existing native-text path. | `internal/api/routes_collection_quality.go:22`; `internal/store/term_report_frame.go:133`, `:344`; `report/content_dates.go:34` |
| Binary bytes containing NUL and invalid UTF-8 cannot become a successful plain-text extraction. | `internal/extract/worker.go:168` |
| Captured files begin with a vault-addition candidate and missing search state. | `internal/store/term_report_frame.go:277` |
| Date review precedes strict incomplete-coverage refusal. | `internal/reporting/service.go:196`, `:219`, `:229` |
| Missing text is counted separately; singleton family counts for one exclusive hit are one in every column. | `report/counts.go:116`, `:125`, `:168`, `:216` |
| Incomplete coverage returns 422; changed report selection returns 409. | `internal/api/routes_term_reports.go:37`, `:53` |
| Report packets have five fixed entries; family JSONL serializes relations. | `report/bundle.go:28`, `:243`, `:265` |
| Two report handles and one job fit current owner capacity. | `internal/reporting/cache.go:92`; `internal/store/export_jobs.go:117` |
| There is an existing stopped-vault check for zero rendition and embedding jobs. | `cmd/docbank/document_inspect_fixture_test.go:100` |

The literal results follow from three selected singletons: one matches only
alpha, one only beta, and the missing file matches neither. The excluded file
matches both terms during fixture readiness but does not enter either output.
The two text dates and the missing file's import date must fall inside both
term ranges for the stated coverage numbers to hold.

## Implementation evidence and limits

`make report-export-browser-test` completed locally with the implementation
based on `b74a0d51` and the uncommitted runner/CI changes. CGO and pure-Go each
executed one case, passed it, and skipped none. The final committed source and
its repeated two-mode results are recorded in the PR handoff.

The browser case exercised the exact selection, strict refusal, ambiguous-date
revision, unchanged parent, coverage/counts, original download, frozen report,
and stale rerun. Python's independent readers checked all three original byte
streams and the report evidence/CSV before and after daemon shutdown. The
stopped database contained no rendition or embedding jobs. No product change
was needed. Empty family IDs designate the three unrelated singletons; they
are not three shared-family identifiers.

The parent read uses the generated URL builder and a same-origin browser fetch
with the original session header. The database URI appends
`?mode=ro&immutable=1` to `Path.resolve().as_uri()` after rejecting a nonempty
WAL. Python runs with `-E` to keep assertions enabled. Missing or relative binary
overrides fail before vault creation. Review captures show the real export,
date-review, and completed-report states with synthetic data only.

This is local Linux Chromium evidence. It does not establish CI execution,
native macOS/Windows browser coverage, family recovery, or processing-provider
behavior. The maintainer should inspect the first main run after merge; no CI
monitoring was performed. The broader frontend suite also exposed two
unchanged `app-opened.test.ts` setup failures under the host's Node 26.10.0
(`localStorage` is undefined); both pass under CI's Node 24 line. This
qualification does not change that unit-test environment.

The Python URI form was exercised on a disposable synthetic database whose
path contains a space. It returned the expected row after the writer closed.
That probe confirms the proposed standard-library invocation, not the new
browser case or the CI job.
