# Retained PDF workflow qualification: author review

Reviewed design: [Qualify retained PDF reports, native exports, and restore](2026-10-02-report-export-qualification-design.md).

- Source revision: `2d100a274a548a9eb1e7f9aaef12eab478c78a1f`.
- Design SHA-256: `a1bd8500e394bb80c47f658fbed3adcc6629e609d9338f16f5628f841e2ee219`.
- The design was reviewed as an uncommitted addition. Only the design and this
  review were added; product source, existing tests, generated clients, and
  dependencies match the clean baseline. There are no Git submodules.
- Relevant dependency versions: Go 1.27.0, fpdf v0.9.0, pdfcpu v0.15.0,
  Huma v2.38.0, Cobra v1.10.2, and Kit v0.29.0, from `go.mod`.

This is the author's source check, not an independent adversarial review. The
design is proposed. Existing component tests passed as recorded below; the
combined PDF and restored-daemon scenario has not been implemented or run.

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

## Verdict

Ready for adversarial specification review. The design is bounded and its
existing-capability claims are source-backed. Implementation planning and
execution remain pending the maintainer's review of the written specification.
