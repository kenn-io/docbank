---
title: Agent Integration Guide
description: Connect an agent to docbank safely using its OpenAPI contract, authenticated HTTP API, revisions, and dry-run maintenance operations.
---

# Agent integration guide

Connect an agent through Docbank's authenticated HTTP API. The daemon owns
the vault; the CLI, agents, and scripts send requests to it. An external
integration must not open `docbank.db` or stored content directly.

Go applications that own a separate archive can use the
[embedded API](../embedding.md).

## Choose the interface

Use the CLI for human-directed shell work and simple orchestration. Use HTTP
for structured agent workflows, pagination, machine-readable errors, and
revision-aware mutations.

For simple shell orchestration, CLI exit codes distinguish invalid usage (`2`),
missing vault objects (`3`), stale state (`4`), busy resources (`5`), and
integrity findings (`6`) from general failures (`1`). Verification may emit a
complete report before exiting `6`; never infer success merely because stdout
contains JSON. The [CLI reference](../cli-reference.md#process-exit-codes)
defines the full contract. Independent integrations should use the richer HTTP
problem `code` values below.

For a small shell workflow, `mv`, `rm`, and `restore` accept `--json` and
return the daemon's complete resulting node receipt. A trash receipt's `path`
is only its pre-trash recovery context; carry the stable `id` and `revision`
forward instead.

Before operating on an unfamiliar machine or switching archives, identify the
selected vault explicitly:

```bash
docbank info --json
```

Treat `vault_id` as the durable identity and `vault_path` as machine-local
placement. `DOCBANK_HOME=/path/to/another/vault docbank info --json` selects
and confirms another independently owned archive without changing a global
profile or opening its database directly.

A workflow that depends on watched ingestion can inspect the daemon's effective
configuration and runner state rather than assuming the local file is active:

```bash
docbank watch list --json
```

Each item reports the local source, virtual destination, settle window,
minimum source age, scan interval, literal exclusions, and current job record.
The settle window is the time a file must remain unchanged. A nonzero minimum
age adds a separate check; it does not replace that window.

This command only inspects configuration. After changing `config.toml`, restart
the daemon. See [Watched inboxes](../configuration.md#watched-inboxes) for the
complete timing and source-identity rules.

The canonical contract is generated from the running route definitions:

```bash
docbank openapi --json > docbank-openapi.json   # offline; no vault needed
```

A running daemon also serves `/openapi.json`, `/openapi.yaml`, and interactive
docs at `/docs`. These contract routes are auth-exempt so a client generator
can discover them before authentication is configured.

The rendered documentation is available to people at directory routes such as
`/agents/integration/`. The same maintained source is published for agents at
the sibling `/agents/integration.md` URL.

## Give an independent client a stable endpoint

The docbank CLI can discover an ephemeral port and per-run key from the
same-user runtime record. An independent long-lived client should instead use
an explicit loopback port and a strong API key:

```toml
# ~/.docbank/config.toml
[server]
bind_addr = "127.0.0.1"
api_port = 7486
api_key = "replace-with-a-long-random-secret"
idle_timeout = "0"
```

Restart after changing config:

```bash
docbank daemon restart
```

The daemon rejects non-loopback binds. Remote access is not a separate mode:
use an SSH tunnel or VPN that terminates at the daemon host's loopback
listener, and protect the API key as a vault credential.

Examples below assume:

```bash
export DOCBANK_URL=http://127.0.0.1:7486
export DOCBANK_API_KEY=replace-with-a-long-random-secret
```

## Prove reachability and authentication separately

First check that the daemon is reachable. `/health` does not require
authentication, so success here does not prove that your key works:

```bash
curl --fail "$DOCBANK_URL/health"
```

Then make an authenticated request. Resolve `/` to obtain the root node and
its ID:

```bash
curl --fail-with-body --get \
  -H "Authorization: Bearer $DOCBANK_API_KEY" \
  --data-urlencode 'path=/' \
  "$DOCBANK_URL/api/v1/path"
```

A node response identifies what the agent inspected:

| Field | Meaning |
|-------|---------|
| `id` | Stable node ID; unchanged by renames and moves |
| `revision` | Current node revision; use it for later conditional writes |
| `kind`, timestamps | Node type and recorded times |
| `current_version_id` | Current content version ID, for files |
| `blob_hash`, `md5`, `size` | SHA-256 identity, auxiliary MD5, and raw byte count, for files |
| `path` | Current path, on live single-node responses |
| `source_metadata` | Typed facts extracted from original bytes, once available |

Directories omit content identity. Version IDs also survive renames. Use a
live path for display or a one-shot operation on that location.

API-key callers receive source fields marked sensitive, such as GPS
coordinates. Browser responses omit those fields, but the same session can
still download the original file. The omission controls display, not access
to information in the original.

The CLI exposes the same distinction without requiring JSON parsing. Human
listings print copyable selectors such as `id:42`, and existing-node commands
accept either that stable selector or an absolute path:

```bash
docbank stat id:42 --json
docbank cat id:42
docbank versions list id:42 --json
docbank mv id:42 /review/approved.pdf --json
```

Use `docbank stat` when a shell agent needs one authoritative node snapshot.
Its JSON includes the node revision and, for files, the current version,
SHA-256, size, and MIME type. A trashed ID remains inspectable but has no live
`path`.

The `mv` destination stays a path because it describes a new coordinate. In
JSON and HTTP requests, node IDs remain numeric rather than `id:` strings.

Trash is the important exception. A successful trash response returns the
node's **pre-trash path** to explain where a restore would try to put it. That
path no longer resolves to the trashed node and may later resolve to a different
node if its name is reused. Retain the response's `id` and `revision` for
subsequent ID-addressed inspection or restore, and treat every path attached to
a trashed node as display or recovery context rather than identity.

## Read a tree without unbounded responses

The CLI tree view is bounded by default to four levels and 1,000 nodes. Set
explicit limits for the task and inspect `truncated` plus `omissions` before
assuming the result is complete:

```bash
docbank tree /taxes -L 3 --max-entries 500 --json
```

Use `--all` only when the complete subtree is known to be appropriately sized.
For finer control, directory children are paginated and sorted with directories
first, then by name. Use `total`, `limit`, and `offset` until the required page
set is read:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/1/children?limit=500&offset=0"
```

### Search current documents

Always inspect `truncated` before treating search results as complete.
Each result's `match` is `name`, `content`, or `filter`. Name matches keep their
ranking and appear before content-only matches.

Content search covers current versions of verified UTF-8 plain text, Markdown,
JSON, and JSONL documents up to 16 MiB. Extraction runs in the background.
After a write, inspect `docbank jobs` or retry briefly before treating a
missing content match as permanent. The daemon does not automatically run
PDF, Office, image, or OCR extraction. See [Searching](../usage/searching.md)
for the processing boundary and exact media types.

`GET /api/v1/search` uses lexical matching: words in names and indexed text.
It does not accept a saved QueryV1 payload or expose semantic or hybrid search.

Use these filters to narrow the same ranking:

- **`tag_id`:** send a canonical tag UUID for one current assignment. Keep the
  echoed ID; a tag's display name can change.
- **`mime_type`:** send a valid base media type without parameters. The daemon
  returns its normalized spelling. `text/plain` includes stored charset
  parameters, but excludes directories and non-current versions.
- **`under_node_id`:** resolve a live directory and send its stable ID. Results
  include descendants, excluding the selected directory. Keep the echoed ID;
  the directory's path can change.
- **`modified_since`:** include nodes at or after this modification time.
- **`modified_before`:** include nodes strictly before this modification time.

Time filters accept RFC3339 offsets and return canonical UTC values. They
refer to the live node's `modified_at`, not the source file's timestamp or a
historical content version's time.

The `q` parameter may be omitted when `tag_id`, `modified_since`, or
`modified_before` is present. This returns a bounded filter page ordered by
`modified_at` descending, with `match: "filter"` on every hit. A MIME or
subtree filter can narrow that page but cannot anchor an empty query by itself;
a blank or whitespace-only query without a tag or time bound returns
`422 search_query_required`. Results include live files and directories, but
exclude the vault root. The limit bounds response size, not database work.
Always inspect `truncated`: a true value means the page is incomplete.
Increasing `limit` or narrowing filters may help, but time bounds cannot split
nodes with identical modification timestamps, such as a restored subtree.
Search has no continuation cursor and cannot guarantee complete enumeration
of a time window.

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
still bounded; inspect `truncated`:

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode 'modified_since=2026-01-01T00:00:00Z' \
  --data-urlencode 'modified_before=2026-04-01T00:00:00Z' \
  --data 'limit=100' \
  "$DOCBANK_URL/api/v1/search"
```

### Share saved queries and highlight sets

Store reusable definitions through `/api/v1/saved-queries`. Use the
[create, read, and edit examples](../usage/searching.md#save-complete-query-intent-over-http)
and the [payload reference](../architecture/http-api.md#saved-query-and-highlight-definitions).
Keep each definition's stable `id` and current `ETag`. Send that ETag as
`If-Match` when editing or deleting; on `412 stale_revision`, read again and
reconsider the change.

These endpoints store definitions only. They do not execute queries, apply
highlights, or count matching documents. A payload with `mode: "hybrid"` does
not enable hybrid execution. Permanent audit history makes saved definitions
read-only: writes return `409 audit_mutation_unsupported`.

### Download and verify content

Use `get` when a shell workflow needs a local file. It keeps incomplete bytes
private and returns a JSON receipt only after verifying and publishing the
complete file:

```bash
docbank get id:42 ./document.bin --json
```

Independent HTTP clients should retrieve file bytes by ID, not path:

```bash
curl --fail \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/content" \
  --output document.bin.staging
```

Before the body, the daemon sends `X-Docbank-Content-Version`,
`X-Docbank-Blob-Hash`, and `X-Docbank-Blob-Size`. After the body, it sends an
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) `Content-Digest`
trailer computed from the bytes it streamed. A trailer is an HTTP field sent
after the response body.

The client must verify the download before publishing it:

1. Write the response to a private staging file and hash the received bytes.
2. Read through successful EOF and require the digest trailer.
3. Compare the computed digest, the trailer, and the node's `blob_hash`.
4. Require the version header to equal `current_version_id` from the node.
5. Sync and close the staging file, then publish it at the destination.

The initial catalog headers alone do not prove that the streamed bytes verify.

List a node's immutable versions with bounded pagination, then address one
record or byte stream without relying on its current path:

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
`offset`. A version record includes its node, node revision, blob identity,
recording time, transition kind, and introducing operation UUID. Version-byte
responses use the same headers and terminal digest contract as current-node
content.

Discard staged content if the request is cancelled, the body ends in error,
or the trailer is absent. A partial stream is not verified content. Docbank
does not drain an abandoned response to complete verification.

### Find references to known content

Find the nodes and versions that retain a known SHA-256 hash:

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode "sha256=$SHA256" \
  --data 'limit=100' \
  --data 'offset=0' \
  "$DOCBANK_URL/api/v1/content-references"
```

The response is a bounded page ordered with live current references first,
then live prior versions, then trash. A result's path is present only for a
live node. No result means no logical content version currently retains the
hash, even if unreferenced physical bytes have not yet been swept by GC.

### Verify one stored file

For a server-side check of one blob, send the revision from the node response:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  "$DOCBANK_URL/api/v1/nodes/42/verify"
```

A successful proof returns `blob_hash`, `computed_hash`, `size`,
`computed_size`, and `verified: true`, bound to `node_id`, `version_id`, and
`revision`.
Missing or damaged content returns HTTP 200 with `verified: false` and
`problem: "missing"`, `"corrupt"`, or `"unreadable"`; those are completed
checks with negative evidence, not request failures. A `412 stale_revision`
means the node changed during or since inspection—read it again before deciding
what content to verify.

## Organize with stable tags

Create a tag once and keep its UUID and revision/ETag. The ETag is the HTTP
representation of the revision. Names can change; IDs continue identifying
the same tag. A tag revision covers its definition and all assignments.

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

Assignment receipts return `changed`, the resulting node revision/ETag, and
the tag's current revision and assignment count. `changed: false` means the
requested assignment state already exists; it is a successful result.

Page through `GET /nodes/{id}/tags` or `GET /tags/{tag_id}/nodes`. The latter
includes trashed nodes with no live path. Set `live_only=true` to receive only
live nodes and paths from one metadata snapshot. `omitted_trashed` counts the
excluded assignments.

Rename or delete by UUID with the inspected tag ETag in `If-Match`. If its
definition or assignments changed, the daemon returns `412 stale_revision`.
Deleting a tag removes its assignments, not nodes or document bytes.

When the desired target is a path, send `{"path":"/records/report.pdf"}` to
`PUT` or `DELETE /path/tags/{tag_id}`. Do not resolve the path with `GET /path`
and then mutate by node ID: an ancestor can move without advancing the target
node's revision. The path endpoint resolves and changes authority in one store
transaction. Use the ID-addressed form only when the stable node ID itself is
the intended authority.

## Follow backup progress without scraping a CLI

Agents that create snapshots can use `POST
/api/v1/backup/snapshots/stream`. It accepts the same JSON object as the
single-response endpoint:

```bash
curl --no-buffer --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"repo":"/absolute/server/path","tag":"before-edit","jobs":1}' \
  "$DOCBANK_URL/api/v1/backup/snapshots/stream"
```

The response is NDJSON: one JSON record per line. Each `progress` line contains
`stage`, `done`, `total`,
`bytes_done`, `bytes_total`, and `final`. The last line is either `result` with
the stable snapshot summary or `error` with the normal problem fields. Treat
EOF before that terminal line as failure. In particular, do not interpret HTTP
200 as snapshot success: it only confirms that streaming began.

## Inspect daemon background work

Before relying on a configured background feature, inspect its task state:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/jobs"
```

The response is `{items: [...]}`, sorted by stable task name. Branch on
`status`: `running` is active; `completed`, `failed`, and `cancelled` are
terminal for this daemon run. Surface a failed task's bounded `error` to the
operator, but do not parse its prose as a protocol. An absent item is not proof
that work completed—it can mean the feature is unconfigured or the daemon
restarted, because status history is intentionally process-local.

## Use revisions for read-modify-write

ID-addressed move, trash, and restore operations require `If-Match`. The
revision belongs to the node state the agent evaluated:

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
4. Bound retries; repeated conflicts require human or higher-level policy.

A missing precondition returns `428 precondition_required`. An invalid header
returns `400 validation`.

Content replacement follows the same read-decide-write rule and adds byte
evidence. Compute the local file's SHA-256 and size, retain the revision from
the node response, then send raw bytes:

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

Accept replacement only when every receipt check passes:

- `computed_hash` and `computed_size` equal the local values.
- The node and version both name node 42.
- The operation is `content_replace`.
- The node revision advances by one.
- The returned blob identity matches.
- `node.current_version_id == version.id`.
- The response ETag encodes the resulting revision.

HTTP 200 alone is insufficient. The old version remains available. On `412`,
read the node again and reconsider the decision before retrying.

`docbank edit` is a human-directed wrapper around this same contract: it opens
an interactive local editor and intentionally has no JSON mode. Agents should
use the raw replacement API or typed client so they can retain and validate the
full byte-identity receipt themselves.

Reversion applies the same concurrency rule without uploading bytes. Select a
prior version belonging to the inspected node and send:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "8"' \
  -H 'Content-Type: application/json' \
  --data '{"source_version_id":"11111111-1111-4111-8111-111111111111"}' \
  "$DOCBANK_URL/api/v1/nodes/42/revert"
```

Accept reversion only when every receipt check passes:

- `source_version.id` equals the requested version ID.
- All three records name node 42.
- The new version is `content_revert` and names the selected source.
- Its hash, size, and media type exactly match that source.
- The node installs the new version at revision 9.
- The ETag matches the resulting revision.

Reversion changes metadata without copying bytes, so it returns no newly
computed digest. Use content verification when the workflow also needs a fresh
check of stored bytes.

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

The other request selectors are `version_ids`, `older_than`, and `all_prior`;
exactly one is allowed. Omitted or false `run` is a dry run. After evaluating
the returned candidate IDs, logical bytes, retained revert dependencies, and
loose/packed maintenance consequences, repeat with `"run":true` and the same
inspected revision. Do not blindly replace a stale `If-Match`: re-read the node
and re-evaluate the selection.

`older_than` is evaluated against the `cutoff` returned by each request. Time
can move versions across that boundary without changing the node ETag, so a
later age-based run can contain additional candidates. If execution must match
the preview exactly, send its candidate IDs through `version_ids` instead of
repeating the age selector. Explicit-ID requests accept at most 1,000 IDs; for
larger sets, execute batches and inspect the advanced node revision before
sending each next batch.

For a dry run, require the response node and ETag to match the inspected node
and revision, `changed:false`, `deleted_versions:0`, and no checkpoint. For an
executed change, require `deleted_versions` to equal the candidate count,
`changed:true`, and exactly one revision advance. When
`checkpoint_required:true`, execution must return a source-free
`content_replace` checkpoint installed as the current version. Blob counts must
partition into shared versus releasable. A releasable blob may have loose
locations pending GC, packed locations pending repack, or both;
`mixed_blobs_pending_maintenance` reports that overlap. The byte totals cover
every authoritative location across every store. These are future maintenance
candidates, not bytes reclaimed by pruning.

Path mutations are intentionally different. `POST /api/v1/path/move` and
`POST /api/v1/path/trash` resolve and mutate inside one store transaction, so
they do not accept `If-Match`. Use them for a one-shot instruction tied to the
path as it exists when the transaction runs. Use ID plus revision when an
agent previously inspected a particular node and wants lost-update protection.

For a reorganization that must not partially apply, send one bounded plan to
`POST /api/v1/batch/move` or use `docbank mv batch`. Each source is either an
absolute `source_path`, resolved inside the transaction, or a stable `node_id`
with the revision the agent inspected. All `destination_path` values are
exact final coordinates whose parents resolve in the planned final tree; an
existing directory does not invoke ordinary `mv`'s “move into” shorthand.
Docbank validates the complete final tree before changing it. This permits file and
directory swaps without temporary names. Require a receipt for every request
item, in the same order, and reconcile its stable node ID, prior path, final
path, and resulting revision. Any error means the entire plan was rejected.

## Inspect document provenance

Use the stable node ID to retrieve the immutable facts describing where a file
was ingested from:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/provenance?limit=100&offset=0"
```

Do not confuse `node.path`, Docbank's current virtual coordinate, with a fact's
`original_path`, which names an external source as it was observed by the
ingest. Branch on `active` when the workflow needs facts that have not been
superseded, but retain fact identities: a correction adds a successor and keeps
the superseded record addressable. Paginate using `total`, `limit`, and
`offset`. A trashed file is
still inspectable by stable ID and returns an empty live path.

Provenance is evidence, not ownership of the external source. Reading it does
not open or modify that source, and it does not make a content version a
retention root.

To record an origin learned after ingest, send the node revision returned by a
prior read:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "7"' \
  -H 'Content-Type: application/json' \
  --data '{"source_kind":"agent","source_description":"triage","original_path":"opaque://laptop/report.txt"}' \
  "$DOCBANK_URL/api/v1/nodes/42/provenance"
```

The response is `201` with the appended fact, resulting node revision, path,
and ETag. `original_path` remains opaque evidence. To correct an active
caller-supplied fact on the same node, include its `identity` as `supersedes`;
the earlier fact remains in history. Operational facts recorded by CLI or
watched-folder ingest cannot be superseded, because they keep re-ingest
idempotent. Add the newly learned origin alongside them instead.
If supplied, `original_mtime` must use canonical UTC RFC3339Nano, for example
`2026-08-26T12:00:00Z`; timestamps with a numeric offset are rejected.

## Create and ingest safely

For a one-shot instruction tied to an exact virtual coordinate, create the
directory by path. Its parent must already exist:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"path":"/receipts/2026"}' \
  "$DOCBANK_URL/api/v1/path/mkdir"
```

The parent resolves inside the mutation transaction. When the workflow has
already selected a particular stable parent identity, create beneath that ID
instead so a concurrent parent move does not change the intended owner:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"parent_id": 1, "name": "receipts", "kind": "dir"}' \
  "$DOCBANK_URL/api/v1/nodes"
```

A `409 exists` response is not automatically success: resolve the existing
name and verify that it is the directory the workflow intended.

`POST /api/v1/ingest` reads absolute paths on the daemon host and is restricted
to loopback callers. It is not a file-upload endpoint:

For a large tree, inventory the exact selection first. This request reads
filesystem metadata but does not open file content or mutate the vault:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Dropbox"],"include":["*.pdf"],"exclude":[".git",".Trash"]}' \
  "$DOCBANK_URL/api/v1/ingest/preflight"
```

Require `errors == 0` and `rejected.files == 0`, inspect every returned
finding, and retain the exact include and exclusion lists for ingest. Findings
and extension groups are bounded; their count and truncation fields say when the detailed
arrays are samples rather than complete lists. A non-UTF-8 filesystem entry is
an error with an escaped printable path and is never opened or imported.

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Downloads/receipt.pdf"],"dest":"/receipts","include":[],"exclude":[],"replace":true}' \
  "$DOCBANK_URL/api/v1/ingest"
```

Inspect `added`, `skipped`, `excluded`, and every item in `failed`. After fixing
a partial failure, repeat the same ingest. Files already accepted become
`skipped` instead of creating another copy.

Set `replace: true` on either ingest form when one destination file represents
a changing source. The daemon then:

1. Reads the destination node and revision before reading source bytes.
2. Adds a version when the content changes, keeping the node ID.
3. Skips equal hash and size without changing stored MIME type or history.

Directories, stale revisions, and concurrent creation of the exact destination
name fail that file. Replacement does not fall back to a suffix. Omit `replace`
to use ordinary collision suffixing.

Include and exclude values use Go's `path.Match` grammar on slash-separated
source-relative paths. Use `/` separators on every platform; backslashes are
rejected. A pattern without `/` matches basenames at any depth;
exclusions win, and include patterns do not prune directories. Invalid patterns
are rejected before traversal. Use bracket expressions such as
`report[[]1].txt` for literal metacharacters instead of backslash escaping.
Watched-inbox exclusions remain literal.

For streaming progress, send the same body to `POST /api/v1/ingest/stream`
with `Accept: application/x-ndjson`. Read every line through EOF. `progress`
events cover the metadata-only `scan` and content `ingest` stages. The one
terminal event is either `result` with the final report or `error`.

HTTP 200 means streaming started. Treat missing terminal events, malformed
events, or data after the terminal event as failure.

Cancellation or disconnection stops traversal and the active blob write.
Files already published remain in the vault and are skipped on a rerun.

Remote writers use a file-granular multipart request. Compute the expected
identity before sending bytes, and address the destination by stable directory
ID:

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

Clients must percent-encode a nontrivial `name` query value. The request has
exactly one part named `file`, and its multipart filename must equal `name`.
The hash and size headers describe that file payload; top-level
`Content-Digest` would instead describe the multipart envelope and is therefore
not the write precondition.

On `201`, require `status: "added"`; an idempotent retry returns `200` with
`status: "skipped"` and the same stable node. In both cases compare
`computed_hash` and `computed_size` with the locally calculated values, then
retain `node.id`, `node.revision`, and `node.blob_hash`. A
`digest_mismatch` or `size_mismatch` is a failed write with no new node/blob
authority. Upload many files as independent requests so each result is
unambiguous and independently retryable.

The receipt proves receive-time agreement. Use the revision-bound single-node
verification endpoint later when policy requires evidence about bytes currently
stored in the vault.

## Enroll permanent history only after an exact preview

Audit enrollment is irreversible. An agent must first preview one live
directory by path or stable node ID:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"path":"/taxes","agent_label":"records policy"}' \
  "$DOCBANK_URL/api/v1/audit/preview"
```

Present or evaluate the returned protected node/version counts, logical and
unique bytes, vault-wide evidence counts, and `baseline_digest`. First
activation permanently retains enrollment-time names, topology, tags,
assignments, ingests, and provenance across the vault, including outside the
selected scope. Unrelated content versions do not become scope members, but the
metadata snapshot remains evidence. Only after that review may a client execute
the exact daemon-held plan:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"preview_token":"<one-use-token>","acknowledge_permanent_retention":true}' \
  "$DOCBANK_URL/api/v1/audit/enable"
```

The token expires after ten minutes, is consumed by one attempt, and does not
survive daemon restart. On `audit_preview_stale`, preview again; never retry the
same execution blindly. `GET /api/v1/audit/status` reports vault-wide evidence.
Add either `?path=/taxes/file.pdf` or `?node_id=57` to inspect sticky membership.
Read that node's canonical timeline with exactly one selector:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/history?node_id=57&limit=50"
```

Events appear newest first. Each identifies its event, operation, scope, and
node revision, plus path or content identities when applicable. Path states
use `/live/paths`, `@trash/known/...`, or `@trash/unknown/...`. Tag and
provenance events include an `attachment` with `kind`, stable `identity`, and
typed `before`/`after` states.

Send `next_cursor` back unchanged to read older events. It belongs to that
node and stays valid when newer events arrive. Do not interpret its encoding.

`audit_not_enrolled` means the node is outside permanent retention.
`invalid_audit_cursor` is a request error. An enrolled node can have no events
if it has not changed since enrollment. Use status membership to determine
protection; an empty timeline is insufficient.

To answer “what changed anywhere in this protected scope?”, use the stable
scope ID returned by audit status:

```bash
curl --fail-with-body \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/scopes/<scope-id>/history?limit=50"
```

The page includes current scope evidence and events from every protected
member. Reconcile each event's `scope_id`, retain its `node_id` as the stable
subject, and follow `next_cursor` unchanged. Scope cursors are bound to that
scope and remain append-stable when newer events arrive.

Independently replay the authority and hash every protected blob with:

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/audit/verify"
```

A successful HTTP response can still report failed verification. Require empty
`metadata_problems` and `problems`, `verified_blobs == protected_blobs`, and a
non-null `evidence` object when `enabled` is true. Record that stable evidence
outside the vault when rollback detection matters: it contains vault and
allocation-lineage identities, allocation count/head, the operation high-water
mark, and every scope count/head. This endpoint hashes protected content only;
the top-level `/api/v1/verify` also covers unaudited blobs.

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

Require `evidence_check.extends: true` as well as clean metadata and blob
results. Equal chains and valid extensions pass. Stable problem codes
are `audit_not_enabled`, `vault_mismatch`, `lineage_mismatch`,
`allocation_shorter`, `allocation_diverged`, `scope_missing`, `scope_shorter`,
and `scope_diverged`. Evidence mismatch is reported in the body rather than as
an HTTP transport error so agents can inspect the current verified evidence and
byte state before escalating.

A vault can have several disjoint permanent directory scopes. Overlapping and
nested scopes are rejected. See
[Permanent Audited History](../usage/audited-history.md) for scope membership
and maintenance rules.

## Treat destructive maintenance as a two-step decision

Trash empty and GC are dry-run operations when `run` is false. An agent should
present or evaluate the report before issuing a separate execution request:

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

Only send `run: true` when policy explicitly authorizes permanent removal.
Treat the execution as a new decision: the vault may have changed since the
preview. Packed bytes reported as pending are logically dead but not yet
physically reclaimed.

`POST /api/v1/verify` validates logical metadata and audit history before
re-hashing every stored blob. It is read-only but can be expensive. Maintenance
requests serialize against mutations and may run without the ordinary request
timeout. A new mutation submitted after maintenance is running or queued gets
`503 maintenance_busy`; retry it after the operator-visible maintenance ends.
This is a transient refusal, not evidence that the requested mutation committed.
Treat either `metadata_problems` or blob `problems` as a failed verification.

Use `POST /api/v1/nodes/{id}/verify` when the decision concerns one inspected
file. Unlike the vault-wide operation it requires `If-Match`, stays bounded to
one blob, and returns the recorded and freshly computed identities directly.

`POST /api/v1/storage/pack` is explicit but non-destructive: it changes the
physical representation without changing document identity or blob read
authority. Use `GET /api/v1/storage` before and after when an operator needs an
auditable result. A positive `max_bytes` bounds raw-byte work softly—the blob
that crosses the budget is committed. `budget_exhausted: true` describes that
crossing, not whether eligible loose blobs remain; inspect storage status before
deciding to issue another request.

`POST /api/v1/storage/repack` physically retires empty packs and rewrites
eligible sparse packs without changing logical content authority. Its selection
thresholds are policy, not a preview guarantee: inspect storage status before
and after. `bytes_repacked` counts live raw bytes rewritten and must not be
reported as reclaimed disk space.

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

Branch on `code`, never `detail`. Useful policy groups:

- Re-read and reconsider: `stale_revision`.
- Correct the request: `validation`, `precondition_required`, `invalid_name`, `invalid_tag`,
  `not_dir`, `not_file`, `is_root`.
- Reconcile desired state: `exists`, `cycle`, `not_trashed`, `not_found`.
- Stop and surface credentials or topology: `unauthorized`, `loopback_only`.
- Retry after the active maintenance operation ends: `maintenance_busy`.
- Release external file locks, then run `storage pack` reconciliation:
  `pack_retirement_deferred`. The preceding repack catalog change already
  committed; never restore the retired mapping or assume rollback.
- Stop automation and preserve evidence: `internal`.

The complete mapping lives in [HTTP API](../architecture/http-api.md) and the
OpenAPI document.

## A safe filing loop

A robust inbox-filing agent follows this sequence:

1. Resolve `/inbox` and page through its children.
2. Read metadata or content for candidate files by ID.
3. Decide a destination; create missing directories deliberately.
4. Re-read the candidate if the decision took long enough for concurrent work
   to be plausible.
5. Move by ID with the revision the decision was based on.
6. On `412`, re-read and reconsider rather than replaying.
7. Record the returned ID, path, and revision as the outcome.

Keep planning and mutation separate in logs. Never log the API key, shutdown
token, or document content by default. Use request IDs from your own workflow
for correlation; docbank's stable node ID is the durable object identity.
