# Native MCP exports: adversarial self-review

Reviewed [the proposed design](2026-09-30-mcp-native-exports-design.md) against
source revision `5f36ee4ec6245a1e558cac2de36218b52700a66f`.
Design SHA-256:
`38974c795f4134fd132058ac9774208c0d0d9f4ff3b48899697e813437daaba0`.

This is an inline author self-review, not an independent implementation review.
The source tree was clean at that baseline. At review, only the new design and
this review were uncommitted; product code, generated clients, dependencies,
and existing documentation were unchanged. There are no gitlinks/submodules.
The Go module declares Go 1.27.0, MCP Go SDK v1.7.0, and jsonschema-go v0.4.3.
The proposed MCP tools have not been executed; observations below are source
checks, not test results for unbuilt behavior.

Read the repository instructions, documentation publishing guide, the CLI
export design and review, and existing MCP/native-export guides. The baseline
includes merged PR #740, including its release and download corrections. Source
references below are relative to that exact revision.

## Findings

### High — spec contradicts the code or is unimplementable

None identified in the final draft.

### Medium — unresolved contract decisions before planning

None identified in the final draft. The design explicitly includes the
metadata and error-mapping work that a simple tool-registration wrapper would
miss. It accepts the existing limits rather than implying a new capacity or
transport guarantee.

### Low — deliberate limits for reviewers to keep visible

- MCP HTTP cannot promise delivery of every permitted 52 GiB archive within
  its two-minute request deadline. The design retains that deadline and names
  CLI/stdio delivery as the recovery path. Large-download background tasks and
  timeout changes would be a separate design.
- The MCP preview schema intentionally requires size and rejects explicit null
  scalar values. The CLI decoder permits some null/omitted scalars as zero.
  Sharing value validation must not accidentally change either boundary.
- Adding `blob_hash` to version items extends an existing closed output schema.
  Update the advertised schema and actual mapper together. Existing fields and
  paging stay intact; this does not require a legacy output mode.
- Release leaves the separate global source/plan limit in place. Reusing a
  retained reviewed plan where appropriate avoids unnecessary previews, but no
  new automatic reuse or eviction policy is part of this adapter.

## Verified

The following 22 groups of source claims were checked. Proposed tool names,
flag, output shapes, and export error mappings are requirements to implement,
not claims that those interfaces already exist.

