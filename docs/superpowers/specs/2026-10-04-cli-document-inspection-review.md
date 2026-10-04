# CLI document inspection: author review

Reviewed design: [Inspect processing coverage and retained text from the CLI](2026-10-04-cli-document-inspection-design.md).

Status: proposed. This is the author's source check, not an independent review
or evidence that the new commands are implemented.

## Baseline

- Source revision: `eba64ff4b039226b545cd25b9d722cc7bb87385c`, the merged #788.
- Design SHA-256: `4be3b6b08cca36ce270d35c8e57090d0225f5a50fa00f8fd67ea9866d127cc3c`.
- Initial design commit: `013af97f`; its design SHA-256 was
  `f9cf46e268eef7c1aea323bef337a6ae3aa61d9aa5e42197b6da395348f3130f`.
- This revision changes only the design and author review. Product source,
  existing tests, generated clients, and dependencies still match the source
  baseline. There are no Git submodules.
- Relevant pins: Go 1.27.0, Huma v2.38.0, Cobra v1.10.2, Kit v0.31.1.

## Findings

### High

None found in this author review.

### Medium

The external review found two contract gaps missed by the initial author
review. Both are addressed in this revision:

1. **Executable profiles only.** `executableProcessingProfiles` omits profiles
   without registered rendition or embedding runtimes. Both the profile list
   and coverage use that filtered inventory. Report selection instead derives
   the configured profile directly. The spec now accepts this restriction,
   explains that retained evidence may still support reports, and specifies
   actionable error advice without implying missing evidence. Successful
   acceptance fixtures use `plaintextProcessingConfig` with the real plaintext
   descriptor, rather than the synthetic-only profile. Fixture publication and
   daemon startup must share the canonical fingerprint. No provider work is
   started by either inspection command.
2. **Catalog limits are not stale selections.** Summary resolution traverses
   the tree with depth, path-byte and filename-character bounds. Excessive
   path depth/bytes can fail input normalization before the query; overlong
   filenames are omitted and reported as stale. The revised discovery path
   checks the resolved path/name before summary resolution and explains the
   specific catalog limit instead of recommending refresh. The spec states
   the full-tree scan cost and distinguishes these limits from node storage
   rules. Pinned reads bypass discovery, including its path/name restrictions.

Diagnostic coverage and immutable continuation remain unchanged. Coverage
can report stale counters without admitting a read, and a continuation never
selects a newer attachment. Discovery compares the returned build and profile
to its summary; pinned reads compare the profile and rely on the immutable
attachment for the build. Both paths retain the existing helper's identity
and bounded-response checks.

### Low

Both external Low notes are incorporated:

- Use unfiltered `AuditStatus` to obtain the vault UUID, including when audit
  is disabled. It avoids the storage statistics and host-path response in
  `VaultInfo`, but still aggregates audit scope state when enrolled. The spec
  does not describe it as constant-cost.
- An explicit attachment skips summary resolution at offset zero and during
  continuation. The window route already checks current/live source identity
  and active attachment membership in one store snapshot. Node resolution,
  named-profile selection, and the returned-profile comparison remain.

Window offsets still rescan the text prefix, and coverage remains one version
per invocation. No bulk-throughput or whole-artifact verification claim is made.

## Verified source claims

