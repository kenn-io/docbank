---
last_edited: 2026-11-13
title: Model Context Protocol
description: Connect a local MCP client to Docbank's read-mostly document tools through the daemon.
---

# Model Context Protocol

Docbank provides a read-mostly Model Context Protocol (MCP) server for local
agents. The server implements only MCP `2026-07-28` using the official Go SDK at
`github.com/modelcontextprotocol/go-sdk` v1.8.0. It rejects older MCP versions
and the legacy `initialize` flow; clients start with `server/discover`.

Standalone MCP connects as a client to your selected vault's daemon. It
discovers or starts that daemon the same way the CLI does and never opens
`docbank.db`, a blob, a pack, or a rendition directly. There is no MCP-only
embedded-vault path and no remote-daemon option.

## Server identity

Both stdio and HTTP clients receive Docbank's website and icon in
`server/discover` metadata. The MCP icon depicts a document with a vault dial
and is separate from the website favicon. Because it is an embedded 256×256 PNG
data URI, clients can load it without an external request, though whether the
icon appears depends on the client. Results from `tools/call` and
`resources/read` repeat a compact server identity without embedding the image
again.

Standalone and daemon-hosted MCP HTTP listeners also serve that PNG at
`/icon.png` and `/favicon.ico`. Those public image routes accept GET and HEAD
without a bearer token, while `/mcp` still requires bearer authentication.

## Start the server

Stdio is the default transport:

```bash
docbank mcp
# equivalent to: docbank mcp --transport stdio
```

To register Docbank with installed Claude Code and Codex clients, run:

```bash
docbank agent setup
docbank agent setup --check
```

Setup enables Codex's `mcp_2026_07_28` feature, and Codex then shows an
under-development feature notice each session. Re-running setup registers the
read-only tools and replaces extra arguments, including `--allow-*`. To
configure write tools, use `docbank mcp config`.

For manual setup, print the configuration for your client:

```bash
docbank mcp config --client claude-code
docbank mcp config --client codex
# Include the write tools you need:
docbank mcp config --client codex --allow-processing
```

