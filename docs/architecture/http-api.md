---
title: HTTP API
description: The agent-first HTTP API — filesystem-shaped endpoints, revision preconditions, and the daemon's error contract.
---

# HTTP API

!!! info "Implemented, with exceptions"
    The endpoints below marked **Implemented** exist in `docbank daemon run`
    today and back the CLI's data commands — the CLI is an HTTP client
    of exactly this surface, with no other path into the vault. Rows
    under the "Planned" admonitions further down (reversion,
    tags, batch move) are designed but not built; see
    [Roadmap](../roadmap.md).

**Design test: an agent must be able to do everything the CLI can,
through the API alone.** Agents are not a secondary interface bolted
onto a human tool; browsing, retrieving, filing, and reorganizing the
tree must work for a client that only speaks HTTP — and the CLI itself
takes no shortcut, so this is enforced by construction rather than by
discipline.

## Shape

Served by `docbank daemon run`: [Huma v2](https://huma.rocks) with the
`humago` (stdlib `net/http`) adapter, a typed OpenAPI contract
(`docbank openapi`, or `GET /openapi.json` / `/openapi.yaml` / `/docs`
on a running daemon), and `X-Api-Key` / `Authorization: Bearer` auth.
Endpoints are filesystem-shaped, under `/api/v1`:

| Endpoint | Purpose | Status |
|----------|---------|--------|
| `GET /nodes/{id}` | stat by id (live or trashed) | Implemented |
| `GET /path?path=/a/b` | stat by virtual path | Implemented |
| `GET /nodes/{id}/children` | list a directory, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/content` | stream document bytes with catalog identity and a computed digest trailer | Implemented |
| `PUT /nodes/{id}/content` | replace raw content under revision, size, and digest preconditions — see [addendum](#addendum-put-nodesidcontent) | Implemented |
| `GET /nodes/{id}/versions` | list immutable content versions newest-first, paginated (`limit`/`offset`) | Implemented |
| `GET /versions/{version_id}` · `GET /versions/{version_id}/content` | inspect or stream one immutable version by stable UUID | Implemented |
| `POST /nodes/{id}/verify` | re-hash one file, bound to an inspected node revision | Implemented |
| `GET /search?q=&limit=` | bounded name search (FTS5), with explicit `truncated` status | Implemented |
| `POST /nodes` | create a directory (`kind: "dir"`) | Implemented |
| `POST /ingest` · `POST /ingest/stream` · `POST /ingest/preflight` | import with JSON or streamed progress / inventory server-side paths — see [addendum](#addendum-post-ingest-post-ingeststream-and-post-ingestpreflight) | Implemented |
| `POST /uploads?parent_id=&name=` | stream one digest-checked remote file — see [addendum](#addendum-post-uploads) | Implemented |
| `PATCH /nodes/{id}` | move and/or rename | Implemented |
| `POST /path/move` · `POST /path/trash` | move / trash by virtual path, resolved and mutated in one store transaction | Implemented |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover | Implemented |
| `GET /trash` · `POST /trash/empty` `{run, older_than}` | list / report or hard-delete trash roots | Implemented |
| `POST /gc` `{run}` · `POST /verify` | reclaim unreachable blobs / re-hash all blobs | Implemented |
| `GET /storage` · `POST /storage/pack` · `POST /storage/repack` | inspect usage / pack loose blobs / compact sparse packs | Implemented |
| `GET /jobs` | inspect daemon-owned background tasks and terminal failures | Implemented |
| `POST /backup/init` · `POST /backup/snapshots` · `POST /backup/snapshots/stream` · `GET /backup/snapshots` | initialize a repository / create with JSON or streamed progress / list snapshots | Implemented |

Root-level, outside `/api/v1` and auth-exempt: `GET /health`, `GET
/api/ping` (daemon discovery), `GET /docs` and the OpenAPI documents,
and `/` (the placeholder web UI, when `[web] enabled`). A hidden `POST
/api/daemon/shutdown` (not in the OpenAPI document) backs `docbank
daemon stop`; it isn't auth-exempt, so it requires both the API key and
its own shutdown token.

!!! info "Planned"
    Not yet implemented: revert, interactive edit, and version pruning
    ([Editing & Versions](editing-and-versions.md));
    tags (`GET /tags` + CRUD, tag filters on search), whose IDs are opaque
    non-reusable UUIDv4 values independent of mutable names; and
    `POST /batch/move` bulk reorganization with `dry_run`.

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename can't strand a concurrent
client's reference.

### Backup repository endpoints

`POST /backup/init` accepts `{"repo": "/absolute/server/path"}` and returns
the repository identity and canonical path. `repo` may be omitted when
`[backup] repo` is configured. `POST /backup/snapshots` accepts the same
optional repository plus `tag`, `jobs`, and `force_unlock`; it returns a
stable logical summary rather than exposing Kit's physical manifest layout.
The `/backup/snapshots/stream` variant accepts the same body and returns
`application/x-ndjson`: zero or more `progress` events followed by exactly one
terminal `result` or `error` event. Progress data carries stage, item counts,
byte counts, and a final-stage marker, so clients can render bars without
parsing human text. Because response headers commit when streaming begins, an
HTTP 200 means only that the stream started; clients must read through EOF and
require the terminal event. The CLI uses this variant for human output and the
single-JSON endpoint for `--json`.
`GET /backup/snapshots?repo=...` returns `{items: [...]}` so pagination can be
added later without changing a top-level array contract.

Explicit repository paths are server filesystem paths and must be absolute.
The CLI resolves a relative `--repo` against its own working directory before
sending it. API clients over an SSH tunnel must reason about the daemon host's
filesystem, not the caller's. Every endpoint requires the daemon API key.

### Background-job status

`GET /jobs` returns `{items: [...]}` in stable job-name order. Each item carries
`name`, `status` (`running`, `completed`, `failed`, or `cancelled`), and a UTC
`started_at`; terminal jobs add `finished_at`, and failures add a bounded
`error`. Records describe this daemon run only and disappear when it restarts.
The endpoint is observation, not control: stopping a task requires stopping or
reconfiguring the daemon feature that owns it.

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
| `PUT /nodes/{id}/content` | required — prevents a replacement from overwriting a head the caller did not inspect |
| `POST /nodes/{id}/trash` | required — target node's revision |
| `POST /nodes/{id}/restore` | required — target node's revision |
| `POST /nodes/{id}/verify` | required — binds the evidence to the exact node state the caller inspected |
| `POST /path/move`, `POST /path/trash` | none — the path is resolved and mutated inside one store transaction, so there is no separate read for a revision to guard |
| `POST /nodes` (create dir) | none — creation has no prior revision; a name collision is `409` |
| `POST /ingest` · `POST /ingest/stream` | none — long-running bulk operations with per-path partial success; the destination directory may legitimately change while they run |
| `POST /uploads` | none — creates or idempotently resolves one file under the stable `parent_id`; name/content collision policy is transactional |
| `POST /trash/empty`, `POST /gc`, `POST /verify` | none — vault-wide maintenance, serialized by the maintenance gate |
| `POST /backup/snapshots`, `POST /backup/snapshots/stream` | none — mutations pause only while pinning one logical snapshot; a preservation lease queues maintenance for the full capture, and the repository has its own exclusive lock |

A stale revision gets `412 Precondition Failed`, telling the caller to
re-read and retry. A required `If-Match` that's missing gets `428
Precondition Required`. Both carry the problem-JSON error envelope
below explaining the rule.

## Content identity and verification evidence

Every file-node representation includes a stable `current_version_id` plus
`blob_hash`, docbank's canonical lowercase SHA-256 content identity, and raw
`size`. Directories omit content identity. Node and version IDs are stable
across moves and renames. Content replacement retains the node ID, creates an
immutable version, and changes its current pointer, hash, media type, and
revision.

`GET /nodes/{id}/content` exposes the catalog identity before streaming in
`X-Docbank-Content-Version`, `X-Docbank-Blob-Hash`, and
`X-Docbank-Blob-Size`. It then hashes the bytes while they pass through the
response and emits the result as the
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) `Content-Digest`
trailer. The response deliberately omits standard
`Content-Length`: HTTP/1.1 cannot carry a trailer on a fixed-length message,
and pre-reading a large loose or packed blob solely to populate a header would
double physical I/O. Clients that need independent transfer proof hash the
body themselves and compare both their digest and the trailer with the node's
`blob_hash`; the version header must equal `current_version_id`.

`GET /nodes/{id}/versions` returns a bounded, newest-first page with `items`,
`total`, `limit`, and `offset`. `GET /versions/{version_id}` resolves immutable
metadata globally, and its `/content` child streams that version with the same
identity headers and digest contract. A path rename cannot strand a retained
version reference.

`POST /nodes/{id}/verify` is the bounded server-side proof. It requires
`If-Match` from a prior node response, reopens the blob through the same mixed
loose/packed store used for downloads, and returns the recorded and computed
version ID, hashes, and sizes. Missing, corrupt, and unreadable content are successful
reports with `verified: false` and a `problem`; transport, validation, and
stale-node failures remain non-2xx responses. The route checks the revision
again after reading, so a concurrent rename, trash, or content replacement
yields `412` instead of ambiguous evidence.

The single-node route is exempt from the ordinary request timeout. It is
bounded in scope, not necessarily short in duration: hashing one very large
blob may legitimately take longer than a minute.

These are integrity receipts from the authenticated daemon, not
non-repudiable attestations against a malicious server. Signed receipts or a
transparency log are outside docbank's current trust model.

## Addendum: `POST /ingest`, `POST /ingest/stream`, and `POST /ingest/preflight`

`POST /ingest/preflight` takes `{paths: [...], exclude: [...]}` and performs a
metadata-only source inventory. It opens no regular-file content and writes no
vault metadata or blobs. Its report includes file/directory/logical-byte totals,
pack-eligible, loose-only, and rejected size classes, exclusion/skip/error
counts, bounded findings, and extension summaries. The route uses the same
absolute-path validation, explicit root-directory-symlink behavior, exclusion
rules, loopback fence, and timeout exemption as the real ingest. Findings are
observations rather than a snapshot lock: sources can still change before
ingest, and metadata-only scanning cannot prove later content readability.

`POST /ingest` takes **server-side local paths** — `{paths: [...],
dest: "/inbox", exclude: [...]}` — and returns an `IngestReport` (`added`, `skipped`, `excluded`,
per-path `failed` entries), backing `docbank add`. Paths must be
**absolute**: the long-lived daemon's working directory is meaningless,
so a relative path is rejected with `422`. The CLI resolves `docbank
add`'s arguments to absolute paths before calling, so the command-line
UX (relative paths, `cwd`-relative sources) is unchanged from Phase 1.
Collisions resolve by the same suffixing rules as Phase 1's import.

`POST /ingest/stream` accepts the same body and returns
`application/x-ndjson`. A metadata-only `scan` stage establishes advisory file
and byte totals, followed by `ingest` progress for bytes read and file outcomes.
Exactly one `result` carrying `IngestReport` or `error` terminates the stream;
HTTP 200 alone is not success. A write failure or client disconnect cancels the
request context used by traversal, blob writing, and metadata transactions.
Already completed files remain valid and converge on retry, while an
incomplete blob never receives node authority.

Because they grant "read any daemon-readable local path," `POST /ingest` and
`POST /ingest/stream` are checked per-request against `RemoteAddr` and
**restricted to loopback callers** regardless of bind address or API key — a
non-loopback client gets `403` (`loopback_only`). There is no remote file-upload
capability on this route: remote bytes use `POST /uploads`, while remote access
to the loopback-bound daemon still terminates through the configured SSH/VPN
tunnel.

Each exclusion is either a bare entry name, matched at any depth, or a relative
path containing `/`, matched within every supplied source. Matching a directory
prunes its subtree. The preflight and ingest implementations share this matcher
so reviewed selection and actual selection cannot drift.

## Addendum: `POST /uploads`

`POST /uploads?parent_id=<id>&name=<filename>` accepts exactly one
`multipart/form-data` file field named `file`. The query uses a stable
destination directory ID rather than a mutable path. The multipart filename
must equal the normalized `name` query value, preventing the envelope and
requested tree entry from describing different files.

Two request headers declare the expected identity of the **file part**, not the
multipart envelope:

- `X-Docbank-Blob-Hash`: canonical lowercase hexadecimal SHA-256;
- `X-Docbank-Blob-Size`: raw byte length, bounded by docbank's explicit 4 GiB
  format-v1 backup ceiling.

The server streams the file once through Kit's durable writer and independently
computes both values. Only after they match, the closing multipart boundary has
been validated, and no extra parts remain does one metadata transaction grant
blob authority and create the node. `201` with `status: "added"` identifies a
new node and its initial `content_create` version. Repeating the same name, hash,
and parent converges to that stable node
with `200` and `status: "skipped"`. The receipt always includes the server's
`computed_hash`, `computed_size`, and the node's ID and revision; clients compare
the values themselves.

Uploads are intentionally file-granular. A caller sending many files issues
independent requests (concurrently when useful), so one failure never makes the
success of another file ambiguous and each item can be retried on its own.
Different content under the same requested name follows normal ingest suffixing
rather than overwriting an existing document.

Objects through 64 MiB are eligible for packing. Larger accepted objects remain
loose, but use the same content-hash authority, verified streaming, backup, and
restore contracts. The 4 GiB admission limit therefore does not imply that a
single object will be moved into a pack.

Digest or size disagreement returns `422 digest_mismatch` or
`422 size_mismatch` and grants no new `blobs` row or node authority. Because
physical bytes are published before metadata by design, a rejected stream may
leave an authority-free loose object; the normal GC untracked-file scan removes
it. Malformed envelopes and extra parts are also rejected before authority.
Request bodies are capped at the declared size plus bounded multipart overhead,
and the route is exempt from the ordinary timeout so a legitimate large upload
is governed by client cancellation rather than a one-minute deadline.

## Addendum: `PUT /nodes/{id}/content`

Content replacement accepts raw bytes rather than multipart. A caller first
reads the file node and sends its revision in `If-Match`, then declares the raw
body's canonical SHA-256 and byte count in `X-Docbank-Blob-Hash` and
`X-Docbank-Blob-Size`. `Content-Type` is normalized and stored on the new
version; an omitted value becomes `application/octet-stream`.

The daemon streams the body into durable authority-free storage and computes
the identity independently. Only exact agreement permits one metadata
transaction to create a `content_replace` version, advance the node's current
pointer, and bump its revision. The old head remains an immutable GC root. A
successful response includes the resulting ETag plus a receipt containing the
node, new version, `computed_hash`, and `computed_size`; clients compare every
field with the request before accepting success.

Clients should send `Expect: 100-continue` for large writes. The daemon checks
the target kind and revision before its first body read, then repeats those
checks in the committing metadata transaction. The early check avoids wasting
bandwidth; only the transactional check grants authority.

Stale revisions return `412 stale_revision`; missing preconditions return
`428 precondition_required`; identity disagreements return
`422 digest_mismatch` or `422 size_mismatch`. A failed operation grants no new
catalog authority, although a completely written loose object can remain
authority-free until GC. Because the body may be binary, this route is an
explicit exception to the raw JSON text validator; its byte count and digest
are the lossless boundary instead. Cancellation propagates through physical
writing and prevents the metadata transaction.

!!! info "Planned"
    Ingest provenance today is filesystem-shaped: each import records
    the source's original path and mtime in the store's `provenance`
    table (the path is a record, never identity). Generalizing it —
    `source_kind` / `source_ref` / `source_meta` fields for non-file
    origins — and node lookup by content hash are planned so external
    applications can trace a node to its origin and deduplicate against
    the vault; see the
    [roadmap](../roadmap.md#phase-2b-features-designed).

## Maintenance gate

`gc --run`, `trash empty`, and `verify` need the vault quiescent while
they run — the same reachability-then-delete race described in
[Concurrency & Locking](locking.md). Rather than the daemon's exclusive
vault lock (held for the daemon's whole lifetime, not per-request), an
in-process `sync.RWMutex`-shaped gate serializes them against regular
mutations: ordinary mutating handlers (`PATCH`, trash, restore, create,
ingest) take the read side and may run concurrently with each other;
maintenance handlers take the write side. Requests queue rather than
fail, and maintenance routes are exempt from the per-request timeout
since `gc`/`verify` can legitimately run long on a large vault.

Backup creation uses the mutation-exclusive side only for Kit's freeze window.
Once Docbank's deferred read transaction is pinned, ordinary mutations continue
into SQLite's WAL while verified metadata and blob streams are captured. The
backup retains a separate shared preservation lease until capture ends;
maintenance takes that lease exclusively, so GC cannot delete a loose blob or
catalog mapping still referenced by the pinned snapshot. The create route is
timeout-exempt; cancellation still propagates through Kit and prevents
publication of a snapshot manifest.

## Auth

`X-Api-Key` or `Authorization: Bearer <key>`, constant-time compared
against the daemon's effective key. The daemon always has one: with
`[server] api_key` unset it generates a fresh key at startup and
publishes it, inside the owner-private `$DOCBANK_HOME`, through the same runtime
record the CLI already uses for discovery — readable only by the
vault's owner, never sent over the network unencrypted, never logged.
Binds are loopback-only: the API is plain HTTP, so a non-loopback bind
would expose the key and vault contents in cleartext, and `docbank
daemon run` refuses to start on one — remote access goes through an SSH
tunnel or VPN (see [Configuration](../configuration.md)). `/health`,
`/api/ping`, `/docs`, the OpenAPI documents, and the web placeholder at
`/` are auth-exempt; everything else, including the shutdown route,
requires the key.

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
| `validation` | 400, 415, or 422 | malformed request (bad `If-Match`, paths, media type, multipart envelope, or generated validation) |
| `precondition_required` | 428 | required `If-Match` header missing |
| `loopback_only` | 403 | server-path ingest or preflight called by a non-loopback peer |
| `digest_mismatch` / `size_mismatch` | 422 | uploaded file bytes disagree with the required declaration; no node/blob authority committed |
| `too_large` | 413 | upload exceeded its declared size plus bounded multipart overhead |
| `pack_retirement_deferred` | 503 | repack authority committed but an old source pack remains physically locked; release the lock, then run `storage pack` reconciliation |
| `unauthorized` | 401 | missing or invalid API key; bad shutdown token |
| `internal` | 500 | unmapped error (still surfaced with a message — this is a single-user local daemon, not a hardened multi-tenant service) |

!!! info "Planned — audited-history API"
    One bounded, cursor-paginated model will expose audit scope status, sticky
    membership, canonical mutation events, content versions, comparison
    metadata, and chain verification to CLI, agent, TUI, and web clients. Status
    and terminal verification responses will expose one rollback-evidence
    bundle: stable vault ID, every scope count/head, and allocation-lineage
    count/head. Expected-state verification checks all of those fields, including
    lineage advances caused only by unaudited operations. Mutation events expose
    a unique per-transaction `operation_id` and an optional `grouping_id` for a
    command or job spanning several transactions; grouping never implies atomic
    commit. Scope preview returns a short-lived token bound to the baseline and
    vault preview generation plus `vault_metadata_retention`: whether the
    vault-wide lineage is newly activated or already active, genesis record
    counts by kind, estimated serialized bytes, the retained metadata classes,
    and `unaudited_content_retained: false`. Enablement requires both that token
    and `acknowledge_vault_metadata_retention: true`; omitting or falsifying the
    acknowledgment is a validation error, never implicit consent.
    `audit_not_enrolled` (409) means
    a requested node timeline has no audit authority. `audit_protected` (409)
    refuses an incompatible mutation and carries a `blocking_scopes` array;
    overlapping scopes are never collapsed to one. `audit_preview_stale` (409)
    means the one-use preview token expired, the daemon restarted, another
    execution consumed it, or authoritative state advanced. Credentialed
    clients will not receive an ordinary audit-destruction operation. See
    [Audited History](audited-history.md).

## Non-goals

- No server-side rendering. The current root is a static placeholder; the
  planned kit-ui portal remains an ordinary authenticated API client and gains
  no privileged data path.
- No multi-user model: one vault, one key. Sharing is out of scope for
  v1.
- No MCP server.
- No remote-daemon mode or `[remote]` configuration.