| Claim | Evidence at the baseline |
| --- | --- |
| The new subcommand names do not collide | Registrations in `cmd/docbank/processing.go:340` and `cmd/docbank/rendition.go:58`. |
| Existing CLI metadata supplies source identities and hashes | `cmd/docbank/stat.go:32` emits `api.Node`; fields in `internal/api/types.go:363`; `cmd/docbank/versions.go:77` emits the complete version page. |
| Input and output helpers already exist | `parseNodeSelector` in `cmd/docbank/node_selector.go:24`; `IsCanonicalUUIDv4` in `internal/daemonconn/receipts.go:915`; `canonicalSHA256` in `cmd/docbank/processing.go:331`; `writeCLIJSON` in `cmd/docbank/json.go:10`. |
| Root command setup does not contact the daemon before these checks | `cmd/docbank/root.go:22` only sets `commandStarted`; the new handlers must validate before their own `Ensure` call. |
| Coverage has a typed GET operation with exact version IDs | `internal/api/routes_processing.go:375` accepts a profile, vault UUID, and 1–4,096 versions; generated `GetDocumentProcessingCoverage` exists in `internal/apiclient/client.gen.go:2039`. |
| Coverage profile/vault/UUID validation is separate from current visibility | `internal/processing/service.go:1823` and `:2137`; the store computes current/live coverage in `internal/store/processing_coverage.go:46`. |
| Both commands are limited to executable profiles; reports are not | `cmd/docbank/daemon.go:274` uses `executableProcessingProfiles`; `cmd/docbank/embedding_runtime.go:171` filters runtime bindings; `cmd/docbank/rendition_runtime.go:84` skips unsupported adapters. Reports use configuration in `internal/api/routes_collection_quality.go:22`. |
| Audit status supplies a vault UUID without enrollment | `internal/api/routes_audit.go:109` selects `Store.AuditStatus(ctx, nil)` without query filters; `internal/store/audit_public.go:641` initializes the vault ID before checking enrollment. The enabled path reads audit scope/member aggregates. |
| Supersession/trash produce stale counts, not an admission error | `internal/store/processing_coverage.go:106`; executed `TestProcessingCoverageTracksTrashRestoreAndSupersessionEligibility`. |
| Optional and unneeded classes affect aggregate state differently | `internal/processing/service.go:1356` initializes an unneeded rendition class; the following embedding loop preserves required/optional policy. |
| Rebuilding and previous-generation-serving overlap intentionally | `internal/store/processing_coverage.go:195` increments both for a complete head with a replacement job. |
| Coverage JSON has no echoed input version list | `internal/api/types.go:224` defines `CoverageReport`; the proposed two-field CLI wrapper supplies the single requested version. |
| Profile discovery supplies the fingerprint without planning work | `internal/api/routes_processing.go:59` calls `Service.Profiles`; `internal/processing/service.go:607` reads the service's executable profile inventory. |
| Exact summary resolution already validates response membership | `internal/daemonconn/document_query.go:57`; request and active-rendition types in `internal/api/document_query_types.go:13`. |
| A moved or replaced summary can be refused | `internal/store/document_query.go:162` checks the node/version/path set in its read transaction and returns `ErrProcessingSourceFenceStaleVersion` on mismatch. |
| Summary resolution walks the bounded live tree from the root | `internal/store/document_query.go:192` passes `/` to the recursive catalog query; `:307` defines the CTE, and `:250` normalizes path bounds. Constants are in `internal/store/walk.go:19` and `internal/store/document_query.go:14`. |
| Catalog name/depth limits do not apply to ordinary node resolution | `internal/store/names.go:14` has no name length cap; `internal/store/node.go:165` resolves path components and `:315` walks ancestors without the catalog bounds. The CTE's name cap applies only to files. |
| The text helper binds the request and bounds its response | `internal/daemonconn/rendition_window.go:22` reads at most 1 MiB plus one byte; `:55` checks tuple, hashes, Unicode count, offsets, EOF, and response bytes. It does not compare build/profile with a separately resolved summary. |
| MCP already uses this text helper | `internal/mcp/read_tools.go:658` calls `Connection.RenditionTextWindow`; no new MCP adapter is needed. |
| Both text-window APIs share one internal read path after #781 | `internal/processing/rendition_window.go:70` and `:106`; route registrations in `internal/api/routes_processing.go:298` and `:487`. |
| Current source and active attachment are checked in one store snapshot | `internal/store/processing_catalog.go:513`, including nonempty source hash, current version, trash state, and active attachment ID. |
| The bounded reader does not freshly verify the complete artifact | `internal/processing/rendition_window.go:171` opens the blob, checks its size, reads a window, and removes only the expected incomplete-verification sentinel on early close. |
| Unicode offsets, empty EOF, and beyond-EOF behavior exist | `internal/processing/rendition_window.go:213`; executed Unicode/window tests listed below. |
| Full rendition verification remains available | `cmd/docbank/rendition.go:42` uses `CopyVerified`; the command keeps its existing complete-stream byte limit. |
| Exit codes need no shared mapping change | `cmd/docbank/exit.go:46` maps usage to 2 and `store.ErrNotFound` to 3, with general failure 1. `stale_version` and `invalid_rendition_window` lack a special mapping in `internal/daemonconn/receipts.go`. |
| Retained fixture publication needs a different configuration and distinct IDs | `cmd/docbank/report_rendition_fixture_test.go:29` uses an unsupported synthetic adapter; `:59` publishes with version-only IDs. `cmd/docbank/daemon_processing_runtime_test.go:34` verifies the executable plaintext configuration defined at `:415`. |

## Existing execution evidence

During the initial author review, these existing tests passed on Linux/amd64
in both CGO SQLite and pure-Go SQLite modes. They ran against the source
baseline, using temporary vaults and local test servers; no developer vault or
external provider was used.

```sh
CGO_ENABLED=1 go test -tags fts5 \
  ./internal/api ./internal/daemonconn ./internal/processing \
  -run 'Test(ProcessingCoverage(ReportsConfiguredEmbeddingUnavailableBeforeFirstRun|TracksTrashRestoreAndSupersessionEligibility)|EvidenceWindow.*|RenditionTextWindow.*|ReadUnicodeRenditionWindow.*|ResolveDocumentSummaries.*)$' \
  -count=1
```

The same command also passed with `CGO_ENABLED=0`. All three packages reported
success in each mode. This covers the existing HTTP coverage behavior, exact
evidence-window identity and visibility checks, shared Unicode reader behavior,
and daemon-client response validation. It does not exercise the proposed CLI
commands, which do not exist yet, or establish native macOS/Windows results.

For this revision, the following existing tests also passed with `-tags fts5`
and `-count=1` in both SQLite modes on Linux/amd64:

- `cmd/docbank`: `TestExecutableProcessingProfilesRegistersPlaintextRendition`.
- `internal/store`: `TestDocumentCatalogRejectsUnboundedTraversal`,
  `TestDocumentCatalogPreservesLongDirectoryNames`, and
  `TestAuditStatusExplainsDormantAndProtectedNodes`.
- `internal/api`: `TestAuditPreviewEnableAndStatusLifecycle` and
  `TestProcessingProfilesRouteListsExecutableProfilesDeterministically`.

These checks exercise the existing plaintext runtime registration, catalog
bounds, and audit/profile reads. The omitted synthetic profile and the proposed
CLI bypass/error behavior are source-backed design requirements, not newly
executed CLI tests. No product or test code changed in this revision.

## Verdict and adversarial focus

The revised contract addresses the two Medium findings and both Low notes.
It remains a proposal, ready for re-review before implementation planning.
Focus on the executable-profile limitation, the discovery-versus-pinned paths,
and the catalog-limit advice. The new commands and their acceptance examples
still require implementation evidence; existing engine tests do not prove them.
