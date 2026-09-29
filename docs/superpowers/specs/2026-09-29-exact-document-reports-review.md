# Exact-document reports: review follow-up

This records the author's source checks and revisions after the supplied
adversarial review of `5a301f2d`. It is not an independent re-review or a test
of implemented behavior. Selected-document scope remains proposed.

## Reviewed input

- Spec: [Reports for selected document versions](2026-09-29-exact-document-reports-design.md).
- Revised spec SHA-256: `d07afa304fe79187cc4e9a53124bf5c0501a3e5c6e99b433ba82b6452319719c`.
- Prior reviewed spec: commit `5a301f2dde3b3bf0f1d8e4c2f238782e09579e3d`,
  SHA-256 `bf89ba48ea368aceacf0e057a67ebcc808a1c8f915eafb078c577f1fd248c538`.
- Repository: `kenn-io/docbank` at `b184ebfc4888f8f4cb6a59dbbb52573134a6bf39`.
- Working state: clean at `5a301f2d` before revision; only these two documents
  changed during follow-up. Product sources, dependencies, and generated
  clients still match the source baseline. No Git submodules/gitlinks.
- Constraints: root `AGENTS.md`, `docs/README.md`, the two maintainer decisions
  recorded in the spec, and the existing search-export guide. No dependency
  upgrade or third-party behavior change is proposed. Huma `v2.38.0`, pinned
  by `go.mod:16`, was read directly from the Go module cache.

Source paths and line numbers below refer to that repository revision. A later
review must compare both the exact spec bytes and these source paths before
reusing any conclusion.

## Findings

The supplied review found no High issues and four Medium issues. All four
have corresponding changes in the revised spec. No additional High or Medium
issue was identified in this source recheck; the revised contract still needs
maintainer review.

### Medium findings addressed

1. **Omission, null, and schema validation.** Spec lines 108 and 125 require
   omission when unused, reject explicit null at both HTTP and CLI input
   boundaries, and keep value/count checks in `NormalizeRequest`. Huma
   `schema.go:929` makes the optional pointer non-nullable; `:612` rejects
   nullable object references. `huma.go:2021` validates before calling the
   handler, and `:2057` produces 422 for schema errors. Spec line 206 explicitly
   separates these errors from application error codes. Making the new document
   list schema-optional lets an empty object reach normalization; existing
   required identity fields keep their ordinary schema behavior.
2. **Unnecessary client vault ID.** Spec line 155 removes the request field,
   asynchronous lookup, wrong-vault response, and extra verifier comparison.
   `internal/store/identity.go:12` and `version.go:650` generate random version
   identities; `internal/store/metadata.go:1016` preserves vault identity on
   restore. The existing node/version/hash comparison is sufficient for this
   workflow. The frame still records its vault at `term_report_frame.go:59`.
   This is not a claim that a UUID cryptographically binds a version to a vault.
3. **Packet membership versus coverage.** Spec lines 59, 165, and 299
   distinguish selected members from date-eligible counts.
   `report/counts.go:125` skips unusable dates before charging coverage;
   `:136` charges only eligible dates, as `docs/usage/search-exports.md:144`
   describes. Available-only examples now preserve packet membership without
   requiring changes to `scoped`. The drawer must explain the count difference.
4. **Member-limit response.** Spec line 211 requires the new selected-member
   limit to wrap `report.ErrReportLimit` and requires the create route to handle
   it before the current 422 fallback at `internal/api/routes_term_reports.go:98`.
   The existing collection-limit error at `report/request.go:31` stays 422.

### Low findings resolved as design choices

- Spec line 217 deliberately omits a conflicting-member list from the 409,
  consistent with the simple conflict response at `internal/api/routes_pages.go:61`.
- Spec lines 145 and 202 classify a wrong hash on a matching current version
  as 422. `internal/store/version.go:663` installs a version with its blob;
  its immutable version identity is not a cue to retry with another hash.
- Spec line 120 makes lowercase hashes intentional for selected scope only.
  `report/request.go:162` continues to accept either hex case elsewhere.
- Spec line 260 records the possible history/export size: up to 100 requests
  near the 8 MiB request ceiling. `internal/store/term_report_history.go:38`
  limits history to 100, `:90` enforces per-record JSON bounds, `:108` bounds
  each page by bytes, and `:160` exports the retained records. A page bound
  does not cap the total history or its metadata export.

## Verified

The original twelve source claims remain valid with the removed vault check
and revised limits wording below. The follow-up also checked the Huma schema
path and coverage calculation described above:

