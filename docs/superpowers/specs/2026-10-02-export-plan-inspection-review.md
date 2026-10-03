# Retained export-plan inspection: author review

Reviewed design: [Inspect retained export plans from the CLI and MCP](2026-10-02-export-plan-inspection-design.md).

Status: implemented. The design review below records the earlier source check;
the implementation evidence at the end covers the new reads.

## Design review baseline (historical)

- Source revision: `83c7a02c9f2c92204db465497e919ad8b591f281`, the merged #766.
- Reviewed design SHA-256: `a392d2037d07f02fef553101839cbb952abadfafd1483b961b4c7d5edd9991e8`.
- Prior reviewed revision: `44c20f7f8ab8ac3eaaed6dca0327a758661676a7`;
  its design SHA-256 was
  `f90b53cfd86d30fafe6a78e8844232da693c81f094cc9610706ccd7e0a43589e`.
- This revision was read as uncommitted changes to the design and review on
  that commit. Product source, existing tests, generated clients, and
  dependencies still match the clean source baseline. There are no Git
  submodules. Only the changed claims were rechecked.
- Relevant `go.mod` versions: Go 1.27.0, Huma v2.38.0, Cobra v1.10.2,
  MCP Go SDK v1.7.0, jsonschema-go v0.4.3, and Kit v0.30.1.

The initial author review missed a Medium contract gap: error messages are
shared by code, so the requested inspection-specific advice had no existing
implementation path. The adversarial review identified it. This revision
chooses shared neutral messages and clarifies both Low notes below.

## Findings

### High

No unresolved High finding in this author review.

### Medium

No unresolved Medium finding in this author review. The draft makes the
following decisions explicit before implementation planning:

1. **Read lifetime differs from admission** (design lines 59–74).
   `internal/store/export_plans.go:70` checks the separate retention column.
   `internal/store/export_jobs.go:108` rejects a new start after the frozen
   header's admission deadline. A successful read cannot imply that a new
   start is valid. Existing fake-clock execution confirms this distinction.
2. **Retained plans have a wider shape than MCP previews**
   (design lines 177–216). `internal/mcp/export_schemas.go:10` accepts only
   explicit, originals-only previews of at most 1,000 members. The stored
   types in `document/bundle/types.go:59` and `:152` include richer sources,
   policies, counts, and volume declarations. The read schema must accept
   those fields without widening preview's write contract.
3. **The pager is fixed and can refuse a large page** (design lines 120–149).
   `internal/store/export_inspection.go:99` keeps at most 50 problems, scans
   every row for the total, and rejects an encoded page over 64 KiB at `:160`.
   The spec preserves that behavior instead of promising arbitrary inspection
   throughput or successful pagination of every retained plan.
4. **Read retries are narrower than unconditional replay**
   (design lines 169–175). `internal/mcp/daemon.go:223` retries a transport
   failure only before any response starts, and at most once. The draft says
   this explicitly; domain errors and partial responses do not trigger replay.

5. **Shared errors need shared wording** (design lines 230–256).
   `internal/mcp/export_tools.go:100` passes only an error to
   `domainToolError`; `internal/mcp/tools.go:374` constructs the result with
   `domainErrorMessage(code)`. Existing conflict, expiry, and limit advice at
   `:532` assumes a narrower context, as does the job-inspection advice for
   timeout, cancellation, and failure at `:546`. The spec now defines six
   operation-neutral replacements in that one shared function. Codes and
   dispatch stay unchanged; recovery instructions belong with each operation.

### Low

Both adversarial Low notes are clarified. The paging fixture explicitly uses
`CreateExportSource` and `CreateExportPlan` through the generated client,
following `TestExportClientFrozenPreview`. `internal/store/export_plans.go:440`
turns missing text into an unavailable role when `AllowUnavailable` is true.
Neither CLI nor MCP preview acquires a new role option. The guide requirement
now states that HTTP shares the same 64 KiB cap and cannot request a shorter
page at that offset (`internal/api/routes_exports.go:230` delegates directly
to the same store pager).

