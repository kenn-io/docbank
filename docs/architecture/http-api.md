---
title: HTTP API
description: The agent-first HTTP API — filesystem-shaped endpoints, revision preconditions, and the daemon's error contract.
---

# HTTP API

The endpoints below exist in `docbank daemon run` and back the CLI's data
commands — the CLI is an HTTP client of exactly this surface, with no other
path into the vault.

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
| `POST /nodes/{id}/revert` | create a new head from a prior version of the same file | Implemented |
| `GET /nodes/{id}/versions` | list immutable content versions newest-first, paginated (`limit`/`offset`) | Implemented |
| `GET /versions/{version_id}` · `GET /versions/{version_id}/content` | inspect or stream one immutable version by stable UUID | Implemented |
| `GET /content-references?sha256=&limit=&offset=` | find every stable node/version pair retaining a content hash | Implemented |
| `GET\|POST /tags` · `GET /tags/by-name` · `GET\|PATCH\|DELETE /tags/{tag_id}` | list, resolve, create, rename, or delete stable tag definitions | Implemented |
| `GET /nodes/{id}/tags` · `GET /tags/{tag_id}/nodes` · `PUT\|DELETE /nodes/{id}/tags/{tag_id}` · `PUT\|DELETE /path/tags/{tag_id}` | inspect and change tag assignments | Implemented |
| `POST /audit/preview` · `POST /audit/enable` · `GET /audit/status` | review permanent first-scope retention, enable the exact reviewed plan, and inspect authority or membership | Implemented |
| `GET /audit/history?path=&node_id=&limit=&cursor=` | read one audited node's canonical newest-first event timeline with a stable continuation cursor | Implemented |
| `GET /audit/scopes/{scope_id}/history?limit=&cursor=` | read canonical newest-first events across every member of one permanent scope | Implemented |
| `POST /audit/verify` | independently replay audit authority, optionally prove recorded evidence is an exact prefix, and re-hash every protected blob | Implemented |
| `POST /nodes/{id}/verify` | re-hash one file, bound to an inspected node revision | Implemented |
| `GET /search?q=&limit=` | bounded name and extracted-content search (FTS5), with match source and explicit `truncated` status | Implemented |
| `POST /nodes` | create a directory (`kind: "dir"`) | Implemented |
| `POST /ingest` · `POST /ingest/stream` · `POST /ingest/preflight` | import with JSON or streamed progress / inventory server-side paths — see [addendum](#addendum-post-ingest-post-ingeststream-and-post-ingestpreflight) | Implemented |
| `POST /uploads?parent_id=&name=` | stream one digest-checked remote file — see [addendum](#addendum-post-uploads) | Implemented |
| `PATCH /nodes/{id}` | move and/or rename, including resolving an absolute `dest_path` transactionally | Implemented |
| `POST /path/move` · `POST /path/trash` | move / trash by virtual path, resolved and mutated in one store transaction | Implemented |
| `POST /batch/move` | validate and apply up to 1,000 moves as one final-state transaction | Implemented |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover | Implemented |
| `GET /trash` · `POST /trash/empty` `{run, older_than}` | list / report or hard-delete trash roots | Implemented |
| `POST /gc` `{run}` · `POST /verify` | reclaim unreachable blobs / validate metadata and re-hash all blobs | Implemented |
| `GET /storage` · `POST /storage/pack` · `POST /storage/repack` | inspect usage / pack loose blobs / compact sparse packs | Implemented |
| `GET /jobs` | inspect daemon-owned background tasks and terminal failures | Implemented |
| `POST /backup/init` · `POST /backup/snapshots` · `POST /backup/snapshots/stream` · `GET /backup/snapshots` | initialize a repository / create with JSON or streamed progress / list snapshots | Implemented |

Root-level, outside `/api/v1` and auth-exempt: `GET /health`, `GET
/api/ping` (daemon discovery), `GET /docs` and the OpenAPI documents,
and `/` (the placeholder web UI, when `[web] enabled`). A hidden `POST
/api/daemon/shutdown` (not in the OpenAPI document) backs `docbank
daemon stop`; it isn't auth-exempt, so it requires both the API key and
its own shutdown token.

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename can't strand a concurrent
client's reference.

For stable-identity moves, `PATCH /nodes/{id}` accepts either the lower-level
`new_parent_id`/`new_name` fields or one absolute `dest_path`; these forms are
mutually exclusive. The latter resolves POSIX-style destination semantics and
checks `If-Match` in the same transaction. `POST /path/move` remains the
coordinate-oriented form when the source path itself is the intended target.

`POST /batch/move` accepts `{moves:[...]}`. Each item selects its source with
either `source_path`, or `node_id` plus the revision previously inspected by
the caller, and supplies `destination_path`. Every selector resolves against
one pre-transaction topology. Each destination is an exact final coordinate,
and its parent resolves against the planned final topology; batch requests do
not apply the single-move “move into an existing directory” shorthand. Docbank
constructs and checks the complete final topology in Go before applying it, so
file and directory swaps and nested
reorganizations do not depend on unsafe intermediate names. A failure rejects
the whole plan. The response preserves request order and returns each node's
prior path plus its complete final node projection and path.

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

### Audit expected-evidence verification

`POST /audit/verify` accepts an empty body for a fresh proof. To prove ancestry,
send the `evidence` object from a previously successful report:

```json
{"expected":{"vault_id":"...","lineage_id":"...","operation_sequence_high_water":12,"allocation_entry_count":12,"allocation_head":"...","scopes":[{"id":"...","entry_count":9,"chain_head":"..."}]}}
```

The current vault is independently replayed before comparison. A successful
prefix proof returns `evidence_check: {"extends":true}`. Evidence disagreement
remains an HTTP 200 verification report so clients can inspect current terminal
evidence and protected-byte problems together; `evidence_check.problems` uses
the stable codes `audit_not_enabled`, `vault_mismatch`, `lineage_mismatch`,
`allocation_shorter`, `allocation_diverged`, `scope_missing`, `scope_shorter`,
and `scope_diverged`. Malformed expected evidence is a `422 validation` request
error.

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

## Concurrency: resource revisions and `If-Match`

Every node carries a `revision` that bumps on each mutation (directories
bump when their contents change). The granularity is deliberate: a
global tree ETag would invalidate every agent's in-flight work whenever
anything anywhere changed, while per-node revisions scope conflicts to
actual contention. SQLite already serializes the writes — preconditions
exist to catch **lost updates across an agent's read-modify-write
turns**, not to lock.

Every tag definition likewise carries a `revision`. It advances when its name
or assignment set changes, so a client cannot rename over a concurrent rename
or delete assignments it did not inspect. Single-tag responses carry an ETag
matching this revision.

`If-Match` is required where a mutation targets one existing node that
the caller read in an earlier request; path mutations, bulk operations,
and maintenance are explicit exceptions:

| Endpoint | Precondition |
|----------|--------------|
| `PATCH /nodes/{id}` | required — target node's revision; an optional `dest_path` is resolved in the same transaction |
| `PUT /nodes/{id}/content` | required — prevents a replacement from overwriting a head the caller did not inspect |
| `POST /nodes/{id}/revert` | required — binds the selected source to the current head the caller inspected |
| `POST /nodes/{id}/trash` | required — target node's revision |
| `POST /nodes/{id}/restore` | required — target node's revision |
| `POST /nodes/{id}/verify` | required — binds the evidence to the exact node state the caller inspected |
| `PATCH /tags/{tag_id}`, `DELETE /tags/{tag_id}` | required — tag definition/assignment-set revision |
| `PUT\|DELETE /nodes/{id}/tags/{tag_id}` | required — target node revision; the tag revision also advances on a real assignment change |
| `POST /path/move`, `POST /path/trash` | none — the path is resolved and mutated inside one store transaction, so there is no separate read for a revision to guard |
| `POST /batch/move` | each path source resolves in the transaction; each stable-ID source carries its own required revision |
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

`GET /content-references` is the inverse identity lookup. It accepts one
canonical lowercase SHA-256 and returns only logical `content_versions`
references backed by blob-catalog authority; it never infers a match from a
loose file or pack entry alone. Each item contains the complete immutable
version, its current node projection, whether that version is the node's
current head, and a path only while the node is live. Results are bounded and
deterministic: live current references, live history, then trashed references.

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
UX still accepts relative and `cwd`-relative sources. Collisions resolve by the
same suffixing rules as other imports.

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
pointer, and bump its revision. The old head remains an immutable history and
GC root until the operator explicitly prunes that version. A
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

## Addendum: `POST /nodes/{id}/revert`

Reversion accepts JSON `{"source_version_id":"<uuid>"}` and requires the
target node's revision in `If-Match`. The source must be an immutable version of
that same node and cannot be its current version. A successful transaction
creates a distinct `content_revert` row, copies the source's blob hash, size,
and media type into it, records `source_version_id`, advances the current
pointer, and bumps the node revision.

This operation is metadata-only: it neither streams nor copies the source blob,
whether loose or packed. Every existing version remains a reachability root
until explicitly selected by version pruning.
The receipt contains `node`, the new `version`, and `source_version`, while the
ETag carries the resulting revision. Clients cross-check all four authorities;
HTTP 200 alone is not sufficient evidence.

A stale target returns `412 stale_revision`. A source from another node returns
`422 version_node_mismatch`, selecting the current head returns
`422 version_already_current`, and an unknown source returns `404 not_found`.

## Addendum: `POST /nodes/{id}/versions/prune`

Version pruning releases selected non-current history without changing current
content. Every request requires the inspected node revision in `If-Match` and
chooses exactly one selector: `version_ids` (at most 1,000 canonical UUIDs),
`keep_newest`, `older_than`, or `all_prior`. The default is a dry run;
`"run":true` performs the reported class of operation under the same node
revision precondition.

Ordinary selectors retain revert-source dependencies and report them
separately. `all_prior` may first install a same-byte, source-free checkpoint
when the current head is a revert, allowing the complete older graph to be
removed safely. A successful run advances the node revision once when it
deletes history and does not advance it for an empty selection. Deleted version
IDs stop resolving.

An `older_than` selector computes and returns its cutoff for each request. The
node ETag protects content-graph changes, but wall-clock aging does not advance
the revision; a later run can therefore include versions that crossed the age
boundary after a preview. Callers needing an exact replay execute the preview's
candidate IDs through `version_ids`.

The receipt separates logical history bytes from physical consequences. Shared
blobs remain reachable, authority-free loose blobs await GC, and dead packed
payload awaits GC followed by repack. Pruning itself does not claim physical
space reclamation.

## Addendum: tags

Tag definitions use stable server-generated UUIDv4 identities, mutable,
unique NFC-normalized names, and a revision covering both the definition and
its assignment set. `POST /tags`, `GET /tags`, `GET /tags/by-name`,
and `GET|PATCH|DELETE /tags/{tag_id}` expose definition lifecycle. `GET
/nodes/{id}/tags` and `GET /tags/{tag_id}/nodes` provide bounded forward and
reverse listings; reverse results include a path only for live nodes.

`PUT|DELETE /nodes/{id}/tags/{tag_id}` assign and unassign under the required
node `If-Match` revision. Their receipt contains the resulting node and tag,
`changed`, and the resulting node ETag. Repeating the requested state returns
`changed: false` without advancing either revision. A real assignment change
advances the node and tag once. Single-tag definition responses carry the tag
ETag; `PATCH|DELETE /tags/{tag_id}` require that ETag in `If-Match`. Renaming
advances the tag and every assigned node; deleting checks the current tag
revision before removing the complete assignment set and advancing each
assigned node. Deletion never removes nodes or document bytes.

Path-oriented clients use `PUT|DELETE /path/tags/{tag_id}` with `{"path":"/..."}`.
The store resolves that live path and changes the assignment in one SQLite
transaction. This is stronger than a separate path lookup followed by the
ID-addressed endpoint: moving an ancestor changes a descendant's path without
changing that descendant's revision.

Ingest provenance is currently filesystem-shaped: each import records the
source's original path and mtime in the store's `provenance` table. The path is
a record, never node identity. Non-file origin fields and lookup by content
hash are not part of the current API.

## Maintenance gate

`gc --run`, `trash empty`, and `verify` need the vault quiescent while
they run — the same reachability-then-delete race described in
[Ownership & Concurrency](locking.md). Rather than the daemon's exclusive
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
| `audit_mutation_unsupported` | 409 | the audited vault does not yet record this logical mutation class |
| `audit_already_enabled` | 409 | this vault already has its first permanent audit scope |
| `audit_preview_stale` | 409 | the one-use enrollment preview expired, was consumed, came from another daemon, or no longer matches the vault |
| `audit_acknowledgment_required` | 422 | enrollment execution omitted the explicit permanent-retention acknowledgment |
| `audit_not_enrolled` | 422 | the selected node exists but is outside every permanent audit scope |
| `invalid_audit_cursor` | 422 | the history cursor is malformed or belongs to another stable node or scope |
| `invalid_batch_move` | 422 | a batch has no moves, too many moves, ambiguous selectors, or an invalid final-state plan |
| `stale_revision` | 412 | `store.ErrStaleRevision` — `If-Match` didn't match the current revision |
| `not_dir` / `not_file` / `invalid_name` / `invalid_tag` / `not_trashed` / `is_root` | 422 | `store.ErrNotDir` / `ErrNotFile` / `ErrInvalidName` / `ErrInvalidTag` / `ErrNotTrashed` / `ErrIsRoot` |
| `validation` | 400, 415, or 422 | malformed request (bad `If-Match`, paths, media type, multipart envelope, or generated validation) |
| `precondition_required` | 428 | required `If-Match` header missing |
| `loopback_only` | 403 | server-path ingest or preflight called by a non-loopback peer |
| `digest_mismatch` / `size_mismatch` | 422 | uploaded file bytes disagree with the required declaration; no node/blob authority committed |
| `too_large` | 413 | upload exceeded its declared size plus bounded multipart overhead |
| `pack_retirement_deferred` | 503 | repack authority committed but an old source pack remains physically locked; release the lock, then run `storage pack` reconciliation |
| `unauthorized` | 401 | missing or invalid API key; bad shutdown token |
| `internal` | 500 | unmapped error (still surfaced with a message — this is a single-user local daemon, not a hardened multi-tenant service) |

## Non-goals

- No server-side rendering. The current root is a static placeholder; the
  planned kit-ui portal remains an ordinary authenticated API client and gains
  no privileged data path.
- No multi-user model: one vault, one key. Sharing is out of scope for
  v1.
- No MCP server.
- No remote-daemon mode or `[remote]` configuration.
