# CLI native exports: review follow-up

Historical design review: the CLI and explicit release are now implemented.
The checks below describe the pre-implementation source baseline.

This records the spec author's source checks and response to the supplied
adversarial review of `65cd4296`. It is not an independent re-review or a test of
the proposed commands. The maintainer approved explicit release and inline execution on 2026-09-29.

## Reviewed input

- Spec: [CLI exports of selected original files](2026-09-29-cli-native-exports-design.md).
- Revised spec SHA-256: `eadae959264ff986b9bddd7225dbe2bf4b37095a0daceed46916f858c08be7d7`.
- Prior reviewed spec: commit `65cd4296d3fb1a239a3f08903ae0d0eb21af6add`,
  SHA-256 `5ee0207acf6fa13642e29e5dc2debdf98a2ee607251ec0cfe32ff24c2cacefd9`.
- Clean source baseline: `0af2361e6ed8d79ef7997a778389c19936d36126`.
- Working state: clean at `65cd4296` before revision; only this spec and review
  document changed during follow-up.
  Product code, dependency pins, and generated clients match the baseline.
  There are no Git submodules/gitlinks.
- Constraints: root `AGENTS.md`, `docs/README.md`, and the existing native export
  guide. These internal documents are excluded from public documentation.
- No dependency change is proposed. The JSON null claim was checked in the
  selected Go 1.27.0 toolchain's `src/encoding/json/v2/arshal.go:298`: null
  stores the target's zero value. No new Huma validation behavior is assumed.

All repository source locations below refer to the pinned baseline. Review the
actual spec bytes and relevant source diffs before reusing these conclusions.

## Findings

The supplied review found no High issue and one Medium capacity gap. The source
confirms that gap. The revision makes its consequence explicit and proposes an
early-release contract. The maintainer subsequently chose explicit release and requested inline execution
and a PR. The contract below records the reviewed proposal that is now approved.

### Medium: shared completed-job capacity

Original design lines 46–47 and 217–218 kept the current owner/retention limits
without describing their effect on repeated CLI use. All non-browser requests
use `master` (`internal/api/routes_exports.go:18`). Admission counts every job
row, without a state filter, and rejects the third for an owner
(`internal/store/export_jobs.go:115`). Completion retains a row for 24 hours
(`:243`); cancellation refuses a completed job (`:274`). Ticket consumption does
not remove it (`internal/exporter/worker.go:282`). Only expiry cleanup deletes
job rows (`internal/store/export_jobs.go:387`). These are source-backed facts;
no production capacity experiment was run.

Two completed jobs therefore block a third until the earliest slot expires and
is cleaned up, generally about a day. Other API-key callers share those slots.
The revised design proposes explicit `export release`, backed by one new DELETE
route, so callers can reclaim finished jobs while downloads remain repeatable by
default. The old promise of no new HTTP endpoint is removed. Raising only the
per-owner quota would still encounter the global eight-job cap.

Release is new behavior, not an existing helper capability. Its proposed contract
checks ownership and terminal state, respects ticket/download leases, removes the
archive before deleting its job row, and recomputes plan/source retention.
`Worker.Cleanup` (`internal/exporter/worker.go:342`) establishes the existing
mutex-before-gate order and lease check; `reduceExportRetention`
(`internal/store/export_jobs.go:294`) owns the retention calculation. Cleanup
currently deletes the row before the file. The new release operation deliberately
reverses that order so a failed filesystem removal does not report a free slot
with an orphan archive; a later store failure instead keeps a retryable row with
a possibly missing file. This ordering and the new public contract need review.

Releasing jobs does not release all source/plan capacity. The revision also names
the global 32-record cap and the example of 16 fresh source/plan pairs filling it
within their admission window. Neither another owner's records nor the resource
limits are silently changed.

### Low findings addressed

1. **Local value checks and global capacity.** Original design lines 88–90 and
   203–210 delegated malformed values to the store. Source reservation precedes
   member validation (`internal/store/export_sources.go:208`, `:288`), and a
   failure retains its reservation (`:251`). The revision adds post-decode local
   checks for IDs, hashes, sizes/revisions, duplicates, and total bytes, with the
   member index in usage errors. It preserves ordinary null decoding and leaves
   live identity checks in the daemon. Standard `uuid.Parse` accepts noncanonical
   spellings, so canonical round-trip, v4, and RFC variant checks are explicit;
   the existing `canonical.IsSHA256Hex` supplies the hash check.
