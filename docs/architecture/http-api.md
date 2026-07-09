---
title: HTTP API
description: The agent-first HTTP API — filesystem-shaped endpoints, revision preconditions, and the daemon's error contract.
---

# HTTP API

!!! info "Implemented, with exceptions"
    The endpoints below marked **Implemented** exist in `docbank serve`
    today and back the CLI's data commands — the CLI is an HTTP client
    of exactly this surface, with no other path into the vault. Rows
    under the "Planned" admonitions further down (versioned editing,
    tags, batch move, multipart upload) are designed but not built; see
    [Roadmap](../roadmap.md).

**Design test: an agent must be able to do everything the CLI can,
through the API alone.** Agents are not a secondary interface bolted
onto a human tool; browsing, retrieving, filing, and reorganizing the
tree must work for a client that only speaks HTTP — and the CLI itself
takes no shortcut, so this is enforced by construction rather than by
discipline.

## Shape

Served by `docbank serve`: [Huma v2](https://huma.rocks) with the
`humago` (stdlib `net/http`) adapter, a typed OpenAPI contract
(`docbank openapi`, or `GET /openapi.json` / `/openapi.yaml` / `/docs`
on a running daemon), and `X-Api-Key` / `Authorization: Bearer` auth.
Endpoints are filesystem-shaped, under `/api/v1`:

| Endpoint | Purpose | Status |
|----------|---------|--------|
| `GET /nodes/{id}` | stat by id (live or trashed) | Implemented |
| `GET /path?path=/a/b` | stat by virtual path | Implemented |
| `GET /nodes/{id}/children` | list a directory, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/content` | stream document bytes | Implemented |
| `GET /search?q=&limit=` | name search (FTS5), best rank first | Implemented |
| `POST /nodes` | create a directory (`kind: "dir"`) | Implemented |
| `POST /ingest` | import server-side paths — see [addendum](#addendum-post-ingest) | Implemented |
| `PATCH /nodes/{id}` | move and/or rename | Implemented |
| `POST /path/move` · `POST /path/trash` | move / trash by virtual path, resolved and mutated in one store transaction | Implemented |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover | Implemented |
| `GET /trash` · `POST /trash/empty` | list / hard-delete trash roots | Implemented |
| `POST /gc` `{run}` · `POST /verify` | reclaim unreachable blobs / re-hash all blobs | Implemented |

Root-level, outside `/api/v1` and auth-exempt: `GET /health`, `GET
/api/ping` (daemon discovery), `GET /docs` and the OpenAPI documents,
and `/` (the placeholder web UI, when `[web] enabled`). A hidden `POST
/api/daemon/shutdown` (token-gated, not in the OpenAPI document) backs
`docbank serve stop`.

!!! info "Planned"
    Not yet implemented: `PUT /nodes/{id}/content` (versioned edit) and
    `GET /nodes/{id}/versions` ([Editing & Versions](editing-and-versions.md));
    tags (`GET /tags` + CRUD, tag filters on search); `POST /batch/move`
    bulk reorganization with `dry_run`; multipart file upload (`POST
    /nodes` with `kind: "file"`) as the remote counterpart to `POST
    /ingest`.

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename can't strand a concurrent
client's reference.

### Path resolution: a query parameter, not a URL segment

`GET /path` takes the virtual path as `?path=/inbox/doc.pdf`, not a
catch-all URL segment (`/path/{path...}`). Stdlib-mux decoding of a
wildcard segment makes a route ambiguous for names containing
`/`-adjacent percent-encoding; a query parameter has one well-defined
encoding instead. The path must be absolute (leading `/`); `?path=/`
resolves the root. The server applies the store's existing NFC name
normalization and validation and returns `422` for an invalid path.

## Concurrency: per-node revisions, `If-Match`

Every node carries a `revision` that bumps on each mutation (directories
bump when their contents change). The granularity is deliberate: a
global tree ETag would invalidate every agent's in-flight work whenever
anything anywhere changed, while per-node revisions scope conflicts to
actual contention. SQLite already serializes the writes — preconditions
exist to catch **lost updates across an agent's read-modify-write
turns**, not to lock.

`If-Match` is required where a mutation targets one existing node that
the caller read in an earlier request; path mutations, bulk operations,
and maintenance are explicit exceptions:

| Endpoint | Precondition |
|----------|--------------|
| `PATCH /nodes/{id}` | required — target node's revision |
| `POST /nodes/{id}/trash` | required — target node's revision |
| `POST /nodes/{id}/restore` | required — target node's revision |
| `POST /path/move`, `POST /path/trash` | none — the path is resolved and mutated inside one store transaction, so there is no separate read for a revision to guard |
| `POST /nodes` (create dir) | none — creation has no prior revision; a name collision is `409` |
| `POST /ingest` | none — long-running bulk operation with per-path partial success; the destination directory may legitimately change while it runs |
| `POST /trash/empty`, `POST /gc`, `POST /verify` | none — vault-wide maintenance, serialized by the maintenance gate |

A stale revision gets `412 Precondition Failed`, telling the caller to
re-read and retry. A required `If-Match` that's missing gets `428
Precondition Required`. Both carry the problem-JSON error envelope
below explaining the rule.

## Addendum: `POST /ingest`

`POST /ingest` takes **server-side local paths** — `{paths: [...],
dest: "/inbox"}` — and returns an `IngestReport` (`added`, `skipped`,
per-path `failed` entries), backing `docbank add`. Paths must be
**absolute**: the long-lived daemon's working directory is meaningless,
so a relative path is rejected with `422`. The CLI resolves `docbank
add`'s arguments to absolute paths before calling, so the command-line
UX (relative paths, `cwd`-relative sources) is unchanged from Phase 1.
Collisions resolve by the same suffixing rules as Phase 1's import.

Because it grants "read any daemon-readable local path," `POST /ingest`
is checked per-request against `RemoteAddr` and **restricted to
loopback callers** regardless of bind address or API key — a
non-loopback client gets `403` (`loopback_only`) pointing at multipart
upload, which is the correct remote-ingest path once it exists.
Multipart upload itself is still planned.

!!! info "Planned"
    Ingest records no provenance today — where the bytes came from is
    forgotten once the report returns, and the source path is consumed,
    never stored as identity. Generic provenance fields
    (`source_kind` / `source_ref` / `source_meta`) and node lookup by
    content hash are planned so external applications can trace a node
    to its origin and deduplicate against the vault; see the
    [roadmap](../roadmap.md#phase-2b-features-designed).

## Maintenance gate

`gc --run`, `trash empty`, and `verify` need the vault quiescent while
they run — the same reachability-then-delete race described in
[Concurrency & Locking](locking.md). Rather than the daemon's exclusive
vault flock (held for the daemon's whole lifetime, not per-request), an
in-process `sync.RWMutex`-shaped gate serializes them against regular
mutations: ordinary mutating handlers (`PATCH`, trash, restore, create,
ingest) take the read side and may run concurrently with each other;
maintenance handlers take the write side. Requests queue rather than
fail, and maintenance routes are exempt from the per-request timeout
since `gc`/`verify` can legitimately run long on a large vault.

## Auth

`X-Api-Key` or `Authorization: Bearer <key>`, constant-time compared
against `[server] api_key`. An empty key is valid only for loopback
binds ("keyless = local-allow"); a non-loopback bind without a key
refuses to start at all (see [Configuration](../configuration.md)).
`/health`, `/api/ping`, `/docs`, the OpenAPI documents, and the web
placeholder at `/` are auth-exempt; everything else under `/api/v1`
requires the key whenever one is configured.

## Error mapping

Errors are RFC 7807 problem-JSON with one extension member, `code`, a
machine-readable string clients branch on instead of parsing `detail`:

```json
{
  "title": "Conflict",
  "status": 409,
  "detail": "node \"report.pdf\" already exists",
  "code": "exists"
}
```

| `code` | HTTP | Source |
|--------|------|--------|
| `not_found` | 404 | `store.ErrNotFound` |
| `exists` | 409 | `store.ErrExists` (name collision) |
| `cycle` | 409 | `store.ErrCycle` (move under own descendant) |
| `stale_revision` | 412 | `store.ErrStaleRevision` — `If-Match` didn't match the current revision |
| `not_dir` / `not_file` / `invalid_name` / `not_trashed` / `is_root` | 422 | `store.ErrNotDir` / `ErrNotFile` / `ErrInvalidName` / `ErrNotTrashed` / `ErrIsRoot` |
| `validation` | 400 or 422 | malformed request (bad `If-Match`, non-absolute `?path=` or ingest path, huma request validation) |
| `precondition_required` | 428 | required `If-Match` header missing |
| `loopback_only` | 403 | `POST /ingest` called by a non-loopback peer |
| `unauthorized` | 401 | missing or invalid API key; bad shutdown token |
| `internal` | 500 | unmapped error (still surfaced with a message — this is a single-user local daemon, not a hardened multi-tenant service) |

## Non-goals

- No server-side rendering or web UI beyond the static placeholder (the
  API serves programs; a real frontend is future work with its own
  design).
- No multi-user model: one vault, one key. Sharing is out of scope for
  v1.
- No MCP server yet — but the OpenAPI contract is designed to make
  wrapping one mechanical, post-v1.
- No remote-daemon mode: config is shaped so it can be added without
  breakage, but `[remote]` doesn't exist yet.
