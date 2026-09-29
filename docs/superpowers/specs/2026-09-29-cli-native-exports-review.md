# CLI native exports: source checks and review brief

This is the spec author's source review, not an independent adversarial review
or a test of the proposed commands. The design remains proposed.

## Reviewed input

- Spec: [CLI exports of selected original files](2026-09-29-cli-native-exports-design.md).
- Spec SHA-256: `5ee0207acf6fa13642e29e5dc2debdf98a2ee607251ec0cfe32ff24c2cacefd9`.
- Clean source baseline: `0af2361e6ed8d79ef7997a778389c19936d36126`.
- Working state during review: only this spec and review document are new.
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

No unresolved High or Medium source contradictions found in this self-review.
That is not approval to implement; the command contract still needs independent
adversarial review and maintainer review.

Two draft ambiguities were corrected before recording the hash above:

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
| Existing transport, output, and error seams are sufficient | `internal/daemonconn/api_transport.go:17` exposes generated operations; `internal/daemonconn/ensure.go:128` disables redirects on the ownership-proven connection. `internal/apiclient/client.gen.go` includes all six required operations. `cmd/docbank/json.go` owns JSON output and `cmd/docbank/exit.go` classifies integrity and usage errors. `cmd/docbank/report.go:53` demonstrates bounded strict JSON input, but its 8 MiB reader is report-specific. |

Existing regression coverage inspected, without treating it as execution of the
proposed CLI: `TestExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune`,
`TestExportPlanReadUsesRetentionWithoutExtendingAdmission`, and
`TestExportPlanFailureRollsBackAdmissionAndAllowsRetry` in
`internal/store/export_test.go`; master-owner archive delivery in
`TestExportDerivedRolesFreezeReceiptsAcrossHeadReplacement` in
`internal/api/routes_exports_test.go`; generated-client composition in
`internal/daemonconn/exports_test.go`; and the file publication helper/tests.
The CLI examples in the design have not been implemented or executed.

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
6. Does any reuse claim require changing the engine, generated API, format, or
   ownership model? Can any proposed wrapper or check be removed while keeping
   these contracts? Is the caller burden reasonable for this first CLI slice?

## Verdict

Ready for independent adversarial review. The source supports the proposed
composition; command behavior and failure handling remain to be implemented
after the written design is reviewed.