2. **Reuse paths and ticket diagnostics.** The design now names
   `Connection.CreatePackageExportTo` (`internal/daemonconn/package_export.go:65`)
   and `exportBatesFile` (`internal/mcp/bates_tool.go:300`) as the concrete patterns
   for transport and publication. It excludes the former's raw HTTP-error return
   at `:97` from that reuse. Go 1.27.0's `net/http/client.go:601` documents the
   `*url.Error` result, and `net/url/url.go:40` formats its URL. The requirement is
   to keep the new command's diagnostics free of tickets, not to claim an
   observed compromise or expand this work into a package-download refactor.
3. **Job recovery after preview expiry.** Original design lines 141–142 did not
   distinguish the recovery commands clearly enough. Source replay checks the
   expiry decoded from canonical JSON (`internal/store/export_sources.go:104`,
   `:173`); plan creation extends the separate SQL retention column
   (`internal/store/export_plans.go:284`). The revision directs callers to replay
   `start` or use `status` with the known job ID. A preview can return 410 while
   that job remains available. A behavioral example now captures the difference.

Two ambiguities corrected in the original draft remain resolved:

- A plan GET itself can succeed after admission expiry, because it checks
  extended retention. The design now forbids adding an admission-expiry check
  before job replay, rather than claiming that any plan GET would block replay.
  Evidence: `internal/store/export_plans.go:70` and `export_jobs.go:70`.
- Cleanup can fail. The design requires an attempt and an error report; it does
  not promise removal of a stage when the filesystem refuses cleanup.
  Evidence: `internal/filepublish/publish.go:91`.

### Advisory choices for review

- Two caller-supplied preview IDs make retries explicit without a local state
  file, but cost more input than an automatic-ID command. Reconsider only with
  a concrete lost-response recovery contract.
- Preview reports frozen counts, bytes, and fingerprint. The request file is
  the member list; this scope adds no per-file listing API.
- Plan creation is limited to 1,000 original-file members. Existing accessible
  plans/jobs remain usable through the lifecycle commands, including larger or
  derivative-containing bundles. Do not accidentally enforce the preview limit
  on download or promise that every downloaded job contains only originals.

## Verified claims

| Claim | Source evidence |
| --- | --- |
| No existing CLI native bundle-export command | `docs/usage/export-bundles.md:20`; command registration under `cmd/docbank` has package exports and reports, but no native export root. `docs/cli-reference.md` is the existing CLI guide. |
| Small explicit requests fit the existing source API | `document/bundle/types.go:8` defines `ChunkMembers = 1000`; `SourceRequest` and `Member` are in the same file. `internal/store/export_sources.go:34` validates source kind and direct count; `:150` checks canonical request size. `internal/api/routes_exports.go:61` caps the HTTP body at 1 MiB. |
| Historical versions and multiple versions per node are valid | `internal/store/export_sources.go:264` rejects duplicate node/version pairs, not repeated nodes; `:286` seals exact retained version/hash/size identities. `internal/store/export_plans.go:391` repeats identity/revision/trash checks without requiring the current version, then freezes current node name/path. |
| Canonical IDs and lowercase hashes already have owners | `internal/store/identity.go:32` validates lowercase UUIDv4 plus RFC variant; `internal/canonical/canonical.go:94` checks lowercase SHA-256. `validateExportMembers` enforces sizes and optional revision preconditions. |
| Source and plan replay are separate, bounded operations | `internal/store/export_sources.go:150` binds the canonical request digest and rejects resolving/failed replay. `internal/store/export_plans.go:92` binds its own request digest, checks admission expiry, and creates the plan transactionally. Neither hashes a sorted replacement of the submitted member array before source replay comparison. |
| Original-only plans need no new policy | `internal/store/export_plans.go:19` accepts the original role without selectors; `:421` emits the original role from the selected version. `document/bundle/types.go` defines the existing `Plan` header and byte counts. |
| Job replay precedes new admission checks | `internal/store/export_jobs.go:70` returns a retained matching job before loading/checking a new plan. New admission verifies the fingerprint and enforces eight retained jobs/two per owner. `:216` gives completed jobs 24-hour retention. |
| Cancellation is not always a state transition | `internal/store/export_jobs.go:261` conflicts on completed jobs, succeeds unchanged on failed/canceled jobs, and fences active work. The HTTP cancel operation returns no job body at `internal/api/routes_exports.go:303`. |
| CLI can reuse the existing ticket route | `internal/api/routes_exports.go:18` selects the master owner for non-browser callers; `:322` issues tickets for those callers. `internal/api/web_download.go:413` consumes the ticket without requiring a browser prepare operation. `:35` defines the two-minute TTL. |
| Both server and client can use the complete archive verifier | `internal/exporter/worker.go:284` acquires a file lease and calls `bundle.Verify` against the job fingerprint, comparing the receipt. `document/bundle/verify.go:91` validates ZIP contents and plan binding; `:254` hashes the entire ZIP and returns its receipt. Its volume branch reuses the same verifier entry point. |
| Existing destination and publication helpers cover the local boundary | `cmd/docbank/get.go:183` checks destination/parent/overwrite policy; `cmd/docbank/package.go:181` additionally checks the Docbank data directory. `internal/filepublish/publish.go:26` returns publication state even on later sync failure; `:60` creates the private stage. Platform files implement no-replace and replacement behavior, including Windows. |
| Existing transport, output, and error seams cover the original commands | `internal/daemonconn/api_transport.go:17` exposes generated operations; `internal/daemonconn/ensure.go:128` disables redirects on the ownership-proven connection. `internal/apiclient/client.gen.go` includes the original six operations; the proposed release route needs generation of a seventh. `cmd/docbank/json.go` owns JSON output and `cmd/docbank/exit.go` classifies integrity and usage errors. `cmd/docbank/report.go:53` demonstrates bounded strict JSON input, but its 8 MiB reader is report-specific. |

