---
last_edited: 2026-08-29
title: Model Context Protocol
description: Connect a local MCP client to Docbank's bounded, daemon-first document surface.
---

# Model Context Protocol

Docbank exposes a bounded, read-mostly Model Context Protocol server for local
agents. It implements exactly MCP `2026-07-28` with the official Go SDK at
`github.com/modelcontextprotocol/go-sdk` v1.7.0. Older MCP versions and the
legacy `initialize` flow are rejected; clients start with `server/discover`.

The MCP process is a client of the selected vault's daemon. It discovers or
starts that daemon exactly like the CLI and never opens `docbank.db`, a blob,
pack, or rendition directly. There is no MCP-only embedded-vault path and no
remote-daemon option.

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
rotate it. It is separate from the daemon API key: Docbank refuses startup if
the values match and repeats that check whenever a restarted daemon is
acquired. The MCP bearer never appears in a flag, URL, runtime record,
discovery result, log, or error.

This bearer is a fixed local credential, not MCP OAuth. Docbank does not
publish protected-resource metadata, authorization-server discovery, dynamic
client registration, scopes, or token refresh. A client may connect locally or
through a trusted tunnel, but it must be able to set the Authorization header;
clients that require the MCP HTTP OAuth flow are unsupported.

Both transports have the fixed read catalog described below.
`--allow-processing` adds only guarded processing start.
`--allow-package-writes` separately permits load-file preflight, import, and
custodian changes. `--allow-photo-edits` separately permits photo asset
mutations. `--allow-migration-writes` separately permits Fotobank inventory.
Enable any combination of flags when starting the process.

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

All inputs and outputs use closed JSON Schema 2020-12 objects: unknown fields
are rejected. Every successful tool result contains both structured content
and its JSON text representation. The complete result, including resource
links, is capped at 1 MiB.

