# Qualify selected reports and original exports in the browser

Status: implemented and locally qualified with CGO and pure-Go SQLite.
The dedicated CI job takes effect after merge, as described below.

Source baseline: `eec4756eeedf751e0f48af9e3d56c9ac7ea98ba8`, after #791.
Parent scope: [local document review and export, #719](https://github.com/kenn-io/docbank/issues/719).

## Outcome and scope

A local operator selects three documents in the browser, reviews an ambiguous
date, sees missing-text coverage, and downloads a report and the same selected
originals. The qualification checks the downloaded files independently of the
UI and proves that an excluded matching document stays excluded.

Use one small real-daemon browser case, added to the existing Playwright
harness. Keep product behavior unchanged unless the case reproduces a defect
inside this workflow. Explain each such defect and its correction in the
implementation review; a new feature or contract change requires a scope
decision first.

This choice follows the PDF recovery qualification (#766), plan inspection
(#774 and its correction), email-family qualification (#788), and CLI evidence
inspection (#791). The status text in #719 predates these results.

Existing browser evidence is useful but separate:

- `frontend/screenshots/search-export.screenshot.ts` creates a selected report,
  verifies its download, preserves the bytes after replacement, and rejects a
  stale rerun. Its fixture is searchable text without a date-review decision.
- `frontend/screenshots/export-drawer.screenshot.ts` exports a 1,001-member
  frozen query, checks historical original bytes, and independently reads its
  ZIP. It does not use the selected-report fixture or review dates.
- `frontend/src/TermReportDrawer.test.ts` covers date choices with mocked HTTP
  responses. `make frontend-test` type-checks the screenshot harness but does
  not execute these daemon-backed browser cases.

The new case joins selection, date review, coverage, and both downloads. It
does not repeat the 1,001-member query test or the PDF/family scenarios.

## Alternatives and boundaries

Family recovery through backup, restore, and maintenance remains important.
It is a separate qualification boundary: #788 exercises trash and frozen
reports, while #766 restores unrelated PDFs. Combining that work here would
mix browser behavior with a different storage lifecycle.

Minimal PDF redaction remains a new product scope. The closed production and
discovery stacks remain references, not an implementation queue. This browser
case does not declare #719 complete or authorize redaction work.

No new endpoint, processing profile, provider, dependency, schema, report
format, native-bundle format, export role, or client feature is planned. Do
not add report release, automatic processing, recursive attachment export,
or a general browser-test framework. Keep existing qualification cases.

## Synthetic sources

Create one private temporary vault and four source files through the branch
CLI. Use no configured processing profiles or hosted provider credentials.
The existing automatic plain-text extractor prepares the text during fixture
setup. Reporting and exporting must not request processing.

| File | Exact source bytes | Selected |
| --- | --- | --- |
| `one.txt` | `alpha. Document dated 2024-05-06.\n` | Yes |
| `two.txt` | `beta. Document dated 2024-05-06. Document dated 2024-06-07.\n` | Yes |
| `missing.bin` | Hexadecimal bytes `00 ff 00 ff 55 aa` | Yes |
| `excluded.txt` | `alpha beta. Document dated 2024-05-06.\n` | No |

Names and virtual paths contain neither search term. Record each node ID,
current version ID, hash, and size through `stat --json`; independently hash
and retain the fixture byte arrays for later comparisons.

Before opening the workflow, wait within a deadline for `search alpha --json`
to return exactly `one.txt` and `excluded.txt`, and for `search beta --json`
to return exactly `two.txt` and `excluded.txt`. Match identities, not just
hit totals. This setup waiting is not a retry of a report or export write.
Assert that `missing.bin` has no searchable text; its bytes are deliberately
neither UTF-8 nor plain text. Do not manufacture search rows or report frames.

Use the daemon-issued `web --no-browser` session and the actual embedded web
application. Keep report timezone UTC. The existing default inclusive cutoff
range starts at 2000-01-01 and ends on the browser's current UTC day; verify
that it includes both literal fixture dates and the vault-addition date.
Do not freeze the daemon clock or change stored dates. All four documents are
unrelated single-document families.

## Browser workflow and expected results

### Strict missing-text refusal

Select only `missing.bin`, open **Report selected documents**, and enter the
term `alpha` with strict coverage. Create the report through the visible
control. Assert `incomplete_coverage` from the actual response and a visible
error. There is no counts table, evidence-download control, successful report
handle, or added history row. Compare the complete parsed responses from
`search-export history --json` before and after. History is vault-wide, so the
CLI's API-key read can observe these browser-created receipts.

This refusal has no ambiguous date: the missing file uses its vault-addition
date. Do not expect the three-member ambiguous request to fail strict coverage
before review; the reporting service resolves review requirements first.

### Export the selected originals

Close the strict-refusal drawer, clear selection, and select `one.txt`,
`two.txt`, and `missing.bin` using the document checkboxes. Keep `excluded.txt`
visible and unselected. Use **Export selection**. Check membership against
the original recorded tuples; a same-sized but different selection is not
acceptable.

Use the drawer's existing originals-only defaults, preview, and start through
the UI. Assert that the real source and plan contain the three selected
versions, exactly three original outputs, and no optional or attachment roles.
Save the shown plan fingerprint and completed job receipt. Download the ZIP
with **Download verified ZIP** and independently compare its size and SHA-256
to the receipt. Reporting's missing text must not prevent original-byte export.

One export job is sufficient. The case need not add a browser release control:
it uses its own disposable browser owner and vault, below the two-job owner
limit. Stop and remove that vault at teardown. The successful report and its
revision use two of the browser owner's eight report slots.

### Exact selection and date review

Close the export drawer and keep the same three documents selected. If panel
navigation clears selection, reselect them and compare their identities to
the admitted export. Open **Report selected documents**, set terms `alpha` and
`beta`, and choose **Available text only — show gaps**.

Observe the real create request. Its selected identities must equal the three
recorded tuples, without an all-documents or collection scope. Do not supply
this request through test code in place of clicking the UI.

The first report is `needs_review`, with one unresolved document. The browser
withholds counts and downloads. Open **Review dates**, locate `two.txt` by the
document identity shown in the review UI, and select its content candidate
for 2024-05-06. Enter the reason `Use the first stated document date.` and
click **Create reviewed revision**. Check that the submitted choice contains
the observed candidate ID, document identity, and evidence hash, with action
`select`. Do not choose a candidate by its array position.

The completed child report has a different handle. Check the parent's live
summary with a same-origin `fetch` inside `page.evaluate`, requesting
`GET /api/v1/search-exports/{parent-id}` and sending `X-Docbank-Web-Session`.
Take that token from the issued URL's `web_session` fragment before navigation
and keep it in memory. Require HTTP 200 and state `needs_review`. The CLI's
API-key summary read has a different owner and cannot perform this check.
The expected counts, in the existing column order, are:

| Term | Hits | Hits Plus Family | Unique Hits | Documents in unique families | Unique Hits Plus Family |
| --- | ---: | ---: | ---: | ---: | ---: |
| alpha | 1 | 1 | 1 | 1 | 1 |
| beta | 1 | 1 | 1 | 1 | 1 |

Overall and both rows have coverage: 3 scoped, 2 searchable, 1 missing text,
0 incomplete families, and 1 fallback date. The missing file remains a packet
member with missing search evidence; zero keyword hits must not imply searched
text. `one.txt` automatically selects its content date; `two.txt` records the
reviewed selection and reason; `missing.bin` selects its vault-addition date.

Assert the visible counts and coverage as well as the response. Download the
evidence ZIP and CSV through their browser buttons and save both via Playwright
download events. Treat a failed or canceled download as failure.

### Frozen report and stale rerun

Keep the completed report drawer open. Export ran first so this check needs
no second tab or recovery of a closed drawer's live handle. History stores
requests and summaries, not a way to reopen the original live artifact.

After both artifacts are saved, replace `/one.txt` through the real CLI with
`Replacement content. Document dated 2024-05-06.\n`. Confirm a new current
version and hash. Download the still-open completed report again through the
browser and require byte equality with the original ZIP.

Use the completed report's history request as a draft, then create it again
without refreshing its fixed document identities. Assert HTTP 409
`report_selection_changed` and the visible refresh-and-reselect advice. No
success handle or history row is added; compare CLI history as for the strict
refusal. Do not silently choose the new head.

This assertion is specific to report admission. Native exports may select
retained historical versions; the test must not impose current-version-only
semantics on exports.

## Independent artifact checks

Use the existing CLI report verifier and CSV extractor for the downloaded
packet. Also parse ZIP, JSON/JSONL, and CSV with independent readers, following
the Python standard-library approach already used in the export screenshot
case. Do not call the report calculator to produce the expected result.

Require exactly the existing five report entries: `manifest.json`,
`members.jsonl`, `families.jsonl`, `dates.jsonl`, and `hits.csv`. Compare the
member identities, literal counts, coverage, and date selections above.
Confirm singleton grouping from the members and an empty `families.jsonl`
relation list; that file stores edges, not one record per family. There is no
relationship to `excluded.txt`.
Compare parsed rows from the browser CSV, packet CSV, and CLI-extracted CSV
against the same literal values. No new CSV transport-proof protocol is added.

For the native bundle, independently read `bundle.json`, `metadata.csv`, and
`SHA256SUMS`. Require exactly the three original entries plus the format's
metadata entries. Compare every original's complete bytes, hash, and size to
the fixture, and each document's identity to the selected report members.
Verify the manifest checksums, member hash, and plan fingerprint using the
existing export case's independent check. Excluded bytes and identities must
not appear as another document or output.

Re-run offline report verification after stopping the daemon. Re-read the saved
original entries then as well. This proves saved-file independence; it does
not claim independent source authenticity or restored live-handle persistence.

## Execution, isolation, and retained evidence

Add a focused command, proposed as `make report-export-browser-test`, that
builds the frontend once and exercises this one Chromium case with both
`CGO_ENABLED=1` and `CGO_ENABLED=0` branch binaries, always with `-tags fts5`.
The runner passes each absolute branch-binary path through
`DOCBANK_SCREENSHOT_BINARY`, the override already used by
`similar-documents.screenshot.ts`. The new case requires this variable when
enabled and invokes that binary for every CLI operation, including daemon
startup and shutdown. There is no fallback to the repository's `docbank` file
or an installed binary. `make build` currently forces CGO on, so setting an
environment variable around that target does not establish the pure-Go run.
Each mode gets its own temporary vault.

Reuse `frontend/screenshots/playwright.config.ts` and a new focused
`.screenshot.ts` case. Use `DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR` as its opt-in
gate and capture directory, following the existing report screenshot pattern.
The runner sets it to a separate directory under `.superpowers/` for each
SQLite mode. Without the gate, normal documentation screenshot generation
skips this case and does not change the published image set. The dedicated
invocation must execute the case, with no skipped qualification. Keep one
worker and bounded readiness/polling deadlines.

Add one Linux/Chromium job to `.github/workflows/ci.yml` using the repository's
existing Go/Node setup and pinned Playwright installation. The docs job already
installs Chromium through the pinned Playwright CLI. No new cross-browser or
operating-system matrix is proposed. Linux browser results do not establish
native macOS or Windows browser coverage; existing native Go checks continue.

The PR dispatcher in `.github/workflows/ci-pr.yml` always calls
`kenn-io/docbank/.github/workflows/ci.yml@main`. Therefore this new job will not
run on the implementing PR; it first takes effect when the workflow merges to
main, whose push event runs it. Later PRs use the updated main-pinned workflow.
Do not change that dispatch policy to obtain pre-merge evidence.

Before merge, run `make report-export-browser-test` locally and record the
tested commit, both SQLite modes, executed-case counts, and outcomes in the
handoff. An existing green PR check does not demonstrate the new browser job.
Distinguish the locally verified command from its not-yet-executed CI wiring.
Recommend that the maintainer inspect the first main run after merge; do not
watch or poll it without an explicit request, as required by `AGENTS.md`.

The harness may use CLI/API operations to seed files, wait for readiness, read
evidence, replace the source, and stop the daemon. Selection, report creation,
date revision, export preview/start, and downloads must use the actual browser
controls. Do not mock responses, create reports through a helper, or inject
internal Svelte state. Start event/response waits before the corresponding click.

Disable telemetry as the existing Playwright configuration does. Confine all
application data, config, runtime records, and downloaded artifacts to the
synthetic workspace. Keep browser tokens out of screenshots and logs. After
the workflow, stop the daemon and confirm it is stopped before reading the
temporary database. From the TypeScript harness, invoke `python3` with Python's
standard-library `sqlite3`, using `sqlite3.connect(uri, uri=True)` and a file
URI with `mode=ro&immutable=1`. Build the URI with `Path.resolve().as_uri()`;
the database path is a separate process argument, not interpolated Python or
shell code. Query `rendition_jobs` and `embedding_jobs`, require zero rows in
both, and close the connection. This matches the stopped-vault check in
`document_inspect_fixture_test.go` without a helper binary or new dependency.
Automatic plain-text extraction during setup is expected and is not a provider
job. This is not an external-network isolation qualification.

Remove temporary vaults, source files, binaries, and downloads on success or
failure. On shutdown failure, report the exact retained scratch path and process
rather than opening or deleting an owned database. Preserve only synthetic
review captures under `.superpowers/`, including date review, completed counts,
and export-ready state. Inspect them before attaching them to the PR. Do not
change the documentation asset pin or publish the docs site.

The implementation evidence must distinguish the new browser result from the
older CLI/MCP/PDF/family qualifications. Re-run relevant frontend checks and
both SQLite Go suites for any product correction; report observed results,
including a browser failure rather than calling a skipped case a pass. The
local qualification has passed in both SQLite modes; CI activation remains
conditional on merge.

Family recovery through maintenance, browser attachment-family selection,
portable-profile inspection, larger report capacity, and minimal PDF redaction
remain outside this slice. This document defines the acceptance contract;
actionable work and its status belong in Kata.