Existing regression coverage inspected, without treating it as execution of the
proposed CLI: `TestExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune`,
`TestExportPlanReadUsesRetentionWithoutExtendingAdmission`, and
`TestExportPlanFailureRollsBackAdmissionAndAllowsRetry` in
`internal/store/export_test.go`; master-owner archive delivery in
`TestExportDerivedRolesFreezeReceiptsAcrossHeadReplacement` in
`internal/api/routes_exports_test.go`; generated-client composition in
`internal/daemonconn/exports_test.go`; and the file publication helper/tests.
At that review, the CLI examples had not been implemented or executed.

## Adversarial review prompt

Review [the design](2026-09-29-cli-native-exports-design.md) against
`0af2361e6ed8d79ef7997a778389c19936d36126`. Verify the spec hash above and report
High/Medium findings with exact spec lines and source evidence. Do not implement
or broaden the scope. Treat this document as claims to check, not proof.

Try to falsify these contracts:

1. Can preview/start replay create new work, extend admission, change membership,
   or incorrectly reject recovery after a lost response? Distinguish admission
   deadlines from extended retention and permanent deduplication.
2. Does original-only preview really preserve historical selected versions,
   optional revision semantics, exact membership, and no attachment expansion?
   Check HTTP input and CLI JSON conversion, including null scalar values.
3. Can CLI callers obtain and consume an existing archive ticket with the web
   interface disabled? Does connection/error handling leak a bearer URL or
   accidentally reconnect through a different transport?
4. Can a truncated, corrupt, oversized, or different valid archive reach the
   destination? Does the expected fingerprint come from outside the ZIP, and
   does the full verified receipt match both job and ticket receipts?
5. Are overwrite races, interrupted work, failed cleanup, and errors after
   publication described honestly on Linux, macOS, and Windows?
6. Can release race a ticket or archive stream, free the wrong owner's slot,
   strand an unrecoverable row after partial failure, or remove retention needed
   by another job? Does two completions → release → third admission work without
   changing resource ceilings? Keep the separate source/plan cap visible.
7. Apart from the explicitly proposed release operation and generated client,
   does any reuse claim require changing the engine, format, or ownership model?
   Can any wrapper or check be removed while keeping these contracts? Is the
   caller burden reasonable for this first CLI slice?

## Verdict

The maintainer approved explicit release and inline execution after this review.
The three Low findings are addressed in the design. Implementation now has behavioral coverage for release, lease protection,
verified downloads, and the real-daemon CLI workflow. This historical design
review is not an independent implementation review.
