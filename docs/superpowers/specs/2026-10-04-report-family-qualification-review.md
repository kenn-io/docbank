# Selected email family qualification: author review

Reviewed design: [Qualify selected email families in reports and original exports](2026-10-04-report-family-qualification-design.md).

Status: proposed. This is the author's source check, not an independent
adversarial review or execution of the proposed qualification.

## Reproducible baseline

- Source revision: `392dbe25905214bbd58d094134206654f730f739`.
- Design SHA-256:
  `0538bb49a0794f8660eb4c2d6c144a164dea001c683ee52c50c792b6fb966ae3`.
- The design was reviewed as an uncommitted addition. The only new files are
  this design and review; product source, tests, generated clients, and
  dependencies match the clean source revision. There are no Git submodules.
- Relevant versions from `go.mod`: Go 1.27.0, Huma v2.38.0, MCP Go SDK v1.7.0,
  Cobra v1.10.2, and Kit v0.30.1.

## Findings and decisions

No unresolved High or Medium issue was found in this author check. The following
distinctions are explicit in the design rather than left to implementation:

1. **The #774 prerequisite is already merged** (design lines 34–37).
   GitHub identifies merged head `79c0eaa3` and merge commit `d1e71f95`.
   `exportSourceSchema` now uses the prefixed fingerprint pattern, and
   `exportWriteToolDefinitions` carries the operation-specific recovery advice.
   The actual tool reads real query, saved-query, and snapshot plans in
   `TestExportInspectionQueryPlans`; that test passed in both SQLite modes.
2. **Family totals count selected documents** (design lines 58–74, 146–184).
   `report.Calculate` builds family flags but increments all five result columns
   over selected members. Its second pass increments `UniqueFamilies` once per
   eligible member when the family has a hit without a competitor. Coverage
   likewise increments incomplete families per member. The literal tables use
   those definitions, not a count of connected components.
3. **Outside members can connect selected parents** (design lines 158–186).
   `readTermReportFamilies` traverses both directions through current, live
   versions. It retains Q→X and R→X even when X is not a member. After X is
   trashed, `currentTermRelationIdentity` rejects that endpoint and each parent
   is marked incomplete. Neither the trash operation nor a fresh capture edits
   an older cache entry. The new end-to-end assertions remain unexecuted.
4. **Incomplete inventory must be isolated from missing text**
   (design lines 188–207). The existing partial-inventory store test publishes
   a real truncated MIME message with no relation rows. The proposed fixture
   separately retains complete report text and an unambiguous date. This makes
   strict refusal evidence about family coverage, rather than another cause.
5. **Original EML bytes include omitted MIME parts** (design lines 39–54,
   209–230). Both preview adapters request only `original`. Attachment-role
   expansion is conditional in `resolveExportDocumentRows`; ordinary explicit
   children are independent members. The test asserts separate rows/files,
   while preserving every byte of the original email.

Low: fixture reuse crosses package-local patterns. `newEmailPipelineFixture`
and the PDF fixture's helpers are not exported utilities. Adapt their small
setup routines within `cmd/docbank`; do not create a general fixture framework
or import another package's `_test.go` helpers. The existing exported MCP
constructor and stdio server provide the required boundary without a product
test hook. The implementation must still prove the combined setup works.

## Source claim ledger

