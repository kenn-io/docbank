---
title: Agent Integration Guide
description: Connect an agent to docbank safely using its OpenAPI contract, authenticated HTTP API, revisions, and dry-run maintenance operations.
---

# Agent integration guide

Docbank is daemon-first: the CLI, agents, and scripts all use the same HTTP
API. An integration never opens `docbank.db` or the blob store directly.

## Choose the interface

Use the CLI for human-directed shell work and simple orchestration. Use HTTP
for structured agent workflows, pagination, machine-readable errors, and
revision-aware mutations.

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
curl --fail --get \
  -H "Authorization: Bearer $DOCBANK_API_KEY" \
  --data-urlencode 'path=/' \
  "$DOCBANK_URL/api/v1/path"
```

A node response includes stable `id`, mutable `revision`, `kind`, timestamps,
and—on single-node responses—its current `path`. IDs survive renames; paths are
for display and one-shot path operations.

## Read a tree without unbounded responses

Directory children are paginated and sorted with directories first, then by
name. Use `total`, `limit`, and `offset` until the complete page set is read:

```bash
curl --fail \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/nodes/1/children?limit=500&offset=0"
```

Search is bounded separately. Always inspect `truncated`; increase the limit
or refine the query rather than assuming the returned array is complete.

```bash
curl --fail --get \
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

## Use revisions for read-modify-write

ID-addressed move, trash, and restore operations require `If-Match`. The
revision belongs to the node state the agent evaluated:

```bash
# A prior GET returned id=42 and revision=7.
curl --fail -X PATCH \
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

Path mutations are intentionally different. `POST /api/v1/path/move` and
`POST /api/v1/path/trash` resolve and mutate inside one store transaction, so
they do not accept `If-Match`. Use them for a one-shot instruction tied to the
path as it exists when the transaction runs. Use ID plus revision when an
agent previously inspected a particular node and wants lost-update protection.

## Create and ingest safely

Create a directory under a known parent ID:

```bash
curl --fail -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"parent_id": 1, "name": "receipts", "kind": "dir"}' \
  "$DOCBANK_URL/api/v1/nodes"
```

A `409 exists` response is not automatically success: resolve the existing
name and verify that it is the directory the workflow intended.

`POST /api/v1/ingest` reads absolute paths on the daemon host and is restricted
to loopback callers. It is not a file-upload endpoint:

```bash
curl --fail -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"paths":["/Users/me/Downloads/receipt.pdf"],"dest":"/receipts"}' \
  "$DOCBANK_URL/api/v1/ingest"
```

Inspect `added`, `skipped`, and every member of `failed`. Repeating the same
ingest after fixing a partial failure is safe; successful content converges to
`skipped` rather than another copy.

!!! info "Planned — remote upload"
    Multipart upload is not implemented. A client on another machine cannot
    send document bytes through the API yet; `POST /ingest` names paths local
    to the daemon host.

## Treat destructive maintenance as a two-step decision

Trash empty and GC are dry-run operations when `run` is false. An agent should
present or evaluate the report before issuing a separate execution request:

```bash
curl --fail -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"older_than":"30d","run":false}' \
  "$DOCBANK_URL/api/v1/trash/empty"

curl --fail -X POST \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"run":false}' \
  "$DOCBANK_URL/api/v1/gc"
```

Only send `run: true` when policy explicitly authorizes permanent removal.
Treat the execution as a new decision: the vault may have changed since the
preview. Packed bytes reported as pending are logically dead but not yet
physically reclaimed.

`POST /api/v1/verify` is read-only with respect to archive content but can be
expensive. Maintenance requests serialize against mutations and may run
without the ordinary request timeout; agents should expose progress as
“waiting” rather than assuming a queued request is hung.

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
- Correct the request: `validation`, `precondition_required`, `invalid_name`,
  `not_dir`, `not_file`, `is_root`.
- Reconcile desired state: `exists`, `cycle`, `not_trashed`, `not_found`.
- Stop and surface credentials or topology: `unauthorized`, `loopback_only`.
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