The new problem pager adapters still need their own observed continuation
test. The existing tests run below establish header retention and a separate
frozen role-summary read, not 51-problem pagination through the proposed
commands. The design requires that gap to be covered through the real daemon.

Full-header result-schema coverage should adapt existing plan fixtures, not
attempt to import package-local test helpers or add a provider fixture system.
The 1,001-member upload pattern already exists in
`internal/store/export_scale_test.go:36`; it is a setup reference, not an
exported test API. New adapter coverage should stay focused on serialization,
forwarded cursor values, errors, and the observed read behavior.

## Source claim ledger

| Claim | Evidence at the baseline |
| --- | --- |
| Both HTTP reads already exist | `internal/api/routes_exports.go:186` returns the plan header; `:230` returns one problem page, with the numeric cursor bound. |
| Generated client needs no regeneration | `internal/apiclient/client.gen.go:3098` and `:3190` implement those operations; `:21537` and `:21545` alias the existing bundle types. |
| API-key clients share an owner | `internal/api/routes_exports.go:18` substitutes `master` for non-browser requests. `internal/store/export_plans.go:73` checks owner before read expiry and maps unknown/wrong-owner records to `ErrNotFound`. |
| Header is frozen and bounded | `internal/store/export_plans.go:53` decodes stored canonical JSON with a `MaxMemberBytes` cap; `:70` returns that header rather than resolving current documents. |
| Reads do not extend retention | The two read paths contain no retention update. `internal/store/export_jobs.go:292` separately reduces retention on release; `:392` removes expired authority during cleanup. |
| Admission limits remain unchanged | `internal/store/export_sources.go:190` counts source and plan rows globally; `internal/store/export_jobs.go:115` counts all retained jobs globally and per owner. The limits are 32, eight, and two respectively. |
| Every supported source and role is represented | `internal/store/export_sources.go:37` validates seven source kinds; `internal/store/export_plans.go:19` validates one through six role policies. `document/bundle/types.go:69` defines optional source fields. |
| Optional volume, count, and duplicate fields exist | `document/bundle/types.go:92`, `:97`, and `:152`; `document/bundle/volumes.go:20` defines volume limits, and `document/bundle/duplicates.go:13` validates duplicate policies. |
| Problem identity and continuation are existing data | `document/bundle/inspection.go:21` and `:29` define the result. `internal/store/export_inspection.go:111` counts and selects problems; `:150` rejects offsets beyond total, and `:154` assigns the next offset. |
| Missing inventories contribute additional problems | `internal/store/export_inspection.go:127` emits a problem per requested attachment role before inspecting unavailable row roles. These are not counts of distinct documents. |
| Problem page uses frozen rows, not current coverage | `internal/store/export_inspection.go:123` calls `WalkExportDocuments`; `internal/store/export_plans.go:330` reads stored document JSON in pages. Row validation precedes returning the result. |
| Response bounds leave MCP headroom | `document/bundle/types.go:19` sets 64 KiB; `document/bundle/inspection.go:3` sets 50 items. `internal/mcp/read_tools.go:798` checks the complete result size after producing both copies; `internal/mcp/schemas.go:22` sets 1 MiB. |
| CLI reuses existing boundaries | `cmd/docbank/export.go:223` registers the export group and JSON flag. `cmd/docbank/export_request.go:21` validates canonical UUIDv4 before connection setup; `cmd/docbank/json.go:10` writes CLI JSON. |
| Existing exit mapping covers the reads | `internal/daemonconn/receipts.go:371` maps `not_found` to `store.ErrNotFound`. `cmd/docbank/exit.go:49` preserves explicit usage and not-found errors and falls back to general failure. |
| Default read annotations and input validation exist | `internal/mcp/tools.go:160` builds the catalog, `:184` sets read annotations, and `:238` validates call arguments before dispatch. `internal/mcp/schemas.go:81` defines canonical UUIDv4. |
| Existing export status supplies a read pattern | `internal/mcp/export_tools.go:170` uses `daemonRead`. `internal/mcp/read_tools.go:26` defines private-cache output and `:798` enforces output schema and complete response size. |
| Domain codes already propagate | `internal/api/routes_exports.go:28` maps store failures to HTTP codes. `internal/mcp/tools.go:469` preserves export domain codes. `domainToolError` at `:369` has no tool-name argument. The proposed shared message replacements at `:532` remove recovery advice that cannot apply to every caller. |

