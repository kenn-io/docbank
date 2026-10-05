# Author source review: browser report and export qualification

Status: author review of a proposed qualification; no implementation or browser
run is claimed. The design is ready for independent adversarial review.

Design: `2026-10-04-browser-report-export-qualification-design.md`.
Design SHA-256:
`109cae7792b4421fb7ad1f0c26b3ca07b2d7ab2888d738ed5ee04d287d80179b`.
Source baseline: `eec4756eeedf751e0f48af9e3d56c9ac7ea98ba8`, merged #791.
Source, dependencies, and generated clients are unchanged from that baseline.
The two new specification documents were untracked during this author review.
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

No unresolved High or Medium source-contract findings in this author pass.
The proposed browser case has not run, so this is not a behavioral verdict.

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
| The existing screenshot runner builds and launches branch code, and the Playwright configuration has one worker and disables telemetry. | `frontend/screenshots/run.mjs`; `frontend/screenshots/playwright.config.ts` |
| `make build` hard-codes CGO on. | `Makefile:26` |
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

## Review limits

This pass inspected source contracts and the existing harness. It did not run
the proposed fixture, inspect rendered UI, or measure added CI duration. The
implementation must demonstrate those outcomes, including both SQLite modes.
Browser selectors and fixture MIME/search readiness remain executable checks,
not assumptions to bypass if they fail.