Paste Claude Code's JSON into your project's `.mcp.json`, or merge the
`docbank` entry into its existing `mcpServers` object. Keep that `.mcp.json`
out of version control because it contains machine-specific paths. For Codex,
paste the TOML into `~/.codex/config.toml` and replace any existing
`[mcp_servers.docbank]` entry. See the
[Claude Code MCP guide](https://code.claude.com/docs/en/mcp) and
[Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).

The printed snippet uses Docbank's absolute launch path, preserving
package-manager symlinks, along with your selected vault's canonical
`DOCBANK_HOME`. All six `--allow-*` flags carry through to stdio arguments.
Printing creates no vault, starts no daemon, and leaves client files untouched.
Your client still needs support for MCP `2026-07-28` and `server/discover`;
configuration alone cannot add protocol support.

Tested clients are Claude Code 2.1.301 and Codex 0.164.0. The Codex snippet
includes this command to persist MCP protocol support in `config.toml` for
either transport:

```bash
codex features enable mcp_2026_07_28
```

After running it, launch Codex with plain `codex`. The generated stdio
configuration also sets `CODEX_MCP_PROTOCOL_VERSION=2026-07-28` for Codex, which
reads that variable to pick the protocol version and does not pass it to the
server. After you upgrade Docbank, rerun `docbank mcp config` and replace the
saved configuration so its protocol selection matches the installed server.

Stdio carries one complete JSON-RPC message per line. Docbank accepts LF or
CRLF terminators, rejects embedded newlines, and limits the JSON payload before
the terminator to 1 MiB. Stdout contains MCP frames only, while bounded,
redacted diagnostics go to stderr.

HTTP requires an explicit IP and port plus a separate named bearer binding:

```toml
# $DOCBANK_HOME/config.toml
[mcp.http]
credential_binding = "credential:mcp-http"

[credential_bindings.mcp-http]
environment_variable = "DOCBANK_MCP_HTTP_TOKEN"
```

Make `DOCBANK_MCP_HTTP_TOKEN` available to the MCP process through your
machine's secret or process manager, then start the listener:

```bash
docbank mcp --transport http --listen 127.0.0.1:7341
```

You can then print the client configuration for that listener:

```bash
docbank mcp config --client claude-code --transport http --listen 127.0.0.1:7341
docbank mcp config --client codex --transport http --listen 127.0.0.1:7341
```

The printed URL ends in `/mcp`. Claude Code receives an `Authorization` header
with `Bearer ${DOCBANK_MCP_HTTP_TOKEN}`, and Codex receives
`bearer_token_env_var = "DOCBANK_MCP_HTTP_TOKEN"`. The command reads the
selected credential binding's variable name without resolving its value. If your
shell has no binding configured, it prints `DOCBANK_MCP_HTTP_TOKEN`, the
listener's conventional variable, though the listener still requires its token.
Make that same variable available to the client as well. Even if the listener
reads `DOCBANK_MCP_HTTP_TOKEN_FILE`, the client still needs
`DOCBANK_MCP_HTTP_TOKEN` containing the token itself. A custom binding uses its
own variable name.

`--listen` accepts an explicit IPv4 or IPv6 address and port, including
wildcards, and rejects hostnames and IPv6 zone identifiers. Printed URLs replace
wildcard addresses with loopback and require a nonzero port. Put HTTP write
flags on the standalone listener's startup command.

Docbank resolves the HTTP credential from its environment binding when the
listener starts and keeps it fixed for the life of the hosting process. To
rotate it, restart that process. This credential is separate from the daemon API
key, and Docbank refuses to start if the two values match. Standalone MCP
repeats that check whenever it acquires a restarted daemon. The MCP bearer never
appears in a flag, URL, runtime record, discovery result, log, or error.

This bearer is a fixed credential rather than MCP OAuth. Docbank does not
publish protected-resource metadata, authorization-server discovery, dynamic
client registration, scopes, or token refresh. Your client can connect locally
or through a trusted tunnel, as long as it can set the Authorization header.
Docbank does not support clients that require the MCP HTTP OAuth flow.

The daemon also hosts the same read-only HTTP tools at `/mcp` on its API port.
For a loopback API, hosting is on by default only when both `[mcp.http] port`
and `[mcp.http] enabled` are unset and the MCP bearer resolves in the daemon's
environment; without that bearer, the daemon logs why and starts without MCP.
Set `[mcp.http] enabled` or `DOCBANK_MCP_HTTP_ENABLED` to `true` to require
hosting, or to `false` to turn it off. Setting `[mcp.http] port` moves hosting
to a separate loopback port and turns the default off, so a separate loopback
port also needs `[mcp.http] enabled = true` or `DOCBANK_MCP_HTTP_ENABLED=true`.
The API port is ephemeral unless you set `[server] api_port`, so pin it for a
stable client URL; the daemon logs the MCP URL at startup. The daemon enforces
the same credential and Host rules and rejects its own API key at `/mcp`. MCP
reads run through that daemon's API, and on shutdown, in-flight reads get up to
five seconds to finish before the API closes. A daemon hosting MCP does not idle
out. When hosting is enabled explicitly, invalid credentials or a failed bind
stop daemon startup. To rotate the daemon's bearer, restart the daemon. See the
[configuration reference](../configuration.md#mcp-http-credential) and
[container example](containers.md#run-mcp-in-the-same-container).

To connect to the daemon-hosted endpoint, pass its pinned `[server] api_port`
or separate `[mcp.http] port` as `--listen` when printing configuration. For
example, `--listen 127.0.0.1:7341` targets the daemon when its API port is 7341.
Start the daemon with the same bearer binding available in its environment;
printing configuration leaves starting the listener to you.

Both transports expose the fixed read catalog described below. Standalone MCP
also accepts six independent write flags that you can combine at startup:

- `--allow-processing` adds only guarded processing start.
- `--allow-package-writes` permits load-file preflight, import, custodian
  changes, Bates exports, reviewed recipient ZIPs, PDF preparation, and
  publication of an
  operator-reserved reviewed PDF.
- `--allow-photo-edits` permits photo asset mutations.
- `--allow-export-writes` enables native export jobs and local downloads.
- `--allow-report-writes` enables frozen report creation, revision, local
  delivery, and explicit release.
- `--allow-kata-writes` permits quoting verified passages into Kata issues.

These flags apply only to standalone MCP. The daemon-hosted catalog stays
read-only.

## Connect over a trusted network

Choosing a non-loopback listener opts into sending the MCP bearer and returned
vault data over the selected network. This transport uses plain HTTP; use a
trusted private container network, an encrypted tunnel, or an HTTPS proxy. The
bearer grants the read-only catalog, with extra tools available only through
standalone MCP's startup flags, and stays separate from the daemon's
full-authority API key. The MCP process still connects to its local daemon;
remote-daemon configuration is not supported.

You can start the listener without `config.toml` by pointing to a mounted bearer
file:

```bash
DOCBANK_MCP_HTTP_TOKEN_FILE=/run/secrets/mcp-bearer \
DOCBANK_MCP_HTTP_ALLOWED_HOSTS=docbank:7341 \
  docbank mcp --transport http --listen 0.0.0.0:7341
```

`DOCBANK_MCP_HTTP_TOKEN` or its `_FILE` form selects an environment-backed named
credential binding and overrides a TOML binding for that process. Alternatively,
you can keep the TOML example above and supply either its named variable or
`<variable>_FILE`. Conflicting nonempty values or invalid selected files fail
HTTP startup and never fall back to another source. The daemon reads the MCP
bearer only when MCP hosting is enabled explicitly or by default. To rotate the
bearer, restart the hosting process.

List accepted client authorities in `[mcp.http] allowed_hosts`, with the
comma-separated `DOCBANK_MCP_HTTP_ALLOWED_HOSTS` replacement, or with additive,
repeatable `--allowed-host docbank:7341` flags. Values are IPs or ASCII DNS
names, optionally with ports; a name without a port permits any port. Docbank
also accepts loopback hosts and a concrete listen IP, while wildcard binds do
not permit arbitrary Host names. These flags are valid only with HTTP.

A network client connects to `http://docbank:7341/mcp` with
`Authorization: Bearer <token>`. Missing or invalid credentials return `401`,
and unconfigured Hosts or cross-origin requests return `403` before
authentication. The
[container guide](containers.md#run-mcp-in-the-same-container) shows how to
enable the daemon-hosted listener in the stock image.

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

- `Content-Type: application/json`
- An `Accept` value that permits both `application/json` and `text/event-stream`
- `Mcp-Protocol-Version: 2026-07-28`
- `Mcp-Method` matching the JSON-RPC method
- `Mcp-Name` matching the tool name for `tools/call`, or the complete resource
  URI for `resources/read`

Missing or mismatched protocol headers return HTTP 400 with `HeaderMismatch`,
and an unknown JSON-RPC method over HTTP returns HTTP 404 with JSON-RPC
`-32601`. `initialize`, missing protocol metadata, and every other protocol
version return the supported-version error and identify only `2026-07-28`.
Successful results include Docbank server information.

`server/discover` advertises only the tools and resources capabilities. The
catalog is fixed when the process starts, so there are no list-change
notifications, and these operations do not emit progress tokens. Cancellation
still works even though `server/discover` does not advertise it: stdio uses
`notifications/cancelled`, and for ordinary HTTP request/response calls,
aborting or closing the request cancels its in-flight handler work while normal
responses return JSON. Notification-only requests can instead return HTTP 202
with no body. `subscriptions/listen` uses SSE, though Docbank advertises no
subscriptions; a call with an empty notification selection acknowledges the
request and completes immediately.

## Tool catalog

All inputs and outputs use closed JSON Schema 2020-12 objects, so the server
rejects unknown fields. Every successful tool result includes both structured
content and its JSON text representation, with the complete result, including
resource links, capped at 1 MiB.

Tests cap the default catalog at 150,000 bytes and the catalog with all write
flags enabled at 237,000 bytes.
They measure compact JSON of the complete client-decoded `tools/list` result
with HTML escaping disabled to match the SDK, including output schemas,
metadata, and cache fields; a larger catalog requires a reviewed budget edit.

| Tool | Contract and important bounds |
| --- | --- |
| `get_report_summary` | Reads an owned frozen report by its 48-character lowercase hexadecimal ID. If the full summary exceeds the result cap, returns `report_limit`; the handle remains usable. |
| `get_report_dates` | Reads frozen date evidence with an optional opaque cursor and 1–100 members, default 50. Requests a 256 KiB page; candidates can continue on the next page. |
| `get_export_plan` | Reads the complete frozen plan header by `plan_id`, including its admission deadline. |
| `get_export_problems` | Reads up to 50 frozen unavailable outputs by `plan_id` and optional numeric `after`. |
| `get_export_status` | Reads one retained native export job, including progress, failure code, and any completed receipt. It never downloads or releases the job. |
| `get_vault_info` | Returns the stable vault ID and aggregate live, trash, version, and blob counts. It never returns the host vault path. |
| `list_changes` | Returns up to 500 retained events with `cursor`, `kind`, `node_id`, and `revision`. Pass the saved `next_cursor` to resume; call again while `has_more` is true. |
| `list_documents` | Lists current, live files. `path_prefix` defaults to `/` and is capped at 16,384 Unicode characters and 16 KiB of UTF-8. Sorts are `path`, `name`, `modified_at`, `size`, and `media_type`, in `asc` or `desc` order. Page size defaults to 50 and is capped at 250. |
| `answer_question` | Searches for evidence with a question and returns passages, leads, coverage, and a receipt. Optional filters, including a daemon-resolved `under_path`, narrow the scope; profile, binding, reranking, and output limits can be selected explicitly. |
| `search_documents` | Requires a 1–8,192-character query and exactly one source selector: 1–4,096 unique content-version IDs or metadata filters, including `under_path`, selecting every matching live file without a 4,096-file cap. The optional profile name is 1–128 characters. Omitting it uses the same profile and binding retries as `answer_question`. Results include the profile used. Mode defaults to `auto` and may be `auto`, `lexical`, `semantic`, or `hybrid`; result limit defaults to 20 and is capped at 100. Optional binding IDs are capped at 128 characters and require an explicit profile. Optional `rerank` is a boolean that requests the profile's configured reranker. |
| `find_conflicts` | Requires a processing profile and uses the query, limit, and source selectors (including `under_path`) from `search_documents`. Retrieval is lexical only, with at most 20 hits. Compares money, percentages, and durations in whole source sentences and returns source dates and evidence IDs. |
| `get_document` | Requires an exact positive node ID and current content-version UUID. A stale, trashed, moved-to-another-version, or mismatched identity is rejected. |
| `get_original_download_url` | Returns a single-use URL for an exact original selection, without the 32 MiB read limit. The URL expires after two minutes. [Download originals](#download-originals). |
| `read_document_content` | Reads original bytes from a selected immutable version as base64. Requires the vault ID from `get_vault_info` and the node, version, SHA-256 and size from `list_document_versions`. Originals are limited to 32 MiB; byte windows default to 64 KiB and are capped at 256 KiB. Returns offset, next offset and EOF. Verify the assembled size and SHA-256 before using the bytes. No export job or local file is created. |
| `list_document_versions` | Lists immutable versions, including each original's `blob_hash` and size, for one live file. Limit defaults to 100 and is capped at 250; offset is capped at 1,000,000. |
| `resolve_text_citation` | Reopens a [saved quotation](#saved-text-citations) from exact retained rendition text. |
| `check_answer` | Checks [answer figures](#check-figures-in-an-answer) against saved citations. |
| `get_kata_context` | Reads an issue's [Docbank evidence and current source states](kata-evidence.md). |
| `get_text_outline` | Reads the headings and whole-section bounds of [retained text](#read-complete-retained-sections). |
| `read_text_section` | Reads an exact page of a retained section, with explicit completion. |
| `read_rendition_text` | Reads the exact vault/node/version/attachment tuple described under [Resources](#resources-and-rendition-windows). |
| `get_processing_plan` | Requires an exact node ID, content-version UUID, and 1–128-character processing profile name. Returns the complete provider, trust-boundary, retention, estimate, consent, and backup disclosure plus its fingerprint. |
| `get_processing_status` | Reads one stable 64-hex-character job identity. A response contains at most 64 embedding job IDs. |
| `get_processing_coverage` | Reports rendition and embedding coverage for 1–4,096 unique version IDs in one exact vault fence and one 1–128-character processing profile. The response has at most 65 coverage classes. |
| `get_package_import` | Reads durable progress for one import operation UUID. |
| `get_package_preflight` | Reads one retained preflight by its exact identity. |
| `list_package_preflight_diagnostics` | Pages through bounded diagnostics for a retained preflight. |
| `list_package_custodians` | Pages through active custodian claims for an exact package scope. |
| `find_people` | Finds bounded active canonical people by word-start folded display-name search, with global display-only identity labels that may contain email addresses, phone numbers, or scoped handles, and folded-name page continuation. |
| `get_person` | Reads one person, its identities, and its external UIDs. |
| `list_packages` | Pages through received and produced load-file packages. |
| `get_package` | Reads one package and its retained source authority. |
| `list_package_members` | Pages through a package's immutable document occurrences. |
| `get_package_record` | Reads one immutable sender row by its package-scoped record key. |
| `lookup_bates_label` | Finds bounded package-scoped matches for an exact received or assigned label. |
| `get_photo_faces` | Reads up to 100 face boxes and depicted-person assertions per page, plus canonical `people` with `person_id`, `display_name`, and `state` for that page. Pages shrink together to fit the response byte limit. Pass `next_offset` as `offset` to continue. |
| `get_photo_asset` | Reads one photo asset by asset UUID or positive node ID. Returns the selected display source, `total_files`, `file_offset`, and a page of complete file decisions. Pass `next_file_offset` as `file_offset` to continue. Pages are live; restart if the asset revision or a member's file revision changes. |

The browser applies
[name visibility and namesake recognition rules](people.md#browser-names-and-visibility),
while MCP `find_people` keeps global search. An invalid cursor, or one from
another query, projection, or ordering, returns the tool error `cursor_expired`;
when that happens, restart from the first page.

Starting the server with `--allow-kata-writes` adds this write tool:

| Tool | Contract and important bounds |
| --- | --- |
| `cite_kata_evidence` | Quotes up to 1,000 Unicode code points from a verified document passage into a new or existing [Kata issue](kata-evidence.md). |

Starting the server with `--allow-photo-edits` adds these write tools:

| Tool | Contract and important bounds |
| --- | --- |
| `create_photo_asset` | Creates an asset for one positive file node. |
| `attach_photo_file` | Attaches one node at an expected asset revision. |
| `detach_photo_file` | Detaches one member at an expected asset revision. |
| `exclude_photo_asset` | Changes inclusion at an expected asset revision. |
| `promote_photo_asset` | Explicitly creates an asset for one live file node. |

To view hidden photo details, run `docbank photos assets inspect /path/to/photo.jpg`
and enter the Hidden passcode. The CLI returns the details while keeping the
photo hidden, and MCP photo tools stay locked. Ordinary MCP document tools keep
access to its files.

Photo writes return the first file page along with the committed asset revision;
if `next_file_offset` is present, continue through `get_photo_asset`. Each photo
write makes one daemon request, and an ambiguous transport failure returns
`processing_outcome_unknown`. Inspect the asset before retrying. Display and
vault settings writes remain HTTP and CLI operations.

Call `list_changes` with `{}` to get an empty page and the current checkpoint.
Save `next_cursor` before scanning documents, then pass it on the next call to
receive changes made since that checkpoint:

```json
{"name":"list_changes","arguments":{"cursor":"SAVED_NEXT_CURSOR"}}
```

Each response includes `events`, `next_cursor`, `has_more`, `ttlMs: 0`, and
`cacheScope: "private"`. Save `next_cursor` even when `events` is empty, so later
calls include changes made afterward. Cursors are opaque strings capped at 128
UTF-8 bytes. An interrupted drain returns no partial page; retry with your
previous cursor.

An omitted or empty cursor starts at the present. For initial synchronization,
save that checkpoint, scan the vault, and replay after the checkpoint. If you
receive `changes_cursor_expired`, call `list_changes` without a cursor, save the
new checkpoint, rescan the vault, and resume with that cursor. See
[event kinds and retention](../agents/integration.md#react-to-vault-changes).

`list_documents` uses live keyset pagination rather than a snapshot, and a
mutation between pages can change later membership or order. Each opaque cursor
is at most 32 KiB of ASCII, expires after 15 minutes, and authenticates the
normalized prefix, sort, direction, page size, position, and traversal. Cursors
carry their own position, so paging does not consume a shared daemon cursor
quota. The size bound accommodates paths up to 16 KiB. Tampered, expired,
reused-for-another-query, or daemon-restart-invalidated cursors fail instead of
restarting at page one.

`list_documents.source_present` and `search_documents.filters.source_present`
optionally exclude documents whose watched source is known to be gone. Both
default to false, push-owned documents stay included, and Docbank applies the
filter before pagination and retrieval.

When you call `search_documents` with filters, it searches every matching live
file without a 4,096-file cap. Filters can select a tag UUID, a MIME type of at
most 255 characters, an absolute folder `under_path` of at most 16,384 UTF-8
bytes or a positive ancestor `under_node_id`, and RFC 3339 `modified_since` or
`modified_before` bounds of at most 64 characters each. Explicit version lists
resolve to an exact source fence of up to 4,096 current, live versions.
`under_path` also scopes `answer_question` and `find_conflicts`, and the daemon
resolves it with the same Unicode normalization and case-exact lookup as
document paths. Passing a file returns `not_dir`, passing a missing or trashed
folder returns `not_found`, and sending both directory selectors fails
validation before any provider call. Only version-list searches return `fence`,
`fence_fingerprint`, and `observed_scope_count`. Version-list searches always
return `vault_id`, while filter searches return it only when there are hits.
Every search reports requested and actual mode, coverage, skipped reasons, and
truncation state, returning at most 100 hits. Excerpts are capped at 512
characters, each hit carries at most 64 evidence identities of up to 1,024
characters each, and a result includes at most 64 skipped reasons of up to 2,048
characters each. The `skipped_reasons` array can contain both failure codes and
setup guidance.

Without a profile, `auto` tries hybrid search through the same query bindings as
`answer_question`, including retries after provider failures, and preserves
`requested_mode: auto` in the result. It can send query text to a query-capable
provider under existing query-text consent and falls back to lexical results
when search by meaning is unavailable. With an explicit profile, `auto`,
including an omitted mode, uses lexical search without semantic setup guidance.
Explicit `lexical` mode searches the first selected profile without querying an
embedding provider.

An unknown profile returns `processing_profile_unavailable` along with the
available names. When no query binding is attempted, `hybrid` and profile-free
`auto` return lexical results with `actual_mode: lexical` and setup guidance in
`skipped_reasons`, naming another query-capable profile or recommending
`docbank processing local` when none exists. An unknown binding returns
`not_found`. Selecting an image binding with `hybrid` returns keyword results
and names a usable text-query binding when the profile has one; with `semantic`,
the same selection returns `semantic_unavailable` and names that text-query
binding. When all query candidates fail, lexical fallback reports the first
failure code and adds guidance for consent or index setup. `semantic` preserves
`consent_required` when consent is missing, expired, or revoked, and other
`semantic` failures return `semantic_unavailable` with the failure cause or
setup guidance.

To request the profile's configured reranker, set `rerank: true`. Docbank sends
the query and bounded candidate excerpts to the provider only under your
existing reranking consent grant. `search_documents` reports
`openWorldHint: true` because a reranked search can send the query and excerpts
to a provider. The optional `reranking` receipt reports `applied`, `skipped`, or
`degraded`, the candidate count, and any failure cause. When the receipt is
`applied`, the first `candidate_count` hits include `rerank_relevance` equal to
their `score`, including zero, and later hits omit `rerank_relevance`. Applied
searches use different `score` scales, with provider relevance for the first
`candidate_count` hits and search scores for later hits, so clients must not
sort the whole list by `score`. A degraded search keeps its original order and
adds `reranking_degraded` to skipped reasons, unless the profile fails closed.
MCP returns `reranking_unavailable` when no reranker is configured and
`reranking_failed` when reranking fails under a fail-closed policy. Both errors
omit provider bodies, credentials, query text, and excerpts.

Document summaries cap paths at 16,384 characters, names and MIME types at 255
characters, and active rendition identities at 64. Processing-plan responses
contain at most 129 flow hops, 129 disclosed classes, and 129 retained classes.
Each runtime disclosure has at most 64 metadata classes and 64 retained-artifact
roles, and each flow hop has at most five input classes. Processor, endpoint,
deployment, model, revision, and vector-space strings are capped at 1,024
characters, provider and class names at 128, and the backup-consequence text at
4,096 characters. Processing status contains at most 64 embedding job IDs, caps
state, phase, and failure codes at 64 characters, and caps `completed_bindings`
at 64. Processing coverage contains at most 65 classes, with class names capped
at 128 characters, states at 64, and each class count at 4,096. These schema
limits are admission bounds, not promises that a configured provider or vault
fills them.

Expected domain failures return `isError: true` with a structured payload capped
at 1 KiB and a stable code:

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

Invalid tool arguments return JSON-RPC `-32602`, and unexpected failures return
a sanitized JSON-RPC internal error. Stderr records the operation and a fixed
error category for diagnosis. Neither path includes host paths, SQL, provider
secrets, daemon keys, or unrequested document text.

### Find differing figures

Call `find_conflicts` with search terms, an explicit profile, and a source
selector; profile discovery is available only in `search_documents`. For
example:

```json
{"name":"find_conflicts","arguments":{"query":"cooling period","profile":"supplied-captions","filters":{}}}
```

If matching sentences say "The cooling period is 7 days." and "The cooling
period is 15 days.", the result is `CONFLICTING`. Each conflict includes the
figures, their sentences, and passage references starting at 1. Each passage
includes its document identity, evidence IDs, and `modified_at`, which is the
catalog modification time rather than a date from the source text.

The tool uses lexical search, so it needs no embedding provider. It rejects
`mode`, `binding_id`, `explain`, and `rerank`, and caps its result limit at 20.

`find_conflicts` compares money in the same currency, percentages written as `%`
or `percent`, and durations in the same unit from minutes through years.
Sentences must match exactly after lowercasing, collapsing whitespace, and
masking the figure and its unit, and each sentence must contain exactly one
figure. A second figure, including a date, makes the sentence ambiguous. The
text outside the figure and its unit must contain at least one letter. Headings,
titles, and neighbouring paragraphs sit outside the match, so sentences that
differ only in that context can appear as conflicts to inspect. List items
joined to a sentence by hard wraps produce a single sentence with several
figures, and the tool skips that sentence.

The tool reads each hit's retained Markdown or original plain text through a
verified read and quotes a whole sentence from that source, located by the
search excerpt. It skips truncated renditions, sources over 256 KiB, unreadable
sources, and hits whose excerpt is absent from or repeated in the source. A
cancelled call fails rather than returning partial results.

Every sentence touched by the excerpt is eligible, including sentences beside
the query match. Line breaks join hard-wrapped text, while blank lines and lines
containing only a code fence end paragraphs and keep sentences separate.
Sentences need a final `.`, `!`, or `?`, optionally followed by a closing quote
or bracket, and must fit within 512 characters. A trailing `...` or `…` marks an
incomplete sentence, as does a final period after a capital letter followed by
up to three letters, such as `Dr.` or `Ltd.`. Name-only evidence is skipped, and
a passage stating different values for the same matching sentence drops that
comparison. `compared: false` marks hits whose eligible figures matched no other
hit, so open those sources.

`NO_CONFLICT` means at least one matching sentence could be compared across
passages and all comparable figures agreed; `NOT_COMPARABLE` means none could be
lined up. These statuses cover only the retrieved, eligible sentences. Check
`truncated` and open the sources to verify disagreements. The tool chooses no
winner.

### Answer questions

`answer_question` reports bounded `pages_without_text` as inclusive ranges for
PDFs in the resolved scope, including documents absent from search results.
Gaps, partial page coverage, and unreadable evidence
(`page_coverage_unavailable`) make coverage incomplete, though the answer can
still contain passages. Narrow the scope or call `get_processing_coverage` with
specific content version IDs to inspect more pages. See
[OCR](document-processing.md#ocr) for options.

`answer_question` returns evidence without writing an answer. Content passages
require every prepared word and exact quoted phrase in one segment. Common words
can be dropped, and regular word forms match. Use key terms. `ANSWERED` means
that a content excerpt matched; it does not verify that the excerpt answers the
question. Open the source to verify any quotation. Excerpts can include
omission markers and are not necessarily contiguous spans. Results without
content text are leads rather than passages. `NO_MATCH` means a completed search
returned no candidates, not that the vault contains no answer. A validated empty
scope is vacuously complete and returns `NO_MATCH` without running a search.
Incomplete indexing, leads, truncated retrieval, or omitted evidence can produce
`INCOMPLETE_COVERAGE`. `coverage_complete` reports embedding coverage only, and
it can be `true` alongside `INCOMPLETE_COVERAGE` when passage text is
unavailable or evidence is omitted. For a nonempty scope with no usable
embedding index, a search with no match returns `INCOMPLETE_COVERAGE` with
`semantic_unavailable`. `limit` accepts 1–50 results and defaults to 20,
`max_passage_bytes` accepts 2,048–65,536 bytes and defaults to 12,288, and the
tool returns at most 20 leads. Passages and resource links arrive in order
within the response budget, with `omitted_passages`, `omitted_leads`, and
`omitted_links` reporting what did not fit.

By default, the scope covers all live files. A scope above 4,096 versions
reports `scope_too_large` along with its observed count; narrow it with filters.
The receipt records the vault ID, scope fingerprint, and observed version count.
That fingerprint identifies the version ID set, not an immutable catalog or
index snapshot. `cited_versions` lists the versions of returned passages, and
`vector_space_ids` come from returned passages and leads. The receipt supports
audit rather than exact reproduction. Search can send the question to a query
embedding provider under the daemon's existing consent policy, and optional
reranking can disclose excerpts under that policy. When present, `relevance`
preserves the applied reranker's score on its own scale and is not necessarily
normalized to 0–1.

Omit `profile` for automatic profile and binding retries, including when a
vector index is stale or a query provider fails. If every binding fails, lexical
search returns available passages with `semantic_unavailable` and
`semantic_failure`. Reranking failures and errors from the lexical retry stop
the search. With `rerank: true`, automatic selection and lexical fallback use
only profiles that support reranking. An explicit `profile` uses its first query
binding in name order unless `binding_id` selects another, and `binding_id`
requires `profile`. Profiles without query bindings use lexical search. With an
explicit `profile`, search failures return without automatic retries or lexical
fallback.

### PDF facts

`get_document` exposes `pdf_facts` for its accepted current content version.
The [PDF evidence reference](../architecture/source-metadata.md#pdf-evidence)
defines its seven fields and omission rules. Missing observations mean unknown,
and PDF evidence that has not been extracted omits `pdf_facts` entirely.

## Download originals

Call `get_original_download_url` with `vault_id` from `get_vault_info` and
`node_id`, `content_version_id`, `blob_hash`, and `size` from
`list_document_versions`. The response includes the selection, `name`, `url`,
and `expires_in_seconds: 120`. The daemon verifies the exact live node and
retained version before issuing the ticket and again when you download. The
original streams without staging another copy, and downloads work even with the
web app disabled.

For HTTP MCP, the URL uses the MCP listener's validated origin at
`/mcp/original?ticket=…`, protected by the same Host and Origin checks as the
tool call. The response also includes `path`, such as `/mcp/original?ticket=…`.
Behind an HTTPS proxy, set `[mcp.http] public_origin` to the client-facing
origin, for example `https://archive.example.test`, and preserve the Host header
at the proxy. Matching HTTPS Origin headers are accepted, and forwarded headers
do not choose the URL. For stdio, the URL uses the local daemon address.

Download the file with `curl --fail -o original.bin "$url"`. The ticket is the
credential, with no API key or MCP bearer needed. Treat the URL as private.
Verify the downloaded size and SHA-256 against `size` and `blob_hash`; failed
verification or an interrupted stream ends the transfer with an error. A
download returns 200 with the whole original, and if it is interrupted, request
a fresh URL before retrying. Download the whole original to a local file before
using a player that seeks; the single-use URL does not support seeking or
reopening.

Each transfer uses its own daemon connection. Downloads have a separate limit of
32 concurrent requests, so tool calls can continue while they stream. A daemon
acquisition has 45 seconds to complete, and a transfer stops after five minutes
without upstream bytes or when a client blocks a write for five minutes. Active
transfers have no total time limit. A second use or an expired ticket returns
404 `download_not_found`. A mismatched vault, hash, or size, or a trashed
document, returns the tool error `download_selection_stale`; refresh the
document and select its version again. A missing node or a version owned by
another node returns `not_found`. Renaming a document does not invalidate its
selected original.

## Resources and rendition windows

`resources/list` is intentionally empty. `list_documents`, `search_documents`,
`find_conflicts`, `answer_question`, and `get_document` attach resource links
for each active rendition, and `resources/templates/list` publishes one RFC 6570
template.

The canonical rendition URI is:

```text
docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}
```

The windowed resource template is:

```text
docbank://vaults/{vault_id}/documents/{node_id}/versions/{content_version_id}/renditions/{attachment_id}{?offset,max_chars}
```

The vault and content-version identities are canonical lowercase UUIDv4 values,
the node ID is a positive decimal integer, and the attachment ID is a lowercase
64-character SHA-256 value. You can read only an active, sanitized Markdown
rendition whose complete tuple matches the URI. Source binaries, raw provider
responses, inactive attachments, superseded versions, and host paths are never
resource content.

`offset` is a Unicode code-point offset from 0 through 2,147,483,647, while
`max_chars` defaults to 8,000 and ranges from 1 through 16,000. Every window
ends on a valid UTF-8 boundary and reports the requested offset, actual start
and end, next offset, EOF state, media type, checksum, response byte count, and
source identities. `read_rendition_text` returns the same window data with the
tool error codes above. Resource reads report unavailable identities and invalid
windows as resource-not-found errors and are capped at 1 MiB.

## Saved text citations

`resolve_text_citation` is a default read-only tool on stdio and HTTP. It
reopens an exact quote after the source version or active rendition has been
replaced, provided the cited evidence is still retained. It needs no write
opt-in, creates no processing jobs, and does not keep evidence alive.

To save a reference:

1. Discover a document and its active rendition with `list_documents`,
   `search_documents`, or `get_document`.
2. Call `list_document_versions` to get the selected version's `blob_hash`, and
   keep it as `content_sha256`; this is the original hash, not the text hash.
3. Read the desired `read_rendition_text` window and keep its `vault_id` as
   `vault_uid`, `node_id`, `content_version_id`, `attachment_id` as
   `rendition_attachment_id`, `build_id`, and `checksum` as `rendition_sha256`.
4. Save those fields with `version: 1`, `start`, and `end`. Measure the range in
   Unicode code points in stored Markdown, with an inclusive start and exclusive
   end. For a whole nonempty window, use `actual_start` and `actual_end`; for a
   subquote, add its code-point offsets to `actual_start`. Search-segment
   offsets and rendered Markdown positions cannot be substituted.

Call `resolve_text_citation` with that flat object, including every field. UUIDs
must be canonical lowercase UUIDv4, and hashes and build/attachment IDs must be
lowercase hex64. Ranges span 1–16,000 code points and end at or before
2,147,483,647. The result returns the unchanged `citation`, exact `text`, its
`text_sha256`, and UTF-8 `text_bytes`, with private cache scope and zero TTL.

Before returning a quote, the daemon reads and verifies the **entire** retained
rendition. Each call can read up to 64 MiB and uses the normal 60-second daemon
deadline, including maintenance waits. Neither this reference nor the returned
quote is an offline proof of the original document.

If a call fails, the error code tells you what to do next:

- `citation_unavailable`: Check the vault and evidence retention. Trash, version
  pruning, and rendition purge can make the reference unavailable, and retrying
  alone cannot restore it.
- `invalid_citation_range`: Provide a range that exists in that exact rendition.
- `citation_limit`: The rendition exceeds this operation's size limit.
- `citation_integrity`: Verification failed and no quote is returned;
  investigate the stored evidence.
- `citation_failed`: Check the daemon log.
- `citation_timeout` or `citation_canceled`: You can make a later deliberate
  retry of the same reference.

The read helper retries a disconnected daemon only before any response starts,
never after a domain failure. The
[HTTP contract](../architecture/http-api.md#retained-text-citations) owns the
complete field and error table. Current/live rendition resources and windows
keep their existing behavior.

## Check figures in an answer

`check_answer` compares a plain-text `answer` against 1–8 saved `citations` on
stdio and HTTP. You can send 1–16,000 Unicode code points, at most 512 figures,
and 512 `[n]` markers. A marker is `[` with one to three digits and `]`, and an
index outside 1 to the number of citations is unknown. Each citation requires a
verified read, including duplicates. `[2024]` counts as a figure. A figure is a
token containing a digit with edge punctuation trimmed. Whitespace, em dashes,
horizontal bars, and `[ ] ( ) < > | * ~` backtick `=` `;` separate tokens. A
leading `-`, `−`, `+`, or `±` stays as a sign, and a leading decimal point
stays. Figures compare across all resolved citations; markers do not scope them.
Normalization adds matches for recognized dates and currency amounts while
preserving every literal match. A recognized figure is found if its normalized
value matches or every digit-containing token in its spelling appears literally
in the citations.

The first matching rule determines the verdict:

- `ungrounded`: an unknown marker or a missing figure with all citations resolved. Example: `30` against cited `20`.
- `incomplete`: any citation is unavailable. Example: one of two saved citations can no longer be read.
- `no_figures`: the answer has no figures. Example: `The invoice is attached.`
- `grounded`: every figure has a literal or normalized match in the cited text. Example: `March 15, 2026` against cited `2026-03-15`.

Other resolver failures return tool errors. Results include figures, unknown
marker indexes, citation statuses, and text digests, with private cache scope
and zero TTL.

- Reported figures keep their spelling. A recognized date or amount can span
  words, so `March 15, 2026` appears as one figure in the result. Literal
  comparison preserves case, signs before digits or currency symbols, and
  decimal points. For example, `-.5` differs from `.5`. Edge punctuation before
  an ISO code is trimmed for literal comparison, so `-USD5` matches `USD5`.
- Identifiers and ordinary numbers keep their spelling. For example, `12-AB`
  differs from `12-ab`, and `5.00` differs from `5`.
- Dates accept `YYYY-MM-DD`, `Month D, YYYY`, and `D Month YYYY`, with Unicode
  whitespace between words. English months can use full names, three-letter
  abbreviations, or `Sept` and `Sept.`. Timestamps and invalid dates compare
  literally. An invalid date can consume a following currency code, so `$40`
  reports `missing` against `2026-02-30 USD 40`.
- Amounts accept `$`, `€`, or `£` before a number, or an uppercase ISO currency
  code before or after it, attached or separated only by spaces or tabs. Other
  whitespace keeps markers separate, so `40\nUSD` compares literally, and
  `USD 20\n\nCAD 30` supplies matches for `$20` and `CAD30`. Codes such as `ALL`
  count as currency markers even when they could be ordinary words. Those
  symbols mean USD, EUR, and GBP. Commas group three digits, and a decimal point
  accepts one or two fractional digits. Leading and trailing zeros normalize, so
  `$1,250`, `USD 1250.00`, and `1,250 USD` match.
- Signed amounts and amounts with multiple currency markers compare literally.
  For example, neither `-USD5.00` nor `-USD 5USD` supplies a normalized match
  for `$5`, and neither `CAD $40`, `$40 CAD`, `CAD ($40)`, nor `$40 : CAD`
  supplies one for `USD40`. The marker check skips edge punctuation and
  formatting delimiters, including punctuation-only tokens, and joins across
  spaces or tabs only. A separate marker after an amount makes that amount
  compare literally and is consumed with it, whatever follows the marker. For
  example, `USD 20, CAD 30` leaves `20` and `30` literal and reports `missing`
  for `CAD30` or `$20`, while `$40 CAD 15 March 2026` supplies a match for
  `2026-03-15` but not `USD40`. In `40 CAD 15 March 2026`, `40 CAD` keeps its
  marker and matches `CAD40`. Likewise, `40USD 30CAD` supplies matches for `$40`
  and `CAD30`. A marker next to a signed figure stays with it, so
  `-5 USD 30 CAD` supplies a match for `CAD30`, while in `USD -5 EUR 30` the
  `30` compares literally.
- Numbered-list numbers count as figures. For example, `1.` contributes `1`.
- Literal matches can also ground different date or currency labels. For
  example, `June 5, 2026` against `July 5, 2026`, or `USD 40` against `EUR 40`,
  grounds because the digit-containing tokens match. Use `$40` against `EUR 40`
  or `March 16, 2026` against `2026-03-15` for examples that report `missing`.
- Both sides undo Markdown punctuation escapes, including literal backslashes in
  plain-text bodies. For example, plain-text `INV\-5` matches `INV-5`.
- Occurrence alone confirms neither the subject nor other claims. For example, a
  cited price does not establish which item it belongs to.

## Read complete retained sections

`get_text_outline` and `read_text_section` are default read-only tools on stdio
and HTTP. They use the saved citation above and need no write opt-in, and a
valid historical rendition works even without a configured or executable
profile.

1. Send `get_text_outline` `{citation, offset: 0}`. If you have no particular
   quote, use citation range `[0,1)` with the exact retained identities.
2. Choose the returned `enclosing_section` or any `sections` entry. The
   enclosing section contains the entire anchor, including an anchor crossing
   smaller sections. To browse more entries, use the optional `next_offset`.
3. Send `read_text_section` `{citation, section: {start,end}, offset: 0,
   max_chars: 16000}`. The section includes its subsections.
4. Append `text`, replace the relative offset with `next_offset`, and continue
   until `complete` is true, keeping the citation and section unchanged. A page
   is not a complete section until then, and a later failure leaves a partial
   result.
5. To cite a nonempty page, copy its citation and substitute `actual_start` and
   `actual_end`. `resolve_text_citation` reopens those exact bytes.

Sections describe stored Markdown and preserve code-point offsets, escapes,
combining marks, and line endings. `title_markdown` is a navigation label
limited to 160 code points with explicit `title_truncated`; it is not quotation
text. The plaintext adapter also normalizes through Markdown, so `#` comments in
Python, YAML, or plain text can appear as headings. This does not reconstruct
source-language structure or the original document's layout.

Each request verifies and parses the whole retained artifact. Navigation is
limited to 4 MiB, 65,536 LF characters, and 4,096 headings, with two concurrent
reads per service. Outline pages contain at most 50 entries, and section pages
contain 1–16,000 code points; use the largest useful page size to avoid repeated
reads. Waiting counts against the daemon deadline. Parsing is synchronous and
cannot be interrupted mid-call. Results use private cache scope and zero TTL.

Malformed arguments, including negative offsets and reversed bounds, return
MCP's generic invalid-arguments error before contacting the daemon. Once the
daemon reads the rendition, `invalid_text_navigation` means you need to correct
whole-section bounds or offsets using that rendition's outline, and
`text_navigation_limit` means the artifact exceeds a navigation cap. Short
citations still support artifacts up to 64 MiB. `rendition get` can download an
attachment still active for its version; a superseded attachment has no general
bulk-download path here. Existing citation errors keep their meanings, and a
missing processing service or a transport/decode failure keeps the existing
generic MCP error behavior.

Every page checks retained authority again. Trash, pruning, or purge between
pages can stop a read sequence; there is no cursor, saved outline, or retention
lease. The full [HTTP contract](../architecture/http-api.md#retained-text-sections)
owns fields, limits, and errors. Neither page hashes nor a saved citation prove
that provider text quotes the original file verbatim.

## Optional processing start

Start the server with `--allow-processing` only when the agent is allowed to
enqueue already-consented work:

```bash
docbank mcp --transport stdio --allow-processing
```

That flag adds `start_processing` to the tool catalog. It accepts only a
content-version UUID and the 64-character plan fingerprint previously returned
by `get_processing_plan` from the same MCP process. The process remembers at
most 4,096 reviewed plans and rejects an evicted plan, a fingerprint from
another process, changed disclosure, a changed profile, or a stale source.

MCP cannot grant consent. The tool sends `consent=false` to the daemon and
succeeds only when the operator has already granted consent for the identical
plan through an operator-controlled Docbank interface. With the public CLI, call
`get_processing_plan` in MCP first, then have the operator verify and consent to
that same plan:

```bash
docbank processing plan id:<node-id> --profile <configured-profile>
docbank processing build id:<node-id> \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

The fingerprint and content-version ID printed by the CLI plan must match the
MCP plan. `processing build --consent` records consent and starts the reviewed
work; it is not a consent-only command. See
[Document processing](document-processing.md) for the complete operator flow.

Once consent is active, `start_processing` returns the stable queued job
identities, including at most 64 embedding job IDs, without polling. An initial
`processing_outcome_unknown` result contains no job ID, so you cannot reconcile
its outcome through MCP. Do not retry it blindly, because MCP has no job lookup
for that case.

## Prepare a retained PDF for review

With `--allow-package-writes`, `prepare_native_pdf` saves the same preparation
JSON as `docbank redaction prepare --json`. Use `get_vault_info` for `vault_id`
and `list_document_versions` for `node_id`, `content_version_id`, `blob_hash`
and `size`. Supply the version explicitly, even for the current PDF.
`destination_path` is an absolute path on the MCP host, outside the Docbank
data directory. Choose a new file name such as `PRIVATE-preparation.json`.

The result gives the saved file's size and SHA-256, its source and map
identities, and each page's UTF-8 byte span and coverage-gap counts. The file
contains unredacted text. Handle it like the original PDF and use it with
explicit masks in [the CLI resolver](document-processing.md#review-and-render-a-native-pdf).
Ordered gaps need covering masks. Unordered gaps need whole-page masks.

Preparation uses the daemon's configured Linux amd64 page runtime and its
60-second request deadline. Refresh the selection after
`redaction_selection_changed`. After a lost reply, inspect the destination
before retrying. `published_durability_unknown` and `cleanup_failed` describe
problems after saving. The destination still names the saved file.

## Publish a reserved numbered PDF

An operator first
[reserves the reviewed PDF](document-processing.md#deliver-one-reviewed-pdf-with-bates-numbers)
through the browser or CLI and consents to retaining the unredacted original
indefinitely. `publish_numbered_pdf` grants no consent, creates no snapshot, and
allocates no numbers. Keep the resulting allocation ID and the three private
review files. Ordinary Bates tools keep their existing sealed-snapshot contract.

With `--allow-package-writes`, call `publish_numbered_pdf` with:

```json
{
  "allocation_id": "11111111-1111-4111-8111-111111111111",
  "preparation_path": "/reviews/PRIVATE-preparation.json",
  "masks_path": "/reviews/PRIVATE-masks.json",
  "review_path": "/reviews/PRIVATE-review.json",
  "review_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
```

Copy `review_sha256` from the saved review's `sha256` field; it is not the hash
of the JSON file itself. The paths must name regular files on the MCP host and
follow the same limits as [checked downloads](#recover-numbered-pdfs). Docbank
refuses a changed review before contacting the daemon. Keep the private files
unchanged until publication and any retry finish. Docbank leaves them outside
the vault and delivery.

The daemon renders from the allocation's retained original, verifies the
numbered PDF, and keeps the first published artifact. The tool returns that
artifact's record and page labels without downloading PDF bytes. For an
independent full check and local delivery, call `export_bates_file` with the
review files. Default and daemon-hosted MCP remain read-only.

A lost response can mean the PDF was already published. If you receive
`bates_outcome_unknown`, query `get_bates_export` for that allocation or repeat
the identical publication, including the review digest and paths. Never reserve
another range to recover a reply. Completed retries return the same artifact
after restart without a page runtime, and pending allocations can finish after
the live source is deleted.

Publication can take ten minutes and holds the daemon's shared native-PDF slot.
`redaction_busy` indicates contention in the daemon, and `redaction_memory`
means the daemon's renderer memory ceiling was exceeded. Standalone HTTP MCP
still has a two-minute request deadline, so use stdio for longer calls and
reconcile uncertain replies. `redaction_review_changed` means the files or
allocation do not match the review, while `redaction_verifier_version` means a
different qualified verifier is required. MCP reports malformed private input
without including its contents or reader diagnostics.

## Recover numbered PDFs

Use `find_bates_exports` to locate a retained delivery by an exact Bates label.
With `--allow-package-writes`, `export_bates_file` saves that PDF outside the
vault. Its `verification` field distinguishes `stamped_labels` for ordinary
stamped exports with the installed engine from `recorded_bytes_and_numbers` for
numbered native redactions or stamps made with a different engine version.
`recorded_bytes_and_numbers` checks the recorded size, SHA-256, and numbering
metadata without rechecking masks, approved text, or rendered numbers, and it
works after restart and backup/restore without the original review files or a
configured page runtime.

For native redactions, the operator reserves through the browser or CLI, and
publication can use MCP as described above. For full checking, pass all three
private review files to `export_bates_file`:

```json
{
  "allocation_id": "11111111-1111-4111-8111-111111111111",
  "destination_path": "/exports/checked.pdf",
  "overwrite": false,
  "preparation_path": "/reviews/PRIVATE-preparation.json",
  "masks_path": "/reviews/PRIVATE-masks.json",
  "review_path": "/reviews/PRIVATE-review.json"
}
```

All paths must be absolute, clean paths on the MCP host. Preparation and review
files are limited to 16 MiB each, and masks to 1 MiB. All three must be readable
regular files; partial, empty, or null inputs are rejected. Those files can
contain unredacted evidence, and you should keep them private and outside the
delivery.

Full checking applies only to numbered redactions and returns
`masks_text_and_numbers`. It verifies the recorded PDF identity, reviewed masks,
approved text, and visible numbers before publishing the destination. A failed
check never falls back to recorded-byte recovery. Full checking does not prove
the origin of the PDF or compare unmasked pixels with the original.

Reviewed PDF exports and recipient ZIPs share one full-check slot per MCP
process. A second concurrent reviewed PDF export or recipient ZIP returns
`redaction_busy` before reading private files or contacting the daemon, while
other tools and exports without review files remain available. A reviewed PDF
export has a ten-minute limit, while standalone HTTP MCP keeps its shorter
two-minute request deadline. The daemon-hosted MCP endpoint stays read-only.

The first full check allocates PDFium's 512 MiB backing and compiled-code cache,
which stay resident until the MCP process exits. The verifier's 1 GiB RSS
ceiling counts the whole process, including unrelated calls. If you receive
`redaction_memory`, retry after other work finishes; Docbank never raises the
ceiling for an export. `redaction_verifier_version` means the files or delivery
need another verifier, and `redaction_review_changed` means the review does not
match. Integrity failures return `redaction_integrity`; cancellation, request
deadlines, and page deadlines return `redaction_canceled`, `redaction_timeout`,
and `redaction_worker_timeout`. Local staging or publication failures return
`bates_local_io`. Check the saved path before retrying a lost publication reply.

The same full check is available through `bates export download`. See
[Numbered delivery](document-processing.md#deliver-one-reviewed-pdf-with-bates-numbers)
for retention consent and verification scope.

## Export a reviewed recipient ZIP

With `--allow-package-writes`, `export_recipient_package` saves completed
numbered reviews as one importable ZIP:

```json
{
  "selection_path": "/reviews/selection.json",
  "destination_path": "/exports/recipient.zip"
}
```

Use the CLI's
[ordered selection file](document-processing.md#package-completed-numbered-pdfs-for-a-recipient).
The tool checks every review before downloading a PDF, then rechecks each input
and fully verifies the retained PDF before assembly. It also checks the finished
package against the pinned PDF and reviewed-text identities. Ordinary stamps are
refused, and a failed full check never falls back. Private review files stay
outside the ZIP, and this operation creates no reservation, render, or new
retention consent.

Use stdio for multi-document packages. The operation can take 30 minutes, with
ten minutes per document, while HTTP keeps its two-minute request deadline and
can end without a tool result on cancellation. During the whole operation, other
reviewed PDF exports and recipient packages return `redaction_busy`, while
ordinary reads remain available. The same retained PDFium backing and process
memory ceiling described above apply.

Both paths are absolute MCP-host paths, and the destination must sit outside the
Docbank data directory and not already exist. A successful call reports `state`,
`destination_path`, `size`, `sha256`, `receipt_sha256`,
`verification: "masks_text_and_numbers"`, the recipient receipt, and
`cleanup_failed`. `published_durability_unknown` means the file was saved but
the directory sync failed. A later cleanup error leaves the saved result intact
and sets `cleanup_failed`; check the stage named in the local log.

`recipient_package_limit` means a document, page, text, or archive bound was
exceeded, and `recipient_package_local_io` means output staging or saving
failed. Other full-check refusals use the codes described above. Malformed
private inputs return invalid arguments without their contents or reader
diagnostics. If the reply is lost, inspect the destination and its hash before
retrying; existing files are never replaced. The ZIP is not retained in the
vault unless it is imported separately.

## Optional package writes

To allow the agent to preflight local load-file sources, import packages, and
change package custodians, start standalone MCP with a separate opt-in:

```bash
docbank mcp --transport stdio --allow-package-writes
```

For load-file imports, this flag adds:

| Tool | Contract |
| --- | --- |
| `preflight_load_file_package` | Reads a local source and retains a preflight with diagnostics before import. |
| `start_package_import` | Starts an import from an exact preflight identity and operation UUID. An exact retry with the same operation UUID returns the existing operation. |
| `resolve_package_custodian` | Links an existing package custodian claim to an exact canonical person, subject to the supplied revision. |
| `assign_package_custodian` | Creates or replaces an operator-owned package custodian, subject to the supplied revision. |

Review the preflight and its diagnostics before starting an import, and keep the
operation UUID to read progress with `get_package_import` and reconcile a retry.
Custodian writes use exact identities and revision checks to reject stale
changes.

Package writes do not use the processing-plan consent flow; enable this flag
only when the agent is allowed to perform these local reads and vault changes.
`--allow-package-writes` does not enable `start_processing`, and
`--allow-processing` does not enable package writes. Pass both flags when you
need both capabilities.

No MCP tool can delete documents, move, rename, tag, restore, prune, pack,
repack, change configuration, select credentials, or grant processing consent.
Package preflight can upload a local source container, though there is no
general document upload tool.

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
public catalogs, except the public icon routes `/icon.png` and `/favicon.ico`,
which send `Cache-Control: public, max-age=86400`.

## HTTP limits and unsupported surface

The `/mcp` endpoint is stateless and POST-only, with each request carrying one
JSON-RPC message. The same listener also answers GET at `/mcp/original` for
original downloads and GET/HEAD at `/icon.png` and `/favicon.ico`, as documented
above. There are no GET streams, sessions, `Mcp-Session-Id`, resume support, or
`Last-Event-ID`. The server allows 10 seconds to receive request headers; after
the headers arrive and the request passes boundary checks, a separate two-minute
deadline covers request-body reads, daemon work, and response writes. The public
limits are:

| Resource | Limit |
| --- | ---: |
| Request body | 1 MiB |
| Combined header names and values | 32 KiB |
| Header field values, counting repeats separately | 128 |
| Bearer token | 4,096 bytes; empty values, spaces, and control bytes are rejected |
| Concurrent requests | 32 total; original downloads have a separate 32 |
| HTTP response body | 1,114,112 bytes |
| Header-read timeout | 10 seconds |
| Idle connection timeout | 30 seconds |

The request Host must be loopback, the concrete listen IP, an explicitly allowed
authority, or the host matching the configured `[mcp.http] public_origin`. An
absent Origin is valid for non-browser clients. If Origin is present, there must
be exactly one Origin: when the Host matches `[mcp.http] public_origin`, that
Origin must equal the configured public origin, which can be HTTPS; otherwise,
it must be a plain-HTTP Origin matching the request Host and port. The server
rejects unsafe or cross-origin requests before authentication, and
forwarded-host headers do not change this decision.

Docbank does not expose MCP prompts, roots, sampling, elicitation, or tasks. It
does not negotiate a legacy protocol, open a remote daemon, provide remote
daemon configuration, implement OAuth, maintain HTTP sessions, resume streams,
or expose general-purpose writes beyond the opt-in tools on this page.

Read calls can reacquire the local daemon and retry once only when a transport
failure happens before any response; failures after a response are never
replayed. Daemon acquisition is capped at 45 seconds, and each HTTP request has
the two-minute post-header deadline above, while stdio work otherwise runs until
completion or client cancellation. Daemon restart, an idle shutdown, or a stale
runtime record therefore produces a bounded failure or recovers on the next safe
read without terminating stdio with diagnostics on stdout or leaving HTTP pinned
to a dead client.

## Native export jobs

Photo-export preparation uses the CLI or Photos, while MCP lets you inspect
retained photo-export plans and jobs.

`get_export_plan`, `get_export_problems`, and `get_export_status` are available
with all write flags disabled over stdio or HTTP. Plan inspection requires a
retained plan ID. `get_export_plan` returns the complete `plan`, including
richer source, role, count, and volume fields when present. Its `expires_at` is
the admission deadline, though a plan can stay readable longer while a job
retains it. Reading does not extend either deadline.

`get_export_problems` returns a `problems` object with `plan_id`, `fingerprint`,
`after`, `next`, `total`, and `items`. Omit `after` for the first page, then
pass a nonzero `next` as `after`; zero marks the last page. These tools return
private results with `ttlMs: 0`. MCP can retry a read once when transport fails
before any response starts, and never retries domain errors or partial
responses.

Plan reads use the shared API-key owner and cannot inspect browser-owned plans.
An expired or removed plan needs a fresh preview with new IDs for a new export;
inspect an existing job through status instead. Problem details remain frozen
after source edits. Totals count unavailable outputs and inventory problems
rather than selected documents, and originals-only previews normally have none.

The fixed 50-item page must fit within 64 KiB, and an `export_limit` response
returns no partial page. HTTP has the same limit and cannot request fewer items,
and releasing jobs cannot shrink that page. See
[retained-plan inspection](export-bundles.md#inspect-a-retained-plan) for
offsets, identity fields, and retention behavior.

To let a client retain exact originals and manage their export, start standalone
MCP with `docbank mcp --allow-export-writes` and review the selection with the
operator:

| Tool | Required arguments and result |
| --- | --- |
| `preview_export` | `source_operation_id`, `plan_operation_id`, and `members`. Returns a retained `plan`. |
| `start_export` | `operation_id`, `plan_id`, and `fingerprint`. Returns the `job`; it does not wait for completion. |
| `cancel_export` | `job_id`. Permanently stops active work. Returns `accepted: true`; completed jobs conflict. |
| `download_export` | `job_id`, absolute `destination_path`, optional `overwrite` (default false). Returns the verified `receipt` and publication state. |
| `release_export` | `job_id`. Deletes a terminal job and its retained archive, returning `released: true`. |

List versions for each selected node, then copy `content_version_id` to
`version_id`, `blob_hash` to `sha256`, and `node_id` and `size` into each
member. That takes one `list_document_versions` call per node, with more pages
if needed. Each preview accepts 1–1,000 retained original versions totaling at
most 50 GiB and can include multiple versions of the same node. `size` is
required, including zero for an empty original. Optional `revision` is a current
node precondition; omit it unless you need it, and do not substitute a version's
historical node revision.

Generate all operation IDs before calling. After `export_outcome_unknown`,
inspect the known job ID or replay the same request and IDs. The first write
after a daemon restart can report this error even when nothing was sent, and
Docbank does not reconnect and repeat writes automatically. A preview replay
must keep member order and stays subject to its original ten-minute admission
window. After a delayed start response, check job status instead of replaying
the preview. Never reuse a released job ID; its replay record has been deleted.

If you receive `export_timeout`, `export_canceled`, or `export_failed` during
start, download, or release, read `get_export_status` before retrying, using the
start operation ID as the job ID. After a download error, inspect the
destination too, because the file might already be saved. After a release error,
not found confirms removal.

MCP shares the API-key owner with the CLI. Completed jobs still occupy the two
shared job slots until released or expired, and release does not delete
originals or local downloads. `export_retained` means a download ticket or lease
still holds the archive; retry release after it closes. Retained source and plan
records share one combined global limit of 32, separate from the job slots. See
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
directory, and an existing destination must be a regular file with
`overwrite: true`. Docbank verifies before publishing: the daemon verifies the
retained archive, then the MCP process streams it into a private stage and
independently verifies the complete bundle against the job and ticket receipts.
The result contains no ZIP bytes or download ticket, and downloading never
releases, cancels, or recreates the job automatically.

`state: "published"` means the verified file is saved, while
`state: "published_durability_unknown"` means the file became visible but the
following directory sync failed; both return the receipt.
`cleanup_failed: true` means private staging cleanup failed after publication
while the saved file stays in place. Before publication, ordinary failures
preserve the destination and attempt stage cleanup. `export_integrity`
identifies a receipt or archive mismatch, and `export_local_io` identifies a
local file-operation failure. The operator log records the operation and its
cause while the client receives a fixed message. A failed stage cleanup can
leave a private stage behind and is logged separately; a secondary cleanup
failure does not replace the original error.

`export_unavailable` means the daemon has no export worker and made no change;
check the daemon and retry. For `export_expired`, preview again with new IDs if
admission expired, or release a retained job whose archive is missing.

The HTTP two-minute deadline covers daemon verification, transfer, and local
verification, and archive size alone cannot predict whether all three fit. For a
large job, use `docbank export download <job-id> <path>` against the same vault
or a suitable stdio invocation. A later download gets a fresh ticket and
transfers the whole file again. If a response is lost or canceled after
publication, a verified destination can already be on disk; inspect that file
before retrying with overwrite.

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

Those writes require `--allow-report-writes`. The two inspection tools stay
available without it and can inspect CLI-created whole-vault or collection
reports too. Creation through MCP accepts only exact current document selections
and starts no processing or provider work.

1. Use `list_documents` or search to choose node/current-version pairs. For each
   node, call `list_document_versions` and match the exact version to get
   `blob_hash`. To inspect a chosen pair, call `get_document` with both IDs.
2. Build `selected_documents.documents` with each positive `node_id`, canonical
   lowercase UUIDv4 `version_id`, and lowercase SHA-256 `sha256`, selecting each
   node once. Follow the [report request format](search-exports.md) for terms,
   inclusive date cutoffs, timezone, and an optional processing profile.
3. Call `create_report`. Omitting `coverage_mode` defaults to `strict`; choose
   `available_only` if incomplete evidence is acceptable. If the selection
   changed, the call returns `report_selection_changed`. Recheck the documents
   and their current versions with `list_documents` and
   `list_document_versions`, and review any replacements with the operator
   before submitting a new selection.
4. For `needs_review`, page through `get_report_dates`, keeping each member's
   earlier candidates when `candidates_complete` is false and continuing with
   the returned cursor until `next_cursor` is absent. Review the evidence with
   the operator, then submit the existing [date-choice format](search-exports.md)
   to `revise_report`. Restart date paging on the child; parent cursors do not
   work on it.
5. Read counts and coverage through `get_report_summary`, and download a
   complete report before it expires. To export the same originals, pass the
   selected identities and their version-list sizes to `preview_export`
   separately.

Creation rejects `all_documents: true`, `collection_ids`, and `date_choices`, as
well as unknown fields and explicit nulls. Expressions allow 8,192 Unicode
characters, and profile and timezone strings allow 128 UTF-8 bytes. A
selection's member count is not the coverage `scoped` count: members without
usable dates can remain in an available-only packet without contributing to
counted coverage.

Create and revise return compact receipts with `report_id`, optional
`parent_id`, `state`, `observed_at`, `expires_at`, and `unresolved_dates`.
Complete receipts also include `bundle_bytes` and `bundle_sha256`, while
needs-review receipts omit them. A large warning summary does not stop delivery
of the new handle. If full summary inspection returns `report_limit`, run
`docbank report show <report-id> --json` or read it through HTTP; dates,
revision, and download remain available.

A revision allows 1,000 choices and 4,096 UTF-8 bytes per reason, while the entire
MCP message must fit within 1 MiB, including escaping and its envelope. Those
two individual maxima do not fit together. Choose shorter reasons where
appropriate or submit batches to successive children; Docbank never splits a
revision for you. For a larger batch, the existing HTTP/CLI revision path
accepts 8 MiB.

CLI and MCP share eight report handles, and every revision uses another slot. An
initial report plus seven revisions fills those slots if no other reports are
retained. All descendants expire 30 minutes after the original observation, and
downloading frees no slot. Release unneeded handles explicitly with
`release_report`, or wait for expiry when capacity is full. The engine also
permits two simultaneous builds and 64 handles or pending builds globally. A
restart loses live handles, and history receipts do not restore their artifacts.

Writes are never automatically replayed. `report_outcome_unknown` means a report
write might have succeeded without a usable reply. For create or revise, inspect
history with the operator using `docbank report history --json` or through
web/HTTP. History records requests and outcomes without establishing live
availability or reliably identifying a lost reply. There is no idempotent
replay; a deliberate retry creates another observation or child and uses another
slot. The first write after a daemon restart can also return this error on a
stale connection.

`release_report` is destructive and requires `--allow-report-writes`. It can
release complete or review-pending reports, discarding any pending date
evidence. Success returns `report_id`, `released: true`, `ttlMs: 0`, and
`cacheScope: "private"`. Child reports keep their original expiry. Shared memory
remains while a child or in-flight operation uses it, so releasing one handle
does not guarantee global build or memory capacity.

An active download returns `report_retained`; retry deliberately after its
server reader closes, even if the download call has already returned. After an
uncertain release reply, inspect `get_report_summary` for the same ID. A
successful read proves the handle remains. `report_unavailable` alone is
inconclusive: MCP omits HTTP status, and the code can mean either a missing
handle or an unavailable reporting service. History does not prove release, and
repeating release for an absent handle returns unavailable. A deliberate retry
uses the same ID and cannot create a new handle.

Downloads compare the summary, stream size and digest, and independently
verified packet before publication, using the same verification path as the CLI.
Success reports `internally_consistent: true` and `source_verified: false`,
which does not establish authenticity of the source vault. The receipt's state
is `published`, or `published_durability_unknown` if the file was saved but the
final directory sync failed. `cleanup_failed` separately reports a staging
cleanup failure, and local causes go to the operator log. If the reply is lost,
inspect and verify the destination before retrying.

Read `bundle_bytes` before choosing a transport. MCP HTTP's two-minute deadline
covers transfer, verification, and publication, so large ZIPs might not finish.
Use stdio for the same live handle, or run
`docbank report download <report-id> --output report.zip` against the same
vault. The CLI verifies the existing packet outside the MCP HTTP deadline and
does not create another report. See
[CLI report recovery](search-exports.md#inspect-or-recover-an-existing-export)
for inspection, history paging, and recovery after a local save error. ZIPs are
capped at 512 MiB, and concurrent MCP downloads share a 1 GiB verification
budget; `report_limit` can also mean that this shared allowance is exhausted.
Source changes after capture leave the frozen report's evidence and counts
unchanged.
