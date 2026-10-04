# CLI document inspection: author review

Reviewed design: [Inspect processing coverage and retained text from the CLI](2026-10-04-cli-document-inspection-design.md).

Status: proposed. This is the author's source check, not an independent review
or evidence that the new commands are implemented.

## Baseline

- Source revision: `eba64ff4b039226b545cd25b9d722cc7bb87385c`, the merged #788.
- Design SHA-256: `f9cf46e268eef7c1aea323bef337a6ae3aa61d9aa5e42197b6da395348f3130f`.
- The two design/review files were new working-tree files during this check.
  Product source, existing tests, generated clients, and dependencies match the
  clean source revision. There are no Git submodules.
- Relevant pins: Go 1.27.0, Huma v2.38.0, Cobra v1.10.2, Kit v0.31.1.

## Findings

### High

None found in this author review.

### Medium

None unresolved. The draft makes these contract choices explicit:

1. Coverage is diagnostic, not admission. The service does not reject a valid
   unknown or superseded version ID. The store counts absent current/live rows
   as stale, while the service can mark an unneeded rendition class
   `not_required`. The spec preserves both facts and keeps coverage separate
   from a successful text read or report.
2. The command must obtain an attachment without a new catalog command. Exact
   summary resolution already exposes active attachment/build/profile identities.
   The CLI uses that operation and checks the selected build/profile after the
   existing window helper validates its own tuple.
3. A nonzero offset alone is not a continuation identity. The spec requires
   the prior attachment ID and forbids selecting a newer rendition on mismatch.
   It permits a fresh first-window inspection without that attachment argument.
4. The retained-rendition test helper currently derives publication IDs only
   from the version. It needs an explicit fixture adaptation for two profiles
   or a replacement on one version; invoking it unchanged would not establish
   those scenarios. The acceptance section calls out that limitation.

### Low

Coverage is deliberately one version per invocation. Window discovery uses
several existing reads and Unicode offsets rescan the prefix. This specification
does not claim bulk-inspection throughput, seek performance, whole-artifact
verification, or availability of text after the active rendition is replaced.
Those limits are documented rather than hidden behind new abstractions.

## Verified source claims

| Claim | Evidence at the baseline |
| --- | --- |
| The new subcommand names do not collide | Registrations in `cmd/docbank/processing.go:340` and `cmd/docbank/rendition.go:58`. |
| Existing CLI metadata supplies source identities and hashes | `cmd/docbank/stat.go:32` emits `api.Node`; fields in `internal/api/types.go:363`; `cmd/docbank/versions.go:77` emits the complete version page. |
| Input and output helpers already exist | `parseNodeSelector` in `cmd/docbank/node_selector.go:24`; `IsCanonicalUUIDv4` in `internal/daemonconn/receipts.go:915`; `canonicalSHA256` in `cmd/docbank/processing.go:331`; `writeCLIJSON` in `cmd/docbank/json.go:10`. |
| Root command setup does not contact the daemon before these checks | `cmd/docbank/root.go:22` only sets `commandStarted`; the new handlers must validate before their own `Ensure` call. |
| Coverage has a typed GET operation with exact version IDs | `internal/api/routes_processing.go:375` accepts a profile, vault UUID, and 1–4,096 versions; generated `GetDocumentProcessingCoverage` exists in `internal/apiclient/client.gen.go:2039`. |
| Coverage profile/vault/UUID validation is separate from current visibility | `internal/processing/service.go:1823` and `:2137`; the store computes current/live coverage in `internal/store/processing_coverage.go:46`. |
| Supersession/trash produce stale counts, not an admission error | `internal/store/processing_coverage.go:106`; executed `TestProcessingCoverageTracksTrashRestoreAndSupersessionEligibility`. |
| Optional and unneeded classes affect aggregate state differently | `internal/processing/service.go:1356` initializes an unneeded rendition class; the following embedding loop preserves required/optional policy. |
| Rebuilding and previous-generation-serving overlap intentionally | `internal/store/processing_coverage.go:195` increments both for a complete head with a replacement job. |
| Coverage JSON has no echoed input version list | `internal/api/types.go:224` defines `CoverageReport`; the proposed two-field CLI wrapper supplies the single requested version. |
| Profile discovery supplies the fingerprint without planning work | `internal/api/routes_processing.go:59` calls `Service.Profiles`; `internal/processing/service.go:607` reads the configured profile inventory. |
| Exact summary resolution already validates response membership | `internal/daemonconn/document_query.go:57`; request and active-rendition types in `internal/api/document_query_types.go:13`. |
| A moved or replaced summary can be refused | `internal/store/document_query.go:162` checks the node/version/path set in its read transaction and returns `ErrProcessingSourceFenceStaleVersion` on mismatch. |
| The text helper binds the request and bounds its response | `internal/daemonconn/rendition_window.go:22` reads at most 1 MiB plus one byte; `:55` checks tuple, hashes, Unicode count, offsets, EOF, and response bytes. It does not compare build/profile with a separately resolved summary. |
| MCP already uses this text helper | `internal/mcp/read_tools.go:658` calls `Connection.RenditionTextWindow`; no new MCP adapter is needed. |
| Both text-window APIs share one internal read path after #781 | `internal/processing/rendition_window.go:70` and `:106`; route registrations in `internal/api/routes_processing.go:298` and `:487`. |
| Current source and active attachment are checked in one store snapshot | `internal/store/processing_catalog.go:513`, including nonempty source hash, current version, trash state, and active attachment ID. |
| The bounded reader does not freshly verify the complete artifact | `internal/processing/rendition_window.go:171` opens the blob, checks its size, reads a window, and removes only the expected incomplete-verification sentinel on early close. |
| Unicode offsets, empty EOF, and beyond-EOF behavior exist | `internal/processing/rendition_window.go:213`; executed Unicode/window tests listed below. |
| Full rendition verification remains available | `cmd/docbank/rendition.go:42` uses `CopyVerified`; the command keeps its existing complete-stream byte limit. |
| Exit codes need no shared mapping change | `cmd/docbank/exit.go:46` maps usage to 2 and `store.ErrNotFound` to 3, with general failure 1. `stale_version` and `invalid_rendition_window` lack a special mapping in `internal/daemonconn/receipts.go`. |
| Retained fixture publication is reusable with an identity caveat | `cmd/docbank/report_rendition_fixture_test.go:59`; build, attachment, and lexical generation IDs currently use only `node.CurrentVersionID`. |

## Existing execution evidence

The following existing tests passed on Linux/amd64 in both CGO SQLite and
pure-Go SQLite modes. They ran against the source baseline, using temporary
vaults and local test servers; no developer vault or external provider was used.

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

## Verdict and adversarial focus

Ready for independent specification review before implementation planning.
Check that the discovery-to-window sequence cannot adopt a newer attachment,
that diagnostic coverage is not presented as admission or report readiness,
that the command-local exit choices match the shared error mapping, and that
the publication fixture adaptation preserves distinct identities for multiple
renditions of one version. The coverage wrapper and pinned continuation rule
are new CLI contracts and need their own implementation evidence.
