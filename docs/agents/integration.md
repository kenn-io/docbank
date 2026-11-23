---
last_edited: 2026-11-02
title: Agent integration guide
description: Connect an agent to Docbank using its OpenAPI contract, authenticated HTTP API, revisions, and dry-run maintenance operations.
---

# Agent integration guide

Connect your agent through Docbank's authenticated HTTP API. The daemon owns
the vault, and the CLI, agents, and scripts all send requests to it. Never open
`docbank.db` or stored content directly from an external integration.

If you are building a Go application that owns a separate archive, you can use
the [embedded API](../embedding.md).

## Choose the interface

Use the CLI for shell orchestration. Run `docbank --help` to see the agent
workflow and output modes. If your integration needs pagination, revision-aware
writes, or the full problem contract, use the HTTP API instead.

In shell scripts, CLI exit codes separate invalid usage (`2`), missing vault
objects (`3`), stale state (`4`), busy resources (`5`), and integrity findings
(`6`) from general failures (`1`). Verification may emit a complete report
before exiting `6`, so JSON on stdout does not mean success. When you pass
`--json` and the command parses it, failures end with a machine-readable error
record on stderr after any existing diagnostics. See the
[CLI reference](../cli-reference.md#process-exit-codes) for its shape and the
full exit-code contract. Independent integrations should branch on the richer
HTTP problem `code` values covered below.

For a small shell workflow, `document move`, `document remove`, and
`document restore` accept `--json` and return the daemon's complete receipt for
the resulting node. A trash receipt's `path` records where the node lived before
it was trashed. Carry the `id` and `revision` forward instead.

If you need exact-version archives, use the
[verified export bundle API](../usage/export-bundles.md). It freezes query or
explicit membership, stores role receipts, and publishes a download ticket only
after verifying the complete ZIP.

Before operating on an unfamiliar machine or switching archives, confirm which
vault is selected:

```bash
docbank vault info --json
```

Treat `vault_id` as the vault's identity and `vault_path` as its location on
this machine. To select and confirm another independently owned archive without
changing a global profile or opening its database directly, run
`DOCBANK_HOME=/path/to/another/vault docbank vault info --json`.

If your workflow depends on watched ingestion, inspect the daemon's active
configuration and runner state rather than assuming the local file is active:

```bash
docbank watch list --json
```

Each item reports the local source, virtual destination, settle window, minimum
source age, scan interval, literal `exclude` and glob `exclude_patterns` lists,
and current job record. The glob list is the effective set, which includes the
default patterns unless `default_excludes = false`. A configured profile also
adds optional `processing` status, and human output appends a `PROCESSING`
column. See the [watch API](../architecture/http-api.md#background-job-status)
for the status fields and [watch processing](../configuration.md#watched-inboxes)
for consent behavior. The settle window is how long a file must remain
unchanged, while a nonzero minimum age adds a separate check that does not
replace that window.

This command only inspects configuration. After you change `config.toml`,
restart the daemon. See [Watched inboxes](../configuration.md#watched-inboxes)
for the complete timing and source-identity rules.

Docbank generates its canonical contract directly from the running route
definitions:

```bash
docbank openapi > docbank-openapi.yaml   # offline; no vault needed
```

A running daemon also serves `/openapi.json`, `/openapi.yaml`, and interactive
docs at `/docs`. These contract routes do not require authentication so a
client generator can discover them before you configure credentials.

Human readers view the rendered documentation at directory routes such as
`/agents/integration/`, while agents can fetch the same maintained source at the
sibling `/agents/integration.md` URL.

## Search transcripts for exact recordings

To match a transcript back to a particular recording, such as an audio message,
send `media_sources` to `POST /api/v1/search`. Each selector names `source_id`,
`source_version_id`, and `content_version_id`, and you include its content
version in the request's `fence`. Use `lexical` or `auto` mode without
reranking. The
[search API reference](../architecture/http-api.md#coverage-and-source-fenced-search)
defines the selector limits and optional supplied-input restrictions.

Require `media_source_selection: true` in the response. Because shared audio
can belong to more than one source, use the evidence's `media_sources` to open
the selected recording. Docbank selects the covering transcripts before ranking
and applying limits, and removed or changed recordings leave coverage
incomplete without crowding out valid matches. Read the exact transcript below
to verify the evidence's build and supplied-input identity.

## Read an exact media transcript

To read a retained transcript, supply all three media IDs: the source, source
version, and content version.

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode 'content_version_id=<content-version-id>' \
  "$DOCBANK_URL/api/v1/media/sources/<source-id>/versions/<source-version-id>/transcript"
```

Check `evidence_state` before using `transcript`. Only `ready` includes text,
while `pending`, `unavailable`, and `stale` contain no transcript. The response
keeps `coverage_state` and `operation_state` separate. Timed units carry
`time_span` with `start_ms` and `end_ms`, and untimed units omit it. The
equivalent CLI command is:

```bash
docbank media transcript SOURCE_ID \
  --source-version-id SOURCE_VERSION_ID \
  --content-version-id CONTENT_VERSION_ID
```

Before attributing a search excerpt, compare `transcript.build_id` with the
search evidence's `build_id`. For supplied text, also compare
`transcript.supplied_input_id` with your original import receipt's
`supplied_input_id`.

## Recover a lost recording-link reply

If the reply to a remote-recording submission never arrives, read the saved
receipt using the operation ID you sent. You do not need to keep the original
link:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/media/operations/00000000-0000-4000-8000-000000000001"
```

A 404 means no committed receipt was visible at that moment, though a
submission still in flight can commit afterward. The equivalent CLI command is
`docbank media operation OPERATION_ID`.

The receipt captures the submission as it was saved. To check the source's
current state, run `docbank media status SOURCE_ID` with the receipt's
`source_id`.

## Give an independent client a stable endpoint

While the docbank CLI can discover an ephemeral port and per-run key from the
same-user runtime record, an independent long-lived client should use a fixed
loopback port and a strong API key instead:

```toml
# ~/.docbank/config.toml
[server]
bind_addr = "127.0.0.1"
api_port = 7486
api_key = "replace-with-a-long-random-secret"
idle_timeout = "0"
```

After changing the config, restart the daemon:

```bash
docbank daemon restart
```

The daemon binds to loopback by default. Network clients need an explicitly
configured keyed listener and allowed Host, or a trusted tunnel. Protect the
API key as a vault credential; see
[Bind validation](../configuration.md#bind-validation).

The examples below assume you set these environment variables:

```bash
export DOCBANK_URL=http://127.0.0.1:7486
export DOCBANK_API_KEY=replace-with-a-long-random-secret
```

## Prove reachability and authentication separately

First confirm that the daemon is reachable. Because `/health` does not require
authentication, a successful response here does not prove your key works:

```bash
curl --fail "$DOCBANK_URL/health"
```

Next, send an authenticated request. Resolve `/` to get the root node and its
ID:

```bash
curl --fail-with-body --get \
  -H "Authorization: Bearer $DOCBANK_API_KEY" \
  --data-urlencode 'path=/' \
  "$DOCBANK_URL/api/v1/path"
```

A node response identifies what your agent inspected:

| Field | Meaning |
|-------|---------|
| `id` | Stable node ID; unchanged by renames and moves |
| `revision` | Current node revision; use it for later conditional writes |
| `kind`, timestamps | Node type and recorded times |
| `current_version_id` | Current content version ID, for files |
| `blob_hash`, `md5`, `size` | SHA-256 identity, auxiliary MD5, and raw byte count, for files |
| `path` | Current path, on live single-node responses |
| `source_metadata` | Typed facts extracted from original bytes, once available |

Directories omit content identity, and version IDs also survive renames. Use a
live path for display or for a one-shot operation on that location.

API-key callers receive source fields marked sensitive, such as GPS
coordinates. Browser source metadata omits those fields, though compact Map
browse rows include latitude and longitude when map tiles are configured. The
same browser session can still download the original file; the omission
controls display, not access to information in the original.

The CLI exposes the same distinction without requiring JSON parsing. Human
listings print copyable selectors such as `id:42`, and commands that act on
existing nodes accept either that selector or an absolute path:

```bash
docbank document stat id:42 --json
docbank document cat id:42
docbank document versions list id:42 --json
docbank document move id:42 /review/approved.pdf --json
```

When a shell agent needs a single snapshot of one node, use
`docbank document stat`. Its JSON includes the node revision and, for files,
the current version, SHA-256, size, and MIME type. A trashed ID remains
inspectable, though it has no live `path`.

The `document move` destination stays a path because it names a new location.
In JSON and HTTP requests, node IDs are numbers, not `id:` strings.

Trash is the exception. A successful trash response returns the node's
**pre-trash path** to show where a restore would try to put it. That path no
longer resolves to the trashed node and may later resolve to a different node
if its name is reused. Keep the response's `id` and `revision` for later
inspection or restore by ID, and treat every path attached to a trashed node as
display or recovery context rather than identity.

## Read a tree without unbounded responses

By default, the CLI tree view stops at four levels and 1,000 nodes. Set limits
that fit your task, and check `truncated` and `omissions` before assuming the
result is complete:

```bash
docbank document tree /taxes -L 3 --max-entries 500 --json
```

Use `--all` only when you know the full subtree is small enough to return. For
finer control, request paginated directory children, which come back sorted with
directories first and then by name. Use `total`, `limit`, and `offset` until you
have read the pages you need:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/1/children?limit=500&offset=0"
```

### Search current documents

Inspect `truncated` before treating search results as complete. Each result's
`match` is `name`, `content`, or `filter`, and when you pass `fuzzy=true` it can
also be `fuzzy_name` or `fuzzy_path`. Name matches keep their ranking and appear
before content-only matches.

Content search covers current versions of verified UTF-8 plain text, Markdown,
JSON, and JSONL documents up to 16 MiB, along with text layers in PDFs.
Extraction runs in the background. After a write, check `docbank jobs` or retry
briefly before treating a missing content match as permanent. The daemon
indexes PDF text layers automatically, and does not automatically run Office,
image, or OCR extraction. See [Searching](../usage/searching.md) for the
processing boundary and supported media types.

`GET /api/v1/search` uses lexical matching over words in names and indexed text.
It does not accept a saved QueryV1 payload or expose semantic or hybrid search.

You can narrow the same ranking with these filters:

- **`tag_id`.** Send a canonical tag UUID for one current assignment. Keep the
  echoed ID, because a tag's display name can change.
- **`mime_type`.** Send a valid base media type without parameters, and the
  daemon returns its normalized spelling. `text/plain` includes stored charset
  parameters and excludes directories and non-current versions.
- **`under_path` or `under_node_id`.** Send an absolute virtual directory path
  or its stable ID, never both. The daemon resolves the path and pins the
  directory for the request. Results include descendants and exclude the
  selected directory. Path requests echo the authored `under_path` and resolved
  `under_node_id`; keep the ID because the directory's path can change. Paths
  are case-exact and at most 16,384 UTF-8 bytes. An empty path fails validation,
  a file returns `not_dir`, and a missing or trashed folder returns `not_found`.
- **`modified_since`.** Include nodes at or after this modification time.
- **`modified_before`.** Include nodes strictly before this modification time.

Time filters accept RFC3339 offsets and return canonical UTC values. They match
the live node's `modified_at`, not the source file's timestamp or a historical
content version's time.

You can omit `q` when `tag_id`, `modified_since`, or `modified_before` is
present. Doing so returns a bounded filter page ordered by `modified_at`
descending, with `match: "filter"` on every hit. A MIME or subtree filter can
narrow that page, but cannot anchor an empty query by itself. A blank or
whitespace-only query without a tag or time bound returns
`422 search_query_required`. Results include live files and directories and
exclude the vault root. `limit` bounds response size, not database work. Check `truncated`: a true value means the page is incomplete. Raising
`limit` or narrowing filters may help, though time bounds cannot split nodes
with identical modification timestamps, such as a restored subtree. Search has
no continuation cursor and cannot guarantee complete enumeration of a time
window.

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode 'q=tax return' \
  --data 'tag_id=<tag-uuid>' \
  --data-urlencode 'mime_type=application/pdf' \
  --data 'under_node_id=42' \
  --data-urlencode 'modified_since=2026-01-01T00:00:00Z' \
  --data-urlencode 'modified_before=2026-04-01T00:00:00Z' \
  --data 'limit=100' \
  "$DOCBANK_URL/api/v1/search"
```

To request live nodes changed in a time window, leave out `q`. The result is
still capped by `limit`, so inspect `truncated`:

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode 'modified_since=2026-01-01T00:00:00Z' \
  --data-urlencode 'modified_before=2026-04-01T00:00:00Z' \
  --data 'limit=100' \
  "$DOCBANK_URL/api/v1/search"
```

### Share saved queries and highlight sets

Store reusable definitions through `/api/v1/saved-queries`. See the
[create, read, and edit examples](../usage/searching.md#save-complete-query-intent-over-http)
and the
[payload reference](../architecture/http-api.md#saved-query-and-highlight-definitions).
Keep each definition's `id` and current `ETag`, and send that ETag in `If-Match`
when editing or deleting. On `412 stale_revision`, read the definition again and
reconsider the change.

The definition endpoints store definitions only. They do not execute queries,
apply highlights, or count matching documents. To execute a saved query, call
the separate `POST /api/v1/saved-queries/{saved_query_id}/runs` endpoint.
Documents runs reject `semantic` and `hybrid` modes with `422 invalid_query`,
while Photos runs (`photos: true`) execute them. Permanent audit history makes
saved definitions read-only, and writes return `409 audit_mutation_unsupported`.

### Download and verify content

When a shell workflow needs a local file, use `get`. It keeps incomplete bytes
private and returns a JSON receipt only after verifying and publishing the
complete file:

```bash
docbank document get id:42 ./document.bin --json
```

Independent HTTP clients should fetch file bytes by ID rather than path:

```bash
curl --fail \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/content" \
  --output document.bin.staging
```

Before sending the body, the daemon sends `X-Docbank-Content-Version`,
`X-Docbank-Blob-Hash`, and `X-Docbank-Blob-Size`. After the body, it sends an
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) `Content-Digest`
trailer computed from the bytes it streamed. A trailer is an HTTP field sent
after the response body.

Your client must verify the download before publishing it:

1. Write the response to a private staging file and hash the received bytes.
2. Read through successful EOF and require the digest trailer.
3. Compare the computed digest, the trailer, and the node's `blob_hash`.
4. Require the version header to equal `current_version_id` from the node.
5. Sync and close the staging file, then publish it at the destination.

The initial catalog headers alone do not prove that the streamed bytes verify.

You can list a node's versions one page at a time, then fetch one version's
record or bytes by version ID without relying on the node's current path:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/versions?limit=100&offset=0"

curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/versions/$VERSION_ID"

curl --fail \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/versions/$VERSION_ID/content" \
  --output version.bin
```

The listing is newest-first and returns `items`, `total`, `limit`, and
`offset`. Each version record includes its node, node revision, blob identity,
recording time, transition kind, and the UUID of the operation that introduced
it. Version-byte responses use the same headers and digest trailer as
current-node content.

If the request is cancelled, the body ends in error, or the trailer is absent,
discard the staged content. A partial stream is not verified content, and
Docbank does not drain an abandoned response to complete verification.

### Find references to known content

To find every node and version that references a known SHA-256 hash, query the
content-references endpoint:

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode "sha256=$SHA256" \
  --data 'limit=100' \
  --data 'offset=0' \
  "$DOCBANK_URL/api/v1/content-references"
```

The response returns one page ordered with live current references first, then
live prior versions, then trash. A result includes a path only when the node is
live. An empty result means no content version currently references the hash,
even if GC has not yet swept the unreferenced bytes.

### Open an exact version in the browser

To open a specific version in the authenticated web application, link to its
root origin with the node ID and immutable version UUID in the fragment:

```text
https://docbank.example/#node=42&version=11111111-1111-4111-8111-111111111111
```

Use the browser sign-in origin configured by the operator, which can differ from
the API endpoint. Host the application at the origin root; its API requests use
root-relative paths. Never put an API key in the link or use an unauthenticated
content URL.

After sign-in, Docbank fetches the node and version directly. The version drawer
shows that exact version's metadata, including versions older than the newest
1,000 entries. Live nodes offer the existing verified download, while trashed
nodes keep their Trash label and metadata and must be restored before you can
download their versions. Docbank does not substitute the current version. Links
survive reload and browser back and forward navigation, and if your session
expires, signing in with an API key in the same tab preserves the selection.
Running `docbank web` opens a new session URL without the node and version
fragment; reopen the link after signing in.

Invalid links, missing or purged records, and versions belonging to another node
show an unavailable selection. Query and photo links continue to work
independently.

### Verify one stored file

To run a server-side check on one blob, send the revision from the node
response:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  "$DOCBANK_URL/api/v1/nodes/42/verify"
```

A successful proof returns `blob_hash`, `computed_hash`, `size`,
`computed_size`, and `verified: true`, bound to `node_id`, `version_id`, and
`revision`. Missing or damaged content returns HTTP 200 with `verified: false`
and `problem: "missing"`, `"corrupt"`, or `"unreadable"`. Those responses are
completed checks that found a problem, not request failures. A
`412 stale_revision` means the node changed during or since inspection. Read
it again before deciding what content to verify.

## Organize with stable tags

Create a tag once and keep its UUID and revision or ETag, where the ETag is the
HTTP representation of the revision. A tag's name can change, while its ID keeps
identifying the same tag. A tag revision covers both its definition and all of
its assignments.

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"name":"taxes"}' \
  "$DOCBANK_URL/api/v1/tags"

curl --fail-with-body -X PUT \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  "$DOCBANK_URL/api/v1/nodes/42/tags/$TAG_ID"
```

Assignment receipts return `changed`, the resulting node revision and ETag, and
the tag's current revision and assignment count. A `changed: false` result is
still a success and means the requested assignment state already exists.

Page through `GET /nodes/{id}/tags` or `GET /tags/{tag_id}/nodes`. The tag-nodes
endpoint includes trashed nodes with no live path. Set `live_only=true` to get
only live nodes and paths from one metadata snapshot, with `omitted_trashed`
counting the excluded assignments.

To rename or delete a tag, address it by UUID and send the inspected tag ETag in
`If-Match`. If its definition or assignments changed, the daemon returns
`412 stale_revision`. Deleting a tag removes its assignments, not nodes or
document bytes.

When your intended target is a path, send `{"path":"/records/report.pdf"}` to
`PUT` or `DELETE /path/tags/{tag_id}`. Do not resolve the path with `GET /path`
and then mutate by node ID. An ancestor can move without advancing the target
node's revision. The path endpoint resolves the path and applies the change in
one store transaction. Use the ID-addressed form only when the node ID itself
is the intended target.

## React to vault changes

An API-key client can follow committed changes without comparing repeated vault
listings:

```bash
docbank changes --follow --json
docbank changes --after CURSOR --follow --json
```

MCP agents use the default read-only `list_changes` tool, which takes an
optional `cursor` and returns up to 500 `events`, `next_cursor`, and `has_more`.
Handle each page, save `next_cursor`, and pass it as `cursor` on the next call,
continuing while `has_more` is true. Save the checkpoint even on an empty page.
An omitted or empty MCP cursor returns an empty page and the current checkpoint.
Save that checkpoint before scanning documents, then resume from it to include
changes made during the scan. See the
[MCP call example](../usage/mcp.md#tool-catalog).

Each output line carries `cursor`, `kind`, `node_id`, and `revision`. Save the
cursor after you handle the event. Reconnecting with `--after` replays
subsequent retained events in commit order. Without `--follow`, the command
drains the retained history and prints `cursor: CURSOR` on stderr before
exiting, even for an empty drain. An empty cursor starts at the oldest retained
event. For initial synchronization, save the completion cursor, scan the vault,
and replay after that cursor. Continuity starts with a saved cursor; an initial
drain covers only the retained window.

| Kind | Meaning |
| --- | --- |
| `created` | A file or directory was created. |
| `version_added` | A content version was replaced, confirmed with new bytes, or reverted, or the node's content history was pruned. |
| `moved` | A node changed its name or parent. |
| `tagged` | The node's tags changed, including a tag rename or deletion. |
| `trashed` | A node was trashed, including each affected descendant. |
| `restored` | A node was restored, including each affected descendant. |
| `processed` | Completed publication of new or changed readable derived output, such as text, source metadata, an email inventory, a terminal email body result, an email PDF, a photo description, or face detection. |
| `removed` | A node was permanently deleted by package cancellation, failed import cleanup, or trash purge, including each deleted descendant. |

The feed retains the latest 100,000 events. Several operations emit nothing:
repeated no-op assignments, deduplicated imports, parent-directory revision
bumps, processing failures, invalidation, explicit derivative purge, and
embedding-index updates. Check job status for processing failures. Extractor
fingerprint changes with identical source-metadata facts, dismissed or fully
filtered photo descriptions, and unchanged or fully protected face results also
emit nothing. Photo authored metadata, including ratings, flags, captions, and
color labels, stays silent, while keyword changes using existing tags emit
`tagged` events. Manual face and person edits, album and asset organization,
display choices, and hide or unhide operations stay silent. Events carry node
identity and revision, and an API key can observe file nodes belonging to hidden
photos. Browser sessions cannot read this feed.

Email inventory selection and a new terminal body result each emit an event,
including when a body changes from pending to unavailable. Republishing the
selected inventory or an existing body result stays silent.

Cursors belong to one database incarnation. Restoring a vault creates a new
incarnation, and backups omit the feed. An expired, foreign, or future cursor
returns HTTP 410 with code `changes_cursor_expired` and a current `cursor`. To
recover:

1. Save that cursor before starting a fresh vault scan.
2. Rescan the vault to rebuild your local view.
3. Resume with `--after` and the saved cursor to include changes made during the
   scan. If you use MCP, call `list_changes` without a cursor first to get a new
   checkpoint, save it before rescanning, and resume from it.

The CLI exits with an error and prints the resume cursor on expiry, including
when retention overtakes an active stream. It also reports interrupted drains
and later read failures. HTTP streams wrap changes in typed records and end
finite drains with a result record carrying the checkpoint cursor. Direct HTTP
clients can request `limit=0` for a checkpoint or a finite page of up to 500
events with `limit=500`. CLI stdout contains only changes. Handle events
idempotently, since a scan and replay can overlap. A later node lookup can
return 404 if that node has since been removed. See the
[HTTP route](../architecture/http-api.md#vault-change-feed) for direct clients.

## Follow backup progress without scraping a CLI

Agents that create snapshots can call `POST /api/v1/backup/snapshots/stream`
with the same JSON object as the single-response endpoint:

```bash
curl --no-buffer --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"repo":"/absolute/server/path","tag":"before-edit","jobs":1}' \
  "$DOCBANK_URL/api/v1/backup/snapshots/stream"
```

The response is NDJSON, with one JSON record per line. Each `progress` line
includes `stage`, `done`, `total`, `bytes_done`, `bytes_total`, and `final`.
The last line is either `result` with the snapshot summary or `error` with the
normal problem fields. Treat EOF before that terminal line as a failure, and do
not interpret HTTP 200 as snapshot success. It only confirms that streaming
began.

## Inspect daemon background work

Before relying on a configured background feature, check its task state:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/jobs"
```

The response is `{items: [...], lanes: [...]}`, with `items` sorted by task
name. Branch on `status`, which can be `queued`, `running`, `completed`,
`failed`, or `cancelled`. For `running`, `busy: false` means waiting and
`busy: true` means working. `completed`, `failed`, and `cancelled` are terminal
for this daemon run. Surface a failed task's `error` to the operator, and do
not parse its prose as a protocol. Items named `storage:<operation-id>` are
durable storage operations read from the vault, so they survive a daemon
restart. An absent item is not proof that work completed. It can mean the
feature is unconfigured, or, for in-process tasks whose status history lives
only in the daemon process, that the daemon restarted.

## Use revisions for read-modify-write

ID-addressed move, trash, and restore operations require `If-Match`, carrying
the revision of the node state your agent evaluated:

```bash
# A prior GET returned id=42 and revision=7.
curl --fail-with-body -X PATCH \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  -H 'Content-Type: application/json' \
  --data '{"new_parent_id": 18, "new_name": "filed.pdf"}' \
  "$DOCBANK_URL/api/v1/nodes/42"
```

If another actor changed the node first, the API returns `412` with
`code: "stale_revision"`. Do not blindly replay the old decision:

1. Re-read the node by ID.
2. Re-evaluate the intended move, name, or deletion against its new state.
3. Retry with the new revision only if the intent still applies.
4. Limit retries. Repeated conflicts need a human or a higher-level policy.

A missing precondition returns `428 precondition_required`, and an invalid
header returns `400 validation`.

Content replacement follows the same read-decide-write rule and adds byte
evidence. Compute the local file's SHA-256 and size, keep the revision from the
node response, and send the raw bytes:

```bash
curl --fail-with-body -X PUT \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  -H "X-Docbank-Blob-Hash: $SHA256" \
  -H "X-Docbank-Blob-Size: $SIZE" \
  -H 'Content-Type: application/pdf' \
  --data-binary @revised.pdf \
  "$DOCBANK_URL/api/v1/nodes/42/content"
```

Accept the replacement only when every receipt check passes:

- `computed_hash` and `computed_size` equal the local values.
- The node and version both name node 42.
- The operation is `content_replace`.
- The node revision advances by one.
- The returned blob identity matches.
- `node.current_version_id == version.id`.
- The response ETag encodes the resulting revision.

HTTP 200 alone is not enough, and the old version remains available. On `412`,
read the node again and reconsider the decision before retrying.

`docbank document edit` is a human-directed wrapper around the same contract.
It opens an interactive local editor and has no JSON mode. Agents should use
the raw replacement API or typed client so they can keep and validate the full
receipt themselves.

Reversion applies the same concurrency rule without uploading bytes. Pick a
prior version belonging to the inspected node and send:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "8"' \
  -H 'Content-Type: application/json' \
  --data '{"source_version_id":"11111111-1111-4111-8111-111111111111"}' \
  "$DOCBANK_URL/api/v1/nodes/42/revert"
```

Accept the reversion only when every receipt check passes:

- `source_version.id` equals the requested version ID.
- All three records name node 42.
- The new version is `content_revert` and names the selected source.
- Its hash, size, and media type match that source.
- The node installs the new version at revision 9.
- The ETag matches the resulting revision.

Reversion changes metadata without copying bytes, so it returns no newly
computed digest. When your workflow also needs a fresh check of stored bytes,
run content verification.

Version retention is unlimited by default. To release unwanted non-current
history, preview exactly one selector through the authenticated pruning route:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "9"' \
  -H 'Content-Type: application/json' \
  --data '{"keep_newest":3}' \
  "$DOCBANK_URL/api/v1/nodes/42/versions/prune"
```

The other request selectors are `version_ids`, `older_than`, and `all_prior`,
and exactly one is allowed per request. An omitted or false `run` runs a dry run.
After reviewing the returned candidate IDs, logical bytes, retained revert
dependencies, and loose or packed maintenance consequences, repeat the request
with `"run":true` and the same inspected revision. Never blindly replace a
stale `If-Match`; re-read the node and re-evaluate the selection.

`older_than` is evaluated against the `cutoff` returned by each request. Time
can move versions across that boundary without changing the node ETag, so a
later age-based run can include additional candidates. If execution must match
the preview exactly, send its candidate IDs through `version_ids` instead of
repeating the age selector. Explicit-ID requests accept at most 1,000 IDs. For
larger sets, run in batches and inspect the advanced node revision before
sending each next batch.

For a dry run, verify that the response node and ETag match the inspected node
and revision, `changed:false`, `deleted_versions:0`, and there is no
checkpoint. For an executed change, verify that `deleted_versions` equals the
candidate count, `changed:true`, and the revision advances by one. When
`checkpoint_required:true`, execution must return a source-free
`content_replace` checkpoint installed as the current version. Blob counts must
partition into shared and releasable. A releasable blob may have loose
locations pending GC, packed locations pending repack, or both, with
`mixed_blobs_pending_maintenance` reporting that overlap. The byte totals cover
every authoritative location across every store. These are future maintenance
candidates, not bytes reclaimed by pruning.

Path mutations work differently. `POST /api/v1/path/move` and
`POST /api/v1/path/trash` resolve and mutate inside one store transaction, so
they do not accept `If-Match`. Use them for a one-shot instruction tied to the
path as it exists when the transaction runs. When your agent previously
inspected a particular node and wants lost-update protection, use ID plus
revision instead.

For a reorganization that must not partially apply, send one plan to
`POST /api/v1/batch/move` or run `docbank document move batch`. Each source is
either an absolute `source_path` resolved inside the transaction, or a
`node_id` paired with the revision your agent inspected. Every
`destination_path` is the node's full final path, and its parent resolves in
the planned final tree. An existing directory does not invoke ordinary
`document move`'s "move into" shorthand. Docbank validates the complete final
tree before changing it, which allows file and directory swaps without
temporary names. Require a receipt for every request item in the same order,
and reconcile its node ID, prior path, final path, and resulting revision. Any
error means the entire plan was rejected.

## Inspect document provenance

Use the node ID to fetch the immutable facts describing where a file was
ingested from:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/provenance?limit=100&offset=0"
```

`node.path` is the file's current path in Docbank, while a fact's
`original_path` names an external source as the ingest observed it. When your
workflow needs facts that have not been superseded, branch on `active`, and
keep fact identities because a correction adds a successor and keeps the
superseded record addressable. Paginate with `total`, `limit`, and `offset`. A
trashed file remains inspectable by ID and returns an empty live path.

Provenance is evidence, not ownership of the external source. Reading it does
not open or modify that source, and it does not make a content version a
retention root.

To record an origin learned after ingest, send the node revision from a prior
read:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  -H 'Content-Type: application/json' \
  --data '{"source_kind":"agent","source_description":"triage","original_path":"opaque://laptop/report.txt"}' \
  "$DOCBANK_URL/api/v1/nodes/42/provenance"
```

The response is `201` with the appended fact, resulting node revision, path,
and ETag, and `original_path` stays opaque evidence. To correct an active
caller-supplied fact on the same node, pass its `identity` as `supersedes`;
the earlier fact stays in history. Operational facts recorded by CLI or
watched-folder ingest cannot be superseded, because they keep re-ingest
idempotent. Add the newly learned origin alongside them instead. If you supply
`original_mtime`, use canonical UTC RFC3339Nano, for example
`2026-08-26T12:00:00Z`. Timestamps with a numeric offset are rejected.

## Create and ingest safely

For a one-shot instruction tied to a specific path, create the directory by
path. Its parent must already exist:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"path":"/receipts/2026"}' \
  "$DOCBANK_URL/api/v1/path/mkdir"
```

The parent resolves inside the mutation transaction. When your workflow has
already selected a specific parent node, create beneath its ID instead, so a
concurrent parent move does not change which directory receives the child:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"parent_id": 1, "name": "receipts", "kind": "dir"}' \
  "$DOCBANK_URL/api/v1/nodes"
```

Do not treat a `409 exists` response as automatic success. Resolve the existing
name and verify that it is the directory your workflow intended.

`POST /api/v1/ingest` reads absolute paths on the daemon host and is restricted
to loopback callers. It is not a file-upload endpoint.

Before ingesting a large tree, inventory the selection first. This request
reads filesystem metadata without opening file content or mutating the vault:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Dropbox"],"include":["*.pdf"],"exclude":[".git",".Trash"]}' \
  "$DOCBANK_URL/api/v1/ingest/preflight"
```

Require `errors == 0` and `rejected.files == 0`, review every returned finding,
and reuse the same include and exclusion lists for ingest. The response caps
findings and extension groups, and their count and truncation fields tell you
when the detailed arrays are samples rather than complete lists. A non-UTF-8
filesystem entry is reported as an error with an escaped printable path and is
never opened or imported.

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Downloads/receipt.pdf"],"dest":"/receipts","include":[],"exclude":[],"replace":true}' \
  "$DOCBANK_URL/api/v1/ingest"
```

Inspect `added`, `skipped`, `excluded`, and every item in `failed`. After
fixing a partial failure, repeat the same ingest. Files already accepted become
`skipped` instead of creating another copy.

When one destination file represents a changing source, set `replace: true` on
either ingest form. The daemon then:

1. Reads the destination node and revision before reading source bytes.
2. Adds a version when the content changes, keeping the node ID.
3. Skips equal hash and size without changing stored MIME type or history.

Directories, stale revisions, and concurrent creation of the destination name
fail that file, and replacement does not fall back to a suffix. Omit `replace`
to use ordinary collision suffixing.

Include and exclude values use Go's `path.Match` grammar on slash-separated
source-relative paths. Use `/` separators on every platform; backslashes are
rejected. A pattern without `/` matches basenames at any depth. Exclusions win,
include patterns do not prune directories, and invalid patterns fail before
traversal. For literal metacharacters, use bracket expressions such as
`report[[]1].txt` instead of backslash escaping. Watched inboxes accept the
same glob syntax in `exclude_patterns`, while `exclude` entries remain literal.

For streaming progress, send the same body to `POST /api/v1/ingest/stream` with
`Accept: application/x-ndjson` and read every line through EOF. `progress`
events cover the metadata-only `scan` and content `ingest` stages. The single
terminal event is either `result` with the final report or `error`.

HTTP 200 confirms that streaming started. Treat missing terminal events,
malformed events, or data after the terminal event as a failure.

Cancellation or disconnection stops traversal and the active blob write. Any
files already published remain in the vault and are skipped on a rerun.

Remote writers upload one file per multipart request. Compute the expected
identity before sending bytes, and pass either `parent_id` or an absolute
`parent_path`. Missing path directories publish atomically with the verified
upload:

```bash
FILE=receipt.pdf
HASH=$(shasum -a 256 "$FILE" | awk '{print $1}')
SIZE=$(wc -c < "$FILE" | tr -d ' ')

curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "X-Docbank-Blob-Hash: $HASH" \
  -H "X-Docbank-Blob-Size: $SIZE" \
  -F 'file=@receipt.pdf;filename=receipt.pdf;type=application/pdf' \
  "$DOCBANK_URL/api/v1/uploads?parent_id=18&name=receipt.pdf"
```

Percent-encode any nontrivial `name` query value. The request carries exactly one part
named `file`, and its multipart filename must match `name`. The hash and size
headers describe that file payload. A top-level `Content-Digest` would describe
the multipart envelope, so it is not the write precondition.

To retain URL fetch evidence, send `X-Docbank-URL-Fetch` as described in the
[upload contract](../architecture/http-api.md#addendum-post-uploads). The CLI's
[URL import](../usage/importing.md#import-from-a-url) downloads and sends this
evidence.

On `201`, require `status: "added"`. An idempotent retry returns `200` with
`status: "skipped"` and the same node. In both cases, compare `computed_hash`
and `computed_size` with your locally calculated values, then keep `node.id`,
`node.revision`, and `node.blob_hash`. A `digest_mismatch` or `size_mismatch`
is a failed write, and no new node or blob enters the vault. Upload multiple
files as independent requests so each result is unambiguous and independently
retryable.

The receipt proves that the bytes matched when the daemon received them. When
policy requires evidence about bytes currently stored in the vault, use the
revision-bound single-node verification endpoint later.

## Enroll permanent history only after an exact preview

Audit enrollment is irreversible. First preview one live directory by path or
node ID:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"path":"/taxes","agent_label":"records policy"}' \
  "$DOCBANK_URL/api/v1/audit/preview"
```

Review or present the returned protected node and version counts, logical and
unique bytes, vault-wide evidence counts, and `baseline_digest`. First
activation permanently retains enrollment-time names, topology, tags,
assignments, ingests, and provenance across the vault, including outside the
selected scope. Unrelated content versions do not become scope members, though
the metadata snapshot remains evidence. Only after that review may a client
execute the plan the daemon is holding:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"preview_token":"<one-use-token>","acknowledge_permanent_retention":true}' \
  "$DOCBANK_URL/api/v1/audit/enable"
```

The token expires after ten minutes, is consumed on one attempt, and does not
survive a daemon restart. On `audit_preview_stale`, preview again instead of
retrying the same execution. `GET /api/v1/audit/status` reports vault-wide
evidence; add either `?path=/taxes/file.pdf` or `?node_id=57` to inspect sticky
membership. Read that node's canonical timeline with exactly one selector:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/history?node_id=57&limit=50"
```

Events appear newest first, and each identifies its event, operation, scope,
and node revision, along with path or content identities when applicable. Path
states use `/live/paths`, `@trash/known/...`, or `@trash/unknown/...`. Tag and
provenance events include an `attachment` with `kind`, `identity`, and typed
`before` and `after` states.

To read older events, send `next_cursor` back unchanged. It belongs to that
node, stays valid when newer events arrive, and should not be decoded or
interpreted.

`audit_not_enrolled` means the node is outside permanent retention, while
`invalid_audit_cursor` is a request error. An enrolled node can have no events
if it has not changed since enrollment. Check status membership to determine
protection. An empty timeline is not enough.

To answer "what changed anywhere in this protected scope?", use the scope ID
returned by audit status:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/scopes/<scope-id>/history?limit=50"
```

Each page includes current scope evidence and events from every protected
member. Reconcile each event's `scope_id`, keep its `node_id` as the subject,
and follow `next_cursor` unchanged. A scope cursor belongs to that scope and
stays valid when newer events arrive.

To independently replay the authority and hash every protected blob, run:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/verify"
```

A successful HTTP response can still report failed verification. Require empty
`metadata_problems` and `problems`, `verified_blobs == protected_blobs`, and a
non-null `evidence` object when `enabled` is true. When rollback detection
matters, store that evidence outside the vault. It contains vault and
allocation-lineage identities, allocation count and head, the operation
high-water mark, and every scope count and head. This endpoint hashes protected
content only; the top-level `/api/v1/verify` also covers unaudited blobs.

To check a later vault against that trusted record, send the prior successful
report's `evidence` object as `expected`:

```bash
jq -c '{expected: .evidence}' audit-evidence.json > audit-expected-request.json
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @audit-expected-request.json \
  "$DOCBANK_URL/api/v1/audit/verify"
```

Require `evidence_check.extends: true` alongside clean metadata and blob
results. Equal chains and valid extensions pass. Stable problem codes are
`audit_not_enabled`, `vault_mismatch`, `lineage_mismatch`,
`allocation_shorter`, `allocation_diverged`, `scope_missing`, `scope_shorter`,
and `scope_diverged`. Evidence mismatch is reported in the body rather than as
an HTTP transport error so agents can inspect the current verified evidence and
byte state before escalating.

A vault can have several disjoint permanent directory scopes, and overlapping
or nested scopes are rejected. See
[Permanent audited history](../usage/audited-history.md) for scope membership
and maintenance rules.

## Treat destructive maintenance as a two-step decision

Trash empty and GC run as dry runs when `run` is false. Present or evaluate the
report before issuing a separate execution request:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"older_than":"30d","run":false}' \
  "$DOCBANK_URL/api/v1/trash/empty"

curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"run":false}' \
  "$DOCBANK_URL/api/v1/gc"
```

Send `run: true` only when policy explicitly authorizes permanent removal.
Treat execution as a new decision. The vault may have changed since the
preview. Packed bytes reported as pending are logically dead and not yet
physically reclaimed.

`POST /api/v1/verify` validates logical metadata and audit history before
re-hashing every stored blob. It is read-only, though it can be expensive.
Maintenance requests serialize against mutations and may run without the
ordinary request timeout. A new mutation submitted while maintenance is running
or waiting for in-flight mutations receives `503 maintenance_busy`. Retry it
after the operator-visible maintenance ends; this is a transient refusal, not
evidence that the requested mutation committed. Treat either
`metadata_problems` or blob `problems` as a failed verification.

When your decision concerns one inspected file, use
`POST /api/v1/nodes/{id}/verify`. Unlike the vault-wide operation, it requires
`If-Match`, checks only one blob, and returns the recorded and freshly computed
identities directly.

`POST /api/v1/storage/pack` is explicit and non-destructive, changing the
physical representation without changing document identity or blob read
authority. When an operator needs an auditable result, check
`GET /api/v1/storage` before and after. A positive `max_bytes` is a soft limit
on raw-byte work; the blob that crosses the budget is still committed.
`budget_exhausted: true` describes that crossing, not whether eligible loose
blobs remain. Inspect storage status before deciding to issue another request.

`POST /api/v1/storage/repack` physically retires empty packs and rewrites
eligible sparse packs without changing logical content authority. Its selection
thresholds are policy rather than a preview guarantee. Inspect storage status
before and after. `bytes_repacked` counts live raw bytes rewritten and must not
be reported as reclaimed disk space.

## Branch on problem codes

Non-2xx responses use RFC 7807 problem JSON:

```json
{
  "title": "Conflict",
  "status": 409,
  "detail": "node \"report.pdf\" already exists",
  "code": "exists"
}
```

Branch on `code`, never `detail`. These policy groups help you route responses:

- Re-read and reconsider: `stale_revision`.
- Correct the request: `validation`, `precondition_required`, `invalid_name`, `invalid_tag`,
  `not_dir`, `not_file`, `is_root`.
- Reconcile desired state: `exists`, `cycle`, `not_trashed`, `not_found`.
- Stop and surface credentials or topology: `unauthorized`, `loopback_only`.
- Retry after the active maintenance operation ends: `maintenance_busy`.
- Release external file locks, then run `storage pack` reconciliation:
  `pack_retirement_deferred`. The preceding repack catalog change already
  committed. Do not restore the retired mapping or assume rollback.
- Stop automation and preserve evidence: `internal`.

See [HTTP API](../architecture/http-api.md) and the OpenAPI document for the
complete mapping.

## A safe filing loop

For an inbox-filing agent, follow this sequence:

1. Resolve `/inbox` and page through its children.
2. Read metadata or content for candidate files by ID.
3. Decide a destination, and create missing directories deliberately.
4. If the decision took long enough for concurrent work to be plausible,
   re-read the candidate.
5. Move by ID using the revision the decision was based on.
6. On `412`, re-read and reconsider rather than replaying.
7. Record the returned ID, path, and revision as the outcome.

Keep planning and mutation separate in your logs, and never log the API key,
shutdown token, or document content by default. Use request IDs from your own
workflow for correlation. Docbank's node ID is the lasting identity of the
object.