| Design claim | Evidence inspected at the source baseline |
| --- | --- |
| CLI can already supply exact version identities and hashes | `cmd/docbank/ls.go`, `stat.go`, and `versions.go`; `internal/api/types.go` defines the node response. |
| Whole retained rendition and search coverage reads exist | `cmd/docbank/rendition.go:runRenditionGet`; coverage output in `cmd/docbank/search.go`. |
| #766 used singleton families | `cmd/docbank/report_pdf_fixture_test.go` and `report_pdf_workflow_test.go` create unrelated PDF nodes and assert five counts of one per term. |
| MIME setup can retain real inventory and physical bytes | `internal/processing/email.go:EnsureEmailTarget` and `publishEmailInventory`; `internal/processing/email_test.go:newEmailPipelineFixture`. |
| Attachment publication is available without new product code | `internal/processing/email_documents.go:PublishEmailDocuments` verifies retained part blobs, then calls the store publisher under the blob mutation lease. |
| Sharing requires explicit child reuse | `document/email_documents.go:EmailDocumentReuse`; `internal/store/email_documents.go:publishEmailDocumentsTx`; `TestEmailDocumentsAtomicStaleDestinationAndExplicitReuse`. |
| Destination revision is a real precondition | `internal/store/email_documents.go:publishEmailDocumentsTx` checks the destination node revision before publishing. |
| Retained text can use existing artifact publication | `internal/processing/artifacts.go:PublishRendition`; `cmd/docbank/report_pdf_fixture_test.go` builds the canonical profile, records artifacts, and publishes fresh lexical generations. |
| Mail and text need different evidence units | `document/evidence_codec.go:validateFamilyUnitKind` accepts `mail`/message and `text`/section; `validateSourceLocator` accepts their non-indexed locators. |
| Profile configuration controls report text capture | `internal/api/routes_term_reports.go:serviceFor` uses `selectCollectionProfile`; `internal/store/term_report_frame.go:readTermReportRendition` requires that profile's retained build in the captured lexical generation. |
| Content date labels avoid vault fallback | `report/content_dates.go` extracts the explicit label; `report/dates.go:SelectDate` prefers usable content document dates over vault addition. Same-day email metadata avoids a different date outcome. |
| Exact selection bounds members and raw matches | `internal/store/term_report_frame.go:readTermReportMembers` scopes node IDs; `MaterializeTermReportFrame` applies match bits only to captured member identities. |
| Relations may extend beyond selected members | `internal/store/term_report_families.go:readTermReportFamilies`; `TestTermReportSharedChildJoinsExactVersionFamily` already checks unselected-child grouping and packet verification in process. |
| Missing live children mark parents incomplete | `internal/store/term_report_families.go:currentTermRelationIdentity` and the `!childCurrent` branch; `internal/store/trash.go:Trash` permits recoverable trash with a revision check. |
| Shared-child warnings identify the child | `internal/store/term_report_families.go` counts retained parent relations per child and appends a warning for counts over one. |
| Five literal count columns and coverage follow the specified semantics | `report/counts.go:Calculate`, `chargeCoverageMember`; column order in `report/csv.go`. |
| Partial inventory can exist without any relation rows | `internal/store/term_report_frame_test.go:TestTermReportPartialFamilyWithoutPublishedAttachments` and the publication-level incomplete marker in `readTermReportFamilies`. |
| Strict rejects incomplete family coverage | `internal/reporting/service.go:Finalize` checks every row's `IncompleteFamilies`; `internal/api/routes_term_reports.go:termReportError` maps `ErrIncompleteCoverage`. |
| Strict refusal produces no handle or history row | `internal/reporting/cache.go:Create` returns before publication when `makeEntry` fails; the API saves history only after cache creation succeeds. |
| Frozen report artifacts do not follow new source state | `internal/reporting/cache.go` retains its captured frame and bundle; existing `TestMCPReportAndNativeExportWorkflow` checks unchanged bytes after source replacement/trash. |
| CLI and MCP use explicit originals-only plans | `cmd/docbank/export.go:exportPreviewCmd`; `internal/mcp/export_tools.go:previewExport`. |
| Separate attachment-role expansion is conditional | `internal/store/export_attachments.go:resolveExportDocumentRows`; the existing engine has `attachment_original` and `attachment_pdf` roles. |
| Native verification accepts an expected plan fingerprint | `document/bundle/verify.go:Verify`; original file comparison already appears in the PDF qualification. |
| Report packet evidence has the five stated files | `report/bundle.go:bundleNames` and packet record types; offline verification reports internal consistency rather than independent source authenticity. |
| Both clients can address one fixture daemon | `cmd/docbank/daemon_test.go:startServe`, `waitForDaemon`; `cmd/docbank/cli_test.go:runCLI`; `internal/mcp/protocol.go:NewServerWithOptions`; `internal/mcp/stdio.go:ServeStdio`; `internal/mcp/daemon.go:newDaemonLease`. |
| The proposed fixture fits existing resource limits | `internal/reporting/cache.go:startBuild` caps handles at eight per owner; `internal/store/export_jobs.go` caps jobs at two per owner; `internal/exporter/release.go` refuses active leases with the retained error. |

## Execution evidence from existing tests

On Linux at the source revision, these existing tests passed with both
`CGO_ENABLED=1` and `CGO_ENABLED=0`, using `-tags fts5` and `-count=1`:

- `TestExportInspectionQueryPlans` in `./internal/mcp`, including query,
  saved-query, and snapshot subtests.
- `TestCountsMatchSevenDocumentOracle` and
  `TestCountsKeepFamilyInsideCollectionScope` in `./report`.
- `TestTermReportSharedChildJoinsExactVersionFamily` and
  `TestTermReportPartialFamilyWithoutPublishedAttachments` in `./internal/store`.

These are prerequisite and component checks. The new CLI/MCP family tests,
literal tables, shared-child trash scenario, and explicit-original archive
comparisons have not been implemented or executed. No new Windows or macOS
execution is claimed.

## Adversarial review prompt

Review the design at the recorded hash against the source baseline. Return
severity-ranked findings with design lines and concrete source evidence; do
not implement. In particular:

1. Recompute every table by hand from the selected members, terms, dates, and
   relation graph. Can an unselected match affect uniqueness? Are any fields
   mistakenly treated as counts of family groups?
2. Can the real MIME/attachment setup and the portable retained-text profile
   coexist under a daemon? Does the setup require an unmentioned policy,
   revision, physical receipt, or identity? Could text/date failure mask the
   intended incomplete-family result?
3. Does trashing the shared child actually remove its current edges from a
   fresh report while preserving both selected parents and their searchable
   text? Does the old packet remain independent of that mutation?
4. Do the export assertions distinguish separately selected attachment files
   from bytes inherently inside an original EML? Are both actual client
   admission paths exercised without introducing attachment-role support?
5. Is this a reviewable qualification PR, with fixtures and independent
   expectations rather than a new test framework or product feature? Are its
   stated limits and remaining #719 evidence gaps honest?

## Verdict

Ready for adversarial specification review. Source inspection and existing
component tests support the proposed cases; they do not establish that the
combined workflow already passes.
