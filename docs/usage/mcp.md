---
last_edited: 2026-10-05
title: Model Context Protocol
description: Connect a local MCP client to Docbank's read-mostly document tools through the daemon.
---

# Model Context Protocol

Docbank provides a read-mostly Model Context Protocol (MCP) server for local
agents. It implements only MCP `2026-07-28`, using the official Go SDK at
`github.com/modelcontextprotocol/go-sdk` v1.7.0. Older MCP versions and the
legacy `initialize` flow are rejected. Clients start with `server/discover`.

The MCP process is a client of the selected vault's daemon. It discovers or
starts that daemon the same way the CLI does and never opens `docbank.db`, a
blob, pack, or rendition directly. There is no MCP-only embedded-vault path and
no remote-daemon option.

## Start the server

Stdio is the default transport:

```bash
docbank mcp
# equivalent to: docbank mcp --transport stdio
```

An MCP client configuration can launch the same command:

```json
{
  "mcpServers": {
    "docbank": {
      "command": "docbank",
      "args": ["mcp", "--transport", "stdio"]
    }
  }
}
```

Stdio carries one complete JSON-RPC message per line. Docbank accepts LF or
CRLF terminators, rejects embedded newlines, and limits the JSON payload before
the terminator to 1 MiB. Stdout contains MCP frames only. Bounded, redacted
diagnostics go to stderr.

HTTP requires an explicit loopback IP and port plus a separate named bearer
binding:

```toml
# $DOCBANK_HOME/config.toml
[mcp.http]
credential_binding = "credential:mcp-http"

[credential_bindings.mcp-http]
environment_variable = "DOCBANK_MCP_HTTP_TOKEN"
```

Make `DOCBANK_MCP_HTTP_TOKEN` available only to the MCP process through the
machine's secret or process manager, then start the listener:

```bash
docbank mcp --transport http --listen 127.0.0.1:7341
```

Configure the client for `http://127.0.0.1:7341/mcp` and
`Authorization: Bearer <token>`. `--listen` accepts an explicit IPv4 or IPv6
loopback address, not a hostname, wildcard, private-LAN address, or public
address.

The HTTP credential is resolved from its environment binding when the MCP
process starts and remains fixed for that process. Restart the MCP process to
rotate it. It is separate from the daemon API key. Docbank refuses to start if
the values match and repeats that check whenever it acquires a restarted
daemon. The MCP bearer never appears in a flag, URL, runtime record, discovery
result, log, or error.

This bearer is a fixed local credential, not MCP OAuth. Docbank does not
publish protected-resource metadata, authorization-server discovery, dynamic
client registration, scopes, or token refresh. A client may connect locally or
through a trusted tunnel, but it must be able to set the Authorization header.
Clients that require the MCP HTTP OAuth flow are not supported.

Both transports have the fixed read catalog described below.
`--allow-processing` adds only guarded processing start.
`--allow-package-writes` permits load-file preflight, import, and custodian
changes. `--allow-photo-edits` permits photo asset mutations.
`--allow-export-writes` enables native export jobs and local downloads.
`--allow-report-writes` enables frozen report creation, revision, local delivery,
and explicit release. Each flag is independent. Enable the combination you need at startup.

## Exact protocol contract

Every request carries these two metadata fields:

```json
{
  "_meta": {
    "io.modelcontextprotocol/protocolVersion": "2026-07-28",
    "io.modelcontextprotocol/clientCapabilities": {}
  }
}
```

Every HTTP request also carries:

- `Content-Type: application/json`;
- an `Accept` value that permits both `application/json` and
  `text/event-stream`;
- `Mcp-Protocol-Version: 2026-07-28`;
- `Mcp-Method` matching the JSON-RPC method; and
- `Mcp-Name` matching the tool name for `tools/call`, or the complete resource
  URI for `resources/read`.

Missing or mismatched protocol headers return HTTP 400 with
`HeaderMismatch`. An unknown JSON-RPC method over HTTP returns HTTP 404 with
JSON-RPC `-32601`. `initialize`, missing protocol metadata, and every other
protocol version return the supported-version error and identify only
`2026-07-28`. Successful results include Docbank server information.

