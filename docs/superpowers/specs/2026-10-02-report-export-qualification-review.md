# Retained PDF workflow qualification: author review

Reviewed design: [Qualify retained PDF reports, native exports, and restore](2026-10-02-report-export-qualification-design.md).

Status: implemented. The initial source review below is historical; execution
found a content-date defect that it missed. See the implementation evidence at
the end for that correction and the retained-PDF qualification results.

## Initial source review

- Source revision: `2d100a274a548a9eb1e7f9aaef12eab478c78a1f`.
- Design SHA-256: `a1bd8500e394bb80c47f658fbed3adcc6629e609d9338f16f5628f841e2ee219`.
- The design was reviewed as an uncommitted addition. Only the design and this
  review were added; product source, existing tests, generated clients, and
  dependencies match the clean baseline. There are no Git submodules.
- Relevant dependency versions: Go 1.27.0, fpdf v0.9.0, pdfcpu v0.15.0,
  Huma v2.38.0, Cobra v1.10.2, and Kit v0.29.0, from `go.mod`.

This was the author's source check, not an independent adversarial review. At
that point the design was proposed. Only the existing component tests had run;
the combined PDF and restored-daemon scenario had not been implemented.

## Findings

### High

No unresolved High finding in this author review.

### Medium

No unresolved Medium finding in this author review. The draft explicitly
resolves the following scope and contract questions:

1. **Retained evidence is the starting point** (design lines 26–32, 84–104).
   `cmd/docbank/rendition_runtime.go:80` registers plaintext, EPUB, and Docling
   ASR, not the PDF client in `document/docling/client.go:88`. A fresh-PDF
   extraction scenario would require additional runtime work. The fixture
   instead publishes real retained rendition artifacts before daemon ownership.
2. **Restore requires local profile provisioning** (design lines 196–200).
   `internal/backupapp/app.go:162` excludes `config.toml`.
   `internal/api/routes_collection_quality.go:22` computes report coverage
   selection from configured portable profiles. The restored test must supply
   the same profile without inventing an inherited runtime configuration.
3. **History cannot preserve a reviewed run as a live handle**
   (design lines 181–205). `internal/store/term_report_history.go:77` clears
   `DateChoices`; `internal/reporting/cache.go:220` derives a live entry's
   expiry from its observation. The design requires a fresh report and review
   after a usable restore, and refusal after a changed-selection restore.
4. **Membership and coverage differ** (design lines 108–140).
   `report/counts.go:125` skips a member without a usable date, and `:137`
   checks each term's range before charging coverage. The fixture explicitly
   includes its fallback date in range; its missing document contributes to
   coverage but not hits. Literal counts are independently specified.
5. **Historical original export has different preconditions**
   (design lines 153–157, 187). `internal/store/export_sources.go:323` looks up
   a retained version without requiring it to be current, but `:335` checks
   a supplied nonzero node revision. The historical request omits that optional
   precondition and excludes the trashed document. Report capture still rejects
   changed current versions.

### Low

The CLI fixture must cross package-local test patterns. The design says to
adapt only the setup needed by this corpus, rather than treating
`publishRenditionTextFixture` or `syntheticPDF` as exported helpers. Startup
and physical restore are part of the untested composition; source inspection
does not establish that the new scenario already passes.

## Verified source contracts