| Tool | Contract and important bounds |
| --- | --- |
| `get_vault_info` | Returns the stable vault ID and aggregate live, trash, version, and blob counts. It never returns the host vault path. |
| `list_documents` | Lists current, live files. `path_prefix` defaults to `/` and is capped at 16,384 Unicode characters and 16 KiB of UTF-8. Sorts are `path`, `name`, `modified_at`, `size`, and `media_type`, in `asc` or `desc` order. Page size defaults to 50 and is capped at 250. |
| `search_documents` | Requires a 1–8,192-character query, a 1–128-character processing profile name, and exactly one source selector: 1–4,096 unique content-version IDs or metadata filters. Mode defaults to `auto` and may be `auto`, `lexical`, `semantic`, or `hybrid`; result limit defaults to 20 and is capped at 100. Optional binding IDs are capped at 128 characters. |
| `get_document` | Requires an exact positive node ID and current content-version UUID. A stale, trashed, moved-to-another-version, or mismatched identity fails closed. |
| `list_document_versions` | Lists immutable versions for one live file. Limit defaults to 100 and is capped at 250; offset is capped at 1,000,000. |
| `read_rendition_text` | Reads the exact vault/node/version/attachment tuple described under [Resources](#resources-and-rendition-windows). |
| `get_processing_plan` | Requires an exact node ID, content-version UUID, and 1–128-character processing profile name. Returns the complete provider, trust-boundary, retention, estimate, consent, and backup disclosure plus its fingerprint. |
| `get_processing_status` | Reads one stable 64-hex-character job identity. A response contains at most 64 embedding job IDs. |
| `get_processing_coverage` | Reports rendition and embedding coverage for 1–4,096 unique version IDs in one exact vault fence and one 1–128-character processing profile. The response has at most 65 coverage classes. |
| `get_package_import` | Reads durable progress for one import operation UUID. |
| `get_package_preflight` | Reads one retained preflight by its exact identity. |
| `list_package_preflight_diagnostics` | Pages through bounded diagnostics for a retained preflight. |
| `list_package_custodians` | Pages through active custodian claims for an exact package scope. |
| `find_people` | Finds bounded canonical person candidates for custodian resolution. |
| `list_packages` | Pages through received and produced load-file packages. |
| `get_package` | Reads one package and its retained source authority. |
| `list_package_members` | Pages through a package's immutable document occurrences. |
| `get_package_record` | Reads one immutable sender row by its package-scoped record key. |
| `lookup_bates_label` | Finds bounded package-scoped matches for an exact received or assigned label. |
| `get_photo_asset` | Reads one photo asset by asset UUID or positive node ID. The response has at most 256 files and includes the selected display source. |
| `list_migration_runs` | Lists at most 50 completed inventory runs. |
| `show_migration_run` | Reads one report and owner map. |

Starting the server with `--allow-photo-edits` adds these write tools:

| Tool | Contract and important bounds |
| --- | --- |
| `create_photo_asset` | Creates an asset for one positive file node. |
| `attach_photo_file` | Attaches one node at an expected asset revision. |
| `detach_photo_file` | Detaches one member at an expected asset revision. |
| `exclude_photo_asset` | Changes inclusion at an expected asset revision. |
| `promote_photo_asset` | Explicitly creates an asset for one live file node. |

Photo writes make one daemon request. An ambiguous transport failure returns
`processing_outcome_unknown`; inspect the asset before retrying. Display and
vault settings writes remain HTTP and CLI operations.

## Optional Fotobank inventory

Enable the migration write surface separately when an operator has selected a
stopped install or recovery archive:

```bash
docbank mcp --transport stdio --allow-migration-writes
```

The catalog then adds `inventory_fotobank`. Its input selects either
`catalog_path` plus `vault_root`, or `archive_root` with an optional
`snapshot_id`, and requires an absolute `owner_map_path`. The daemon reads the
source, writes the exclusive owner-map template, and stores the immutable run.
The tool returns the report and output path. A transport failure is not
replayed because the write may already have completed. Use
`list_migration_runs` or `show_migration_run` to inspect saved history.

`list_documents` uses live keyset pagination, not a snapshot. A mutation between
pages can change later membership or order. Each opaque cursor is at most 32 KiB of ASCII, expires after 15 minutes, and
authenticates the normalized prefix, sort, direction, page size, position, and
traversal. Cursors carry their own position, so paging does not consume a shared
daemon cursor quota. The size bound accommodates paths up to 16 KiB. Tampered,
expired, reused-for-another-query, or daemon-restart-invalidated cursors fail
instead of restarting at page one.

`search_documents` resolves its selector to an exact current, live source
fence before searching. Filters may select a tag UUID, a MIME type of at most
255 characters, a positive ancestor node ID, and RFC 3339 `modified_since` or
`modified_before` bounds of at most 64 characters each. A scope above 4,096 versions returns
`scope_too_large` with the observed count and is never truncated or broadened.
The result reports the exact fence and fingerprint, requested and actual mode,
coverage, skipped reasons, and truncation state. It returns at most 100 hits;
each excerpt is capped at 512 characters and each hit has at most 64 evidence
identities of at most 1,024 characters each. A result contains at most 64
skipped-reason codes of at most 64 characters each.

Document summaries use paths of at most 16,384 characters, names and MIME
types of at most 255 characters, and at most 64 active rendition identities.
Processing-plan responses contain at most 129 flow hops, 129 disclosed
classes, and 129 retained classes. Each runtime disclosure has at most 64
metadata classes and 64 retained-artifact roles; each flow hop has at most
three input classes. Processor, endpoint, deployment, model, revision, and
vector-space strings are capped at 1,024 characters; provider and class names
are capped at 128. The backup-consequence text is capped at 4,096 characters.
Processing status contains at most 64 embedding job IDs and caps state, phase,
and failure codes at 64 characters; `completed_bindings` is capped at 64.
Processing coverage contains at most 65 classes, with class names capped at
128 characters, states at 64, and each class count at 4,096. These schema
limits are admission bounds, not promises that a configured provider or vault
will fill them.

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
content-version UUID and the exact 64-character plan fingerprint previously
returned by `get_processing_plan` from the same MCP process. The process
remembers at most 4,096 reviewed plans. An evicted plan, a fingerprint from
another process, changed disclosure, changed profile, or stale source fails
closed.

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
reviewed work; it is not a consent-only command. See [Document
processing](document-processing.md) for the complete operator flow.

After consent is active, `start_processing` returns the stable queued job
identities, including at most 64 embedding job IDs, and does not poll. An
initial `processing_outcome_unknown` result contains no job ID, so its outcome
cannot be reconciled through MCP. Do not blindly retry it: MCP has no job lookup
for that case.

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
repack, change configuration, select credentials, grant processing consent,
or return source bytes. Package preflight may upload a local source container;
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

HTTP is stateless and POST-only. Each request contains one JSON-RPC message;
there are no GET streams, sessions, `Mcp-Session-Id`, resume support, or
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
plain-HTTP Origin must match that local Host and port; unsafe or cross-origin
requests are rejected before authentication. Forwarded-host headers do not
change this decision.

Docbank does not expose MCP prompts, roots, sampling, elicitation, or tasks. It
does not negotiate a legacy protocol, open a remote daemon, provide remote
daemon configuration, implement OAuth, maintain HTTP sessions, resume streams,
or expose a general write surface.

Read calls may reacquire the local daemon and retry once only when transport
failure occurs before any response. Failures after a response are not replayed.
Daemon acquisition is capped at 45 seconds. Each HTTP request has the
two-minute post-header deadline above; stdio work otherwise runs until
completion or client cancellation. Daemon restart, an idle shutdown, or a
stale runtime record therefore produces a bounded failure or recovers on the
next safe read without terminating stdio with diagnostics on stdout or leaving
HTTP pinned to a dead client.