These entries establish reuse and current source behavior. They do not establish
that the new CLI output or MCP schemas have been implemented or tested.

## Existing execution evidence

During the initial review, on Linux at the baseline above, these existing
tests passed with both
`CGO_ENABLED=1` and `CGO_ENABLED=0`, using `-tags fts5 -count=1`:

- `TestExportPlanReadUsesRetentionWithoutExtendingAdmission`
- `TestExportPreviewUsesFrozenReceiptsAndOwner`
- `TestExportClientFrozenPreview`
- `TestDaemonReadRetriesAtMostOnce`
- `TestDaemonReadNeverRetriesAfterResponse`
- `TestNativeExportPreviewInput`

The first command selected the first three names in `./internal/store` and
`./internal/daemonconn`. The second selected the last three in `./internal/mcp`.
Fixtures use temporary stores and local HTTP servers; the retention test uses
fake time. No developer vault or external provider was used. No new product
test, cross-platform run, benchmark, or combined inspection workflow is claimed.
These tests were not rerun for this documentation-only revision; the product
source and test fixtures are unchanged.

## Design verdict and adversarial review prompt (historical)

Ready for implementation planning after resolving the adversarial review's
message-policy decision. The proposal uses the existing error path with shared
neutral text, specifies the real paging setup, and acknowledges the HTTP cap.
Product implementation had not started at that review.

Review the design at the hash and source revision above. Return severity-ranked
findings with design lines and source evidence; do not implement. In particular:

1. Can every valid retained plan fit the proposed closed MCP schema, including
   non-explicit sources, optional fields, attachment roles, and volumes?
2. Does any output or recovery advice confuse admission, retention, and job
   lifetime, or imply that a new preview recovers the original snapshot?
3. Are problem totals, empty terminal pages, per-inventory problems, fixed page
   limits, and escaped text presented accurately by both client contracts?
4. Does the proposed evidence exercise the actual pager and owner checks without
   broadening the scope into another archive or provider qualification project?

## Implementation evidence

Implemented design SHA-256: `61b8710b20f3090bd7de34aa40fb21af57503fcaaa983a201246cf312f2701db`.

Implementation baseline: `5d4dc2f1b3b12d1b97f95f9517673f451a97351c`. The changes
since the design baseline do not alter the export paths used by this feature.
The CLI and MCP use the existing generated reads and preserve preview's narrower
write schema. Shared MCP messages use the six agreed neutral replacements.

The new CLI and MCP workflow tests read real daemon plans and verify 50+1
problem paging, exact identities, terminal cursors, and frozen results after a
source edit. Additional checks cover preflight rejection, default stdio/HTTP
catalogs, a real 1,001-member upload plan, optional header wire fields, escaped
human output, near-cap pages, domain errors, and invalid or partial responses.
The existing browser-owner fixture now checks both read routes against another
browser session and the API-key owner. Existing fake-clock tests continue to
cover admission versus retention; no production lifetime rule changed.

Optional source and attachment fields use wire fixtures for schema coverage,
alongside the production nodes/upload plans. They do not claim to qualify new
attachment production. All data is synthetic; no developer vault or provider
was used. Platform execution here is Linux only.

The inspection, native-export, owner, and retention tests pass with `-tags fts5`
in both SQLite modes. The strict documentation build and `prek run` pass.
