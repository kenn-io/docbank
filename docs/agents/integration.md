---
title: Agent Integration Guide
description: Connect an agent to docbank safely using its OpenAPI contract, authenticated HTTP API, revisions, and dry-run maintenance operations.
---

# Agent integration guide

Standalone Docbank is daemon-first: the CLI, external agents, and scripts all
use the same HTTP API. An external integration never opens `docbank.db` or the
blob store directly. Go applications that own a separately rooted archive can
instead use the [embedded API](../embedding.md).

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

`/health` is intentionally auth-exempt:

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

A node response includes stable `id`, mutable `revision`, `kind`, and
timestamps. File nodes also include stable `current_version_id`, immutable
SHA-256 `blob_hash`, and raw `size`; directories omit content identity. Live
single-node responses include the current `path`. Node and version IDs survive
renames; use a live path only for display or a one-shot path operation.

The CLI exposes the same distinction without requiring JSON parsing. Human
listings print copyable selectors such as `id:42`, and existing-node commands
accept either that stable selector or an absolute path:

```bash
docbank cat id:42
docbank versions list id:42 --json
docbank mv id:42 /review/approved.pdf --json
```

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

Search is bounded separately. Always inspect `truncated`; increase the limit
or refine the query rather than assuming the returned array is complete.
Each result's `match` is `name` or `content`. Name matches keep their established
ranking and always precede content-only matches, so adding content indexing does
not reorder an agent's filename-based workflow. Content search covers only the
current version of verified UTF-8 plain text, Markdown, JSON, and JSONL documents
up to 16 MiB. Extraction runs asynchronously; after a write, inspect
`docbank jobs` or retry briefly instead of treating an immediate content miss as
permanent. PDF, Office, image, and OCR text extraction are not implemented.

```bash
curl --fail-with-body --get \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  --data-urlencode 'q=tax return' \
  --data 'limit=100' \
  "$DOCBANK_URL/api/v1/search"
```

Retrieve file bytes by ID, not path:

```bash
curl --fail \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/42/content" \
  --output document.bin
```

The content response sends `X-Docbank-Content-Version`,
`X-Docbank-Blob-Hash`, and `X-Docbank-Blob-Size` before the body. After the body
it sends an
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) `Content-Digest`
trailer computed from the bytes actually streamed. A client
that needs independent transfer proof hashes `document.bin` itself and compares
that digest, the trailer, and the file node's `blob_hash`. Require the version
header to equal the node's `current_version_id`; do not treat catalog headers
alone as a fresh physical verification.

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

Resolve known bytes to every authoritative node/version reference without
guessing from paths or physical storage:

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

Read the response through successful EOF and require the trailer. A readable
prefix is not verified content: if the request is cancelled, the body ends in
error, or the trailer is absent, discard any staged output rather than
publishing it. Docbank does not drain an abandoned response merely to complete
verification.

For a bounded server-side check, send the revision from the node response:

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

Create a tag once, then retain its returned UUID and revision/ETag. Names are
mutable display labels; IDs are durable authority. A tag revision covers both
its definition and complete assignment set.

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
the tag's current revision and assignment count. `changed: false` is a successful
idempotent convergence, not an error. Page `GET /nodes/{id}/tags` and `GET
/tags/{tag_id}/nodes`; the latter includes trashed nodes without pretending
they have a live path. Rename and delete by UUID with the most recently
inspected tag ETag in `If-Match`; a concurrent definition or assignment change
returns `412 stale_revision`. Deleting a tag removes assignments only, never
nodes or document bytes.

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

The response is NDJSON. Each `progress` line contains `stage`, `done`, `total`,
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

Do not accept HTTP 200 alone. Require the receipt's `computed_hash` and
`computed_size` to equal the local values; require its node and version to both
name node 42; require `content_replace`, the next node revision, matching blob
identity, and `node.current_version_id == version.id`; and require the response
ETag to encode that resulting revision. The old version remains addressable.
A `412` means the decision is stale—re-read and decide again rather than
blindly retrying with a fresh revision.

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

Require `source_version.id` to equal the requested ID and all three records to
name node 42. Require the new version to be `content_revert`, to name that source,
and to reproduce its hash, size, and media type exactly. The node must install
the new version at revision 9 and the ETag must agree. Reversion is metadata-only,
so it has no computed digest receipt; it relies on already-authoritative source
bytes and does not copy them. Use the ordinary content verification surface when
a fresh physical proof is part of the workflow.

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
partition into shared versus releasable, and releasable into loose pending GC
versus packed pending repack. These are future maintenance candidates, not
bytes reclaimed by pruning.

Path mutations are intentionally different. `POST /api/v1/path/move` and
`POST /api/v1/path/trash` resolve and mutate inside one store transaction, so
they do not accept `If-Match`. Use them for a one-shot instruction tied to the
path as it exists when the transaction runs. Use ID plus revision when an
agent previously inspected a particular node and wants lost-update protection.

## Create and ingest safely

Create a directory under a known parent ID:

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
  --data '{"paths":["/Users/me/Dropbox"],"exclude":[".git",".Trash"]}' \
  "$DOCBANK_URL/api/v1/ingest/preflight"
```

Require `errors == 0` and `rejected.files == 0`, inspect every returned
finding, and retain the exact exclusion list for ingest. Findings and extension
groups are bounded; their count and truncation fields say when the detailed
arrays are samples rather than complete lists. A non-UTF-8 filesystem entry is
an error with an escaped printable path and is never opened or imported.

```bash
curl --fail-with-body -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Downloads/receipt.pdf"],"dest":"/receipts","exclude":[]}' \
  "$DOCBANK_URL/api/v1/ingest"
```

Inspect `added`, `skipped`, `excluded`, and every member of `failed`. Repeating the same
ingest after fixing a partial failure is safe; successful content converges to
`skipped` rather than another copy.

For an interactive or long-running local integration, send the same body to
`POST /api/v1/ingest/stream` with `Accept: application/x-ndjson`. Read every
line through EOF. `progress` events cover the metadata-only `scan` and content
`ingest` stages; a `result` carrying the final report or an `error` is the
single terminal event. HTTP 200 means only that streaming started. Treat EOF
without a terminal event, malformed events, or data after the terminal event
as failure. Cancelling or disconnecting cancels traversal and the active blob
write; only files whose individual publication already completed retain
authority and are safely reported as skipped on a rerun.

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

Events are newest first and bind stable event, operation, scope, node-revision,
and optional path/content identities. A path state distinguishes `/live/paths`
from canonical `@trash/known/...` and `@trash/unknown/...` coordinates. Tag and
provenance events include an `attachment` object with a discriminating `kind`,
stable `identity`, and typed `before`/`after` states, so clients do not need to
decode canonical audit internals. Follow `next_cursor` to read older events;
send it back unchanged and never derive meaning from its encoding. The cursor
is node-bound and remains stable when newer operations append. Treat
`audit_not_enrolled` as a valid answer that the node is outside permanent
retention, and `invalid_audit_cursor` as a request error. A protected node may
have an empty timeline when it was adopted at enrollment and has not changed;
use status membership, not event count, to decide protection.

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

The current public boundary permits one permanent scope per vault.

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
timeout; agents should expose progress as “waiting” rather than assuming a
queued request is hung. Treat either `metadata_problems` or blob `problems` as a
failed verification.

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
