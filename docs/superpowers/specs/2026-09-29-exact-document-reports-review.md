# Exact-document reports: adversarial self-review

This is the spec author's source review, not an independent review or a test
of implemented behavior. The proposed selected-document scope does not exist
in the reviewed product code.

## Reviewed input

- Spec: [Reports for selected document versions](2026-09-29-exact-document-reports-design.md).
- Exact spec SHA-256: `bf89ba48ea368aceacf0e057a67ebcc808a1c8f915eafb078c577f1fd248c538`.
- Repository: `kenn-io/docbank` at `b184ebfc4888f8f4cb6a59dbbb52573134a6bf39`.
- Working state during review: the new spec was untracked; product sources,
  dependencies, and generated clients matched that commit. This review is the
  second new documentation file. There were no Git submodules/gitlinks.
- Constraints: root `AGENTS.md`, `docs/README.md`, the two maintainer decisions
  recorded in the spec, and the existing search-export guide. No dependency
  upgrade or third-party behavior change is proposed.

Source paths and line numbers below refer to that repository revision. A later
review must compare both the exact spec bytes and these source paths before
reusing any conclusion.

## Findings

No unresolved High or Medium design findings in this self-review. This is a
source-backed assessment of the proposed approach, not evidence that the new
behavior passes tests.

The author corrected two draft gaps before recording the spec hash:

- **Preserve the existing request error.** Spec line 181 uses 422
  `invalid_report_request` for malformed scope, matching the create route at
  `internal/api/routes_term_reports.go:98`. The draft had proposed a needless
  new error distinction. The 413 member-limit response must also be handled
  at this initial normalization boundary, before its generic 422 fallback.
- **Carry selection through the actual web flow.** Spec lines 52 and 64 now
  cover asynchronous vault-ID lookup and selected-scope history wording.
  `frontend/src/actionRunner.ts:18` already provides the lookup;
  `frontend/src/TermReportDrawer.svelte:119` and `:355` currently assume editable
  all-document/collection scope. Copy rows before awaiting the lookup and
  discard an action whose session changes.

## Verified

Twelve load-bearing claims were checked against the baseline:

| Spec claim | Source evidence and consequence |
| --- | --- |
| Reuse the request and exact document identity | `report/types.go:13` has node/version/hash identity; `:55` owns the durable request. `report/request.go:21` validates without a vault and copies request slices. The nested selected object needs its own copy. |
| Use the store's identity encoding | `internal/store/identity.go:12` generates lowercase UUIDv4; `:32` validates it. `internal/store/store.go:143` creates vault identity and `internal/store/version.go:650` creates version identity through it. Apply new shape checks only to the new scope. |
| Validate at the existing observation boundary | `internal/store/term_report_frame.go:31` binds capture to one lexical generation and SQLite snapshot; `:211` reads live current members. Exact-set comparison belongs inside that observation, not in an earlier HTTP lookup. |
| Keep capture protection, without permanent source retention | `internal/api/gate.go:116` holds the preservation gate for capture. `internal/reporting/service.go:41` wraps preparation with it and reads captured text. No report-lifetime source lookup or new retention reference is required by the design. |
| Existing scope checks need deliberate changes | `report/counts.go:102` and `report/verify.go:82` both demand collection witnesses whenever `AllDocuments` is false. Merely adding a request field and SQL filter would leave selected scope unusable. |
| Family evidence can reference unselected documents | `internal/store/term_report_families.go:66` traverses relationships through current related versions outside the population. `report/counts.go:159` derives hit counts from members. Preserve the relation graph while keeping exact selected membership. |
| Packet verification already recomputes report consistency | `report/bundle.go:82` validates evidence and calculated results; `report/verify.go:227` reconstructs the packet. Add selected-set equality and vault binding there. At `:356`, verification explicitly reports `SourceVerified: false`. |
| Date revisions retain the captured observation and deadline | `internal/reporting/cache.go:145` takes the parent's shared frame; `:220` derives expiry from its observation plus 30 minutes. `:352` returns a copied request without choices. Extend nested copying; do not re-admit sources on revision. |
| History stores JSON and does not require live sources | `internal/store/term_report_history.go:42` structurally validates request/summary; `:77` saves JSON and drops date choices; `:190` imports a validated record. The 100-record bound is at `:38`. The optional request field does not require another table. |
| Restore preserves the authority needed by a future run | `internal/store/metadata.go:997` imports logical records and installs the saved vault ID. Its node/version record import retains identities. This supports rerunning still-live selections; it does not restore cached report handles. |
| Existing clients provide the required seams | `frontend/src/App.svelte:1467` copies displayed row version/hash fields for exports; `frontend/src/selection.ts:10` instead carries node/revision for mutations. `frontend/src/snapshotWorkspace.svelte.ts:66` clears selection on page changes. `frontend/src/TermReportDrawer.svelte:111` loads history drafts. `cmd/docbank/report.go:128` owns existing create/verify/CSV commands. Generated client shapes remain owned by `Makefile:50`. |
| Existing bounds and publishing rules cover this proposal | `internal/api/routes_term_reports.go:93` caps request JSON at 8 MiB; the store frame caps members at 50,000. `frontend/src/exports.ts:11` uses a different 100,000 limit, so its whole validator is not reusable unchanged. `docs/README.md:14` excludes these specs from public navigation and publication. |

Also inspected for these conclusions: `report/request.go`, the remaining
bundle/calculation/verification flows, `internal/reporting/service.go` and
`cache.go`, `internal/store/term_report_history.go` and metadata restore,
`frontend/src/actionRunner.ts`, `docs/usage/search-exports.md`, `go.mod`,
`frontend/package-lock.json` state, and the documentation build entry points.
All implementation references are local project code; this review makes no
new claim about an external library's behavior.

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

Use the behavioral examples in the spec to assess a later implementation.
None were executed as selected-scope tests in this documentation-only change.

## Verdict

Ready for maintainer review of the written specification. The source supports
extending the existing reporting path without another subsystem. Implementation
and its plan remain pending written-spec approval; this review does not grant
merge authority or claim product completion.