| Claim | Source evidence |
| --- | --- |
| The current MCP catalog has no native export lifecycle tools | `internal/mcp/tools.go:48` owns read definitions and `:158` builds the opt-in catalog. Existing file export tools are Bates and load-file exports. |
| Startup options feed the same server catalog for both transports | `cmd/docbank/mcp.go:49` builds `ServerOptions`; `internal/mcp/protocol.go:36` defines them and `:82` supplies catalog instructions/registration. The new export flag/option does not yet exist. |
| Current version results omit an available original hash | `internal/mcp/read_tools.go:582` defines the item and `:646` maps fetched versions without BlobHash. `internal/api/types.go:426` defines `ContentVersion.BlobHash`, Size, and historical NodeRevision. `internal/mcp/schemas.go:658` defines the 250-item version page. |
| Inputs and outputs have existing schema and size boundaries | `internal/mcp/tools.go:210` validates before dispatch; `internal/mcp/read_tools.go:280` rejects unknown members. `internal/mcp/schemas.go:31` closes object schemas and `:78`/`:88` define UUIDv4/lowercase hash shapes. `internal/mcp/protocol.go:97` bounds complete results. |
| Private cache fields and structured/text results already exist | `internal/mcp/read_tools.go:22` defines privateCache with ttlMs zero; `boundedToolSuccess` in that file supplies structured and text content. `internal/mcp/schemas.go:496` owns the cache field schemas. |
| Required size is intentionally stricter than the CLI reader | `cmd/docbank/export_request.go:17` uses a value `bundle.Member` slice; `:47` decodes with json/v2. `document/bundle/types.go:35` uses an int64 Size. The proposed MCP required/type schema is an additional boundary, not a Go decoder feature. |
| Value validation can use existing owners | `cmd/docbank/export_request.go:53` validates IDs, duplicate pairs, nonnegative values, and overflow-safe 50 GiB totals. `internal/daemonconn/receipts.go:911` owns IsCanonicalUUIDv4; `internal/canonical/canonical.go:94` owns IsSHA256Hex. |
| Historical versions and multiple versions per node are allowed | `internal/store/export_sources.go:264` keys duplicates by node/version, and `:286` seals retained identities against current node availability and optional revision. `internal/store/export_plans.go:391` checks them again without requiring the current version. |
| Preview can reuse exactly two existing mutations | `cmd/docbank/export.go:28` creates an explicit source then an original-only plan. `document/bundle/types.go:152` defines the bounded Plan header. `internal/store/export_plans.go:421` emits the original role; attachment policies are handled separately. |
| Source and plan replay retain admission deadlines and request order | `internal/store/export_sources.go:150` hashes canonical request JSON before any member sorting and checks source admission/replay state. `internal/store/export_plans.go:92` checks the plan request digest and original admission expiry. Retention extends separate SQL expiry fields. |
| Job replay runs before new admission | `internal/store/export_jobs.go:70` returns a retained matching job before checking plan expiry/fingerprint for new work; the supplied operation ID becomes its ID. |
| Retained-job and source/plan capacity are distinct | `internal/store/export_jobs.go:119` counts every retained row and enforces eight global/two per owner. `internal/store/export_sources.go:190` counts combined source and plan records against 32. Source reservation precedes live identity checks. |
| Retention and cancellation semantics match the draft | `internal/store/export_sources.go:202` sets ten-minute admission; `internal/store/export_plans.go` sets the same plan window. `internal/store/export_jobs.go:124` sets a two-hour deadline, `:216` sets failed/completed retention, and `:261` implements cancellation/no-op/conflict cases. |
| CLI and MCP share the master export owner | `internal/api/routes_exports.go:18` maps non-browser authenticated callers to master. `internal/mcp/daemon.go:60` creates the lease from daemonconn.Ensure; adapters use the ordinary API-key connection. There is no per-MCP-client export owner. |
| Explicit release already owns authorization, leases, and deletion | `internal/exporter/release.go:13` takes the worker lock and gate; `internal/store/export_release.go:13` checks owner and terminal state before archive removal and row deletion. `internal/store/export_jobs.go:294` reduces retention. `internal/exporter/worker.go:320` maps a missing archive to ErrExpired. |
| Generated clients cover the lifecycle without new endpoints | `internal/apiclient/client.gen.go:2790`/`:2837`/`:2877`/`:2923` provide create/release/get/cancel job; `:3051` creates plans and `:3241` creates sources. Ticket acquisition is already used by the merged download helper. |
| The read retry helper is unsuitable for automatic write recovery | `internal/mcp/daemon.go:223` retries a callback once after a transport failure before any response starts. `:271` provides single-attempt write behavior but uses a processing-specific outcome sentinel. `internal/mcp/tools.go:364` maps that sentinel to a processing-specific tool error. |
| Generic sanitization would lose local integrity/conflict causes | `internal/mcp/daemon.go:28` retains only a message and extracted problem facts; its Unwrap returns the mapped problem error. `:296` uses ExtractProblemFacts, which recognizes daemon problem responses, not arbitrary local sentinels (`internal/daemonconn/receipts.go:208`). The export adapter must classify local errors before this conversion. |
| One existing helper performs the full native download verification | `internal/daemonconn/export_download.go:19` binds job, ticket, and verified receipts, bounds the copy, and invokes bundle.Verify. `internal/daemonconn/ticket_download.go:13` validates the relative ticket and strips the URL-bearing transport wrapper. `internal/exporter/worker.go:284` verifies the server lease. |
| Destination and publication helpers have the stated boundaries | `internal/mcp/bates_tool.go:374` validates an absolute clean path and resolved parent outside the data directory, with no prohibition on parent symlinks. `internal/filepublish/publish.go:26` preserves the published boolean after sync failure, `:60` creates a private stage, and `:95` exposes cleanup errors. The existing MCP file exporters supply the publication-state pattern. |
| Stdio and HTTP retain different deadline behavior | `internal/mcp/stdio.go:16` and `:94` cap complete inbound frames at 1 MiB. `internal/mcp/http.go:25` defines HTTP bounds, `:34` sets the two-minute default, and `:239` applies socket and context deadlines. `internal/mcp/protocol.go:132` propagates HTTP request cancellation. |
| Export error codes require explicit MCP mapping | `internal/api/routes_exports.go:28` maps engine failures to the existing export problem codes. `internal/mcp/tools.go:364`/`:430` whitelist domain facts and messages without these export codes. `internal/daemonconn/export_download.go:34` can also return a local bundle.ErrConflict. |

Existing test entry points inspected include native download receipt verification
in `internal/daemonconn/export_download_test.go`, release races/partial failure
in `internal/exporter/release_test.go`, real package-file MCP publication in
`internal/mcp/tools_packages_test.go`, and socket deadline behavior in
`internal/mcp/transport_test.go`. Reuse those seams; this review did not rerun
them or substitute them for future native MCP coverage.

## Adversarial review prompt

Review the linked design against the exact source baseline and spec hash above.
Treat this self-review as a claim ledger, not proof. Report High/Medium findings
with design line numbers and source evidence; do not implement or broaden scope.

Try to falsify these contracts:

1. Can a client obtain every required selection field through the proposed MCP
   surface, including retained historical versions, without opening the vault or
   guessing hashes/revisions? Does the hash addition preserve existing paging?
2. Can a disabled write be called anyway? Does preview retain authority despite
   its name? Do CLI and MCP contend for the same two master slots, and does
   explicit release leave the separate 32-record cap visible?
3. Can reconnect/retry, response decoding, reordered inputs, preview expiry, or
   released IDs duplicate work or mislead recovery? Distinguish the read helper
   from the single-attempt write path and daemon failures from local publication.
4. Can a damaged or different valid archive reach the destination? Can adapter
   sanitization lose integrity/conflict identity or expose a ticket? Does a job
   created elsewhere remain downloadable without incorrectly applying preview
   size/member/role restrictions?
5. Does the full publication path preserve destination/receipt state on a race,
   cancellation, directory-sync failure, cleanup failure, or lost MCP reply?
   Can HTTP's deadline be honored without promising 52 GiB delivery in two minutes?
6. Does any reuse claim require an unmentioned API, schema, dependency, ownership,
   or engine change? Can any proposed wrapper or field be removed while keeping
   the complete local operator workflow and its recovery behavior?

## Verdict

Ready for maintainer and independent adversarial review before implementation
planning. No High or Medium issue remains in this author review. The selected
scope remains proposed; implementation and its verification have not begun.
