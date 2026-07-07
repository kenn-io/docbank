---
title: HTTP API
description: The agent-first HTTP API design — filesystem-shaped endpoints, revision preconditions, and batch reorganization.
---

# HTTP API

!!! info "Planned — Phase 2"
    This page is the design contract for `docbank serve`; none of it is
    implemented yet. It exists now because the API's ergonomics are a
    first-class requirement — the design test below is a commitment the
    implementation will be reviewed against, and the endpoint shapes
    here become the OpenAPI spec.

**Design test: an agent must be able to do everything the TUI can,
through the API alone.** Agents are not a secondary interface bolted
onto a human tool; browsing, retrieving, filing, editing, and
reorganizing the entire tree must work for a client that only speaks
HTTP.

## Shape

Served by `docbank serve`, following msgvault's daemon patterns:
[Huma v2](https://huma.rocks) with a typed OpenAPI contract from day
one, API-key auth, and a generated client. Endpoints are
filesystem-shaped:

| Endpoint | Purpose |
|----------|---------|
| `GET /nodes/{id}` · `GET /path/{path}` | stat — both return the node with its ID |
| `GET /nodes/{id}/children` | list a directory, paginated |
| `GET /nodes/{id}/content` | document bytes |
| `GET /nodes/{id}/versions` · `GET /nodes/{id}/versions/{n}/content` | edit history |
| `GET /search?q=…` | FTS + filters (tag, MIME, date, path prefix), paginated |
| `POST /nodes` | mkdir / multipart upload |
| `PUT /nodes/{id}/content` | replace content (versioned edit), requires `If-Match` |
| `PATCH /nodes/{id}` | rename / move / retag, requires `If-Match` |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover, requires `If-Match` |
| `POST /batch/move` | bulk reorganization with `dry_run` |
| `GET /tags` + tag CRUD | tag management |

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename can't strand a concurrent
client's reference.

## Concurrency: per-node revisions, `If-Match`

Every node carries a `revision` that bumps on each mutation (directories
bump when their contents change). All mutating endpoints require
`If-Match: <revision>`; a stale revision gets `412 Precondition Failed`,
telling the agent to re-read and retry.

The granularity is deliberate. A global tree ETag would invalidate every
agent's in-flight work whenever anything anywhere changed; per-node
revisions scope conflicts to actual contention. SQLite already
serializes the writes — preconditions exist to catch **lost updates
across an agent's read-modify-write turns**, not to lock. This is the
same mechanism that makes concurrent document *editing* safe
([Editing & Versions](editing-and-versions.md)).

## Batch reorganization

Agent-driven filing is the marquee use case: "reorganize these 400
inbox scans into the tree." Doing that one `PATCH` at a time is slow and
can strand a half-applied plan. `POST /batch/move` takes a list of
`{node_id, new_parent_id, new_name?}` operations and:

- **`dry_run: true`** validates the entire batch — collisions, cycles,
  missing IDs, stale revisions — and reports per-operation outcomes
  without applying anything. The agent iterates on its plan against
  reality before touching the tree.
- **Execution is all-or-nothing** in one transaction. A batch either
  fully applies or fully doesn't; there is no partially filed state to
  reason about.

## Error mapping

The store's typed errors map onto status codes so agents can branch on
semantics rather than parse messages:

| Store error | HTTP |
|-------------|------|
| `ErrNotFound` | 404 |
| `ErrExists` (name collision) | 409 |
| `ErrCycle` (move under own descendant) | 409 |
| `ErrNotDir` / `ErrNotFile` / `ErrInvalidName` | 422 |
| stale `If-Match` revision | 412 |

## Non-goals

- No server-side rendering or web UI (the API serves programs).
- No multi-user model: one vault, one key set. Sharing is out of scope
  for v1.
- No MCP server in Phase 2 — but the OpenAPI contract is designed to
  make wrapping one mechanical, post-v1.