| Claim group | Evidence at the source baseline |
| --- | --- |
| Valid PDF generation and parsing already used | `internal/pdfstamp/synthetic_test.go:16` creates PDF bytes with fpdf; `:98` uses pdfcpu's `api.PageCount`. Both dependencies are already in `go.mod`. |
| Retained evidence fixture pattern | `internal/api/routes_rendition_text_test.go:57` builds normalized page evidence, sanitized Markdown, a canonical profile, and publication records; `:128` calls the production publisher. |
| Publisher owns real artifact and lexical publication | `internal/processing/artifacts.go:159` validates the staged rendition and verifies blob receipts before making its build and lexical generation available. |
| Portable profile need not have a runtime | `internal/config/config.go:667` requires deployment credentials only when a rendition runtime is configured; `:835` permits a nil runtime. `cmd/docbank/embedding_runtime.go:133` builds a separate executable-profile map. |
| Report uses portable configured selection | `cmd/docbank/daemon.go:487` passes the original configuration to API dependencies. `internal/api/routes_term_reports.go:78` selects coverage through `selectCollectionProfile`, independently of executable processing profiles. |
| Exact retained text binding | `internal/store/term_report_frame.go:393` captures rendition attachment, build, artifact, profile, and generation under the frame's observation; `internal/reporting/service.go:39` prepares candidates from captured metadata and text. |
| Current-version selection enforcement | `internal/store/term_report_frame.go:75` validates availability/current versions before hashes. `internal/api/routes_term_reports.go:52` maps changed selection to HTTP 409 `report_selection_changed`. |
| Review and revision lifecycle | `internal/reporting/cache.go:145` revises a retained frame, admits a new handle, and keeps the parent's expiry. `:607` refuses artifact acquisition while date review is pending. |
| Count and coverage oracle | `report/counts.go:125`, `:137`, and `:164` implement date eligibility and the five counts; `:216` charges coverage, including fallback dates. `report/csv.go:12` defines the output column order. |
| Missing-evidence strict behavior | `internal/reporting/service.go:228` rejects strict results with missing text or incomplete families. `internal/api/routes_term_reports.go:38` maps that error to `incomplete_coverage`. |
| CLI report observations | `cmd/docbank/report.go:118` registers capture/review and offline verify/CSV operations. `cmd/docbank/report_inspect.go:18` and `:43` provide JSON live summary and history; `cmd/docbank/report_download.go:13` downloads an existing report. |
| Offline verification boundary | `report/verify.go:359` returns internal consistency with `SourceVerified: false`. Native archive verification is separate, through `document/bundle/verify.go:91`, and requires the expected plan fingerprint. |
| Native CLI uses the existing engine | `cmd/docbank/export.go:28` makes an explicit source and an originals-only plan; `:73` starts a caller-identified job. `cmd/docbank/export_download.go:20` uses verified local publication. |
| Explicit release and capacity | `internal/exporter/release.go:13` refuses release while a lease exists; `internal/api/routes_exports.go:35` maps that to `export_retained`. `internal/store/export_jobs.go:119` caps retained jobs at two per owner; `internal/store/export_release.go:39` deletes a released job. |
| History scope | `internal/store/term_report_history.go:16` stores request and summary; `:77` removes date choices. Its metadata codec exports/imports these records, not live report packets. |
| Physical restore checks retained authority | `internal/backupapp/restore.go:241` checks restored rendition catalog authority and physical bytes. `cmd/docbank/cli_test.go:1645` exercises backup verification and restore proof through the real commands. |
| Real daemon fixture lifecycle | `cmd/docbank/cli_test.go:39` runs the actual Cobra root. `cmd/docbank/daemon_test.go:264` starts and joins `runServe`; `:285` waits for discovery and health. No new production fixture interface is needed. |
| Report capacity and lifetime | `internal/reporting/cache.go:91` admits at most eight handles per owner, including revisions; `:220` sets observation plus 30 minutes. Restart is sufficient to test unavailable handles without waiting for expiry. |

The exact combined corpus counts, retained PDF behavior after daemon startup,
and both restored-daemon outcomes are acceptance requirements derived from
these contracts. They are not claimed as execution evidence from this review.

## Existing execution evidence

On Linux at the source revision above, the following existing tests passed
with both `CGO_ENABLED=1` and `CGO_ENABLED=0`, using `-tags fts5` and `-count=1`:

- `TestMCPReportAndNativeExportWorkflow`
- `TestMCPReportAvailableOnlyKeepsMissingEvidence`
- `TestTermReportHistoryRetainsReusableRequestAcrossMetadataRoundTrip`
- `TestReportCLIHistoryAfterRestart`
- `TestExportCLIHistoricalVersionsAndExplicitRelease`
- `TestBackupInitCreateListVerifyCLI`
- `TestPackageSuppliedTextSearchSurvivesBackupRestore`