| Spec claim | Source evidence and consequence |
| --- | --- |
| Reuse the request and exact document identity | `report/types.go:13` has node/version/hash identity; `:55` owns the durable request. `report/request.go:21` validates without a vault and copies request slices. The nested selected object needs its own copy. |
| Use the store's identity encoding | `internal/store/identity.go:12` generates lowercase UUIDv4; `:32` validates it. `internal/store/store.go:143` creates vault identity and `internal/store/version.go:650` creates version identity through it. Apply new shape checks only to the new scope. |
| Validate at the existing observation boundary | `internal/store/term_report_frame.go:31` binds capture to one lexical generation and SQLite snapshot; `:211` reads live current members. Exact-set comparison belongs inside that observation, not in an earlier HTTP lookup. |
| Keep capture protection, without permanent source retention | `internal/api/gate.go:116` holds the preservation gate for capture. `internal/reporting/service.go:41` wraps preparation with it and reads captured text. No report-lifetime source lookup or new retention reference is required by the design. |
| Existing scope checks need deliberate changes | `report/counts.go:102` and `report/verify.go:82` both demand collection witnesses whenever `AllDocuments` is false. Merely adding a request field and SQL filter would leave selected scope unusable. |
| Family evidence can reference unselected documents | `internal/store/term_report_families.go:66` traverses relationships through current related versions outside the population. `report/counts.go:159` derives hit counts from members. Preserve the relation graph while keeping exact selected membership. |
| Packet verification already recomputes report consistency | `report/bundle.go:82` validates evidence and calculated results; `report/verify.go:227` reconstructs the packet. Add selected-set equality there. At `:356`, verification explicitly reports `SourceVerified: false`. |
| Date revisions retain the captured observation and deadline | `internal/reporting/cache.go:145` takes the parent's shared frame; `:220` derives expiry from its observation plus 30 minutes. `:352` returns a copied request without choices. Extend nested copying; do not re-admit sources on revision. |
| History stores JSON and does not require live sources | `internal/store/term_report_history.go:42` structurally validates request/summary; `:77` saves JSON and drops date choices; `:190` imports a validated record. The 100-record bound is at `:38`. The optional request field does not require another table. |
| Restore preserves the authority needed by a future run | `internal/store/metadata.go:997` imports logical records and installs the saved vault ID. Its node/version record import retains identities. This supports rerunning still-live selections; it does not restore cached report handles. |
| Existing clients provide the required seams | `frontend/src/App.svelte:1467` copies displayed row version/hash fields for exports; `frontend/src/selection.ts:10` instead carries node/revision for mutations. `frontend/src/snapshotWorkspace.svelte.ts:66` clears selection on page changes. `frontend/src/TermReportDrawer.svelte:111` loads history drafts. `cmd/docbank/report.go:128` owns existing create/verify/CSV commands. Generated client shapes remain owned by `Makefile:50`. |
| Existing bounds and publishing rules cover this proposal | `internal/api/routes_term_reports.go:93` caps request JSON at 8 MiB; the store frame caps members at 50,000. `frontend/src/exports.ts:11` uses a different 100,000 limit, so its whole validator is not reusable unchanged. `docs/README.md:14` excludes these specs from public navigation and publication. |

Also inspected for these conclusions: `report/request.go`, the remaining
bundle/calculation/verification flows, `internal/reporting/service.go` and
`cache.go`, `internal/store/term_report_history.go` and metadata restore,
`document/bundle/types.go`, `docs/usage/search-exports.md`, `go.mod`,
`frontend/package-lock.json` state, and the documentation build entry points.
The Huma references above mean `github.com/danielgtaylor/huma/v2@v2.38.0`.
Its source establishes the schema behavior; no new dependency behavior is
assumed or proposed. Product and dependency diffs from the source baseline
were empty; changed spec claims and all supplied findings were rechecked.

## Challenges for the next reviewer

Try to falsify these contracts against the pinned source and proposed design:

1. Can any admission path report a subset or a newer version while claiming
   to have used the supplied selection? Include same-hash replacement and
   available-only coverage.
2. Can a recomputed packet omit a requested member and still verify? Hold the
   request fixed; changing both request and evidence is outside an internal
   consistency verifier's authenticity claim.
3. Does restricting counts accidentally sever a family connection through an
   unselected document, or does preserving that connection add its hits?
4. Does any history, restore, or date-revision path recheck live source state
   where the historical-evidence decision forbids it?
5. Can a workspace refresh, session change, history draft, or caller-owned
   nested slice silently replace selected identities?
6. Does an implementation add snapshot export jobs, provider work, database
   policy, or a new selection abstraction that this outcome does not need?
7. Do real HTTP and CLI input paths honor omission-only scope and preserve the
   distinction between schema errors, application 422, selected-member 413,
   and version-change 409? Does a collection-limit request still return 422?

Use the behavioral examples in the spec to assess a later implementation.
None were executed as selected-scope tests in this documentation-only change.

## Verdict

Ready for re-review of the revised specification. The four Medium findings and
four Low comments have explicit resolutions in the design. Implementation and
planning remain pending written-spec approval; this source review does not
claim that the proposed behavior is implemented or tested.