`server/discover` advertises tools, resource templates, resource reads, and
cancellation. The catalog is fixed when the process starts, so there are no
list-change notifications. These operations do not emit progress tokens.
Stdio cancellation uses `notifications/cancelled`. For ordinary HTTP
request/response calls, aborting or closing the request cancels its in-flight
handler work, and the response is JSON. Notification-only requests may instead
return HTTP 202 with no body. `subscriptions/listen` uses SSE, but Docbank
advertises no subscriptions. A call with an empty notification selection
acknowledges the request and completes immediately.

## Tool catalog

All inputs and outputs use closed JSON Schema 2020-12 objects, so unknown
fields are rejected. Every successful tool result contains both structured
content and its JSON text representation. The complete result, including
resource links, is capped at 1 MiB.

| Tool | Contract and important bounds |
| --- | --- |
| `get_report_summary` | Reads an owned frozen report by its 48-character lowercase hexadecimal ID. If the full summary exceeds the result cap, returns `report_limit`; the handle remains usable. |
| `get_report_dates` | Reads frozen date evidence with an optional opaque cursor and 1–100 members, default 50. Requests a 256 KiB page; candidates can continue on the next page. |
| `get_export_plan` | Reads the complete frozen plan header by `plan_id`, including its admission deadline. |
| `get_export_problems` | Reads up to 50 frozen unavailable outputs by `plan_id` and optional numeric `after`. |
| `get_export_status` | Reads one retained native export job, including progress, failure code, and any completed receipt. It never downloads or releases the job. |
| `get_vault_info` | Returns the stable vault ID and aggregate live, trash, version, and blob counts. It never returns the host vault path. |
| `list_documents` | Lists current, live files. `path_prefix` defaults to `/` and is capped at 16,384 Unicode characters and 16 KiB of UTF-8. Sorts are `path`, `name`, `modified_at`, `size`, and `media_type`, in `asc` or `desc` order. Page size defaults to 50 and is capped at 250. |
| `search_documents` | Requires a 1–8,192-character query, a 1–128-character processing profile name, and exactly one source selector: 1–4,096 unique content-version IDs or metadata filters. Mode defaults to `auto` and may be `auto`, `lexical`, `semantic`, or `hybrid`; result limit defaults to 20 and is capped at 100. Optional binding IDs are capped at 128 characters. |
| `get_document` | Requires an exact positive node ID and current content-version UUID. A stale, trashed, moved-to-another-version, or mismatched identity is rejected. |
| `list_document_versions` | Lists immutable versions, including each original's `blob_hash` and size, for one live file. Limit defaults to 100 and is capped at 250; offset is capped at 1,000,000. |
| `read_rendition_text` | Reads the exact vault/node/version/attachment tuple described under [Resources](#resources-and-rendition-windows). |
| `get_processing_plan` | Requires an exact node ID, content-version UUID, and 1–128-character processing profile name. Returns the complete provider, trust-boundary, retention, estimate, consent, and backup disclosure plus its fingerprint. |
| `get_processing_status` | Reads one stable 64-hex-character job identity. A response contains at most 64 embedding job IDs. |
| `get_processing_coverage` | Reports rendition and embedding coverage for 1–4,096 unique version IDs in one exact vault fence and one 1–128-character processing profile. The response has at most 65 coverage classes. |
| `get_package_import` | Reads durable progress for one import operation UUID. |
| `get_package_preflight` | Reads one retained preflight by its exact identity. |
| `list_package_preflight_diagnostics` | Pages through bounded diagnostics for a retained preflight. |
| `list_package_custodians` | Pages through active custodian claims for an exact package scope. |
| `find_people` | Finds bounded active canonical people by folded display-name prefix. |
| `get_person` | Reads one person, its identities, and its external UIDs. |
| `list_packages` | Pages through received and produced load-file packages. |
| `get_package` | Reads one package and its retained source authority. |
| `list_package_members` | Pages through a package's immutable document occurrences. |
| `get_package_record` | Reads one immutable sender row by its package-scoped record key. |
| `lookup_bates_label` | Finds bounded package-scoped matches for an exact received or assigned label. |
| `get_photo_asset` | Reads one photo asset by asset UUID or positive node ID. Returns the selected display source, `total_files`, `file_offset`, and a page of complete file decisions. Pass `next_file_offset` as `file_offset` to continue. Pages are live; restart if the asset revision changes. |

Starting the server with `--allow-photo-edits` adds these write tools:

| Tool | Contract and important bounds |
| --- | --- |
| `create_photo_asset` | Creates an asset for one positive file node. |
| `attach_photo_file` | Attaches one node at an expected asset revision. |
| `detach_photo_file` | Detaches one member at an expected asset revision. |
| `exclude_photo_asset` | Changes inclusion at an expected asset revision. |
| `promote_photo_asset` | Explicitly creates an asset for one live file node. |

Run `docbank photos unhide /path/to/photo.jpg` and enter the passcode before using MCP photo commands on a hidden asset. Ordinary MCP document tools retain access to its files.

Photo writes return the first file page with the committed asset revision. Continue through `get_photo_asset` when `next_file_offset` is present. Photo writes make one daemon request. An ambiguous transport failure returns
`processing_outcome_unknown`. Inspect the asset before retrying. Display and
vault settings writes remain HTTP and CLI operations.

`list_documents` uses live keyset pagination, not a snapshot. A mutation
between pages can change later membership or order. Each opaque cursor is at
most 32 KiB of ASCII, expires after 15 minutes, and authenticates the
normalized prefix, sort, direction, page size, position, and traversal. Cursors
carry their own position, so paging does not consume a shared daemon cursor
quota. The size bound accommodates paths up to 16 KiB. Tampered, expired,
reused-for-another-query, or daemon-restart-invalidated cursors fail instead of
restarting at page one.

Before searching, `search_documents` resolves its selector to a source fence:
the exact set of current, live versions it will search. Filters may select a
tag UUID, a MIME type of at most 255 characters, a positive ancestor node ID,
and RFC 3339 `modified_since` or `modified_before` bounds of at most 64
characters each. A scope above 4,096 versions returns `scope_too_large` with
the observed count and is never truncated or broadened. The result reports the
fence and its fingerprint, requested and actual mode, coverage, skipped
reasons, and truncation state. It returns at most 100 hits. Each excerpt is
capped at 512 characters and each hit has at most 64 evidence identities of at
most 1,024 characters each. A result contains at most 64 skipped-reason codes
of at most 64 characters each.

Document summaries use paths of at most 16,384 characters, names and MIME types
of at most 255 characters, and at most 64 active rendition identities.
Processing-plan responses contain at most 129 flow hops, 129 disclosed classes,
and 129 retained classes. Each runtime disclosure has at most 64 metadata
classes and 64 retained-artifact roles, and each flow hop has at most three
input classes. Processor, endpoint, deployment, model, revision, and
vector-space strings are capped at 1,024 characters, and provider and class
names at 128. The backup-consequence text is capped at 4,096 characters.
Processing status contains at most 64 embedding job IDs and caps state, phase,
and failure codes at 64 characters. `completed_bindings` is capped at 64.
Processing coverage contains at most 65 classes, with class names capped at 128
characters, states at 64, and each class count at 4,096. These schema limits
are admission bounds, not promises that a configured provider or vault will
fill them.

Expected domain failures return `isError: true` with a structured payload
capped at 1 KiB and a stable code:

- `not_found`
- `stale_version`
- `stale_revision`
- `plan_changed`
- `consent_required`
- `processing_outcome_unknown`
- `scope_too_large`
- `cursor_expired`
- `daemon_unavailable`
- `invalid_document_cursor`
- `invalid_rendition_window`
- `invalid_rendition_encoding`
- `invalid_photo_asset`
- `photo_node_not_eligible`
- `photo_node_owned`
- `audit_mutation_unsupported`

Invalid tool arguments use JSON-RPC `-32602`. Unexpected failures use a
sanitized JSON-RPC internal error. Stderr records the operation and a fixed
error category for diagnosis. Neither path includes host paths, SQL, provider
secrets, daemon keys, or unrequested document text.

## Resources and rendition windows

`resources/list` is intentionally empty. `list_documents`,
`search_documents`, and `get_document` attach resource links for each active
rendition, and `resources/templates/list` publishes one RFC 6570 template.

The canonical rendition URI is:

```text
docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}
```

The windowed resource template is:

```text
docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}{?offset,max_chars}
```

The vault and content-version identities are canonical lowercase UUIDv4
values, the node ID is a positive decimal integer, and the attachment ID is a
lowercase 64-character SHA-256 value. Only an active, sanitized Markdown
rendition whose complete tuple matches the URI can be read. Source binaries,
raw provider responses, inactive attachments, superseded versions, and host
paths are never resource content.

`offset` is a Unicode code-point offset from 0 through 2,147,483,647.
`max_chars` defaults to 8,000 and ranges from 1 through 16,000. Every window
ends on a valid UTF-8 boundary and reports the requested offset, actual start
and end, next offset, EOF state, media type, checksum, response byte count, and
source identities. `read_rendition_text` returns the same window data with the
tool error codes above. Resource reads report unavailable identities and invalid
windows as resource-not-found errors, and are capped at 1 MiB.

## Optional processing start

Start a process with the additional tool only when the agent is allowed to
enqueue already-consented work:

```bash
docbank mcp --transport stdio --allow-processing
```

The tool catalog then adds `start_processing`. It accepts only a
content-version UUID and the 64-character plan fingerprint previously returned
by `get_processing_plan` from the same MCP process. The process remembers at
most 4,096 reviewed plans. An evicted plan, a fingerprint from another process,
changed disclosure, changed profile, or stale source is rejected.

MCP cannot grant consent. The tool sends `consent=false` to the daemon and
succeeds only when the operator already granted consent for the identical plan
through an operator-controlled Docbank surface. With the public CLI, first call
`get_processing_plan` in MCP, then have the operator verify and consent to that
same plan:

```bash
docbank processing plan id:<node-id> --profile <configured-profile>
docbank processing build id:<node-id> \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

The fingerprint and content-version ID printed by the CLI plan must match the
MCP plan. `processing build --consent` records consent and also starts the
reviewed work. It is not a consent-only command. See
[Document processing](document-processing.md) for the complete operator flow.

After consent is active, `start_processing` returns the stable queued job
identities, including at most 64 embedding job IDs, and does not poll. An
initial `processing_outcome_unknown` result contains no job ID, so its outcome
cannot be reconciled through MCP. Do not retry it blindly, because MCP has no
job lookup for that case.

## Optional package writes

Allow the agent to preflight local load-file sources, import packages, and
change package custodians with a separate opt-in:

```bash
docbank mcp --transport stdio --allow-package-writes
```

This flag adds four tools:

| Tool | Contract |
| --- | --- |
| `preflight_load_file_package` | Reads a local source and retains a preflight with diagnostics before import. |
| `start_package_import` | Starts an import from an exact preflight identity and operation UUID. An exact retry with the same operation UUID returns the existing operation. |
| `resolve_package_custodian` | Links an existing package custodian claim to an exact canonical person, subject to the supplied revision. |
| `assign_package_custodian` | Creates or replaces an operator-owned package custodian, subject to the supplied revision. |

Review the preflight and its diagnostics before starting an import. Keep the
operation UUID to read progress with `get_package_import` and to reconcile a
retry. Custodian writes use exact identities and revision checks to reject
stale changes.

Package writes do not use the processing-plan consent flow. Enable this flag
only when the agent may perform these local reads and vault changes.
`--allow-package-writes` does not enable `start_processing`, and
`--allow-processing` does not enable package writes. Use both flags when both
capabilities are needed.

No MCP tool can delete documents, move, rename, tag, restore, prune, pack,
repack, change configuration, select credentials, grant processing consent, or
return source bytes. Package preflight may upload a local source container, but
there is no general document upload tool.

## Cache behavior

| Result | `ttlMs` | `cacheScope` |
| --- | ---: | --- |
| `server/discover` | `60000` | `public` |
| `tools/list` | `60000` | `public` |
| `resources/list` | `60000` | `public` |
| `resources/templates/list` | `60000` | `public` |
| Tool success containing vault or job data | `0` | `private` |
| `resources/read` | `0` | `private` |

Catalog results are public only because they contain no vault-derived data and
are identical for every caller of that process. HTTP adds
`Cache-Control: no-store` to every response, including errors and otherwise
public catalogs.

## HTTP limits and unsupported surface

HTTP is stateless and POST-only. Each request contains one JSON-RPC message.
There are no GET streams, sessions, `Mcp-Session-Id`, resume support, or
`Last-Event-ID`. The server allows 10 seconds to receive request headers. After
the headers arrive and the request passes the boundary checks, a separate
two-minute deadline covers request-body reads, daemon work, and response
writes. The public limits are:

| Resource | Limit |
| --- | ---: |
| Request body | 1 MiB |
| Combined header names and values | 32 KiB |
| Header field values, counting repeats separately | 128 |
| Bearer token | 4,096 bytes; empty values, spaces, and control bytes are rejected |
| Concurrent requests | 32 total |
| HTTP response body | 1,114,112 bytes |
| Header-read timeout | 10 seconds |
| Idle connection timeout | 30 seconds |

The request Host must identify a loopback address or `localhost`. An absent
Origin is valid for non-browser clients. If Origin is present, exactly one
plain-HTTP Origin must match that local Host and port. Unsafe or cross-origin
requests are rejected before authentication. Forwarded-host headers do not
change this decision.

Docbank does not expose MCP prompts, roots, sampling, elicitation, or tasks. It
does not negotiate a legacy protocol, open a remote daemon, provide remote
daemon configuration, implement OAuth, maintain HTTP sessions, resume streams,
or expose a general write surface.

Read calls may reacquire the local daemon and retry once only when transport
failure occurs before any response. Failures after a response are not replayed.
Daemon acquisition is capped at 45 seconds. Each HTTP request has the
two-minute post-header deadline above. Stdio work otherwise runs until
completion or client cancellation. Daemon restart, an idle shutdown, or a stale
runtime record therefore produces a bounded failure or recovers on the next
safe read without terminating stdio with diagnostics on stdout or leaving HTTP
pinned to a dead client.

## Native export jobs

`get_export_plan`, `get_export_problems`, and `get_export_status` are available
with all write flags disabled, over stdio or HTTP. Plan inspection requires a
retained plan ID. `get_export_plan` returns the complete `plan`, including
richer source, role, count, and volume fields when present. Its `expires_at` is
the admission deadline. The plan can stay readable longer while a job retains
it. Reading does not extend either deadline.

`get_export_problems` returns a `problems` object with `plan_id`,
`fingerprint`, `after`, `next`, `total`, and `items`. Omit `after` for the
first page, then pass a nonzero `next` as `after`. Zero marks the last page.
The tools return private results with `ttlMs: 0`. MCP may retry a read once
when transport fails before any response starts. Domain errors and partial
responses are never retried.

Plan reads use the shared API-key owner and cannot inspect browser-owned plans.
An expired or removed plan needs a fresh preview with new IDs for a new export.
Inspect an existing job through status. Problem details remain frozen after
source edits. Totals count unavailable outputs and inventory problems, not
selected documents. Originals-only previews normally have none.

The fixed 50-item page must fit within 64 KiB. An `export_limit` response
returns no partial page. HTTP has the same limit and cannot request fewer
items. Releasing jobs cannot shrink that page. See
[retained-plan inspection](export-bundles.md#inspect-a-retained-plan) for
offsets, identity fields, and retention behavior.

Start with `docbank mcp --allow-export-writes` to let a client retain exact
originals and manage their export. Review the selection with the operator.

| Tool | Required arguments and result |
| --- | --- |
| `preview_export` | `source_operation_id`, `plan_operation_id`, and `members`. Returns a retained `plan`. |
| `start_export` | `operation_id`, `plan_id`, and `fingerprint`. Returns the `job`; it does not wait for completion. |
| `cancel_export` | `job_id`. Permanently stops active work. Returns `accepted: true`; completed jobs conflict. |
| `download_export` | `job_id`, absolute `destination_path`, optional `overwrite` (default false). Returns the verified `receipt` and publication state. |
| `release_export` | `job_id`. Deletes a terminal job and its retained archive, returning `released: true`. |

List versions for each selected node. Copy `content_version_id` to
`version_id`, `blob_hash` to `sha256`, and copy `node_id` and `size` into each
member. This takes one `list_document_versions` call per node, with more pages
if needed. Each preview accepts 1–1,000 retained original versions totaling at
most 50 GiB. It can include multiple versions of the same node. Size is
required, including zero for an empty original. Optional `revision` is a
current node precondition. Omit it unless needed. A version's historical node
revision is not that precondition.

Generate all operation IDs before calling. After `export_outcome_unknown`,
inspect the known job ID or replay the same request and IDs. The first write
after a daemon restart can report this even when nothing was sent. Docbank does
not reconnect and repeat writes automatically. Preview replay must keep member
order and remains subject to its original ten-minute admission window. After a
delayed start response, use job status instead of replaying preview. Never
reuse a released job ID: its replay record has been deleted.

For `export_timeout`, `export_canceled`, or `export_failed` during start,
download, or release, read `get_export_status` before retrying. Use the start
operation ID as the job ID. After a download error, inspect the destination
too, because the file may already be saved. After a release error, not found
confirms removal.

MCP shares the API-key owner with the CLI. Completed jobs still occupy the two
shared job slots until released or expired. Release does not delete originals
or local downloads. `export_retained` means a download ticket or lease still
holds the archive. Retry release after it closes. The source and plan records
have separate global limits. See
[export limits and recovery](export-bundles.md).

To save an export:

1. Call `preview_export` with your exact members and two fresh operation IDs.
2. Review `plan.total` and `plan.role_bytes`, then call `start_export` with a
   fresh operation ID, `plan.id`, and `plan.fingerprint`.
3. Call `get_export_status` with that job ID. When it completes, inspect
   `job.receipt.size` before choosing a download transport.
4. Call `download_export` with the job ID and a destination on the MCP host.
5. After saving the file, call `release_export` to free the retained job slot.

The destination parent must exist and resolve outside the Docbank data
directory. An existing destination must be a regular file and requires
`overwrite: true`. Verification occurs before publication: the daemon verifies
the retained archive, then the MCP process streams it into a private stage and
independently verifies the complete bundle against the job and ticket receipts.
The result contains no ZIP bytes or download ticket. Download never releases,
cancels, or recreates the job automatically.

`state: "published"` means the verified file is saved.
`state: "published_durability_unknown"` means the file became visible but the
subsequent directory sync failed. Both return the receipt.
`cleanup_failed: true` means private staging cleanup failed after publication.
The saved file remains. Before publication, ordinary failures preserve the
destination and attempt stage cleanup. `export_integrity` identifies a receipt
or archive mismatch, and `export_local_io` identifies a local file-operation
failure. The operator log records the operation and its cause. The client
receives a fixed message. A failed stage cleanup can leave a private stage
behind and is logged separately. A secondary cleanup failure does not replace
the original error.

`export_unavailable` means the daemon has no export worker. That request made
no change. Check the daemon and retry. For `export_expired`, preview again with
new IDs if admission expired, or release a retained job whose archive is
missing.

The HTTP two-minute deadline includes daemon verification, transfer, and local
verification. Archive size alone cannot predict whether all three fit. For a
large job, use `docbank export download <job-id> <path>` against the same vault
or a suitable stdio invocation. A later download obtains a fresh ticket and
transfers the whole file again. A lost response or cancellation after
publication can leave a verified destination. Inspect that file before retrying
with overwrite.

## Frozen search reports

Enable report writes to capture exact current versions, review dates, save an
evidence ZIP on the machine running MCP, and release unneeded live reports:

```bash
docbank mcp --allow-report-writes
# Include original-file exports in the same workflow:
docbank mcp --allow-report-writes --allow-export-writes
```

| Tool | Contract |
| --- | --- |
| `create_report` | Takes a version 1 `request` with `selected_documents`, `timezone`, and `terms`. Captures 1–1,000 current document identities and 1–128 term rows. |
| `revise_report` | Takes `report_id` and 1–1,000 reviewed date `choices`. Creates a child, retaining unmentioned parent choices and the original expiration. |
| `download_report` | Takes `report_id`, an absolute `destination_path`, and optional `overwrite` (default false). Saves a verified ZIP outside the data directory. |
| `release_report` | Takes `report_id`. Discards this live packet and date-review evidence and frees its handle slot. History, saved files and published children remain. |

These writes require the report flag. The two inspection tools remain available
without it and can inspect CLI-created whole-vault or collection reports too.
Creation through MCP accepts only exact current document selections. It starts
no processing or provider work.

1. Use `list_documents` or search to choose node/current-version pairs. For
   each node, call `list_document_versions` and match the exact version to
   obtain `blob_hash`. `get_document` inspects a chosen pair. It requires both
   IDs.
2. Build `selected_documents.documents` with each positive `node_id`, canonical
   lowercase UUIDv4 `version_id`, and lowercase SHA-256 `sha256`. Select each
   node once. Use the [report request format](search-exports.md)
   for terms, inclusive date cutoffs, timezone, and optional processing profile.
3. Call `create_report`. Omitted `coverage_mode` means `strict`. Choose
   `available_only` if incomplete evidence is acceptable. A changed selection
   returns `report_selection_changed`. Refresh and reselect rather than
   substituting newer versions automatically.
4. For `needs_review`, page through `get_report_dates`. Keep each member's
   earlier candidates when `candidates_complete` is false. Continue with the
   returned cursor until `next_cursor` is absent. Review the evidence with the
   operator, then submit the existing [date-choice format](search-exports.md)
   to `revise_report`. Restart date paging on the child. Parent cursors cannot
   be used on it.
5. Read counts and coverage through `get_report_summary`. Download a complete
   report before it expires. To export the same originals, pass the selected
   identities and their version-list sizes to `preview_export` separately.

Creation rejects `all_documents: true`, `collection_ids`, and `date_choices`.
Unknown fields and explicit nulls are invalid. Expressions allow 8,192 Unicode
characters. Profile and timezone strings allow 128 UTF-8 bytes. A selection's
member count is not the coverage `scoped` count: members without usable dates
can remain in an available-only packet without contributing to counted coverage.

Create and revise return compact receipts: `report_id`, optional `parent_id`,
`state`, `observed_at`, `expires_at`, and `unresolved_dates`. Complete receipts
also contain `bundle_bytes` and `bundle_sha256`. Needs-review receipts omit
them. A large warning summary does not prevent delivery of the new handle. If
full summary inspection returns `report_limit`, use
`docbank search-export show <report-id> --json` or read it through HTTP. Dates,
revision, and download remain available.

A revision allows 1,000 choices and 4,096 UTF-8 bytes per reason, but the entire
MCP message must fit within 1 MiB, including escaping and its envelope. The two
individual maxima do not fit together. Choose shorter reasons where appropriate
or submit batches to successive children. Docbank never splits a revision for
you. For a larger batch, the existing HTTP/CLI revision path accepts 8 MiB.

CLI and MCP share eight report handles. Every revision uses another slot. An
initial report plus seven revisions fills those slots if no other reports are
retained. All descendants expire 30 minutes after the original observation.
Download frees no slot. Release unneeded handles explicitly with `release_report`,
or wait for expiry when capacity is full. The engine also permits two simultaneous builds
and 64 handles or pending builds globally. A restart loses live handles, and
history receipts do not restore their artifacts.

Writes are never automatically replayed. `report_outcome_unknown` means a
report write may have succeeded without a usable reply. For create or revise, inspect
history with the operator using `docbank search-export history --json`, or
through web/HTTP. History records requests and outcomes but does not establish
live availability or reliably identify a lost reply. There is no idempotent
replay. A deliberate retry creates another observation or child and uses
another slot. The first write after a daemon restart may also return this error
on a stale connection.

`release_report` is destructive and requires `--allow-report-writes`. It can
release complete or review-pending reports; pending date evidence is lost.
Success returns `report_id`, `released: true`, `ttlMs: 0`, and
`cacheScope: "private"`. Child reports keep their original expiry. Shared memory
remains while a child or in-flight operation uses it, so releasing one handle
does not guarantee global build or memory capacity.

An active download returns `report_retained`; retry deliberately after its
server reader closes, even if the download call has already returned. After
an uncertain release reply, inspect `get_report_summary` for the same ID. A
successful read proves the handle remains. `report_unavailable` alone is
inconclusive: MCP omits HTTP status, and the code can mean either a missing
handle or an unavailable reporting service. History does not prove release.
Repeating release for an absent handle returns unavailable. A deliberate retry
uses the same ID and cannot create a new handle.

Downloads compare the summary, stream size and digest, and independently
verified packet before publication. The CLI uses the same verification path.
Success reports `internally_consistent: true` and `source_verified: false`.
This does not establish authenticity of the source vault. The receipt's state
is `published`, or `published_durability_unknown` if the file was saved but the
final directory sync failed. `cleanup_failed` separately reports a staging
cleanup failure. Local causes go to the operator log. If the reply is lost,
inspect and verify the destination before retrying.

Read `bundle_bytes` before choosing a transport. MCP HTTP's two-minute deadline
covers transfer, verification, and publication, so large ZIPs may not finish.
Use stdio for the same live handle, or `docbank search-export download
<report-id> --output report.zip` against the same vault. The CLI verifies the
existing packet outside the MCP HTTP deadline and does not create another
report. See
[CLI report recovery](search-exports.md#inspect-or-recover-an-existing-export)
for inspection, history paging, and recovery after a local save error. ZIPs are
capped at 512 MiB, and concurrent MCP downloads share a 1 GiB verification
budget. `report_limit` can also mean that this shared allowance is exhausted.
Source changes after capture leave the frozen report's evidence and counts
unchanged.