The package set was `./cmd/docbank ./internal/mcp ./internal/store
./internal/processing`; `-run` selected the exact test names above. These are
component and existing text-workflow checks, not the proposed PDF qualification.
No macOS or Windows execution is claimed by this author review.

## Adversarial review prompt

Review the design at the hash and product revision above. Challenge this source
ledger and return severity-ranked findings with design lines and code evidence.
Do not implement. In particular:

1. Can the proposed portable-profile fixture use production rendition
   publication and real daemon startup without adding a PDF runtime or relying
   on legacy extraction-cache state? Is any setup claim missing a required
   authority field or lifecycle boundary?
2. Does either restore scenario accidentally assume configuration, live
   handles, date choices, or transient export jobs survive a backup? Do the
   current-report and historical-export preconditions remain distinct?
3. Are the literal count and coverage expectations correct for these terms,
   singleton families, and source/fallback dates? Do the assertions distinguish
   exact membership and original bytes from internal packet consistency?
4. Is this a reasonable single test-focused PR? Does it claim coverage of an
   untested provider, attachment, browser, platform, or maintenance boundary?

## Initial verdict

The design was ready for adversarial specification review. Its bounded scope
and source checks were sufficient for planning, but did not prove the combined
workflow. The maintainer subsequently authorized inline implementation.

## Implementation evidence

Implementation is based on `6c922f78`, including the reviewed design originally
committed as `9f3dd967`. The original review hash above identifies the proposal;
it does not describe the updated implemented design.

Implemented design SHA-256:
`42619af3f2db9382bb00efc2f0d52d87c642f4da7bd22822e0c4d7e5834cb3c9`.

The three specification-review notes are resolved:

- Each publication has distinct build, attachment, and lexical-generation IDs.
  A temporary fixture probe reused one generation ID; the second publication
  failed its exact-build coverage check before daemon startup. The review's
  prediction of silent missing text did not occur through this publisher.
- The fixture uses pdfcpu v0.15.0 to parse every generated PDF as one page.
- One configuration supplies all portable profile blocks. Every source and
  restored daemon startup asserts the same canonical profile fingerprint.
  Restored storage settings are preserved and evidence is never republished.

The first complete workflow run returned a complete report without date review,
with three fallback dates instead of one. Rendition generation escapes ISO
dates as `2024\-05\-06`; the date scanner did not recognize them. The focused
scanner test reproduced zero candidates before the correction. The scanner now
handles rendition punctuation escapes without changing retained evidence or its
offsets. Numeric-date order and explicit interpretation use the same retained
tokens. The focused tests cover ISO dates, month names, numeric dates, ambiguity,
quotes, hashes, and byte offsets.

`TestReportPDFWorkflow` now exercises the real CLI report/review/export/release
flow and strict missing-evidence refusal. `TestReportPDFRestore` checks frozen
bytes after replacement and trash, both physical snapshots and restore proofs,
history recovery without live handles or choices, renewed review from unchanged
selection, refusal of changed selection, and exact historical original export.
Both tests passed on Linux amd64 with `CGO_ENABLED=1` and `CGO_ENABLED=0`:

```sh
go test -tags fts5 ./cmd/docbank -run '^TestReportPDF' -count=1
```

At implementation revision `5454e879`, a fresh whole-branch review found no
Critical, Important, or Minor issues and no behaviors it declined to judge.
The reviewer reran the focused date and PDF tests in both SQLite modes.

Final local checks on Linux amd64 passed:

```sh
CGO_ENABLED=1 make test
CGO_ENABLED=0 go test -timeout 20m -tags fts5 -parallel 8 ./...
CGO_ENABLED=1 go test -race -tags fts5 ./report ./cmd/docbank \
  -run '^(TestReportPDF|TestContentDates)' -count=1
make lint
make docs-build
prek run
```

The two default-parallel pure-Go `make test` attempts failed different existing
API deadline tests: storage evacuation, then browser-upload shutdown. Each
passed three isolated repetitions. The full suite passed with eight parallel
tests per package; neither unrelated test nor its product code was changed.
The release-script and timing-budget checks in both `make test` attempts passed.
These results do not establish that the default-parallel timing failures are
fixed. No macOS or Windows execution is claimed.
