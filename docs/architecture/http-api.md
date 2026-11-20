---
last_edited: 2026-11-01
title: HTTP API
description: The agent-first HTTP API, with filesystem-shaped endpoints, revision preconditions, and the daemon's error contract.
---

# HTTP API

Use the HTTP API to browse, retrieve, file, and reorganize documents.
`docbank daemon run` serves it, and every CLI data command talks to the daemon
through it instead of opening the vault directly.

This page defines the wire contract: routes, authentication, preconditions,
content verification, and errors. For a task-oriented starting point, see
[Docbank for agents](../agents.md).

| Reader question | Contract |
| --- | --- |
| Which operation should I call? | [Endpoint map](#shape) |
| How do I process and search a document version? | [Document processing](#document-processing) |
| How do I reject a stale write? | [Revisions and `If-Match`](#concurrency-resource-revisions-and-if-match) |
| When may I trust downloaded bytes? | [Content verification](#content-identity-and-verification-evidence) |
| Which credentials does a request need? | [Authentication](#auth) |
| How do I handle a failed request? | [Error mapping](#error-mapping) |

## Vault change feed

`GET /api/v1/changes?after=CURSOR&follow=true` requires an API key and returns
`application/x-ndjson`. Each change record has the shape
`{"type":"change","change":{"cursor":"…","kind":"created","node_id":2,"revision":1}}`.
A finite drain ends with `{"type":"result","cursor":"…"}`, where the cursor
marks the final snapshot head even when the drain is empty. If the stream
reaches EOF before that record, the drain was interrupted.

The `after`, `follow`, and `limit` parameters are optional. If you omit
`after`, reading starts at the oldest retained event. If you omit
`follow=true`, the response drains and closes. Pass `limit=0` to fetch only the
current checkpoint, or pass `limit=1` through `limit=500` for one bounded
snapshot page; you cannot combine `limit` with `follow=true`. A bounded result
still reports the snapshot head even when more events remain. Resume after the
last returned change cursor, or after the head if the page is empty. To include
changes made during a full scan, save a checkpoint first and replay from it
afterward. A follow response checks for new changes every second and closes on
client disconnect or daemon shutdown. Cancellation interrupts blocked writes,
while ordinary slow readers have no write timeout.

The daemon validates query encoding and the cursor before sending response
headers. A malformed query or cursor returns 400 `validation`. An expired
cursor, a cursor from another database, or a sequence beyond the current head
returns 410 `changes_cursor_expired`, with the current head in the problem
body's `cursor` field. Save that cursor before rescanning, then resume after
it. If a read fails after streaming starts, the daemon sends
`{"type":"error","error":{...}}` and closes the stream. The nested error uses
the same problem fields, including `changes_cursor_expired` and its recovery
`cursor` when retention overtakes the reader. For event kinds, retention, and
recovery, see
[React to vault changes](../agents/integration.md#react-to-vault-changes).

A `processed` event announces completed publication for a current live
document. Timeline and attribution publish an event on first publication and
whenever checksums or states change, while identical results and freshness-only
refreshes stay silent. Invalidation and explicit derivative purge also stay
silent; a later successful republication emits an event when its prior head is
absent. Email inventory head changes and new terminal body results each emit an
event, including a body changing from pending to unavailable. Source-metadata
publication compares evidence checksums; an extractor fingerprint change with
identical facts stays silent. Dismissed or fully filtered photo
descriptions and unchanged or fully protected face results also stay silent.

## Mailbox imports and explicit EML transfers

Mailbox routes live under `/api/v1/mailbox`. Owner identity comes from the
authenticated vault principal, never from a supplied owner field.

| Route | Contract |
| --- | --- |
| `POST /containers` | Declare stable `id`, `sha256`, `size`, and `format` (`mbox` or `zip`). JSON is limited to 1 MiB. |
| `PUT /containers/{id}/chunks/{index}` | Stream one ordered 64 MiB chunk (short final chunk allowed), declaring `X-Docbank-Blob-Hash` and `X-Docbank-Blob-Size`. The response repeats the verified identity. |
| `GET /containers/{id}` · `POST /containers/{id}/seal` · `DELETE /containers/{id}` | Inspect, verify the complete source and seal it, or abort an incomplete upload. Sealed sources cannot be aborted. |
| `POST /containers/{id}/preview` | Supply `dialect`. Inspect up to three messages, reading at most 16 MiB of expanded message data, and 100 entry descriptors, with total `entry_count` and `has_more`. |
| `POST /jobs` | Supply `id`, `container_id`, `container_sha256` and `settings.destination_id`. Dialect defaults to `mboxrd`; recipe and label-to-tag mappings are optional. The response includes normalized settings and the parser/MIME recipe identity. |
| `GET /jobs` · `GET /jobs/{id}` | Inspect durable work. Lists return newest first and use `after` job ID and `limit` (maximum 100). |
| `GET /jobs/{id}/occurrences` | Read up to 100 receipts after the `after` ordinal. |
| `GET /jobs/{id}/events` | NDJSON `progress`, `result` or `error` events. Disconnecting does not cancel the job. |
| `POST /jobs/{id}/cancel` · `POST /jobs/{id}/resume` | Cancel, or supply the exact prior `request` and boolean `continuation` to resume/continue. |
| `POST /archives` | Register `id` and `description` for an application-independent source archive. |
| `POST /transfers` | Stream EML bytes. `X-Docbank-Transfer` is base64url-encoded JSON containing archive ID, stable reference, hash, size, settings, destination ID, name and optional expected revision. |

Job lists select the page's IDs and timestamps before reading full records in
the same query, keeping large retained entry-hash lists out of the history-wide
sort. Go orders the decoded page, which holds at most 100 jobs, by descending
`started_at` and then job ID. Each record goes through the same validation as a
single-job read, and one invalid record fails the whole page. The daemon
resolves an `after` cursor with an owner-scoped job read. Listing still scans
history; the page limit bounds returned jobs rather than total database work.

Container manifests hash a canonical JSON object with ordered fields `sha256`,
`size`, and `chunks`, where each chunk lists `index`, `sha256`, and `size` in
that order. You can independently verify the sealed `manifest_sha256` against
your source and ordered chunks. Browser byte uploads use the existing
ownership-proved WebSocket channel instead of raw HTTP chunk writes, and
browser credentials cannot register external archives or transfer explicitly
identified EML. No mailbox endpoint accepts an arbitrary daemon-host path.

Conflicting immutable declarations, changed resume settings, and changed
targets return 409. Invalid input and capacity limits return 422, oversized
request bodies return 413, and unexpected server failures return 500. Preview
samples messages, while the background job verifies all ZIP entries before
publishing and retains their hashes for resume. For retention, capacity, and
retry guarantees, see
[Mailbox archives](../usage/importing.md#mailbox-archives).

## Shape

`docbank daemon run` serves the API with [Huma v2](https://huma.rocks) and the
`humago` (stdlib `net/http`) adapter. It publishes a typed OpenAPI contract
through `docbank openapi`, or at `GET /openapi.json`, `/openapi.yaml`, and
`/docs` on a running daemon, and authenticates requests with `X-Api-Key` or
`Authorization: Bearer`. Endpoints are filesystem-shaped and live under
`/api/v1`:

| Endpoint | Purpose | Status |
|----------|---------|--------|
| `GET /vault` | read the selected vault's ID without counting its contents | Implemented |
| `GET /nodes/{id}` | stat by id (live or trashed) | Implemented |
| `GET /path?path=/a/b` | stat by virtual path | Implemented |
| `GET /formats/capabilities?family=&format=&extension=` | read the running binary's per-format capability snapshot | Implemented |
| `GET /nodes/{id}/children` | list a directory, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/content` | stream document bytes with catalog identity and a computed digest trailer | Implemented |
| `PUT /nodes/{id}/content` | replace raw content under revision, size, and digest preconditions (see [addendum](#addendum-put-nodesidcontent)) | Implemented |
| `POST /nodes/{id}/revert` | create a new head from a prior version of the same file | Implemented |
| `GET /nodes/{id}/versions` | list immutable content versions newest-first, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/provenance` | inspect immutable ingest-origin facts newest-first, paginated (`limit`/`offset`) | Implemented |
| `POST /nodes/{id}/provenance` | append an immutable origin fact under the node revision | Implemented |
| `GET /photos/events` | stream committed photo changes as server-sent events | Implemented |
| `GET /photos/assets/{asset_id}` · `GET /photos/nodes/{node_id}/asset` | inspect one bounded photo graph by asset or member node | Implemented |
| `POST /photos/assets/flags` | read current display flags for up to 20,000 visible assets | Implemented |
| `POST /photos/assets/query` | browse matching assets with live keyset pages or frozen semantic and hybrid pages | Implemented |
| `POST /photos/authored/prepare` · `POST /photos/authored/edit` · `POST /photos/authored/{receipt_id}/undo` · `POST /photos/authored/{file_id}/xmp` | prepare bounded file targets from assets or a query, atomically edit fields and keywords with required asset, file, and node revision fences and optional `content_version_id` fencing the asset's displayed version, undo if revisions match, and resolve an XMP sync conflict by choosing the catalog or the sidecar | Implemented |
| `POST /photos/suggestions/{suggestion_id}/response` | accept selected caption or tags, or answer a rotation proposal with the displayed `rotation` angle and asset, source, and node revision fences; rotation requires a current nonzero proposal, accepts the displayed angle even if the live angle changed, and dismisses by confirming the current authored angle | Implemented |
| `POST /photos/assets/{asset_id}/preview` | prepare a fit or large preview for the current content version | Implemented |
| `GET /photos/assets/{asset_id}/previews/{generation_id}` | read verified bytes for an eligible display preview | Implemented |
| `POST /photos/assets` · `POST /photos/assets/{asset_id}/files` · `DELETE /photos/assets/{asset_id}/files/{file_id}` | create, attach, or detach photo membership | Implemented |
| `POST /photos/assets/{asset_id}/exclude` · `POST /photos/nodes/{node_id}/promote` | change exclusion or promote a live file | Implemented |
| `POST /photos/rejects/preflight` · `POST /photos/rejects/trash` | count scoped rejects and mixed pairs, then confirm a bounded atomic move to recoverable trash | Implemented |
| `POST /photos/assets/{asset_id}/trash` | atomically move every live photo member to recoverable trash | Implemented |
| `PUT /photos/assets/{asset_id}/display` · `GET\|PUT /photos/settings` | set an asset display override or vault preference | Implemented |
| `POST /photos/assets/{asset_id}/hide` · `POST /photos/assets/{asset_id}/unhide` | change Photos visibility under `If-Match` | Implemented |
| `GET /photos/hidden` · `POST /photos/hidden/{setup,change,disable,unlock,lock,reset}` | manage the vault passcode and configurable unlock lifetime | Implemented |
| `POST /photos/checkouts/estimate` · `POST /photos/checkouts` | estimate or create writable photo copies; creation is loopback-only | Implemented |
| `POST /photos/checkouts/{id}/scan` · `POST /photos/checkouts/{id}/commit` | inspect working edits or commit stable bytes as new versions under the recorded bases | Implemented |
| `POST /photos/checkouts/{id}/rebind` | verify and bind a restored checkout folder; loopback-only | Implemented |
| `POST /photos/checkouts/{id}/retire` | request retirement; release holds and reservation when queued or running work finishes or is cancelled | Implemented |
| `GET /photos/checkouts` · `GET /photos/checkouts/{id}` | list checkout definitions or read checkout status with paged entries and their `working_state` | Implemented |
| `GET /people` · `POST /people` | list, search, and create active canonical people | Implemented |
| `GET /people/by-id/{person_id}` | inspect one person, its identities, and its external UIDs | Implemented |
| `PATCH /people/by-id/{person_id}` · `POST /people/by-id/{person_id}/retire` | rename or retire one person under `If-Match` | Implemented |
| `POST /people/by-id/{person_id}/merge` · `POST /people/by-id/{person_id}/split` | merge or split person authority under revision and operation fences | Implemented |
| `POST /versions/{version_id}/download-ticket` | issue a two-minute single-use original download capability for an exact vault, live file node, retained version, hash and size | Implemented |
| `GET /versions/{version_id}` · `GET /versions/{version_id}/content` | inspect or stream one immutable version by stable UUID | Implemented |
| `GET\|POST /versions/{version_id}/email` | read or synchronously ensure canonical email metadata for one immutable version | Implemented |
| `GET /versions/{version_id}/email/generations/{generation_id}` | read one immutable email generation attached to that version | Implemented |
| `GET /versions/{version_id}/email/generations/{generation_id}/parts/{part_path}/{role}` | stream one verified raw-header, decoded-payload, or UTF-8 body artifact | Implemented |
| `GET /content-references?sha256=&limit=&offset=` | find every stable node/version pair retaining a content hash | Implemented |
| `GET /duplicates?limit=&offset=` | list live current documents sharing content, with exact counts and bounded reference previews | Implemented |
| `GET\|POST /tags` · `GET /tags/by-name` · `GET\|PATCH\|DELETE /tags/{tag_id}` | list, resolve, create, rename, or delete stable tag definitions | Implemented |
| `GET /nodes/{id}/tags` · `GET /tags/{tag_id}/nodes` · `PUT\|DELETE /nodes/{id}/tags/{tag_id}` · `PUT\|DELETE /path/tags/{tag_id}` | inspect and change tag assignments | Implemented |
| `GET\|POST /saved-queries` · `GET\|PATCH\|DELETE /saved-queries/{saved_query_id}` | list, create, inspect, edit, or delete named query and literal highlight definitions | Implemented |
| `POST /saved-queries/{saved_query_id}/runs` | execute one saved query at the revision named by `If-Match` and return its durable run receipt with the first snapshot page | Implemented |
| `POST /queries/parse` | validate complete QueryV1 intent and resolve dependency revisions without executing a search | Implemented |
| `POST /workspace/queries` | execute complete QueryV1 intent and return the first page of one exact bounded result snapshot | Implemented |
| `POST /workspace/queries/{id}/pages` | read another page from an existing query snapshot with its opaque cursor | Implemented |
| `GET\|POST /search-exports` · `GET /search-exports/{id}` · `POST /search-exports/{id}/dates` · `POST /search-exports/{id}/revisions` · `POST /search-exports/{id}/download` · `GET /search-exports/{id}/csv` · `GET /search-exports/{id}/bundle` | create dated search counts, review frozen evidence, download artifacts, and list recent exports | Implemented |
| `POST /audit/preview` · `POST /audit/enable` · `GET /audit/status` | review permanent first-scope retention, enable the reviewed plan, and inspect authority or membership | Implemented |
| `GET /audit/history?path=&node_id=&limit=&cursor=` | read one audited node's canonical newest-first event timeline with a stable continuation cursor | Implemented |
| `GET /audit/scopes/{scope_id}/history?limit=&cursor=` | read canonical newest-first events across every member of one permanent scope | Implemented |
| `POST /audit/verify` | independently replay audit authority, optionally prove recorded evidence is an exact prefix, and re-hash every protected blob | Implemented |
| `POST /nodes/{id}/verify` | re-hash one file, bound to an inspected node revision | Implemented |
| `GET /search?q=&tag_id=&mime_type=&under_node_id=&under_path=&modified_since=&modified_before=&fuzzy=&limit=` | bounded name and extracted-content search (FTS5), optionally restricted by stable tag identity, current base media type, descendants of a live directory, and current node modification time, with match source and `truncated` status; `fuzzy=true` appends bounded one-typo filename/path discovery with a coverage receipt | Implemented |
| `GET /processing/profiles` · `POST /processing/plans` | list executable profiles / preview one source version and its provider disclosures | Implemented |
| `POST /processing/jobs` · `GET /processing/jobs/{id}` | run a reviewed plan with streamed job identity / read aggregate status | Implemented |
| `POST /processing/consent/grants` · `POST /processing/consent/revocations` | grant reviewed profile consent / revoke this operator's processing consent | Implemented |
| `GET /renditions/{attachment_id}` · `POST /renditions/select` | stream retained sanitized Markdown by attachment or source selector | Implemented |
| `POST /evidence/windows` | read bounded text from an exact current/live file and active rendition | Implemented |
| `POST /text-citations/resolve` | reopen an exact quote from retained rendition text | Implemented |
| `POST /text-citations/outline` | read headings and whole-section bounds from retained text | Implemented |
| `POST /text-citations/section` | read an exact page of a retained text section | Implemented |
| `POST /kata/cite` | quote a verified document passage into a new or existing Kata issue | Implemented |
| `POST /kata/context` | read saved Docbank evidence from a Kata issue with its current source status | Implemented |
| `GET /coverage?profile=&vault_uid=&content_version_id=` · `POST /search` | inspect separate rendition/embedding coverage / search an authorized source-version set | Implemented |
| `POST /search/similar` | stored-vector neighbors | Implemented |
| `POST /derivatives/purge-plans` · `POST /derivatives/purge-jobs` | preview / run a live derivative purge without changing immutable backups | Implemented |
| `POST /nodes` · `POST /path/mkdir` | create a directory beneath a stable parent ID or at one exact virtual coordinate | Implemented |
| `POST /ingest` · `POST /ingest/stream` · `POST /ingest/preflight` | import with JSON or streamed progress / inventory server-side paths (see [addendum](#addendum-post-ingest-post-ingeststream-and-post-ingestpreflight)) | Implemented |
| `GET /collections` · `GET /collections/{id}` · `GET /collections/{id}/members` · `GET\|PUT /collections/{id}/label` | browse live ingest-run membership and inspect, set, or clear its label under a revision precondition | Implemented |
| `GET /collections/{id}/quality` | inspect bounded document distributions and processing coverage for one collection | Implemented |
| `GET /push/sources?push_name=&after=&limit=` | page through a push name's last accepted source digests (see [addendum](#addendum-folder-push)) | Implemented |
| `POST /push/uploads?parent_path=&name=&push_name=&source_ref=&run_id=` | verify and record one push source (see [addendum](#addendum-folder-push)) | Implemented |
| `POST /uploads?parent_id=&name=` | stream one digest-checked remote file (see [addendum](#addendum-post-uploads)) | Implemented |
| `PATCH /nodes/{id}` | move and/or rename, including resolving an absolute `dest_path` transactionally | Implemented |
| `POST /path/move` · `POST /path/trash` | move / trash by virtual path, resolved and mutated in one store transaction | Implemented |
| `POST /batch/move` | validate and apply up to 1,000 moves as one final-state transaction | Implemented |
| `POST /batch/tags` · `POST /batch/tags/preview` | atomically assign/remove one tag on a selected set of nodes checked against their revisions, or inspect its exact membership | Implemented |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover | Implemented |
| `GET /trash` · `POST /trash/empty` `{run, older_than}` | list nodes, grouping photos on paginated requests / report or hard-delete eligible complete groups | Implemented |
| `POST /gc` `{run}` · `POST /verify` | reclaim unreachable blobs / validate metadata and re-hash all blobs | Implemented |
| `GET /storage` · `POST /storage/pack` · `POST /storage/repack` | inspect usage / pack loose blobs / compact sparse packs | Implemented |
| `GET /jobs` | inspect daemon-owned background tasks and terminal failures; a `running` job with `busy: false` is waiting for work or its next run, and clients show it as idle | Implemented |
| `GET /watches` | inspect effective watched-inbox configuration and runner state | Implemented |
| `GET\|PUT /backup/schedule` | inspect or override the scheduled interval and retention | Implemented |
| `POST /backup/init` · `POST /backup/claim` · `POST /backup/snapshots` · `POST /backup/snapshots/stream` · `GET /backup/snapshots` | initialize or claim a repository / create with JSON or streamed progress / list snapshots | Implemented |

### Format capability coverage

`GET /formats/capabilities` returns the flat `format-coverage/v1` object with
`contract_version`, `formats`, `pending`, and `generated_by` as top-level
members. If you pass `format` or `extension`, the response also includes
`lookup`. The `family` parameter filters the returned format rows, while
`lookup` still resolves against the full runtime snapshot and
`generated_by.catalog_rows` still describes that complete source catalog.

`family` and `format` accept at most 64 bytes, and `extension` accepts at most
16 bytes. Pass either `format` or `extension`, not both; supplying both returns
`422 invalid_format_query`. Pending and unknown queries return HTTP 200 with
`lookup.match` set to `pending` or `unknown_format`. For the capability and
state contract, see [Format coverage](format-coverage.md).

Search accepts an optional `q` query. If you omit `q` or pass an empty or
whitespace-only value, you must supply `pdf_filter`, `tag_id`,
`modified_since`, or `modified_before`. This is a request rule, and `limit`
bounds the response size rather than the database work. Filter-only hits
include live files and directories other than the vault root, ordered by
current `modified_at` descending and then by name and ID, with `match: "filter"`
identifying their source. `mime_type`, `under_node_id`, and `under_path` narrow
a page, but they cannot anchor a blank query on their own; a blank unanchored
query returns `422 search_query_required`.

`under_path` takes an absolute virtual directory path of at most 16,384 UTF-8
bytes. The daemon resolves it on the search normalizer's querier and applies
the same descendant filter as `under_node_id`. Pass only one of those two
selectors. Sending both returns `422 validation`, targeting a file returns
`422 not_dir`, and targeting a missing or trashed folder returns
`404 not_found`. GET search receipts echo the supplied path as `under_path`
along with the resolved stable `under_node_id`. An explicitly empty
`under_path` fails validation on both GET routes. The text-coverage GET route
and document source-fence filters accept the same selector. Clients require a
matching daemon protocol so an older daemon cannot ignore the GET field and
widen the search scope.

Search scope validation checks that the tag definition exists without counting
its assignments. Tags with no assignments are valid scopes. Lexical searches
check each candidate's tag membership through the node/tag index. Filename
queries bind that check to the text index candidate ID, and filter-only
searches select tagged nodes through the tag index. Time-window selection uses
a live-node index that matches the result ordering. Even so, combined filters
can still do substantial work before reaching `limit`. The normal `limit` and
`truncated` contract applies without a cursor: if `truncated` is true, the page
is incomplete. Narrowing time bounds cannot split a group of nodes with
identical modification timestamps, such as nodes restored together.

`GET /api/v1/search/text-coverage` accepts the ordinary search filters and
counts current files after an empty text search. `no_text` counts files that a
processing profile could make searchable, grouped by `families` (`pdf`,
`office`, `other`), and `pending` counts native-text files still queued for
extraction. PDF page coverage reads at most 16 distinct evidence builds; when
`pdf_page_coverage_partial` is true, coverage is incomplete and the PDF and
page counts are lower bounds.

To append bounded ASCII filename and path discovery after existing hits, set
`fuzzy=true` on ordinary `GET /search`. Blank text returns `422 validation`,
and text outside the supplemental syntax returns ordinary hits with `not_run`
and `query_ineligible`. The response adds an optional `fuzzy: {status, reasons}`
object where `status` is `complete`, `limited`, or `not_run` and `reasons` is
always a sorted array. Without `fuzzy=true`, the response shape and ordinary
ranking stay unchanged. Supplemental hits report `fuzzy_name` or `fuzzy_path`
as their source.

Every scope filter runs before the supplemental candidate cap, and candidate
metadata, scope eligibility, and bounded display paths all share one read
snapshot. If a timeout hits, the daemon discards supplemental results and keeps
the ordinary hits, while request cancellation and real storage or validation
errors propagate. For coverage limits and exact matching and ranking rules, see
the [Searching guide](../usage/searching.md#how-complete-is-fuzzy-discovery).
`truncated` still identifies a known omitted match independently of uncertain
fuzzy coverage. Daemon protocol revision 77 requires this contract, and
opted-in direct clients also reject a missing or invalid coverage receipt. The
separate processing `POST /search` contract stays unchanged.

### Document processing

The routes in this section use the normal [API authentication](#auth). Plan and
grant requests use a source selector and a reviewed plan fingerprint instead of
`If-Match`. Browser sessions can call the built-in processing, consent,
coverage, search, and attachment-read routes, while selector reads and
derivative purge require the master API credential. A rendition is a derived,
readable representation of a document.

`GET /processing/profiles` returns an array of executable profiles with `name`,
`fingerprint`, `rendition`, and `embedding_bindings`. An empty array means no
profiles are executable, which is the default configuration. Pick one of the
returned names for your requests.

`POST /processing/plans` accepts a `selector`:

```json
{
  "selector": {
    "node_id": 231,
    "content_version_id": "11111111-1111-4111-8111-111111111111",
    "profile": "private"
  }
}
```

Replace these synthetic values with an inspected node, its immutable version,
and an executable profile. Node IDs are positive integers, and version IDs are
canonical UUIDv4 values. Profile names are 1 to 128 characters long, start with
a lowercase letter, and contain only lowercase letters, digits, `_`, or `-`.

The response includes `fingerprint`, `vault_uid`, `selector`,
`profile_fingerprint`, `flow`, `disclosed_classes`, `retained_classes`,
`estimate`, `consent_required`, `consent_state`, and `backup_consequence`. When
the profile includes a rendition stage, the daemon inspects the source locally
before answering. If inspection refuses the source,
`rendition_ineligible_reason` names the reason, such as
`unbounded_media_family`, `semantic_units_exceeded`, or
`unsupported_media_type`, and starting the plan fails with
`rendition_source_ineligible`. Locked PDFs are the exception: when no
configured password opens a PDF or no password file is configured, starting the
plan creates a job that ends `failed` with `failure_code=pdf_password_required`.
If the daemon refuses the configured password file, planning or starting
returns 409 `rendition_operator_required` with the fixed detail
`PDF passwords file can't be read; check media.pdf_passwords_file`, omitting
the file's path. Other operator-required errors use
`document rendition requires operator intervention`.

Each flow names the provider, capability, trust boundary, input classes, any
filename disclosure, and a `runtime_disclosure` object. Review it before
granting consent or starting work. The runtime disclosure contains:

| Field | Meaning |
|-------|---------|
| `immediate_processor`, `ultimate_processor` | The adapter or processor receiving the input and the processor it ultimately uses |
| `endpoint`, `deployment` | The configured destination and deployment identity; a local provider reports `in-process` |
| `model`, `model_revision`, `vector_space` | Model and vector-space identities, when applicable |
| `metadata_classes` | Metadata disclosed alongside the content or query |
| `retained_artifact_roles` | Artifacts retained for this hop; query embedding retains none |

These values feed into the plan fingerprint. Consent state is advisory and does
not authorize a provider call on its own.

#### Remote recording references

`POST /api/v1/media/sources` also accepts a JSON remote recording reference.
`reference_url` is required and write-only; it holds the protected acquisition
input, including any occurrence-specific query. `canonical_url` is optional,
write-only, and limited to 8,192 bytes, holding the caller's sanitized identity
URL. It must be an absolute HTTP(S) URL without userinfo. To compute the
permanent source identity, Docbank lowercases the scheme and hostname, converts
Unicode domains to punycode, removes the default port and fragment, turns an
empty path into `/`, keeps a trailing dot in the hostname distinct, and leaves
all other path and query encoding unchanged.

```json
{
  "operation_id": "00000000-0000-4000-8000-000000000451",
  "reference_url": "https://private.invalid/share/call?token=synthetic",
  "canonical_url": "https://recordings.invalid/share/call?clip=2",
  "occurrence": {
    "ref": "call-1",
    "revision": "1",
    "filename": "call.wav",
    "message": {}
  }
}
```

When the daemon has a registered self-hosted Cap origin, an exact `/s/<id>`,
`/embed/<id>`, or SDK `/dev/<id>` path on that origin takes the registered
branch. Because the source identity is the Cap video ID, these routes identify
one source. If you include `canonical_url`, it must name the same registered
origin and video ID, or the request returns `422 invalid_media_plan`. A
nonempty `provider_hint` must equal `cap.self-hosted`, and any different hint
also returns `422 invalid_media_plan`. A credential binding must be empty or
match the binding registered for that origin; a foreign binding returns
`422 credential_cross_origin`.

When a registered origin has a probe, `GET /api/v1/media/origins` reports
`adapter_contract`, `deployment_revision`, `probe_state`, and `probed_at`.
Probe results reflect the configured network and TLS policy at daemon startup
and do not refresh during the process lifetime. `deployment_revision` is the
operator's configured value rather than a remotely verified version. Recognized
self-hosted Cap references remain `access_required`, and `acquire: true`
returns `503 capability_unavailable`.

Registration applies to new submissions only. A generic source retained before
registration stays separate, and its manual import stays unchanged.

Outside registered self-hosted origins, the canonical path does not resolve
DNS, follow redirects, read credentials, or download a recording, and
`provider_hint` is a bounded replay value that does not prove a provider. A
fresh canonical submission returns `outcome: "unsupported"` with a pending
occurrence, whereas a submission that omits `canonical_url` uses its configured
origin policy and returns `access_required`. Acquisition planning uses the
configured origin and `reference_url`, and `canonical_url` does not affect its
plan.

The canonical URL determines the provider identity:

- **Cap Cloud.** A `canonical_url` on `https://cap.so` or `https://www.cap.so`
  whose path is exactly `/s/<id>`, `/embed/<id>`, or the documented SDK
  `/dev/<id>` route uses the `cap` provider identity, keyed by that video ID.
  Either host, any of those routes, and any query or fragment select the same
  source. The ID is case-sensitive and must not be percent-encoded. Because Cap
  documents no download route for received links, the submission returns
  `outcome: "unsupported"` whether `acquire` is set or not. Import the file
  with the artifact route described next.
- **Loom.** A `canonical_url` on `https://loom.com` or
  `https://www.loom.com` whose path is exactly `/share/<id>` or
  `/embed/<id>` uses the `loom` provider identity. Because Loom does not
  document equality between share and embed references, the route is part of
  the source key, while queries and fragments do not affect identity. Loom
  returns `outcome: "unsupported"`, even with `acquire: true`, and makes no
  network requests. Import your exact MP4 and SRT files with the artifact
  route.
- **Any other URL.** The generic `url` identity uses the whole canonical URL,
  and `acquire: true` returns `503 capability_unavailable`.

Docbank recognizes Cap links from the URL alone. It does not contact Cap or
check whether a Cap video is public, private, or password-protected. Other Cap
paths keep the generic `url` identity.

```json
{
  "operation_id": "00000000-0000-4000-8000-000000000461",
  "reference_url": "https://cap.so/s/synthcap01?t=synthetic",
  "canonical_url": "https://cap.so/s/synthcap01",
  "acquire": true,
  "occurrence": {
    "ref": "cap-1",
    "revision": "1",
    "filename": "cap.wav",
    "message": {}
  }
}
```

To add a local original, send exactly the two multipart parts required by the artifact
route with metadata `kind: "media"`. WAV and MP3 keep their existing rules. A
remote recording can use an MP4 named `.mp4` with `video/mp4`, limited to
20 MiB, 2,088,960 coded pixels, 300,000 milliseconds, and 18,000 frames. The
metadata's filename and media type must match the inspected bytes, and the
service verifies the declared size and SHA-256 before publishing.

In one transaction, the store publishes the verified original and binds it to
the selected visible occurrence. A pending occurrence receives a new source
version, while an existing exact version can be reused without adding a
revision. Sending a changed original for an already bound occurrence returns
`409 source_conflict`. A caption or transcript must follow the original. A
caption with `application/x-subrip` stays a retained input until you select the
`supplied-captions` profile, which publishes timed `media-transcript/v1`
evidence and supports lexical and auto search. The retained `origin` label does
not restrict caption processing, so imported files produce supplied evidence in
the same way as imported transcripts. For limits, see the
[caption timing limits](../document-understanding.md#supplied-caption-timing-limits).
Semantic and hybrid search are not configured for supplied captions. A
transcript requires a processing plan, consent, and retry before it can produce
`transcribed` coverage.

Media retry selects the caller's newest visible occurrence for the source and
cannot target an older occurrence. To process an older recording, call the
ordinary processing API with that recording's node and current content version.

Status and list responses select the source version bound to the visible
occurrence they report. After the original is retained, they show
`content_available` and `unprocessed`, then read processing receipts for that
same source version, deriving each receipt's state from its processing job at
read time. A failed retry can leave an earlier successful transcript visible
for the same version, while a transcript from an older version cannot cover
newer bytes. Source lists page distinct caller-visible source IDs before
selecting each source's newest visible occurrence, breaking ties on equal
occurrence timestamps by descending occurrence ID. Repeated occurrences do not
consume page slots or repeat the newest-occurrence lookup for the same source.
Raw URLs and credential bindings never appear in receipts, errors, logs,
renditions, search results, or portable metadata.

#### Remote recording receipts

`GET /api/v1/media/operations/{operation_id}` returns the receipt saved for one
remote-recording submission under the daemon's media principal. Call it when a
`POST /api/v1/media/sources` reply is lost. The operation ID is enough, and the
caller never resends the protected link. An unknown ID, an ID owned by another
principal, and an ID from any other media operation all return `404 not_found`,
and a malformed ID returns 422. Authenticated responses, including 404 and 422,
carry `Cache-Control: no-store`. A 404 describes only that point-in-time read.
A submission still in flight can commit afterward.

#### Exact media transcript reads

Read one retained transcript by its complete stable identity:

```text
GET /api/v1/media/sources/{source_id}/versions/{source_version_id}/transcript?content_version_id={content_version_id}
```

Authentication supplies the vault and principal, and both the source version
and the expected content version must match the caller-visible occurrence. The
response reports `evidence_state`, `coverage_state`, and `operation_state`
separately, and only `evidence_state: "ready"` includes `transcript`. Its units
are the normalized evidence of the active rendition for that content version
and processing profile, the same build `/api/v1/renditions/select` serves. Each
unit keeps its text, an optional `speaker`, and an optional `time_span` with
`start_ms` and `end_ms` in the same shape search evidence uses; untimed units
omit `time_span`. `origin` is `supplied` when the build came from a supplied
transcript or caption, and `generated` otherwise.

`transcript.build_id` identifies that active build. Supplied evidence also
includes `transcript.supplied_input_id` for the input bound to that build, and
a failed retry keeps the identities of the earlier covering build. Generated
evidence omits `supplied_input_id`, and responses without ready evidence omit
`transcript`.

If the content version no longer matches the source version or is no longer the
file's current version, or if this source version cannot see the build's
supplied input, the route returns `stale` with no text. Unknown or hidden
sources return `404`, and invalid identities return `422`.

#### Processing consent

To grant consent without running document processing, send
`POST /processing/consent/grants` with the plan's exact `selector` and its
`fingerprint` as `plan_fingerprint`. If present, the optional `expires_at`
field must be a future RFC3339 timestamp; omitting it grants consent without
expiry. The response returns `plan_fingerprint`, `profile_fingerprint`, and any
`expires_at`.

The grant covers this operator's use of the profile across documents and
searches, including the profile's document inputs, retained classes, and
`query_text` for providers that support query embedding. A configured reranker
adds `query_text_and_excerpt` to the same reviewed approval. These operations
have separate grant records that are approved together, and consent is not
limited to the document used for preview. The daemon uses the `daemon:operator`
principal and `document-processing` scope for these routes.

`POST /processing/consent/revocations` takes no body, returns `revoked_at`, and
revokes this operator's processing grants across all profiles. Any new grant
must come from a reviewed plan, and revocation does not delete existing
derivatives.

Semantic and hybrid searches require active `query_text` consent for the
selected binding's provider disclosure. When `rerank` is true, every mode also
requires a separate `query_text_and_excerpt` grant for the reranking provider.
Lexical and auto searches read retained local text without query embedding or
query-text consent. Provider work checks consent before egress and before
publication, and stored vectors alone do not authorize a query disclosure.

#### Start work and recover its status

`POST /processing/jobs` accepts `selector`, `plan_fingerprint`, and `consent`.
The fingerprint must match the current plan. Passing `consent: true` grants the
reviewed profile consent without expiry before running, while `false` relies on
existing active grants. The HTTP API accepts both, whereas the CLI requires
`--consent`.

Once the daemon accepts the work, it returns `200 application/x-ndjson` with
`Cache-Control: no-store`. A complete stream has two records:

1. `sequence: 1`, `type: "job"`, and `job` with the durable `id`, source version,
   profile fingerprint, and any known rendition, attachment, or embedding IDs.
2. `sequence: 2`, `terminal: true`, and either `type: "status"` with `status`, or
   `type: "error"` with the job and `processing_status_unavailable` error.

HTTP 200 alone does not mean processing succeeded, because a terminal status
can report failure. The first job receipt does not report a state or phase.
Read `state` and `failure_code` from a status response. Once accepted, work
continues under the daemon lifecycle even if your connection closes. If the
stream ends early, keep the first job ID and call `GET /processing/jobs/{id}`
to read `job_id`, `state`, `phase`, `embedding_job_ids`, `completed_bindings`,
and any `failure_code`. Job IDs are lowercase SHA-256 strings.

#### Read a rendition

`GET /renditions/{attachment_id}` reads one active sanitized-Markdown
attachment. Its optional `max_bytes` query accepts 1 to 67,108,864 and defaults
to 67,108,864. Browser sessions must omit query parameters on this route.
Setting `max_bytes` requires the master API credential. To read without first
discovering the attachment ID, call `POST /renditions/select` with `selector`
and a required `max_bytes` in the same range. Attachment IDs are lowercase
SHA-256 strings.

The response is `text/markdown; charset=utf-8` with `Cache-Control: no-store`.
`X-Docbank-Rendition-Attachment`, `-Build`, `-Artifact`, `-Profile`,
`-Completeness`, and `-Warnings` identify the result, while
`X-Docbank-Content-Version`, `X-Docbank-Blob-Hash`, and `X-Docbank-Blob-Size`
identify its source version and complete artifact. Before accepting the bytes,
read to completion and compare the byte count and SHA-256 against those headers
and the base64 SHA-256 `Content-Digest` trailer. Do not treat an incomplete
stream or a missing digest as verified.

The GET route accepts a single `Range: bytes=...` header and returns `206` with
`Content-Range`. The artifact identity headers still describe the complete
rendition, while the digest trailer covers only the returned range. Invalid or
unsatisfiable ranges return `416 invalid_rendition_range`. For the envelope and
body-relative navigation, see the
[Markdown contract](document-derivatives.md#sanitized-markdown-contract).

#### Exact evidence windows

`POST /api/v1/evidence/windows` (`readEvidenceWindow`) reads an exact cited
sanitized-Markdown rendition. It requires the ordinary API key and a JSON
request of at most 16 KiB, and its character and identity limits keep the JSON
response under 128 KiB.

Supply `vault_uid`, a positive `node_id`, `content_version_id`,
`content_sha256`, `rendition_attachment_id`, `build_id`, and
`rendition_sha256`. All of these identities are required, and hashes, build
IDs, and attachment IDs are lowercase 64-character hex strings. Get these
identities from file-version and rendition metadata and store them together; an
existing stored build alone does not authorize a read.

`offset` is a nonnegative Unicode scalar offset. If you omit `max_chars` or set
it to zero, the daemon uses 8,000; explicit limits accept 1 through 16,000. The
response echoes every identity and adds `text`, `actual_start`, exclusive
`actual_end`, `next_offset`, `eof`, UTF-8 `response_bytes`, and
`media_type: "text/markdown"`. An offset at EOF returns empty text and
`eof: true`.

The daemon validates request syntax, vault identity, current/live node and
version, content hash, active attachment, build, Markdown artifact hash, and
then the range. If a well-formed reference no longer matches visible authority,
the route returns `404 evidence_unavailable` without text or details about
hidden components. Invalid JSON or references rejected by the shared reader
return 400, schema violations (including unknown fields) return 422, and
offsets beyond EOF return 416. Unavailable physical storage returns 503 with
`content_missing`, `store_fenced`, or `store_unavailable`. Cancellation and
actual integrity or cleanup failures propagate as-is. Reads never run
processing or switch to a newer version or rendition.

Renaming a file preserves the citation, whereas replacing its content or
rendition, trashing it, pruning it, or retiring it invalidates the citation.
Digests identify catalog authority, and a partial window does not claim fresh
whole-artifact hash verification. Search segment offsets are not Markdown
offsets. Start an overview at zero, or continue from offsets returned by an
earlier evidence window.

`POST /api/v1/renditions/windows` keeps its existing request fields, defaults,
and current/live visibility checks. Both routes require a nonempty source blob
hash and check active rendition authority in the same catalog snapshot, with
the stricter evidence route adding mandatory identity preconditions to the same
bounded reader.

#### Retained text citations

`POST /api/v1/text-citations/resolve` (`resolveTextCitation`) returns an exact
quotation from retained sanitized Markdown, including historical versions and
replaced rendition heads. It requires an API key, rejects browser sessions,
creates no retention root, and runs no processing.

Send a flat `document.TextCitation` object with every field present; the route
rejects unknown fields and explicit nulls.

| Fields | Contract |
| --- | --- |
| `version` | Exactly `1` |
| `vault_uid`, `content_version_id` | Canonical lowercase UUIDv4, including hyphens |
| `node_id` | Positive signed 64-bit node ID |
| `content_sha256` | Original version's lowercase SHA-256 hex digest |
| `rendition_attachment_id`, `build_id`, `rendition_sha256` | Exact published attachment, build, and sanitized-Markdown digest; each lowercase hex64 |
| `start`, `end` | Inclusive start and exclusive end in Unicode code points; `0 <= start < end <= 2147483647`, spanning at most 16,000 code points |

The response returns `citation` unchanged along with `text`, `text_sha256`, and
`text_bytes`, where the last two describe the returned UTF-8 bytes. For
example, `aé界🙂z` at `[1,4)` yields `é界🙂`, which is nine bytes. Escapes, line
endings, and combining marks stay unchanged, and offsets refer to stored
Markdown rather than rendered text or search segments. To assemble the
reference, see [MCP citation discovery](../usage/mcp.md#saved-text-citations).

Before returning any quote, the daemon verifies the entire rendition's size,
digest, and UTF-8 encoding, so a corrupt suffix fails even a prefix quote. The
request body limit is 16 KiB, the artifact limit is 64 MiB, and the daemon's
normal 60-second request deadline includes any maintenance wait. The client
caps the JSON response at 1 MiB and checks the echoed identities, quote length,
byte count, and digest. Successful responses carry `Cache-Control: no-store`.

| Status / code | Meaning |
| --- | --- |
| 400 / `invalid_text_citation` | Strict JSON decoding failed |
| 422 / `validation` | HTTP schema rejected a field or missing value |
| 422 / `invalid_text_citation` | Cross-field identity or range validation failed |
| 413 / body rejection or `citation_limit` | Request or retained artifact exceeds its bound |
| 404 / `citation_unavailable` | Exact retained authority or its bytes are unavailable |
| 416 / `invalid_citation_range` | The verified rendition is shorter than the requested range |
| 504 / `citation_timeout` | Deadline expired, including a maintenance wait |
| 408 / `citation_canceled` | Caller canceled; no quote is returned |
| 500 / `citation_integrity` | Rendition size, checksum, or UTF-8 verification failed |
| 500 / `citation_failed` | Another read failure; the daemon log retains its cause |
| 503 / `processing_unavailable` | The processing service is not configured |

For retention and concurrency rules, see
[document-processing architecture](document-processing.md#reopen-a-saved-quotation).
The returned quote is verified retained text, not an offline proof or a claim
about the original document's bytes.

#### Retained text sections

`POST /api/v1/text-citations/outline` (`getTextOutline`) and
`POST /api/v1/text-citations/section` (`readTextSection`) let you navigate the
exact Markdown identified by a [saved citation](#retained-text-citations). Both
routes require an API key and reject browser sessions. Each request body is
limited to 16 KiB, uses the normal 60-second deadline, and returns
`Cache-Control: no-store`. Every request field listed next is required, there
are no implicit defaults, and the daemon rejects unknown members and explicit
nulls, including inside nested objects.

The citation must remain a valid nonempty anchor of at most 16,000 code points.
If you have no particular quote in mind, use `[0,1)`; empty artifacts have no
valid anchor. The section you read does not have to contain the anchor.

**Outline.** Send `{citation, offset}`, where `offset` is an entry index from 0
to 4,097. The result contains:

| Field | Meaning |
| --- | --- |
| `citation` | Unchanged input reference |
| `document_chars` | Positive total code-point count |
| `enclosing_section` | `{start,end}` containing the entire anchor |
| `offset`, `total` | Echoed entry offset; total entries from 1 to 4,097 |
| `sections` | Non-null array of at most 50 entries, in root-then-source order |
| `next_offset` | Present only when more entries remain; equals `offset + len(sections)` |

Each entry contains `start`, `end`, `level` from 0 to 6, `title_markdown`, and
`title_truncated`. The synthetic root comes first with bounds
`[0,document_chars)`, level 0, and an empty untruncated label. Only ATX and
Setext headings directly under the Markdown document root create additional
entries; headings inside lists or blockquotes stay content, and fenced or
indented code never creates headings. Empty and repeated headings still create
distinct entries.

A heading starts at the beginning of its first physical title line, including
indentation and markers, and ends at the next heading of equal or smaller level
or at EOF, so it includes subsections and intervening whitespace. Multiline
Setext headings start at their first title line rather than the underline, and
the root and first heading can share bounds. The enclosing section is the
greatest-start heading that contains the entire citation, or root when none
does, so a quote spanning siblings selects their enclosing parent or root.

Labels keep inline Markdown and escapes from the heading's source segments,
trim outer whitespace, and join multiple title lines with one ASCII space.
Labels hold at most 160 code points; truncation sets `title_truncated` without
adding an ellipsis. Labels are not quotation text. Generated headings also
appear in the outline, and plaintext `#` comments can become stored headings.
The outline describes the rendition's Markdown, not the original file's layout
or source-language structure.

An outline offset equal to `total` returns an empty final array without a next
offset, while an offset beyond `total` fails. Both `total` and the enclosing
section always come from the entire admitted artifact; heading overflow never
returns a partial outline as if it were complete.

**Section.** Send `{citation, section: {start,end}, offset, max_chars}`.
Section bounds are absolute code-point positions and must match the root or a
complete heading section. `offset` is relative to the section, from zero
through its length, and `max_chars` accepts 1 to 16,000. All positions must be
at most 2,147,483,647, and section end must exceed start. A whole section can
exceed the citation length limit.

The response echoes `citation`, `section`, and `offset`, and returns
`actual_start`, `actual_end`, `next_offset`, `complete`, `text`,
`text_sha256`, and `text_bytes`:

```text
actual_start = section.start + offset
actual_end = min(actual_start + max_chars, section.end)
next_offset = actual_end - section.start
complete = (actual_end == section.end)
```

`text` is the exact UTF-8 byte slice for that code-point range: its code-point
count equals `actual_end - actual_start`, its byte count equals `text_bytes`,
and its lowercase SHA-256 equals `text_sha256`. At the terminal offset, `text`
is empty with zero bytes, the standard empty digest, `complete: true`, and an
unchanged offset. Beyond-end offsets and invented section bounds fail instead
of being clamped, and every nonterminal success advances the offset. You can
change `max_chars` between pages. To turn a nonempty page into a regular
citation, substitute its absolute `actual_start` and `actual_end` into the
echoed citation.

Navigation caps the artifact at 4 MiB, 65,536 LF characters, and 4,096
headings. The daemon refuses a declared oversize artifact before doing I/O.
Otherwise, the full size, UTF-8, and digest must verify and the stream must
close before the daemon reports content-dependent range errors or parser
limits. Extra or missing bytes in an admitted stream count as integrity
failures rather than ordinary limit refusals, and an interrupted stream keeps
its I/O cause. Existing deadline and cancellation precedence stays unchanged.

| Status / code | Meaning and recovery |
| --- | --- |
| Huma validation response, ordinarily 422 | Correct missing/null fields, types, schema bounds or unknown members; handler codes do not replace schema errors |
| 400 / `invalid_text_navigation` | Strict decoding rejected the body after schema validation |
| 422 / `invalid_text_citation` | Correct the citation's identities or cross-field range |
| 416 / `invalid_citation_range` | Choose an anchor inside this rendition |
| 422 / `invalid_text_navigation` | Fetch the outline and correct whole-section bounds or offset |
| 422 / `text_navigation_limit` | Use short citations within their limits; bulk download requires an attachment still active for its version |
| 404 / `citation_unavailable` | Check retained authority and bytes; the read does not regenerate evidence |
| 500 / `citation_integrity` | Investigate stored evidence; no text or outline is returned |
| 504 / `citation_timeout`, 408 / `citation_canceled` | Retry the same read deliberately later; includes admission and maintenance waits |
| 503 / `processing_unavailable` | Restore processing-service availability |
| 500 / `citation_failed` | Inspect the daemon log for the preserved cause |

The ordinary 413 body limit stays separate. The daemon connection caps each
navigation response at 256 KiB, permits Huma's `$schema`, and checks echoed
identities, bounds, and pagination. Section responses also go through
independent UTF-8, code-point count, byte count, and hash checks, while short
citation responses keep their 1 MiB bound. Without the full artifact, the
client cannot prove Markdown section boundaries.

For repeated-read cost, maintenance exclusion, and cancellation limits, see
[read lifetime and admission](document-processing.md#read-the-section-around-a-quotation).
No page keeps evidence alive for the next one. If a later page fails,
previously returned text remains partial.

#### Similar documents

`POST /api/v1/search/similar` accepts `selector`, an optional `binding_id`, an
optional `limit` from 1 to 100, and `fence`. The selector identifies a current
live file by `node_id`, `content_version_id`, and configured `profile`, and the
normalized fence must contain that version before the daemon inspects coverage.

The report returns `state`, `source`, `binding_id`, `coverage`, `results`, and
`truncated`. When `state` is `unavailable`, the response has no results and
includes `missing_coverage` with kind `embedding`, binding, profile
fingerprint, and source version. When `state` is `ready`, `results` can still
be empty.

Each result carries a rank, score, path, document identity, `blob_hash`,
`duplicate_count`, and one embedding evidence reference. The daemon excludes
the source node, collapses eligible copies with identical bytes into one
result, and applies `limit` after grouping. Because L2 scores are negative
distances, higher wire scores always rank first. Invalid source identity, stale
source, storage failures, and stale index errors remain errors. This route
starts no processing and makes no provider request. Browser sessions can POST
this exact path with an empty query string.

#### Coverage and source-fenced search

`GET /coverage` takes `profile`, `vault_uid`, and repeated `content_version_id`
query parameters. It returns `profile_fingerprint`, aggregate `state`, and
separate `renditions` and `embeddings` classes. Each class reports its name,
required status, state, and complete, unavailable, stale, ineligible, and total
counts, along with `rebuilding` and `previous_generation_serving`. A rebuilding
cell is not also counted as complete, and `previous_generation_serving` counts
the subset of rebuilding cells whose prior complete result is still available.
Because of that, a queued first build has `rebuilding: 1` and
`previous_generation_serving: 0`. Both rendition and embedding counts come from
one catalog snapshot, including current source visibility and serving evidence.
Missing required coverage keeps the aggregate state `partial` even if another
class is rebuilding. This route grants no consent and starts no provider work.

For `docbank-pdf-text`, `pages_without_text` lists source version IDs and
one-based PDF page numbers that have no text layer. Each response inspects at
most 16 candidate builds and returns at most 8,192 page numbers; if
`page_coverage` is `partial`, narrow the source fence to inspect more pages.
`page_coverage_unavailable` reports evidence the daemon could not read. Text
rendition receipts also expose `pages_without_text` for the source PDF
independently of the selected profile, or `page_coverage: "unavailable"` when
the daemon could not read its evidence.

`POST /search` takes a JSON body and is separate from ordinary lexical
`GET /search`:

```json
{
  "query": "renewal terms",
  "mode": "lexical",
  "profile": "private",
  "limit": 50,
  "content_first": true,
  "fence": {
    "vault_uid": "22222222-2222-4222-8222-222222222222",
    "content_version_ids": ["11111111-1111-4111-8111-111111111111"]
  },
  "explain": true,
  "rerank": true
}
```

The `fence` object is the source fence: the vault UUID and the content versions
the request is authorized to read. Supply the actual vault UUID and 1 to 4,096
distinct canonical UUIDv4 versions for both coverage and search. The daemon
rejects a foreign vault or an invalid source fence, and caps `/api/v1/search`
request bodies at 16 MiB.

Search accepts either `fence` or `scope`, not both. To search all live files
without listing versions, send `"scope": {"kind": "all_live", "filters": {...}}`.
Optional filters are `source_present`, `tag_id`, `mime_type`, `under_node_id`,
`under_path`, `modified_since`, and `modified_before`, using the same semantics
as source-fence resolution. Semantic and hybrid coverage report
`scoped_documents` and `complete_documents` over that population. You cannot
combine `scope` with `media_sources`, and explicit fences keep the 4,096-version
limit. A scope with no live files returns an empty report with complete 0/0
coverage for semantic and hybrid modes, or unknown coverage for lexical and
auto modes.

The CLI's automatic search, web app, TUI, and MCP filter searches send a scope,
while the Go library and MCP version lists send fences.

Search requires nonblank `query` text of at most 8,192 characters, `profile`,
and `mode` (`lexical`, `semantic`, `hybrid`, or `auto`). `limit` defaults to 20
and accepts 1 to 100. `binding_id` selects the embedding binding: when the
profile has embeddings, an unknown binding returns `404 not_found`, while
profiles without embeddings ignore it for lexical and auto search. If you omit
`binding_id`, the daemon uses the profile's first binding; set it explicitly
for semantic or hybrid search when several bindings are configured (the
explicit CLI processing form requires that choice). `auto` uses lexical
retrieval. Before choosing a mode that embeds query text, see
[processing consent](#processing-consent).

Semantic search requires a binding whose provider supports text queries;
otherwise, after validating the source and binding, the route returns
`422 semantic_unavailable`. Hybrid search on a profile without embeddings
returns that same error. An index without an active generation returns
`409 index_unavailable`, so you can try another binding while invalid
selections still return `404 not_found`. Daemon protocol revision 123 requires
these distinct errors for MCP profile selection.

For exact recording attribution, add `media_sources` with up to 4,096 distinct
objects. Omitting it keeps ordinary search, while empty arrays and `null` are
rejected. Each object names `source_id`, `source_version_id`, and
`content_version_id`, and every content version must belong to the fence. An
optional `supplied_input_ids` list of up to 64 canonical lowercase SHA-256 IDs
allows a selected supplied transcript only when its exact input ID is in the
set; omitting the list allows any selected input, an empty array allows only
generated transcripts, and `null` is rejected. Generated transcripts stay
eligible in every case. Excluded selections make coverage incomplete and use no
ranking budget, and these constraints are not echoed in evidence source
identities. Only selected transcripts contribute to results and coverage;
fence members without a selector are excluded. `source_id` and
`source_version_id` must be nonempty UTF-8 strings of at most 256 bytes. Use
`lexical` or `auto` without reranking. Docbank selects each source's covering
transcript before ranking and limits, including while a retry is pending.

Reports with `media_sources` include `media_source_selection: true` even when
there are no matches. `media_selections` lists each final-stable eligible
source tuple with its actual `origin`, `supplied_input_id` when supplied, and
`completeness`, regardless of query matches. Unready or input-excluded sources
are omitted, an empty selection summary is `[]`, and ordinary search omits the
summary. Each selected content/build pair gets its own result whose evidence
carries `media_sources` and `build_id` alongside the existing segment, excerpt,
and timing. Coverage counts content versions and stays incomplete while any
requested selection lacks ready evidence, while pending or unavailable
transcripts leave ready matches usable. Unknown, hidden, deleted, or changed
selections also contribute incomplete coverage and have their associations
removed while healthy matches stay usable. Consumers require the selection
marker to distinguish older producers.

To prefer retained content matches, including transcripts, over filename-only
matches, set optional `content_first: true`. Selection applies the source fence
before cutoff and keeps content evidence when a filename also matches, whereas
omitting the field or setting it to false keeps filename priority. `auto` and
`lexical` use this order, `hybrid` uses these lexical ranks alongside semantic
ranks, and pure `semantic` accepts the option without changing vector results.
Optional reranking can reorder the selected candidates. `POST /search/validate`
accepts the same option.

The response returns `requested_mode`, `actual_mode`, `coverage`,
`degradations`, `results`, `truncated`, and `trace` (populated when
`explain: true`). When you opt into reranking, the response also returns a
`reranking` receipt with `applied`, `degraded`, or `skipped`, a bounded
candidate count, and a degradation cause if applicable. Each result carries its
vault, node, and content-version identity with bounded evidence references. The
source fence runs before retrieval, and vector scoring uses only eligible rows
from current, live attachments. After query embedding, semantic and hybrid
search revalidate the complete result set, including lexical matches without
embeddings: trashed or replaced candidates are removed, current paths are
refreshed, result ranks are renumbered, and coverage is captured at that final
check. Removing a candidate sets `truncated: true`. Each candidate must keep
its current content version and cite current evidence. A filename match also
requires an unchanged node name, so renaming removes it, while a tag edit or
other metadata change keeps the candidate and a move between lexical and
semantic lane reads keeps the candidate under its current path. Check
visibility immediately before displaying a result. For the consumer contract,
see [Automatic and processing search](../usage/search.md).

Reranking degradation causes are `authorization_denied`, `timed_out`,
`malformed_output`, and `unavailable`. `malformed_output` covers invalid
provider scores and local inputs that cannot be reranked: a candidate with no
readable excerpt or a query over the reranking size limit. Those local failures
happen before any reranking provider call. Under
`degrade`, the response keeps the original order and adds `reranking_degraded`
to `degradations`. Under `fail_closed`, the request fails with
`reranking_failed`.

#### Processing errors and derivative purge

Before a stream starts, failures use the normal `application/problem+json`
envelope. Common processing codes include:

| HTTP status | Code | Meaning |
|-------------|------|---------|
| 428 | `processing_consent_required` | No matching grant; review and grant consent |
| 412 | `processing_consent_expired`, `processing_consent_revoked` | Existing consent cannot authorize this operation |
| 409 | `processing_plan_changed` | Preview the changed source or profile again |
| 409 | `index_unavailable` | The selected vector index has no active generation |
| 409 | `stale_index` | The vector index source changed and the index is stale |
| 409 | `rendition_operator_required` | Processing needs operator intervention; an unreadable PDF passwords file returns the fixed detail `PDF passwords file can't be read; check media.pdf_passwords_file` without its path |
| 422 | `processing_profile_unavailable`, `foreign_vault`, `version_node_mismatch` | Profile or source identity does not match |
| 422 | `invalid_processing_consent_expiry`, `search_query_required`, `validation` | Invalid expiry, blank query, or request schema violation |
| 422 | `semantic_unavailable` | The profile has no query-capable binding for semantic search, or no embeddings for hybrid search |
| 422 | `rendition_failed` | Required rendition work failed |
| 422 | `rendition_source_ineligible` | Local inspection refuses to send this source to the rendition provider; the plan's `rendition_ineligible_reason` says why. A locked PDF that no configured password opens instead creates a job that ends `failed` with `failure_code=pdf_password_required` |
| 404 | `not_found` | Requested node, version, binding, job, or active rendition is absent |
| 503 | `processing_unavailable` | The processing service is not configured |
| 500 | `processing_failed` | An otherwise unclassified processing failure |

After the first job event, inspect the terminal event; if it is missing or
reports `processing_status_unavailable`, recover through the status route.

`POST /derivatives/purge-plans` previews `content_version_ids`,
`attachment_ids`, `build_ids`, or `all: true`, with each ID list capped at
1,000 entries. `POST /derivatives/purge-jobs` accepts the same selection plus
the returned `plan_fingerprint` and emits one terminal NDJSON `result` event
with a receipt. Partial or deferred cleanup includes an `error` alongside that
receipt. Stale plans return `409 derivative_purge_plan_changed`, and invalid
selections return `422 invalid_derivative_purge`. Immutable backup copies stay
untouched.

### Query compilation preview

`POST /queries/parse` accepts one QueryV1 object and returns its canonical
`query`, `query_fingerprint`, and `dependencies` (`kind`, stable `id`, and
observed `revision`). Definition reads share one read transaction, and the
response returns no result rows, membership snapshot, or executable SQL. Both
authenticated API clients and browser sessions can call this endpoint, though
browser requests must use POST without query parameters.

The compiler preserves expression scope and separate structured filters.
Missing references, including negative tag and collection operands, fail with
`422 invalid_query`, and expression errors include a `position` object with
half-open UTF-8 byte `offset` and `end`. Backend failures stay server errors.
For grammar and limits, see
[query grammar and limits](../usage/searching.md#preview-a-field-aware-query).
Existing `/search` requests do not gain advanced syntax from this endpoint.

### Exact query snapshots

`POST /workspace/queries` executes one strict QueryV1 payload and returns HTTP
200 with its first page. Send `query`, an optional configured processing
`profile`, `page_size` of 50, 100, or 250 (default 100), and any subset of
`collections`, `tags`, `media_family`, `extension`, `modified`, `size`,
`text_coverage`, `duplicates`, and `person_role` facets, up to nine dimensions.

Set `photos: true` to capture a Photos query in its photo ordering, returning
one display-file row per matching asset along with an `asset_id` and its frozen
display-file `flag` (document snapshot rows omit `flag`). This mode uses Photos
filters and sorts, requires an empty facet list, and follows the same snapshot
paging and expiry rules described next. When you omit `profile`, photo
snapshots skip profile selection and use `unconfigured` processing coverage.
The request requires `query` and rejects `snapshot_id`. A semantic or hybrid
`mode` returns `422 invalid_query`; use a saved photo query run to capture
those modes.

The response freezes the canonical query, dependency revisions, selected
lexical generation and processing coverage, ordered row metadata, and exact
node/content-version membership observed at creation, along with exact `total`
and `total_bytes` values, `member_hash`, `snapshot_fingerprint`, `snapshot_id`,
and creation and expiry times. Later edits, moves, tag changes, content
replacements, or dependency changes never rewrite an existing snapshot; create
another snapshot to observe them.

Document rows can include `matched_roles`, the sorted, distinct roles named by
positive query operands or people filters that the named people hold on the
document within your evidence scope. A role can appear even when its query
branch did not select the row. `mentioned` marks a display-name or alias phrase
from a positive `mentions:` operand found in document text. When duplicate
collapse is active, matching members contribute roles to the result's chips.

`POST /workspace/queries/{id}/pages` accepts only the required `cursor`. Send
the returned `next_cursor` or `previous_cursor` unchanged; it is bound to the
snapshot, owner, ordering, direction, and page size. The response omits a
directional cursor when no page exists in that direction, so an empty snapshot
needs no page request.

Requested facets share the creation-time read snapshot. Each facet omits only
its matching outer structured filter so you can see alternative values, while
expression operands and nested saved-query scope stay in force. `total` counts
distinct documents in that self-excluded population, `missing` counts documents
without a value, and `other` sums value counts omitted from the response.
Because a document with several tags or collections contributes once to each
value, those value counts can exceed `total`.
[Photo browse](../usage/photos.md) counts distinct assets instead.

The `person_role` facet keeps the leading 50 people plus selected people, with
each value's optional `roles` listing the frozen roles for that person. Counts
and roles use browser-safe evidence for every caller, excluding blind-copy and
sensitive claims. Only root `filters.people` entries are omitted when counting,
while `person_ids`, expression operands, and nested saved queries stay in
force. Selecting a value adds a root people entry with the roles shown.

Each ordinary available facet keeps its leading 50 values plus any selected
QueryV1 values outside that set, including a selected value whose count is
zero. Size uses the fixed `<1 MiB`, 1 to 10 MiB, 10 to 100 MiB, 100 MiB to
1 GiB, and `>=1 GiB` buckets, and modification time uses UTC calendar-month
buckets. Fixed categorical facets include their defined zero-count buckets. If
a facet cannot be computed within its coverage, member, or time budget, it
returns `available: false` with a `reason` and omits count fields rather than
reporting a successful zero.

Materialization admits at most 250,000 rows and 64 KiB of serialized data per
row. Across the daemon, the cache admits at most 1,000,000 rows and 512 MiB of
serialized snapshot data, with at most eight handles per authenticated owner
and two snapshot builders running at once. Each build has 30 seconds, and all
requested facets share a five-second budget. Handles expire after 15 minutes
idle or 30 minutes of total lifetime. These are cache-admission bounds, not a
claim that the process uses at most 512 MiB of RSS.

Snapshot ownership follows authentication. A browser token can read only its
own handles, and revoking the browser session invalidates them; master-key
requests share the daemon's master owner. All handles live in daemon-lifetime
cache state and disappear on restart or shutdown.

`POST /saved-queries/{saved_query_id}/runs` runs the saved query revision named
by `If-Match`. Its body accepts only `profile`, `page_size`, `facets`,
`photos`, and `replaces_snapshot_id`, and cannot replace the saved QueryV1
payload. `photos` defaults to false for document-node execution; setting it to
true freezes grouped Library photo assets, rejects facets, and rejects hidden
scope. With `photos: true`, `replaces_snapshot_id` protects a live snapshot
owned by the caller from eviction while its replacement builds, while missing,
expired, or foreign handles are ignored and the run proceeds without
protection. After publication, the old handle returns to normal cache eviction.
Run receipts record this mode and compare only with earlier runs in the same
mode. The response is `{run,snapshot}`. `run` durably records the definition
revision, fingerprints, exact totals, expiry, and comparison with the previous
run, whereas the snapshot rows stay ephemeral: backup and restore retain the
receipt without reconstructing rows or silently rerunning the query. If you get
`410 snapshot_gone`, run the saved query again.

Each run compares with the preceding committed receipt for the same saved query
and execution mode, even when concurrent runs materialize out of order. The
predecessor lookup uses the saved-query index and reads only that definition's
run history.

Snapshot execution supports duplicate and text-coverage constraints, and
text-coverage predicates require a configured processing profile. Documents
reject semantic and hybrid modes with positioned `422 invalid_query` errors.
Photos saved-query runs support both modes and return retrieval status directly
on `snapshot` through `requested_mode`, `actual_mode`, `truncated`,
`coverage_incomplete`, and an optional `fallback_reason`. Relevance ordering
works for Photos and is rejected for Documents snapshots. A Photos snapshot
executes a saved `path` sort as `name asc` while keeping the original
definition and query fingerprint.

### Search exports

Search exports freeze one observation and produce dated counts with an evidence
packet. For the version 1 request and choice formats, count definitions,
evidence layout, and limits, see the
[Search exports guide](../usage/search-exports.md). Every path in this table
starts with `/api/v1/search-exports` and requires authentication.

| Method and suffix | Request and response |
| --- | --- |
| `POST /` | Version 1 request, up to 8 MiB; returns a summary with state `complete` or `needs_review`. Counts and downloads are withheld in `needs_review`. |
| `POST /selection` | Read-only comparison of exact document identities in one snapshot. Returns each selected identity with status `current`, `changed`, `trashed`, or `missing`, and current identity, path, and version dates when available. The request uses `{"documents":[...]}` and the same selection limits as report creation. Results are ordered by node ID. |
| `GET /` | `offset` (0–100) and `limit` (1–50, default 20); returns `items` containing reusable `request` and `summary`, and `total`. Lists vault history, including expired receipts. |
| `GET /{id}` | Returns the caller's live frozen summary. |
| `DELETE /{id}` | API-key release of an owned live handle; returns 204 without a body. ID must be 48 lowercase hexadecimal characters. |
| `POST /{id}/dates` | JSON `{"cursor":"","limit":50,"max_bytes":262144}`; limit is 1–100. Omitted or zero `max_bytes` defaults to 1 MiB; positive values must be 64 KiB–1 MiB. Returns `members` and an optional `next_cursor`. A page may stop within a member's candidates; check `candidates_complete` and continue with the opaque cursor. |
| `POST /{id}/revisions` | JSON `{"choices":[...]}`, up to 8 MiB; returns a new summary bound to the original observation and expiry. |
| `GET /{id}/csv` | API-key download of `search-export.csv`. |
| `GET /{id}/bundle` | API-key download of `search-export.zip`. |
| `POST /{id}/download` | Browser-session JSON `{"format":"csv"}` or `{"format":"bundle"}`; returns a one-use download `url`. |

Create accepts all-document, collection, or exact current-document scope, while
browser searches refuse `bcc:` and `blind_copy` role filters. The daemon admits
the selected identities in the same read observation as the report. A stale
selection returns `409 report_selection_changed`; call `/selection` to compare
the full set before choosing replacements. A malformed selection or a wrong
hash for a current version returns `422 invalid_report_request`. Selected-member
overflow returns `413 report_limit`, while collection-count overflow still
returns 422. For null/empty semantics and identity requirements, see the guide.

Selection comparison accepts up to 8 MiB. An empty, malformed, or duplicate
selection returns `422 invalid_report_request`, and more than 50,000 identities
returns `413 report_limit`. Because comparison checks the full identity, a
wrong hash on the current version reports `changed` here, even though report
creation rejects that hash as an invalid request.

History pages are also capped at 16 MiB. When the byte limit shortens a page,
advance `offset` by the number of returned items. Deleting a source never
removes a receipt or revokes an already captured report.

Releasing a handle frees a handle slot for either a complete or review-pending
report. It discards that handle's live packet and date evidence while keeping
history, saved files, and already published child reports with their original
expiry. Shared evidence stays in memory until its last user finishes, and a
concurrent revision can publish only while its parent stays available.

An active download blocks release with `409 report_retained`. You can retry
deliberately after the transfer closes; nothing retries automatically. Unknown,
expired, released, and other-owner handles return `410 report_unavailable`.
Browser sessions cannot release reports and receive
`403 web_session_read_only`. Downloading does not release a handle.

The create and history paths have no trailing slash. Date pages cap at 1,000
candidates and the selected byte ceiling even when you ask for a larger member
limit, and that ceiling includes the continuation cursor. A populated page
stops before the next candidate that cannot fit and returns a cursor to it,
including on the default 1 MiB pages. An invalid byte ceiling or a required
first item that cannot fit returns `413 report_limit`. Direct downloads include
`Content-Length` and `X-Docbank-Report-SHA256` for the complete artifact, while
browser sessions use the one-use ticket route instead. You cannot use an export
handle or cursor owned by another caller.

Malformed date choices return `400 invalid_report_choice`, and a choice whose
document or evidence no longer matches the frozen observation returns
`409 stale_evidence`. Request or query validation and incomplete strict
coverage return `422`. Trying to download before date review returns
`409 date_review_required`, expired or invalidated handles return
`410 report_unavailable`, and resource limits return `413 report_limit` (naming
the document when one has too many date candidates). `503 report_capacity`
means the cache is full, `503 report_timeout` means the build deadline expired,
and `503 report_unavailable` means the server has no export service.

### Rendered photo exports

Photo plans refuse a non-default `duplicate_policy` and any `volume_limits`
before rendering.

`POST /photos/authored/prepare` returns each RAW or image member's stored XMP
status without reading sidecar bytes. `POST /photos/authored/{file_id}/xmp`
accepts the conflict's `conflict_id` and a `choice` of `catalog` or `sidecar`,
returning `412` if the photo is no longer in that conflict. When either choice
imports values, it returns `200` with that import's authored receipt so other
views can apply the change; when resolution imports no catalog values, it
returns `204`. Call prepare again to read the committed status and any undoable
import receipt. The ordinary authored Undo route undoes sidecar imports. Hidden
photos require an unlock for both routes.

`POST /exports/sources` accepts a `photos` selection of up to 16 display
members, and `POST /exports/plans` accepts the `photo_rendered` role with
`photo_render` settings for JPEG or PNG, quality, long edge, image metadata,
and GPS removal. Existing Jobs and ZIP tickets deliver the reviewed artifacts.
Image metadata settings control EXIF/XMP, while the verified ZIP keeps original
filenames, vault paths, and source hashes. For details, see
[Export photos](../usage/export-bundles.md#export-photos).

### Saved query and highlight definitions

`POST /saved-queries` stores a name, an optional description, an immutable
`kind`, and one complete structured `payload`. `kind` is either `query` for
QueryV1 or `highlight_set` for an ordered list of literal text/color pairs. The
response adds a stable UUID, canonical payload fingerprint, revision,
timestamps, and a quoted numeric `ETag`. Payloads travel as JSON objects on the
wire, not base64 strings.

QueryV1 accepts these fields, applying defaults when a field is omitted:

| Field | Accepted value | Default |
|-------|----------------|---------|
| `v` | `1` | `1` |
| `text` | Up to 8,192 Unicode characters, preserved exactly | Empty text |
| `syntax` | `simple` or `advanced` | `simple` |
| `mode` | `lexical`, `semantic`, or `hybrid` | `lexical` |
| `filters` | Object described below | `{}` |
| `sort.field` | `name`, `path`, `modified_at`, `size`, `media_type`, `relevance`, `capture_time`, `import_time`, or `added_time` | `name` |
| `sort.direction` | `asc` or `desc` | `asc` |

The `filters` object accepts the following saved choices. These are storage
fields, not additional parameters for `GET /search`:

| Fields | Values |
|--------|--------|
| `paths`, `exclude_paths` | Absolute virtual paths; at most 64 in each set |
| `collection_ids`, `exclude_collection_ids`, `tag_ids`, `exclude_tag_ids` | Canonical UUIDv4 values; at most 64 in each set |
| `no_tags`, `has_duplicates`, `collapse_duplicates` | Boolean choices |
| `mime_types` | At most 64 concrete media types without parameters |
| `extensions` | At most 32 lowercase extensions without a leading dot |
| `media_families` | Array of at most 13 entries: `email`, `document`, `spreadsheet`, `presentation`, `image`, `audio_video`, `text`, `source_code`, `web`, `calendar`, `archive`, `cad`, or `unknown` |
| `modified_after`, `modified_before` | RFC3339 timestamps, normalized to UTC |
| `size_min`, `size_max` | Byte counts from 0 through 9,007,199,254,740,991 |
| `text_coverage` | Array of at most 6 entries: `complete`, `partial`, `failed`, `unprocessed`, `none`, or `unavailable` |
| `rating_min`, `rating_max` | Inclusive integer bounds from 0 through 5; minimum must be <= maximum |
| `flags` | At most 64 entries: `""`, `pick`, or `reject` |
| `labels` | At most 64 entries: `""`, `red`, `yellow`, `green`, `blue`, or `purple` |
| `kinds` | At most 64 entries, `photo` or `video` |
| `cameras`, `lenses` | At most 64 complete make/model strings, matched with Unicode case folding; each 1 through 256 Unicode characters |
| `locations` | At most 64 complete location labels, matched exactly; each 1 through 256 Unicode characters |
| `iso_min`, `iso_max` | Inclusive safe nonnegative integer bounds; zero is accepted |
| `capture_after`, `capture_before` | Strict YYYY-MM-DD local capture dates; inclusive lower and exclusive upper bounds; a photo without a capture date matches by the UTC day of its file modified time |
| `gps_bounds` | Decimal-string `south`, `west`, `north`, `east`; each at most 64 characters, latitude within -90 through 90, longitude within -180 through 180, south <= north; west > east crosses the antimeridian |
| `focus_min`, `focus_max`, `blur_min`, `blur_max`, `brightness_min`, `brightness_max`, `framing_min`, `framing_max`, `aesthetics_min`, `aesthetics_max`, `color_red_min`, `color_red_max`, `color_green_min`, `color_green_max`, `color_blue_min`, `color_blue_max` | Inclusive 0..1 decimal-string bounds, each at most 64 characters; each minimum must be <= its maximum. Numeric JSON bounds return 422. Missing measurements fail numeric comparisons |
| `unevaluated` | `true` selects live, included photo display versions with pending or unavailable measurements; structured `false` acts as omission |
| `person_ids` | At most 64 canonical person UUIDv4 values |
| `people` | At most 64 person-and-role entries, each with a canonical UUIDv4 `person_id` and a nonempty `roles` list. Entries are ANDed; roles within an entry are ORed. Browser sessions reject `blind_copy`. |
| `asset_ids` | At most 64 canonical UUIDv4 values |
| `set_ids` | At most 64 canonical album UUIDv4 values |

Typed `flags:[""]` and `labels:[""]` select unflagged and unlabeled files, while
expression operands must be nonempty. In Photos, `NOT flag:pick` matches
unflagged and rejected photos, and `NOT label:red` matches unlabeled photos and
other colors. In Documents, both also match files that are not photos.

Quality filters read the same version as other photo metadata filters: Photos
uses the asset's selected display, while Documents uses each document's own
version. `unevaluated:true` applies only to versions eligible for measurement.
Expressions reject `unevaluated:false`; use `focus_min:0` to match measured
photos.

Photo browse returns quality `ready` with scores, `unavailable` with
`signals: null` when the current grid preview is unsupported, failed, or its
verified bytes fail to decode, and `pending` with `signals: null` otherwise.

The daemon sorts and deduplicates filter sets when saving, leaves query text
untrimmed and unchanged, and rejects unknown fields and duplicate JSON object
keys. A raw payload can be at most 128 KiB, its normalized encoding can be at
most 64 KiB, and nested JSON can be at most 16 levels deep.

A `highlight_set` payload contains `v: 1` and `terms`, where each term has
`text` and `color`. Supply 1 to 64 terms with unique literal text, each 1 to 256
Unicode characters long, and lowercase `#rrggbb` colors. The daemon preserves
term order and treats terms as literal strings, not regular expressions. For
examples, see
[creation examples](../usage/searching.md#save-complete-query-intent-over-http).

Names are unique across both kinds, normalized with Unicode NFC, and must be
1 to 256 UTF-8 bytes after normalization. Blank names and control characters
are rejected. Descriptions hold at most 4,096 UTF-8 bytes, and Windows CRLF
line endings normalize to LF on create and update.

An omitted or `null` size bound is unset. `size_max: 0` keeps an empty-file
bound, while `size_min: 0` behaves like no lower bound. A minimum greater than
the maximum is rejected.

`GET /saved-queries` returns a consistent name-then-ID-sorted page with
`items`, `total`, `limit`, and `offset`. The default limit is 100 and the
maximum is 1,000. An optional `kind` selects one definition type, and
`photos=true` filters to query definitions with a nonempty kind scope
containing only photos or videos before counting and paging. The stable-ID
route returns one definition and its current ETag. `PATCH` accepts only `name`,
`description`, and `payload`: omitted fields stay unchanged, `null` and empty
patches are rejected, and `kind` cannot change. Both update and delete require
`If-Match`, a canonical no-op keeps its revision and timestamp, and replacing
`payload` replaces the entire payload rather than nested fields. A stale
revision returns `412 stale_revision`. A successful create returns `201`, while
update and delete return `200` with the resulting or deleted definition and its
ETag. Saved definitions are included in metadata backup and restore.

These definition endpoints store intent without executing a query, translating
QueryV1 into the current `/search` query string, reading document content,
rendering highlights, or returning result counts. Use the separate `/runs`
endpoint to execute a saved query. Query text stays intact, including quotes,
parentheses, and Boolean operators.

Once audit is enabled for a vault, saved-definition reads still work, but
create, update, and delete return `409 audit_mutation_unsupported`. Because the
audited history format does not yet record this mutation class, the server leaves
every saved row and revision untouched.

Current backups include saved definitions. Restoring an older supported backup
upgrades it to the current schema with no saved definitions when none were
present. A backup that holds saved-definition authority must be restored with
the release that wrote it or a newer one; downgrade readability is not
promised, and an older reader rejects the unknown authority instead of silently
dropping it.

These root-level routes sit outside `/api/v1` and are auth-exempt:
`GET /health`, `GET /api/ping` (daemon discovery), `GET /docs` and the OpenAPI
documents, and `/`, `/searches`, `/productions`, `/imports`, `/trash`,
`/photos`, `/photos/people`, `/photos/map`, `/photos/map/{set_id}`,
`/photos/albums`, `/photos/albums/{set_id}`, and `/assets/` (the static web
application when `[web] enabled` is set). Album page IDs must be canonical
UUIDv4 values.

`[web] base_path` mounts the browser app and its API requests under a
configured prefix. The daemon strips that prefix before authorization, loopback
checks, and timeout classification, leaving the endpoint contracts unchanged.
Root API endpoints stay available to CLI clients. Browser launch URLs include
the prefix, while `public_origin` still holds only the scheme and authority.
For setup details, see [proxy subpaths](../usage/web.md#serve-docbank-under-a-path).

The running daemon leaves `POST /api/daemon/shutdown` out of the OpenAPI
document it serves, though the committed `internal/api/openapi.yaml` includes
it (operation `shutdownDaemon`). It backs `docbank daemon stop` and is not
auth-exempt: it requires both the API key and its own shutdown token.
`POST /api/daemon/web-session` exchanges that master authority for a random
daemon-lifetime browser token, an independent upload-proof secret, and the
fresh loopback origin dedicated to that daemon lifetime.
`DELETE /api/daemon/web-session` revokes the calling browser session. Passing
one `duration_bucket` query value of `under_1m`, `1_to_5m`, `5_to_30m`, or
`over_30m` asks it to queue `session_ended` with `surface: web`. The route
removes the token first, and only the request that removes a live token tries
to queue the event without waiting for delivery. Unrelated query keys are
ignored; a missing, invalid, or repeated bucket, or a malformed query, emits no
event and still revokes the session. A configured API key also enables browser
key login:

| Route | Authentication and behavior |
| --- | --- |
| `GET /api/daemon/web-auth` | Unauthenticated; reports `enabled: true` only when key login is configured and the request uses the sign-in Host and, if supplied, Origin. Other hosts get `enabled: false`. Returns no vault data or credentials. |
| `POST /api/daemon/web-auth/login` | Exact Host and Origin, JSON `{"api_key":"..."}`; exchanges the configured key for scoped tab credentials and a shared HttpOnly instance cookie. Never echoes the key. |
| `GET /api/daemon/web-auth/sessions` | Master API key only; lists live key-login session IDs and creation/expiry dates. |
| `DELETE /api/daemon/web-auth/sessions/{id}` | Master API key only; revokes the named tab and cancels its resources. |

Key-login requests require both `X-Docbank-Web-Session` and the instance
cookie, and mutations also require the exact configured Origin. A missing tab
token returns 401, while a cookie, Host, or Origin rejection returns 403; the
cookie alone grants no authority. `DELETE /api/daemon/web-session` signs out
only the calling tab, leaving the shared cookie and other tabs intact. Login
accepts at most 32 KiB of JSON (enough for a 4096-byte key with every character
escaped) and rejects unknown members. Because the same key works on every API
route, login has no failed-attempt lockout; instead, configuration requires a
key of at least 32 characters when `public_origin` is set. Tab credentials are
process-local; reloads and daemon restarts require a new key login. Every
request must pass the server Host allowlist. `public_origin` adds its host and
`[web] allowed_hosts` to that list and enforces a stricter check: every
shared-listener request, including API-key and unauthenticated health/static
requests, must use one of those authorities or a concrete loopback backend
authority. Protected responses carry `Cache-Control: private, no-store` and
vary on `Cookie` and `X-Docbank-Web-Session`. For lifetime, deployment trust,
and session-management commands, see
[server sign-in](../usage/web.md#sign-in-on-a-server-or-container).

A hidden `GET /api/daemon/web-map-tiles/{z}/{x}/{y}` (not in the OpenAPI
document) fetches one map tile from the configured provider so the provider URL
and key never reach the browser. It exists only when `[web] enabled` and
`[web.map_tiles]` are set, accepts a browser session or the API key, returns
400 for coordinates outside the zoom range, and returns 502 for any provider
failure. For configuration, see
[photo map tiles](../configuration.md#photo-map-tiles).

Browser session tokens authenticate only the routes used by the built-in
document, photo browsing, album, tag-definition/assignment, saved-definition,
recoverable-trash, folder-upload directory creation (`POST /api/v1/nodes`,
directories only), storage, job, configured-backup, verified-download,
map-tile, and usage-event workflows. They are not general API credentials.
Browser file bytes travel over the hidden `/api/daemon/web-upload` WebSocket
instead: the page verifies a challenge proof over the upload secret before
sending bytes, binds the socket to one session, and verifies a fresh proof when
replacing it. On an unconfirmed file retry, the daemon checks for an existing
matching upload before receiving bytes. During stored-content verification,
`verification_progress` messages carry the request ID and the increasing
`verified_size` read from storage, and each message renews the browser's
60-second inactivity deadline; a stalled check ends without sending file bytes.
An ordinary browser token is forbidden from `POST /api/v1/uploads`.

Photo folders use `plan_photo_import` and `begin_photo_group` on
`/api/daemon/web-upload`, with server grouping and hash verification before
each group commits. The plan sends an empty string for `expected_hash` on files
the browser has not hashed yet. When such a file's size matches an eligible
earlier browser photo, the server replies `photo_import_hashes_needed` with
those `indexes`, and the browser resends the plan with only those hashes. Other
files hash with their group, and the server pins each hash on its first
attempt. The plan returns an `import_id` owned by that connection, and each
group must reference it and match the planned paths and sizes. The selection
shares one ingest run. An optional `source_id` UUID in the plan is kept
separately from the connection's plan ID and appears in browser provenance
paths; omitting it creates a new source, and browser source IDs preserve
provenance and grouping. The `GET /api/v1/photos/browser-imports` route returns an `import` summary by optional `source_id`, or the newest recorded browser import. The vault records no completion marker, so a finished import may also be offered. Recovery keeps a checkpoint's source ID. Without a checkpoint, it reuses the sole earlier import with at least one selected file at the same relative path and hash, provided no selected path has different recorded bytes; no match or multiple matches starts a fresh import. Recovery requests every file's hash and marks a group present only when every member matches the chosen import's exact path and bytes. For a checkpointed import, a selected path with different recorded bytes returns `photo_import_changed`. Recovery keeps paths under its chosen source and ignores folder-continuation records, so renamed or previously continued folders can be uploaded again; new selections retain their usual continuation behavior.

Before uploading, the browser saves the source ID, destination, picker kind,
and a versioned SHA-256 digest over the sorted complete manifest of paths,
sizes, and modified times through authenticated
`/api/daemon/photo-import-checkpoint` routes. The vault stores one private
version-2 JSONL checkpoint per browser profile outside database, audit, and
backup records. A persistent HttpOnly, SameSite=Strict cookie scoped to
`docbank.localhost` locates it across fresh random daemon origins without
carrying authorization, and a header round trip confirms cookie persistence
before saving or uploading. `GET /api/daemon/photo-import-checkpoint/header`
reads only the bounded identity record, `GET /api/daemon/photo-import-checkpoint`
reads the selection digest and picker kind, and
`PUT /api/daemon/photo-import-checkpoint` checks the expected source and
revision before atomic replacement or clearing. Both PUT fields are required
and nullable: `expected: null` creates a checkpoint, and `next: null` clears
it. Checkpoint requests allow 64 KiB, and the upload plan allows at most 32,768
files in an 8 MiB declaration. Successful writes refresh the 365-day profile
cookie, clearing removes the checkpoint file, and minting a new profile cookie
removes matching checkpoints and atomic temporary files older than 366 days.
Host and Origin must match the current browser origin. Resume verifies the full
selection and reuses that source ID on a fresh connection, while a new
selection gets a new source ID. Checkpoint replacement detects a competing tab,
and confirmed completion clears it. Receipts include the ambiguity reason when
pairing needs manual review, and daemon-host path imports still require the
master key. Control messages are capped by `type`: 8 MiB for
`plan_photo_import`, about 2.1 MiB for `begin_photo_group`, and 4 KiB for every
other message. The server closes the socket with status 1009 when a message
exceeds its limit.

Attachment navigation also allows exact `GET /api/v1/versions/{id}`,
`GET /api/v1/email-document-publications/{operation_id}`, and bounded
`GET /api/v1/email-document-relations` reads. Relation queries require either
one parent version or one child version, accept an optional paired
operation/order cursor and a page limit of at most 250, and reject unknown or
repeated parameters. These browser capabilities do not allow publication,
removal, email processing, or consent mutations. The inspector checks relation
and receipt agreement before presenting inventory authority, and separately
checks the related version's current node access before opening verified
content.

`GET /nodes/{id}/children` binds the live directory projection (including its
current canonical path) and the requested child page to one read transaction,
so refresh clients never mix an earlier directory name with a later child
listing.

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename cannot strand a concurrent client's
reference.

For stable-identity moves, `PATCH /nodes/{id}` accepts either the lower-level
`new_parent_id`/`new_name` fields or one absolute `dest_path` (mutually
exclusive). The `dest_path` form applies POSIX-style destination rules and
checks `If-Match` in the same transaction. `POST /path/move` is the
coordinate-oriented form when the source path itself is the intended target.

Directory creation offers the same identity-versus-coordinate choice.
`POST /nodes` takes a previously resolved `parent_id`, so a concurrent parent
move does not change which directory receives the child. `POST /path/mkdir`
takes `{"path":"/projects/2026"}` and resolves the existing parent inside the
creation transaction, preventing the coordinate from being redirected between
separate client requests. Both return the new node and its canonical path from
that transaction, and neither creates missing ancestors.

`POST /batch/move` accepts `{moves:[...]}`. Each item identifies its source
with either `source_path` or `node_id` plus your previously inspected revision,
and supplies `destination_path`. Every selector resolves against one
pre-transaction topology, each destination is an exact final coordinate, and
its parent resolves against the planned final topology. Batch requests do not
apply the single-move "move into an existing directory" shorthand. Because
Docbank resolves and checks requested paths before applying moves, file and
directory swaps and nested reorganizations never rely on unsafe intermediate
names, and any failure rejects the whole plan. The response preserves request
order and returns each node's prior path alongside its complete final node
projection and path. Receipts reuse the validated final paths and read updated
node metadata inside the same transaction, avoiding a second ancestry walk for
each move.

### Backup repository endpoints

`POST /backup/init` accepts `{"repo": "/absolute/server/path"}` and returns the
repository identity and canonical path. You can omit `repo` when `[backup] repo`
is configured.

`GET /backup/schedule` returns the runtime overrides `interval_seconds` and
`keep_last` (when set), along with `effective_interval_seconds`,
`effective_keep_last`, and the vault settings `revision` (also sent as `ETag`).
`PUT /backup/schedule` requires `If-Match` and accepts `interval_seconds`,
`reset_interval`, `keep_last`, and `reset_keep_last`. It rejects an empty body,
`null`, and any value that would leave an enabled schedule invalid against
`config.toml`. Both routes require the master API credential, and the
repository path is not a runtime override.

`POST /backup/claim` accepts `{repo?, fingerprint?}` to assign a repository's
history to this vault installation. Omit `fingerprint` to preview its `id`,
`path`, `snapshot_count`, optional `newest_at`, `owned`, `takeover`, and
`fingerprint`. `takeover` means another installation of this vault, such as the
original of a restored copy, currently owns the repository and loses capture
access once you confirm the claim. Send the preview's `fingerprint` to confirm;
if the repository changed since the preview, the route returns
`409 backup_claim_changed`. A claim cannot take another vault's repository, and
it uses the same repository selection and master API authority as
initialization.

`POST /backup/snapshots` accepts the same optional repository plus `tag`,
`jobs`, and `force_unlock`, returning a stable logical summary instead of
exposing Kit's physical manifest layout. The `/backup/snapshots/stream` variant
accepts the same body and returns `application/x-ndjson`: zero or more
`progress` events followed by exactly one terminal `result` or `error` event. Progress
events carry stage, item counts, byte counts, and a final-stage marker so
clients can render progress bars without parsing human text. Because response
headers commit when streaming begins, HTTP 200 only means the stream started;
read through EOF and require the terminal event. The CLI uses this variant for
human output and the single-JSON endpoint for `--json`.

`GET /backup/snapshots?repo=...` returns `{repository: {id, path}, items: [...]}`
so you can identify the repository behind the immutable manifests and
pagination can be added later without breaking a top-level array contract. A
browser session can call this GET only without `repo`, confining the web app to
the daemon's configured repository. Backup mutations and arbitrary server-path
selection still require the master API authority. Because explicit `repo`
values and every restore name daemon-host paths, they also require a loopback
peer; non-loopback clients get `403` (`loopback_only`) and can create, list,
and verify snapshots only in the configured repository.

Explicit repository paths are paths on the server's filesystem and must be
absolute. The CLI resolves a relative `--repo` against its own working directory
before sending it, while API clients over an SSH tunnel must pass paths valid on
the daemon host. Every endpoint requires the daemon API key.

### Audit expected-evidence verification

`POST /audit/verify` accepts an empty body for a fresh proof. To prove
ancestry, pass the `evidence` object from a previous successful report:

```json
{"expected":{"vault_id":"...","lineage_id":"...","operation_sequence_high_water":12,"allocation_entry_count":12,"allocation_head":"...","scopes":[{"id":"...","entry_count":9,"chain_head":"..."}]}}
```

The daemon independently replays the current vault before comparing. A
successful prefix proof returns `evidence_check: {"extends":true}`, while
evidence disagreement still returns an HTTP 200 verification report so you can
inspect current terminal evidence alongside protected-byte problems.
`evidence_check.problems` uses the stable codes `audit_not_enabled`,
`vault_mismatch`, `lineage_mismatch`, `allocation_shorter`,
`allocation_diverged`, `scope_missing`, `scope_shorter`, and `scope_diverged`.
Malformed expected evidence returns a `422 validation` request error.

### Background-job status

`GET /jobs` returns `{items: [...]}` sorted by stable job name, plus optional
`lanes` listing every controllable lane sorted by lane (empty when controls
cannot be read). Each item carries `name`, `status` (`queued`, `running`,
`completed`, `failed`, or `cancelled`), and a UTC `started_at`; terminal jobs
add `finished_at`, and failures add a bounded `error`. Supervised task records
describe the current daemon run only and disappear on restart. To stop one,
stop or reconfigure the daemon feature that owns it. Photo previews also have
durable lane controls.

Every job includes `controllable`, `paused`, and `can_set_concurrency`, and
controllable jobs add `concurrency` and `control_revision`, with `kind` or the
supervisor `name` identifying the lane. Document and maintenance workers have
read-only controls. `GET /jobs/lanes/{lane}` returns settings and their
revision, and `PUT /jobs/lanes/{lane}` accepts `paused` and `concurrency` with
the current revision in `If-Match`. Missing preconditions return `428`, stale
ones return `412`, unsupported lanes and storage concurrency changes return
`400`, and preview concurrency outside 1 through 4 returns `422`. An unreadable
`lane-controls.json` returns `500` with code `lane_controls_unreadable`, while
browser sessions receive a generic `internal` error.

Supported lanes are `derive:visual-previews`, `photo_import`, `photo_checkout`,
`place`, `evacuate`, `repair`, and `salvage`. Only previews accept concurrency
changes; storage lanes keep concurrency 1 in this contract and retain their
existing execution limits. Pausing finishes the current photo, import group, or
storage object before waiting. Controls survive restart in the vault's `lane-controls.json` and stay
out of backups; restored vaults start with unpaused lanes, concurrency 1, and
revision 1. When that file cannot be read, `GET /jobs` still returns every
job without control fields and adds `lane_controls_error` (replaced with a
generic message for browser sessions). Lane-control reads require the daemon
API key; browser sessions read settings through `GET /jobs` and can `PUT`
supported lanes without query parameters.

Durable storage operations, such as placement and photo imports, also appear as
`storage:<operation_id>` items with `operation_id`, `kind`, object progress,
`cancel_requested`, and `can_cancel` while queued or running. The list returns
the newest 1,000 durable storage records. `GET /jobs/{operation_id}` returns
the full operation and its receipt, and `POST /jobs/{operation_id}/cancel`
requests cancellation at the next durable object boundary (returning `409` once
the operation is terminal or finalizing). A browser session can list jobs with
their lane settings, update supported lane controls without query parameters,
and cancel an operation by UUID, but it cannot read `GET /jobs/lanes/{lane}` or
`GET /jobs/{operation_id}`. For browser sessions, error text is replaced with a
generic message and the cancel response omits the receipt so host source paths
stay with API-key clients.

`GET /watches` returns `{items: [...]}` sorted by stable watch name. Each item
joins the effective machine-local source, virtual destination, settle and scan
durations, and `exclude` and `exclude_patterns` lists with its current
`watch:<name>` job record. `exclude_patterns` is effective, including default
patterns unless `default_excludes = false`. Watches with a single processing
profile include `processing` with `profile` and `consent_state` (`active`,
`required`, `expired`, or `revoked`), while mapped watches include
`processing_routes` sorted by `media_family`, with one entry per family holding
`media_family`, `profile`, and its consent state or lookup `error`. The
corresponding watch jobs include the same routes. If consent lookup fails,
`error` describes the status failure instead. `GET /jobs` includes the same
optional status on watch jobs so browser sessions can display it without access
to machine-local watch configuration. Scanner failures stay in the job's own
`error` field. The job is omitted only when no runner has been registered,
which does not happen in an ordinary live daemon. Configuration stays
file-owned: this endpoint does not create, edit, or restart watches.

### Path resolution: a query parameter, not a URL segment

`GET /path` takes the virtual path as `?path=/inbox/doc.pdf` instead of a
catch-all URL segment (`/path/{path...}`). Stdlib-mux decoding of a wildcard
segment becomes ambiguous for names containing `/`-adjacent percent-encoding,
whereas a query parameter has one unambiguous encoding. The path must be
absolute (leading `/`), and `?path=/` resolves the root. The server applies the
store's NFC name normalization and validation and returns `422` for an invalid
path.

## Live photo changes

`GET /photos/events` returns an authenticated `text/event-stream`. Each
`change` frame is `{}`: it signals that a committed change touched data Photos
reads, without naming any photo. Every frame goes to every open stream,
including streams without a Hidden unlock; each client refetches what it shows,
and the read path hides photos from locked viewers. Unread frames merge into
one. The stream sends no initial event and never replays missed changes, so
clients refetch after every connection. The daemon sends a keepalive comment
every 15 seconds and allows at most 128 concurrent streams across all clients;
further requests return `429 photo_event_stream_limit`. Writers signal changes
only while a stream is open.

## Batch tag assignment

`POST /api/v1/batch/tags` accepts a canonical UUIDv4 `operation_id`, a tag UUID
`tag_id`, a required boolean `assign`, and `nodes: [{node_id, revision}]`. The
body is capped at 1 MiB and accepts 1 to 1,000 unique live file or directory
IDs with positive revisions. This route uses no query, subtree expansion, or
`If-Match` header. Invalid structure returns 422, missing or trashed targets
return 404, and a stale target returns 412 (`stale_revision`). Every target,
including assignment no-ops, is validated in the same transaction as the
changes and receipt persistence. Actual changes write the existing canonical audit events
with a separate audit operation for each changed node; the receipt's operation
ID identifies the batch retry, not an audit operation ID.

Success returns 200 with `version: 1`, `operation_id`, `request_digest`,
`tag_id`, `assign`, final `tag_revision`, final `assignment_count`,
`completed_at`, and numerically sorted
`nodes: [{node_id, expected_revision, revision, changed}]`. Every requested
identity appears once: changed nodes advance by one revision, and unchanged
nodes keep their expected revision.

Request order does not affect semantics. The lowercase SHA-256 request digest
hashes UTF-8 text: `docbank-tag-batch-v1` followed by a newline, the canonical
tag UUID followed by a newline, `1` for assignment or `0` for removal followed
by a newline, and one `node_id:revision` line per target in ascending numeric
node-ID order. Every line, including the last, ends with a newline, and
integers use unpadded decimal. The operation UUID is a separate lookup identity
and does not enter that digest.

Repeating an operation with the same canonical request returns its original
receipt even after later edits or deletion, while reusing the operation UUID
with different input returns 409 `batch_tag_operation_conflict`. Receipts stay
indefinitely without node/tag cascade deletion and contain no names, paths, or
document bodies. Metadata and physical backup and restore preserve receipts
present at the backup checkpoint; an earlier backup cannot know about later
operations. A historical success does not assert current membership or recreate
a deleted entity. Clients must validate complete receipt identity and revision
outcomes before accepting success.

`POST /api/v1/batch/tags/preview` takes only `tag_id` and the same `nodes` list
with the same limits and revision checks. One read snapshot returns `tag_id`,
`tag_revision`, and sorted `nodes: [{node_id, revision, assigned}]` without
making durable changes, giving you exact selected-set membership without
inferring omissions from a truncated tag listing. Both routes require
authentication, and browser sessions can call only these exact POST paths
without query arguments.

## People search

`GET /api/v1/people` and MCP `find_people` search active people by the start of
any word in their folded display name. A read-time Unicode word-start predicate
filters names before visibility checks and pagination, while literal `%`, `_`,
and NUL keep their meaning (for example, `Smith` matches `John Smith` and
`John—Smith`). The daemon reads identity labels only for the returned matches
and orders results by folded name and person ID, returning a query-bound
`next_cursor` when another page exists.

Each item carries `person_id`, `display_name`, `state`, and `revision`.
`display_name_folded` lets you group case variants consistently across pages,
and `created_at` gives a date fallback when a namesake has no visible identity.
The optional display-only `identity_label` prefers email, then phone, then a
handle with its scope, breaking ties by normalized value and identity ID. It
identifies a chooser row without asserting a new identity or changing the
selected person ID. Browser labels include operator-entered identities and
identities backed by visible, non-sensitive document evidence; locked Hidden
photos never supply a label, whereas CLI and MCP labels use global visibility.

The optional `same_name_continues` flag is true when the last item's folded
name also starts the next page. The browser chooser reads one page per search
or explicit continuation, withholding a namesake group that spills onto the
next page until you select **Load more** to complete it, and keeping person
creation unavailable while a group is incomplete. Other paginated callers can
follow `next_cursor` regardless of the flag.

Active operator assertions other than `blind_copy` make names searchable on
visible versions before document-people publication. Browser search omits
people known only from automatic sensitive document actors or blind-copy
assertions even while Hidden is unlocked, and omits people whose only evidence
lives in locked Hidden photos until unlock, while keeping people with visible,
non-sensitive evidence or manually created unlinked people. CLI and MCP search
stay global. Automatically provisioned exact-identifier people stay hidden
while document processing is pending or failed until a visible assertion or
non-sensitive published document link backs their name. Unlinked people with
supplied identities stay searchable with blank identity labels. The browser
explains the duplicate risk inline, lets creation proceed directly, and lets
you merge a duplicate after unlock. Browser sessions can search people, create
a person while naming a face, rename people, and merge people. Separate person
creation, person details, retirement, and splitting require the API key.

Setting `photo_summary=true` adds each person's distinct `library_count`, a
separate `hidden_count` while Hidden is unlocked, and an optional `face` with
the exact current source version, normalized crop, rotation, and ready grid
generation. Counts require a current, live, included source and count a paired
RAW/JPEG asset once. The portrait uses a readable key face (including Hidden
while unlocked), or otherwise prefers assigned Library faces over Hidden faces,
then a ready preview, then the largest face box; `face_hidden=true` marks a
Hidden portrait. The page lists only people with a visible photo (Library, or
Hidden while unlocked), and the browser serves this view at `/photos/people`.

Setting `recent_first=true` ranks name groups by the newest `recorded_at` among
active, visible `depicted` assertions, then by folded name and ID, keeping
namesakes together at their group's rank. Naming a person on a new photo
records a new assertion and moves the group up, while adding a face or
whole-photo mark to an existing active assertion keeps its time, and merges
keep retained and moved assertion times. Locked Hidden evidence supplies no
rank, and people without visible depicted evidence follow in name order.
Because paging reads live ranks, a falling rank can repeat a person and a
rising rank can move a group ahead of the cursor and skip it for the rest of
that pass; restart from the first page to see skipped groups. Cursors bind the
query, projection, order, and Hidden scope for browser, photo-summary, and
recent-first listings. A refused cursor returns `422 cursor_expired`, which
requires restarting from the first page.

Recent-first ranking starts typed-name searches from matching active people and
looks up each person's newest eligible `depicted` assertion through the
timestamp-ordered `person_document_assertions_person` index, with namesakes
sharing the newest time. Empty queries scan the assertion table, including
unrelated mail assertions, and finish timestamp ties before stopping.

Measured costs on an AMD EPYC 7763 with CGO SQLite use two fixtures: 3,000
people and 40,000 photos, and 50,000 people, 10,000 mail contacts, and
2,025,400 assertions.

| Benchmark case | 3,000 people / 40,000 photos | 50,000 people / mail-heavy |
| --- | ---: | ---: |
| `page-24-name` | 0.796 ms | 0.812 ms |
| `page-24-recent` | 607.109 ms | 281.015 ms |
| `page-24-name-summary` | 418.499 ms | 334.208 ms |
| `page-24-recent-cursor` | 611.103 ms | 325.532 ms |
| `page-24-recent-summary` | 617.731 ms | 322.855 ms |
| `page-100-recent-prefix` | 3.011 ms | 24.040 ms |
| `page-100-recent-prefix-broad` | 101.775 ms | 442.267 ms |
| `page-100-recent-prefix-none` | 2.035 ms | 23.165 ms |
| `page-24-recent-unranked-tail` | N/A | 352.098 ms |

The assertion index occupies 201.8 MiB on the mail-heavy fixture, and an
existing vault builds it once on its first open after upgrade. To measure the
standard cases, run
`CGO_ENABLED=1 go test -tags fts5 ./internal/store -run '^$' -bench 'BenchmarkPeopleRecentAtVaultScale/(3000|50000)' -benchtime=3x -count=1`.

Browser rename and merge apply the same visibility rule to the canonical person
inside the write transaction. A person the browser cannot list, such as a
sensitive-only person or a locked Hidden-only person, returns `404 not_found`.
While Hidden is locked, browser merges require unlocking if either person has
assertions or face candidate evidence on a Hidden photo. A merge replay
resolves the survivor through aliases and checks its current visibility before
returning the original receipt, even after another merge. Browser sessions
receive merge receipts with zero moved-evidence counts and an empty absorbed
name, while API-key writes keep their full scope.

A face conflict returns `409 person_merge_conflict` with up to 20 visible
`face_ids` and matching `face_version_ids`. Each version is the exact content
version holding the conflicting face, which can be a retained older version.
The detail lists each face/version pair and says "more faces also conflict"
when the list is truncated. `hidden_face_conflict` and
`inaccessible_face_conflict` report conflicts you cannot open.

## Photo faces

`GET /api/v1/photos/people/{person_id}/face-previews` returns up to three live
linked photos in `items`, ordered by timeline (`sort_key DESC, asset_id ASC`).
Within each photo, source selection prefers a boxed face, then a ready grid,
then larger face area, breaking ties by source version and face ID. Each item
carries `asset_id`, the face's exact current source `content_version_id`, its
authored `rotation`, an optional normalized `box`, and a ready grid
`generation_id` when available. Whole-photo marks have no box, and dismissed
faces and withdrawn assertions supply no crops. A paired RAW uses its own
preview to keep its framing, and the chooser prepares missing source grids from
these three candidates per person. Person evidence and person-scoped preview
preparation and delivery use the current version of a live image/RAW member.
Browser sessions can call this route; `hidden=true` selects Hidden photos and
requires the existing unlock cookie.

`GET /api/v1/photos/versions/{content_version_id}/faces` reads a current or
retained image/RAW version of a live photo member and returns `faces`,
`assertions`, and `people` (at most 1,000 each). The page-only `people` list
maps each referenced `person_id` to its canonical `display_name` and `state`,
read in the same snapshot as the faces and assertions. Retired labels stay
readable, while browser Library links require active people. Assertions carry
person IDs and include saved `depicted` links with action `assert` as well as
suppressed links still referenced by a face; check `action` before treating a
person as depicted. Each assertion includes its ID, canonical `person_id`,
version, revision, and recording time. Whole-photo marks appear in `assertions`
even when there are no face boxes. MCP `get_photo_faces` returns canonical
`people` only for its current face/assertion page and recalculates them when
the response byte limit trims that page.

`POST` on the same path accepts `box: {x, y, width, height}` and optional
`person_id`, or `person_id` alone for a whole-photo mark. To create a person
with that mark, also pass `person_name` and a caller-chosen UUIDv4 `person_id`,
and keep both fields for retries. A new name must be nonblank and at most 200
UTF-8 bytes. Passing a name without an ID, an existing ID with a different
name, or `person_name` with a box returns `422 invalid_photo_face`, and a
retired person returns `404 not_found`. Repeating the same ID and name reuses
the saved person and assertion. Because the person and mark save in one
transaction, a refused mark leaves no new person behind. A boxed request
requires a caller-supplied UUIDv4 `face_id` (keep it for retries: repeating the
same ID, version, and box returns the current saved result, including a named
face whose person has since retired, and any supplied person must match its
canonical person). Conflicting reuse returns `409 exists`, and writing past
1,000 faces or 1,000 depicted people on one version returns
`409 photo_face_limit`. The response returns `face` for a box and the saved
`assertion` when a person is supplied; repeated whole-photo marks reuse their
assertion.

Boxes use normalized coordinates in the EXIF-oriented preview before authored
rotation. API-key clients can call every face route. Browser sessions can list
faces, create boxed faces, mark an existing person or create a person by name
in a whole photo, and name, clear, reject, or remove a face through
revision-fenced `name`, `retract`, `reject`, or `dismiss` decisions; they can
also clear an existing whole-photo mark with `retract`, its `assertion_id`, and
its assertion `revision`. `withdraw_rejection` and `key` require the API key,
and Hidden photos also require the existing unlock cookie. Replies carry
`Cache-Control: no-store`, and audited vault writes fail with
`audit_mutation_unsupported`.
[Automatic face detection](https://github.com/kenn-io/docbank/blob/main/docs/configuration.md#automatic-face-detection)
adds unnamed faces when configured and consent is granted. Face suggestions use
the local face embedding profile after consent, and `docbank processing faces`
installs the face runtime and model assets. For replay, merge, and backup
rules, see
[photo face storage](https://github.com/kenn-io/docbank/blob/main/docs/internal/storage-design.md#photo-face-authority).

If an older text embeddings server is running, `docbank processing faces`
downloads the required assets and prints the server PID to restart. Stop that
server and rerun the command to start the updated runtime and turn on face
suggestions.

`POST /api/v1/photos/versions/{content_version_id}/face-decisions` takes
`face_id`, `revision`, and `action` (`name`, `retract`, `reject`, `dismiss`,
`withdraw_rejection`, or `key`). Naming accepts either `person_id` or
`person_name` (up to 200 UTF-8 bytes), and `retract` accepts either one
`face_id` or one whole-photo `assertion_id`. Clearing a whole-photo mark keeps
its assertion active while named faces still use it, and withdraws it
otherwise. A new name creates the person and assigns the face in one
transaction, rolling both back if the revision check fails. Keep `operation_id`
for an identical replay after an uncertain response. Rejection, withdrawal, and
key selection take `person_id`; key selection also takes `person_revision`, and
omitting `face_id` clears the key. Existing IDs replay through create, while
edits use this decision route. Each decision returns `receipt_id`. Undoing a
face's new-name decision retires the new person unless it was edited or has
other evidence, and undoing that undo returns not found once the person is
retired; clearing a name or whole-photo mark leaves the person intact.
`POST /api/v1/photos/face-decisions/{receipt_id}/undo` restores changed rows
while their saved revisions still match, returning `412 stale_revision` after a
conflicting edit. Face reads include `rejected_person_ids` and `dismissed`;
dismissed records stay in the API for undo and disappear from browser rows,
boxes, and closeups.

`GET /api/v1/people/face-candidates` returns open face suggestions as `items`
and `total`, accepting `limit` (default 24, maximum 250) and `offset` (maximum
10,000). Each item carries `candidate_id`, `revision`, `display_name`, optional
`suggested_person_id`, and exact source `faces`, with visibility filtering
running before pagination. Known-person suggestions propose that person, while
unnamed groups require choosing or creating a person.

`POST /api/v1/people/face-candidates/{candidate_id}/decision` requires the
candidate revision in `If-Match`. Actions are `link` with `person_id`,
`new_person` with `display_name`, or `reject`. Optional `face_ids` selects up
to 48 faces to confirm, leaving omitted faces unnamed. Rejecting an unnamed
group dismisses that exact suggestion while keeping visible, nameable faces,
and changed evidence can form a different suggestion. Browser sessions can make
these decisions under the existing Hidden and person visibility rules. The
response returns the new candidate `revision` and `state`, with the revision in
`ETag`.

Person detail accepts `assertion_cursor` and `assertion_limit` (1 to 1,000) and
returns `assertions_next_cursor`, `key_face_id`, and document and photo
assertion selectors, with Hidden filtering running before pagination. Merge
conflicts include up to 20 visible `face_ids`, `more`, and
`hidden_face_conflict`, and retained versions can be corrected through the same
Hidden gate.

## Concurrency: resource revisions and `If-Match`

Photo assets are a revisioned graph over ordinary file nodes. Image files and
concrete `video/*` files enroll on creation, while generic RAW files require an
explicit promote or create call. Enrollment is forward-only and uses
`email_document_relations.child_version_id` to exclude published email
children; processing metadata does not turn its source into an email child.

`GET /api/v1/photos/assets/{asset_id}` and
`GET /api/v1/photos/nodes/{node_id}/asset` return at most 256 members, the
resolved display pointer, its source (`asset`, `vault`, `default`, or `none`),
and the asset ETag. The default display order is RAW, image, then video; the
vault `image` preference puts image before RAW, and an asset override wins over
both. A sidecar must point to a same-asset RAW or image member and is never
displayable.

Create and promote act on live file nodes, while attach, detach, exclude,
display, and settings mutations require `If-Match`. The store verifies the
revision inside the transaction and returns the new ETag. A changed human
decision writes one bounded immutable photo receipt, whereas a no-op decision
keeps the revision and writes no receipt. Permanent node deletion repairs
affected graphs while keeping an empty asset identity.

`POST /photos/assets/{asset_id}/trash` requires the asset revision in
`If-Match`, moves all live members together, returns the updated asset and
ETag, and writes one receipt. An asset with no live files returns
`422 invalid_photo_asset`. Each photo moves atomically; browser selections
submit sequentially and can make partial progress. Audited trash follows the
existing audited node rules.

`POST /photos/rejects/preflight` accepts `{query, hidden, coverage}` and
returns complete counts, mixed flags, and `targets` in asset-ID order, capped
at 1,000 live files. A photo that would push a batch past that limit waits for
a later batch while smaller photos after it still join the current one.
Coverage uses the same configuration and profile selection as photo queries.
`POST /photos/rejects/trash` accepts `{hidden, targets}` and moves those photos
atomically, returning `moved` in target order. Each target carries `asset_id`,
the asset `revision`, and `member_revision` (the sum of the original members'
node revisions). A missing target, an asset or original revision changed by
more than sidecar syncing, a changed reject flag, or a changed Library/Hidden
scope returns `412 stale_revision`. `unchanged` counts scoped photos that stay
in Docbank, including mixed pairs, and `photos` includes eligible photos left
for later batches. Audited trash conflicts return
`409 audit_mutation_unsupported` with the asset ID and failing file name.
Invalid or duplicate targets, or more than 1,000 live files, return
`422 invalid_photo_asset`, and request schema violations (including fewer than
1 or more than 1,000 targets) return `422 validation`. Hidden scope requires an
active unlock at both steps.

Paginated `GET /trash?limit=&offset=` groups independently trashed photo
members into one item with optional `photo_asset_id` and `photo_file_count`
(the count of trashed files recovered together), while unpaged listing stays
per node. Restoring any member recovers its entire group atomically, including
companions inside trashed folders, under the selected member's revision.
Ordinary Documents deletion can trash one member, but permanent deletion waits
until every connected member is trashed, old enough, and free of retention
references; trash the remaining members or detach live companions to make a
partial group eligible.

Photo assets, file memberships, settings, and receipts travel in the
deterministic metadata JSONL stream and are validated as one graph on restore.
Older supported metadata streams restore an empty photo authority. Audit-active
vaults skip automatic enrollment and refuse membership, display, and settings
mutations while keeping any graph that existed before audit was enabled.
Display and settings writes work through HTTP and the CLI; MCP exposes them
only as reads in this slice.

`GET /photos/albums` lists albums with `member_count` for members that still
have a file, `included_count` for visible included members, and current ready
grid covers. `POST /photos/albums` creates an album,
`GET /photos/albums/{set_id}` inspects it, `PUT /photos/albums/{set_id}`
updates optional `name` or `starred`, `PUT /photos/albums/{set_id}/cover` picks
a cover member by `asset_id` (or clears the override when omitted),
`POST /photos/albums/{set_id}/duplicate` creates a copy with a new `name`, and
`DELETE /photos/albums/{set_id}` removes the album while keeping its photos.
Writes to existing albums require `If-Match` and return the album with its
ETag.

Album creation accepts `name` and optional paired `snapshot_id` and
`member_hash`. A frozen snapshot copies all asset members atomically, up to
250,000; expired snapshots or members no longer eligible for Library reject the
whole write. Later metadata or display-file edits keep eligible frozen
membership intact.

Album summaries compute both counts in one membership scan: a display file
proves a member still has a file, and members without one fall back to a
file-existence lookup. Visibility and trash filters apply only to the included
count, so hidden, excluded, and trashed photos keep their album membership
count. Counts and cover selection share one read transaction, and a zero
included count skips the cover search because no visible cover can exist.

`POST /photos/albums/{set_id}/members/add` and `/members/remove` accept either
`asset_ids` (at most 1,000 entries) or a strict `query` with optional
`coverage`. The full query scope resolves inside the atomic write and, like
photo browsing, covers only visible photos; removing hidden members requires
their `asset_ids` and an unlock. Hidden explicit cover IDs are omitted while
locked, and selecting a hidden cover requires an unlock. Changes advance the
album revision once and record every changed asset ID across bounded receipts,
while no-ops keep the revision. Scoped browser sessions can list, create,
update, duplicate, and delete albums, pick covers, and add or remove members
through these exact method/path pairs with canonical UUIDv4 IDs and empty query
strings, keeping the same revision checks on existing-album writes. For member
ordering, visibility, and cover selection, see
[Albums](../usage/photos.md#albums).

`POST /photos/imports` starts a folder import from a daemon-host path and
returns `202` with the queued `StorageOperation`, or `422` if the path is not
an existing folder. The import runs as a durable `photo_import` job.
`GET /jobs/{operation_id}` returns its group progress and receipt (added,
skipped, changed, failed, ambiguous, and unsupported counts, plus groups left
unpaired), and `POST /jobs/{operation_id}/cancel` stops it before the next
group. Starting an import requires the API key on a loopback connection.

`POST /photos/checkouts/estimate` accepts `asset_ids`, `album_ids`, inclusive
`years: {from, to}`, or `all`; combine asset, album, and year selectors to copy
their union, or pass `all` alone. `POST /photos/checkouts` accepts
`{selection, destination, byte_limit}` and returns `202` with
`{checkout, operation}`. Creation requires a loopback connection and daemon API
credentials, and all checkout routes refuse browser sessions.

`GET /photos/checkouts/{id}` returns
`{checkout, files, bytes, published_files, published_bytes, entries, next_after}`,
with entries ordered by file ID. Pass `after` with the previous `next_after`
and `limit` from 1 through 250 (default 100) to page through working states.
Name errors skip the whole affected photo. The estimate and the checkout
returned by creation, status, and list report up to 20 skipped photos with
`asset_id`, `file_id`, `name`, and `reason`, plus `skipped_photo_count`, and
totals count only kept photos. A selection with no remaining files fails,
sidecars require a selected eligible original, and copy errors name the
relative path and file ID. Jobs reports progress and accepts cancellation by
operation ID.

Failed and cancelled creations release unpublished holds and destination
reservations while keeping history. Published and uncertain copies keep their
bases until retirement, and backups carry those holds through restore until
retirement.

`POST /photos/checkouts/{id}/retire` returns the checkout. Terminal or restored
unbound checkouts retire immediately with `retired_at` set and
`retention_released: true`. Queued or running work returns a pending request
with `retirement_requested: true` and `retired_at: null`, completing retirement
once the worker stops; if the lane is paused, resume it or cancel the job
through Jobs to finish retirement. Pending retirement stays local; after a
restore, reissue retirement without rebinding. Retirement is idempotent and
stays available after audit activation.

`POST /photos/checkouts/{id}/scan` and `/rebind` return `202` with
`{checkout, operation}`. `GET /jobs/{operation_id}` reports inspected files and
a receipt with `hashed_bytes`, `total_bytes`, final `checkout_results` counts,
at most 100 non-clean `{path, state}` items in `paths`, and at most 100
`untracked` paths (counts still include omitted paths).
`POST /jobs/{operation_id}/cancel` stops inspection during hashing. Interrupted
jobs restart inspection from current bytes, and cancelled rebinds leave
restored checkouts unbound. `POST /photos/checkouts/{id}/commit` creates a
supervised job in the `photo_checkout` pause lane, and failed or cancelled
commits keep their destination reservation until retirement. Rebind verifies a
folder for an idle bound or restored unbound checkout by hashing and
classifying every present tracked path and marking absent paths as missing; at
least one tracked regular file must exist. All checkout operations share the
`photo_checkout` pause lane. Checkout `state` keeps its lifecycle,
`last_operation_kind` and `last_operation_state` describe the latest job
starting with creation, and `last_operation_id` identifies the latest scan,
rebind, or commit job even after it finishes.

Scan and commit work only on the checkout's already-bound destination. Creation
and rebind accept a `destination` path on the daemon's computer, with rebind
taking `{destination}` in its request body.

Released folders, including retired ones, stay protected from watches and
storage bindings until empty or missing. An overlap or unreadable folder makes
the affected watch or binding wait and log a warning while the daemon keeps
running. Replacing the folder or an ancestor with a regular file also releases
admission protection. For destination safety, frozen-version retention, and
restore behavior, see
[Create writable copies](../usage/photos.md#create-writable-copies).

`POST /photos/assets/flags` accepts `{asset_ids}` with 1 through 20,000 UUIDs
and a 1 MiB body limit. It returns `{items:[{asset_id, flag}]}` in asset-ID
order with one row per live, visible, included asset, reflecting the current
display file; missing IDs mean the asset is unavailable (including hidden,
trashed, excluded, and deleted assets).

`POST /photos/assets/query` runs
[photo asset browsing](../usage/photos.md#browse-photo-assets-over-http). It
accepts strict `query`, optional `coverage`, `page_size` from 1 through 250
(default 50, or up to 1,000 with `projection: "map"`; larger pages fail
validation), `facets`, `cursor`, `snapshot_id`, `stacks`, and `hidden` (default
false; true lists only hidden assets and validates the unlock on every
request). It returns one item per eligible matching asset, the full `total`,
`relevance_available`, and any requested `camera`, `lens`, `year`, `location`,
`set`, `rating`, `flag`, or `label` facets on the first page. Facets count
distinct assets across the whole scope while omitting their own root filter.
Camera and lens group Unicode-folded make/model identities and emit original
spellings as keys; one asset can count under both make and model.

Supported sort fields are `relevance`, `capture_time`, `import_time`,
`added_time`, `name`, `modified_at`, `size`, and `media_type`, defaulting to
name ascending. Lexical relevance returns one page of up to `page_size` matches
with no `next_cursor`, ordering by evidence tier and numeric score in the
requested direction, then capture placement descending and asset UUID
ascending. Descending relevance puts filename evidence first, authored text
second, technical metadata third, and retained content-only evidence fourth,
comparing scores within each tier. Operands keep their field, prefix, phrase,
and NEAR meaning. Lexical relevance has no snapshot row limit or build
deadline, and passing a live cursor with it returns `invalid_photo_cursor`.

Semantic and hybrid searches freeze ranked membership through
`QuerySnapshotService` in descending relevance order and return `snapshot_id`,
`member_hash`, and an optional `next_cursor`. To continue, send both
`snapshot_id` and `cursor` with the same query, coverage, page size, and
projection, or send an empty cursor with `snapshot_id` to reopen the first
page. Pages keep accepted membership and order while hydrating current display
facts. With `stacks: true`, pages include every live display row for each
intersecting stack among captured members, so a response can exceed
`page_size`; each stack takes its highest-ranked member's position with its
representative first, and continuations can repeat asset IDs that clients
deduplicate. Filter counts and the date timeline stay unavailable. These
snapshots share cache admission limits and the 30-second build deadline, and
missing, expired, or revoked snapshots return `snapshot_gone`.

Search status fields are `requested_mode`, `actual_mode`, `fallback_reason`,
`truncated`, and `coverage_incomplete`. An image binding without
`min_query_similarity` returns `similarity_floor_unset`, and a missing or
failed provider returns `provider_unavailable`. Metadata fallback freezes up to
1,000 matches in a snapshot, matching the ranked limit; continue with the
original requested query plus the returned `snapshot_id` and cursor. Snapshot
pages use the same stack grouping and omit filter counts and the date timeline.
Hidden searches use live metadata results and fall back with `hidden_photos`.
`truncated` means ranked or metadata fallback matches exceeded the captured
limit, and `coverage_incomplete` reports incomplete retrieval coverage.

Live sorts other than relevance return an optional forward `next_cursor`.
Capture keys fall back to the recorded modified time of the displayed bytes,
sort photos with neither last in both directions, and break ties by ascending
asset UUID. Text keys compare their first 1,024 characters, so names or media
types sharing that prefix fall back to UUID order. Album filters narrow the
matching population before duplicate representatives are chosen. `added_time`
requires exactly one normalized `filters.set_ids` value and uses that album's added
dates. Album-by-query membership ignores sort and selects the full matching
population. Document snapshots reject `capture_time`, `import_time`, and
`added_time` with a field-specific error pointing callers to Photos.

Later live pages reuse the first page's total while rows stay live; start a new
browse to refresh the count. Cursors expire 15 minutes after issuance and bind
resolved saved-query revisions, effective coverage, canonical query, hidden
scope, requested facet dimensions, and page size. Photo facets share a
30-second budget, returning an unavailable reason on time, member, or byte
limits (retry timed-out counts, or narrow the query for member or byte limits).
Invalid options return `invalid_photo_query`, invalid or changed bindings
return `invalid_photo_cursor`, expiry returns `cursor_expired`, and invalid
expressions keep their operand positions.

`POST /photos/authored/edit` targets accept an optional `content_version_id`
that fences the asset's displayed version regardless of which member is edited;
if that version was replaced or the asset now displays a different file, the
edit returns HTTP 412 `stale_revision`.

When nothing changes, the endpoint returns an empty `receipt_id` and empty
`before` and `after` arrays without creating a receipt, leaving Undo aimed at
the last real edit.

`POST /photos/assets/{asset_id}/hide` and `/unhide` require `If-Match` and
return the asset and ETag. Interactive Photos reads and mutations on
already-hidden assets require an unlock, while background imports keep updating
hidden files and returning ordinary import receipts. Hide requires configured
credentials, unhide of an already-hidden asset requires an unlock, and a
visible no-op still requires the current revision. Ordinary Documents reads
keep file access.

`GET /photos/hidden` returns `configured`, opaque equality token `change_id`,
and optional `expires_at` and `locked_until`, each paired with whole seconds
remaining in `expires_in` and `locked_for`. POST `setup`, `unlock`, and
`disable` accept `passcode`, and `change` also accepts `new_passcode`; lock and
reset require the JSON body `{}`. Unlock sets the HttpOnly, SameSite=Strict
`docbank-hidden-<SHA-256 of request host and port>` cookie for the configured
lifetime (five minutes by default; loopback HTTP omits Secure). Lock revokes
every session, and revoking or ending any browser session does the same.
Disable verifies the passcode and atomically unhides assets, while reset keeps
hidden flags and clears auth. Unlock and lock stay available under audit. Five
failures in sixty seconds persist a five-minute lockout with `Retry-After`. The
active Library or Hidden view checks access every two seconds, hides its view
and deletes cached previews on a changed `change_id`, on 401 or 403, after any
Hidden read failure, or after three consecutive Library read failures, and
refreshes before redisplay (HTTP 401 triggers the browser session-expired
flow). A browser window that has opened Photos queues a keepalive lock on tab
hide after pending Hidden writes, including an in-flight unlock, and offers
**Retry lock** after failure in any workspace; Hidden stays concealed until a
lock succeeds. Closing the page sends an immediate keepalive lock and, when
Hidden writes are pending, queues another after them; closing during an unlock
remains best effort. On return, Library and the open album's photo list reload
before redisplay, the Albums index keeps its album list, and Documents snapshots
stay valid.

`GET /photos/assets/{asset_id}/previews/{generation_id}` returns complete
verified JPEG bytes for an included asset's current display version, or for an
assigned source grid version when `person_id` is supplied. An unavailable or
stale generation returns 404, missing retained bytes return
`photo_preview_unavailable`, and failed byte verification returns
`photo_preview_corrupt`. Success includes Content-Length, Content-Digest, the
quoted generation ETag, and `X-Content-Type-Options: nosniff`. Visible previews
use `Cache-Control: private, no-cache`, while hidden previews require an unlock
and use `no-store`, including on `304`. A matching `If-None-Match` returns a
bodyless `304` after checking current display eligibility without reopening the
blob, whereas excluded or replaced displays return `404` even with a matching
validator. Responses also vary on Cookie. Browser sessions can call the exact
list POST, timeline POST, `POST /photos/assets/flags`,
`POST /photos/authored/prepare`, `POST /photos/authored/edit`,
`POST /photos/authored/{receipt_id}/undo`,
`POST /photos/authored/{file_id}/xmp`, preview GET with an optional
`person_id`, `GET /photos/events`, hidden-state GET, interactive hidden POSTs,
asset hide/unhide POSTs, `POST /photos/assets/{asset_id}/trash`,
`POST /photos/rejects/preflight`, and `POST /photos/rejects/trash` with empty
query strings. Reset requires the API key. Preview slot states are `missing`,
`ready`, `unsupported`, and `failed`, and only ready results carry URLs. This
route never generates a derivative.

Optional `projection: "map"` returns compact `map_items` carrying asset ID,
display file ID, revision, name, capture time, file modified time (set only
when it, rather than a capture date, placed the photo), dimensions, display
rotation, coordinates, and the grid preview state/generation/dimensions;
`items` is empty, and `map_items` is omitted when empty. It uses the same
query, total, and cursor contract; omit the projection for viewer metadata and
fit/large previews.

Browse rows include current-display camera make/model, lens make/model, ISO,
aperture, exposure time/bias, focal length, orientation, `rotation` (0, 90,
180, or 270 degrees), `rating` (0 to 5), and `flag` (`""`, `"pick"`, or
`"reject"`). Album summaries include `cover_rotation` with the same values, and
absent metadata fields serialize as JSON null. Compact Map rows alone include
`latitude` and `longitude` as decimal-degree numbers from the selected display
file (or JSON null when absent). Browser-session requests receive null for both
unless `[web.map_tiles]` is configured, while API-key responses do not depend
on tile configuration.

`POST /photos/assets/{asset_id}/preview` accepts `content_version_id`, `size`
(`grid`, `fit`, or `large`), optional `person_id`, optional `face_id`, and
optional `hidden` (default false); hidden preparation requires an unlock and
returns a preview slot. Every `grid` request requires either `person_id` or
`face_id`, not both, while `fit` and `large` requests accept neither. Source
preparation verifies live photo membership, an active person assignment on that
version, and Hidden access before and after production. The byte route accepts
the same `person_id` query parameter for a source grid generation and repeats
those checks, returning source reads with `no-store`. Ordinary requests verify
the current eligible display version before and after on-demand production
under the mutation gate with a 30-second request limit. A version mismatch at
admission returns 409 `photo_display_changed`, and a display replaced or
excluded during production returns 404. Missing or mismatched source bytes
return 503 `photo_preview_unavailable`; retry after restoring the source.
Preparation timeouts and cancellation return 503 `photo_preview_unavailable`
with distinct messages and a retry instruction. Retained failed and unsupported
slots return unchanged.

`POST /photos/assets/timeline` counts the photo assets that browsing would
return, grouped by recorded capture day. It accepts strict `query` and optional
`coverage` (the same scope inputs as `POST /photos/assets/query`), and browser
sessions can call it. The response returns `query`, `dependencies`,
`generation`, `coverage`, `observed_at`, and `facets` with exactly one `capture_day`
facet, without creating a snapshot handle, rows, hashes, or cursors.

The facet counts each included asset once using the selected live display
file's active metadata and recorded calendar date, keeping every scope filter
(capture-date filters, nested saved queries, member predicates, and duplicate
collapse). Values list every observed day in descending order with `other: 0`,
`missing` counts assets without a readable date, and `total` equals the sum of
the values plus `missing`. Photo sorts are accepted and do not affect the
counts.

The values array holds up to 250,000 days. The facet returns `available: false`
with `member_budget_exceeded` when the scope spans more days, and with
`time_budget_exceeded` when counting passes the five-second facet budget; it
never returns incomplete days. Counting runs on the snapshot service, reserving
serialized bytes without cached rows, and has its own two builder slots. A
third concurrent count returns `429 snapshot_busy`; a full byte budget evicts
the least recently used cached snapshot, as a snapshot build does, and returns
`429 snapshot_capacity` only when nothing can be evicted. Invalid queries
return `422 invalid_query`, and invalid coverage returns `422 invalid_profile`.
Workspace snapshots and saved-query runs reject the `capture_day` facet.

Person reads return the canonical row and, for `GET /people/by-id/{person_id}`,
the identities and external UIDs used by split. Rename, retire, merge, and
split check the person revision inside the store transaction, and merge also
checks the absorbed revision from the body. A successful merge or split
advances the document-people binding epoch so the daemon backfill republishes
derived links. Split returns `source_revision_after` and an ETag for that
accepted revision. Because the store binds the receipt to the original request
digest, repeating the same operation request replays its original fence even
after later edits. The 1 MiB request body limit applies before decoding, so an
oversized split returns 413 before the source revision changes.

Every node carries a `revision` that increments on each mutation (directories
increment when their contents change). Revisions are per node because a global
tree ETag would invalidate every agent's in-flight work whenever anything
anywhere changed, whereas per-node revisions limit conflicts to actual
contention. SQLite already serializes writes; preconditions catch **lost
updates across an agent's read-modify-write turns**, not locking.

Every tag definition also carries a `revision` that advances whenever its name
or assignment set changes, preventing a client from renaming over a concurrent
rename or deleting assignments it did not inspect. Single-tag responses carry
an ETag matching this revision.

`If-Match` is required whenever a mutation targets one existing node you read
in an earlier request. Path mutations, bulk operations, and maintenance are the
exceptions:

| Endpoint | Precondition |
|----------|--------------|
| `PATCH /nodes/{id}` | required: target node's revision; an optional `dest_path` is resolved in the same transaction |
| `PUT /nodes/{id}/content` | required: prevents a replacement from overwriting a head the caller did not inspect |
| `POST /nodes/{id}/revert` | required: binds the selected source to the current head the caller inspected |
| `POST /nodes/{id}/trash` | required: target node's revision |
| `POST /nodes/{id}/restore` | required: target node's revision |
| `POST /nodes/{id}/verify` | required: binds the evidence to the node state the caller inspected |
| `PATCH /tags/{tag_id}`, `DELETE /tags/{tag_id}` | required: tag definition/assignment-set revision |
| `PATCH /saved-queries/{saved_query_id}`, `DELETE /saved-queries/{saved_query_id}` | required: saved-definition revision |
| `POST /saved-queries/{saved_query_id}/runs` | required: executes the saved definition revision the caller inspected |
| `PUT\|DELETE /nodes/{id}/tags/{tag_id}` | required: target node revision; the tag revision also advances on a real assignment change |
| `POST /photos/assets/{asset_id}/files`, `DELETE /photos/assets/{asset_id}/files/{file_id}`, `POST /photos/assets/{asset_id}/exclude`, `POST /photos/assets/{asset_id}/trash`, `PUT /photos/assets/{asset_id}/display`, `POST /photos/assets/{asset_id}/hide`, `POST /photos/assets/{asset_id}/unhide` | required: photo asset revision |
| `PUT /photos/settings` | required: photo library settings revision |
| `PUT /backup/schedule` | required: vault settings revision, shared with photo settings |
| `POST /photos/rejects/trash` | body: each previewed target's `revision` and `member_revision`; no `If-Match` header |
| `PATCH /people/by-id/{person_id}` | required: person revision |
| `POST /people/by-id/{person_id}/retire` | required: person revision |
| `POST /people/by-id/{person_id}/merge` | required: survivor revision; the body carries the absorbed revision |
| `POST /people/by-id/{person_id}/split` | required: source person revision |
| `POST /path/move`, `POST /path/trash` | none: the path is resolved and mutated inside one store transaction, so there is no separate read for a revision to guard |
| `POST /batch/move` | each path source resolves in the transaction; each stable-ID source carries its own required revision |
| `POST /nodes` (create dir) | none: creation has no prior revision; a name collision is `409` |
| `POST /path/mkdir` | none: the parent coordinate resolves inside the creation transaction; a name collision is `409` |
| `POST /ingest` · `POST /ingest/stream` | none: long-running bulk operations with per-path partial success; the destination directory may change while they run |
| `POST /uploads` | none: creates or idempotently resolves one file under the stable `parent_id`; name/content collision policy is transactional |
| `POST /trash/empty`, `POST /gc`, `POST /verify` | none: vault-wide maintenance, serialized by the maintenance gate |
| `POST /backup/snapshots`, `POST /backup/snapshots/stream` | none: mutations pause only while pinning one logical snapshot; a preservation lease queues maintenance for the full capture, and the repository has its own exclusive lock |

A stale revision returns `412 Precondition Failed` so you can re-read and
retry, and a missing required `If-Match` returns `428 Precondition Required`.
Both use the problem-JSON error envelope described in
[Error mapping](#error-mapping) and explain which precondition rule applies.

## Content identity and verification evidence

Every file-node representation carries a stable `current_version_id`,
`blob_hash` (Docbank's canonical lowercase SHA-256 content identity), and raw
`size`, while directories omit content identity. Node and version IDs stay
stable across moves and renames. Replacing content keeps the node ID, creates
an immutable version, and updates the current pointer, hash, media type, and
revision.

File nodes and versions also carry `md5`, an auxiliary checksum of the same
logical bytes for tools that key on MD5; it is never an identity. Single-node
and version-detail responses include `source_metadata` when the daemon has
extracted metadata from the original bytes (see
[Source metadata](source-metadata.md)). Browser-session reads omit fields the
extractor marked sensitive, while API-key reads receive every field. Ingest
preflight reports `cloud_placeholders` for files whose bytes a cloud-drive
provider has not materialized locally.

`GET /nodes/{id}/content` sends the catalog identity before streaming in
`X-Docbank-Content-Version`, `X-Docbank-Blob-Hash`, and
`X-Docbank-Blob-Size`, then hashes the bytes as they stream and emits the
result as the [RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html)
`Content-Digest` trailer. The response omits standard `Content-Length` because
HTTP/1.1 cannot carry a trailer on a fixed-length message and pre-reading a
large loose or packed blob only to populate a header would double physical I/O.
If you need independent transfer proof, hash the body yourself and compare both
your digest and the trailer against the node's `blob_hash`, and confirm that
the version header equals `current_version_id`. If copying or blob verification
fails, the response ends without a digest trailer rather than aborting the
connection.

`GET /nodes/{id}/versions` returns a bounded, newest-first page with `items`,
`total`, `limit`, and `offset`. `GET /versions/{version_id}` resolves immutable
metadata globally, and its `/content` child streams that version with the same
identity headers and digest contract. A path rename cannot strand a retained
version reference.

The version's `/content` route accepts a single byte `Range`, returning `206`
with `Content-Range`, or `416 invalid_content_range` for an invalid or
unsatisfiable range. The version, hash, and size headers still identify the
whole version, while the `Content-Digest` trailer covers only the returned
window. MCP clients verify the assembled bytes against the selected version's
size and SHA-256. Because every window streams from the start of the blob up to
the requested bytes, a window's cost grows in proportion to its offset.

`POST /api/v1/versions/{version_id}/download-ticket` issues an original
download ticket and requires API authentication and a live file node. The
request body binds `vault_id`, `node_id`, `blob_hash`, and `size` to the path's
`version_id`. A retained older version is valid, while a mismatched selection
returns 409 `download_selection_stale`. The response returns `url`, `name`, and
`expires_in_seconds: 120`. Issuing a ticket stages no content and does not
require the web app.

The returned relative `/api/daemon/web-download/file?ticket=…` URL needs no API
key. GET atomically consumes the ticket, rechecks its selection, and streams
the full original through the verified blob reader. Identity headers name the
selected version, SHA-256, and size, and the `Content-Digest` trailer covers
the returned bytes; a verification or streaming failure fails the response.
HEAD returns 405 without spending the ticket, one GET spends it, and reuse or
expiry returns 404 `download_not_found`. Expiry and shutdown revoke tickets
without removing retained content. For the MCP relay, proxy setup, and transfer
limits, see [Download originals](../usage/mcp.md#download-originals).

`GET /nodes/{id}/provenance` returns the requested file node, its live path
when one exists, and a bounded newest-ingest-first page from one read snapshot.
Each fact carries its canonical SHA-256 identity, active/superseded state,
ingest identity and time, source kind and description, original path and mtime,
and an optional superseded-fact identity. A trashed node stays inspectable by
ID without a live `path`. This route is read-only: it never accesses the source
or changes retention authority.

`POST /nodes/{id}/provenance` accepts `source_kind`, `source_description`,
`original_path`, an optional canonical UTC RFC3339Nano `original_mtime` (for
example, `2026-08-26T12:00:00Z`), and an optional `supersedes` identity, with
the node's current revision in `If-Match`. A successful request returns `201`
with the appended fact and its ETag, advances the node revision once, and
records a generic ingest alongside the fact. `original_path` is opaque evidence
and is never opened. A supersession must point to an active caller-supplied
fact on the same node, leaving the old fact visible and immutable. Operational
CLI and watched-folder ingest facts cannot be superseded because they keep
re-ingest idempotent; record a newly learned origin as an additional fact
instead.

The encoded request body can be at most 1 MiB (1,048,576 bytes); a larger body
returns `413` before the append runs.

### Email metadata and exact parts

Email files keep their original bytes as the immutable content version. The
built-in decoder adds a separate canonical MIME inventory with ordered raw
header spans, decoded fields including Bcc, message and part structure,
alternatives, diagnostics, and exact SHA-256 and size receipts for retained
artifacts. It does not normalize the source, deduplicate messages by
Message-ID, or turn attachments into child documents.

Encoded headers, addresses, and multipart structure use the Go standard
library, and the recipe records the Go version and fixed resource limits. A
structure rejected by the parser stays a partial inventory with a diagnostic,
including malformed part headers and multipart delimiter or preamble lines
longer than 4 KiB. Original bytes and already inventoried parts stay readable.

`GET /versions/{version_id}/email` returns the selected generation for that
version. An eligible version still waiting for the daemon worker returns `202`
with its populated version and `state: "pending"`, an undeclared source returns
`422 email_not_supported`, and a suppressed inventory returns
`409 email_derivative_suppressed`. `POST` with `{}` explicitly attempts
extraction without changing the stored media type or bytes.

Generation and part URLs are immutable. A generation must be attached to the
version in the URL, and you select a part by its dotted structural path plus
one role: `raw_headers`, `decoded_payload`, or `body_utf8`. Missing roles
return `409 email_part_unavailable`, and invalid selectors return
`422 invalid_email_part`. Part responses always download as
`application/octet-stream` with `nosniff`, complete version, generation,
attachment, and part identity headers, expected SHA-256 and size, and an actual
`Content-Digest` trailer. Read through verified EOF; a partial or corrupt
stream has no successful digest proof.

Browser-session credentials can read the `GET` email metadata, generation, and
part routes when the request has no query string, and the web app's email
viewer uses them. `POST /versions/{version_id}/email` still requires the master
API credential. These routes do not render PDFs, index attachments, sync
mailboxes, or import MBOX files.

`GET /content-references` is the inverse identity lookup. Pass one canonical
lowercase SHA-256 to get only logical `content_versions` references backed by
blob-catalog authority, meaning a `blobs` row (see
[Identity, addressing, and authority](overview.md#identity-addressing-and-authority)).
The route never infers a match from a loose file or pack entry alone. Each item
carries the complete immutable version, its current node projection, whether
that version is the node's current head, and a path only while the node is
live. Results are bounded and deterministic: live current references, then live
history, then trashed references.

`GET /duplicates` finds hashes shared by at least two live current file nodes,
returning `items`, `total`, and `total_references` for that population while
excluding historical versions and trash. `limit` defaults to 50 and accepts 1
to 100, and `offset` is nonnegative. Groups sort by canonical SHA-256, and
counts stay exact even on an exhausted page. Counts, references, paths, and
collection labels share one read transaction per request.

Each group carries `sha256`, `size`, `reference_count`,
`representative_node_id`, `references`, and `references_truncated`. Up to 16
references appear in earliest `(modified_at,node_id)` order. Each reference
wraps the existing content-reference identity in `reference`, plus
`collections` (up to 16 distinct eligible IDs and optional labels, sorted by
ID), `collection_count`, and `collections_truncated`. Multiple import
memberships never multiply document counts. Both API-key clients and browser
sessions can call this route, though browser access permits only GET on the
exact route with optional singleton `limit` and `offset` parameters. There is
no duplicate-deletion operation.

For pagination and historical-lookup guidance, see
[duplicate discovery](../usage/searching.md#find-documents-with-identical-content).

`POST /nodes/{id}/verify` provides a bounded server-side proof. Send `If-Match`
from a prior node response; the daemon reopens the blob through the same mixed
loose/packed store used for downloads and returns the recorded and computed
version ID, hashes, and sizes. Missing, corrupt, and unreadable content still
return a 200 report with `verified: false` and a `problem`, while transport,
validation, and stale-node failures return non-2xx responses. Because the route
rechecks the revision after reading, a concurrent rename, trash, or content
replacement returns `412` instead of ambiguous evidence.

This single-node route is exempt from the ordinary request timeout: it is
bounded in scope, not necessarily short in duration, because hashing one very large
blob can take longer than a minute.

These integrity receipts come from the authenticated daemon and are not non-repudiable
attestations against a malicious server. Signed receipts and transparency logs
sit outside Docbank's current trust model.

## Addendum: `POST /ingest`, `POST /ingest/stream`, and `POST /ingest/preflight`

`POST /ingest/preflight` takes
`{paths: [...], include: [...], exclude: [...]}` and runs a metadata-only
source inventory without opening regular-file content or writing vault metadata
or blobs. The report returns file, directory, and logical-byte totals,
pack-eligible, loose-only, and rejected size classes, exclusion, skip, and
error counts, bounded findings, and extension summaries. It shares the real
ingest's absolute-path validation, explicit root-directory-symlink behavior,
exclusion rules, loopback fence, and timeout exemption. Findings are
point-in-time observations rather than a snapshot lock: sources can still
change before ingest, and metadata-only scanning cannot prove later content
readability. Include and exclude patterns use `/` separators on every platform
and reject backslashes. Entries filtered by a rule are excluded without
failure, while selected non-regular entries appear as skipped findings.

`POST /ingest` takes **server-side local paths**:
`{paths: [...], dest: "/inbox", include: [...], exclude: [...], replace: false, collection_label: "Review set"}`
(with `collection_label` optional). It returns an `IngestReport` (`ingest_id`,
`added`, `skipped`, `excluded`, and per-path `failed` entries) and backs
`docbank document add`. Paths must be **absolute**, because the long-lived
daemon's working directory is meaningless, so relative paths return `422`. The
CLI resolves `docbank document add` arguments to absolute paths before calling,
so the CLI still accepts relative and `cwd`-relative sources. When `replace` is
false or omitted, collisions follow the same suffixing rules as other imports.

With `replace: true`, the daemon resolves the exact destination name and
records its node revision before reading source content. Changed bytes create a
new version on that node, while an equal hash and size skip without altering
the stored MIME type or version history. A live directory fails before source
I/O, and a stale revision or exact-name create race fails the file without
falling back to a suffix.

`POST /ingest/stream` takes the same body (including `replace`) and returns
`application/x-ndjson`. A metadata-only `scan` stage reports advisory file and
byte totals, followed by `ingest` progress for bytes read and file outcomes,
and ends with exactly one `result` carrying `IngestReport` or one `error`. HTTP 200
alone does not mean success. A write failure or client disconnect cancels the
request context used by traversal, blob writing, and metadata transactions;
already completed files stay valid and converge on retry, and an incomplete
blob never receives node authority.

One request equals one logical ingest run, and `ingest_id` appears only after
at least one document observation commits. Because of that, a partial import
returns the run owning its successful files alongside individual failures,
while a scan, an excluded-only request, or an all-failed request returns no
`ingest_id` and creates no collection or label authority. An initial label
collision, or an initial label attempted after permanent audit enrollment,
reports through the same per-file failure protocol while other files in the
request keep the normal partial-import behavior.

Repeating a logical filesystem import starts a new run and records the existing
same-content node as a member without creating a content version. That new
provenance advances the node revision even when `collection_label` and
`replace` are omitted or `replace: true` sees identical bytes. A client holding
the previous ETag must re-read the node before writing (the old `If-Match`
returns `412 stale_revision`). Within one run, the same observation
is idempotent. The separate unkeyed `POST /uploads` retry contract stays
unchanged: an equal retry returns the existing node without changing its
revision and does not expose an unused run identifier.

Because they grant "read any daemon-readable local path," `POST /ingest` and
`POST /ingest/stream` check `RemoteAddr` on every request and are **restricted
to loopback callers** regardless of bind address or API key; non-loopback
clients receive `403` (`loopback_only`). This route has no remote file-upload
capability: send remote bytes to `POST /uploads`, or tunnel remote access over
SSH or VPN to a loopback listener.

Each include or exclude pattern uses Go's `path.Match` grammar against a
slash-separated source-relative path. Use `/` separators on every platform;
backslashes are rejected. A pattern without `/` matches a basename at any
depth, while a pattern containing `/` matches the relative path. `*` and `?` do
not cross `/`, and `**` has no recursive globstar behavior. An include filters
regular files only, and a matching exclusion wins and prunes a directory's
subtree. Preflight and ingest share this compiler so reviewed selection and
actual selection never drift. Patterns must be relative, and invalid syntax or
parent traversal fails before filesystem access. For literal metacharacters,
use bracket expressions such as `report[[]1].txt` instead of backslash
escaping. Watched inboxes share this compiler for `exclude_patterns`, while
their `exclude` entries keep the literal contract.

## Addendum: ingest-run collections

`GET /api/v1/collections/{id}/quality` returns a current aggregate receipt with
the collection, `source_fingerprint`, dimensions, zero-byte and mismatch
counts, duplicate-document counts, and descriptive concentrations. Optional
`fields` accepts up to seven distinct comma-separated names: `extension`,
`media_type`, `media_family`, `modified_month`, `size`, `text_coverage`, and
`duplicates` (all seven by default). Unknown or repeated fields return 422.

Only quality accepts `profile=<configured name>` and includes
`collection.coverage`; ordinary list, detail, and members reads do not inspect
processing state. When one profile is configured, the daemon selects it
automatically; zero profiles return `unconfigured`, and multiple profiles
without a choice return `profile_required` (both unavailable states carry null
counts). An unknown profile name returns 422. Configured coverage includes the
selected name and fingerprint, active generation, and complete, partial,
failed, unprocessed, and none counts over current members. Selection names
policy rather than runtime readiness: retained active output wins over a later
failed attempt, and source hashes alone cannot transfer profile authority.

Quality runs over one source snapshot capped at 250,000 members, 64 MiB of
projected census data, and five seconds. Size bounds return
413 `quality_too_large`, and interruption or timeout returns
503 `quality_unavailable` rather than partial results. Frequency dimensions
keep the top 50 values alongside missing and other counts. Duplicate membership
is vault-wide current content counted within this collection: the census groups
only hashes present in the selected collection, its size preflight reserves
fixed space for the counts, and duplicate grouping runs only while reading the
admitted rows. Concentrations require at least ten members and 80% of the
collection. Each request reads current source data. Extension and MIME buckets
follow the query operand rules: values that cannot name a concrete operand
count as missing, and MIME parameters do not affect the bucket, matching the
MIME search predicate.

A collection is an immutable ingest-run identity with live document membership,
not a folder, so moving or renaming a member leaves its run identity intact.
Membership excludes caller-supplied `embedded:` provenance, superseded
provenance, directories, and trashed files. Counts and byte totals deduplicate
nodes within a run and describe each member's current version as browsing
observations rather than an export snapshot.

Lists default to 100 results, accept `limit=1..1000` and a nonnegative
`offset`, and order by `started_at` descending and then ingest ID. Member pages
use the same bounds and return current `Node` representations with live paths.
The list hides empty runs, while a directly addressed run stays readable when
it holds retained label authority even after its last member is trashed or
purged.

```console
$ curl -sS -H "X-Api-Key: $DOCBANK_API_KEY" \
    'http://127.0.0.1:43210/api/v1/collections?limit=100&offset=0'
{"items":[{"id":"5ca58787-4608-4f69-8e67-8d5794970f77","source_kind":"cli","source_description":"/srv/import/review","started_at":"2026-09-10T09:30:00Z","file_count":2,"total_bytes":31,"label":"Review set","label_revision":1,"label_updated_at":"2026-09-10T09:30:00Z"}],"total":1,"limit":100,"offset":0}

$ curl -sS -H "X-Api-Key: $DOCBANK_API_KEY" \
    'http://127.0.0.1:43210/api/v1/collections/5ca58787-4608-4f69-8e67-8d5794970f77/members?limit=100&offset=0'
```

Request only extension counts when that is all you need. Coverage still describes
all current members; without a processing profile, its counts are null.

```console
$ curl -sS -H "X-Api-Key: $DOCBANK_API_KEY" \
    'http://127.0.0.1:43210/api/v1/collections/5ca58787-4608-4f69-8e67-8d5794970f77/quality?fields=extension'
```

```json
{
  "collection": {
    "id": "5ca58787-4608-4f69-8e67-8d5794970f77",
    "source_kind": "cli", "source_description": "/srv/import/review",
    "started_at": "2026-09-10T09:30:00Z", "file_count": 2, "total_bytes": 31,
    "label": "Review set", "label_revision": 1, "label_updated_at": "2026-09-10T09:30:00Z",
    "coverage": {
      "configuration": "unconfigured", "profile": "", "profiles": [],
      "profile_fingerprint": "", "generation_id": "", "counts": null
    }
  },
  "source_fingerprint": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "dimensions": [{"field": "extension", "values": [{"value": "txt", "count": 2}], "missing": 0, "other": 0}],
  "zero_bytes": 0, "mismatches": 0, "duplicate_documents": 0, "spikes": []
}
```

Labels have their own resource and revision. Read that resource to get its
ETag, then send the quoted positive revision in `If-Match`. The PUT body holds
exactly one required `label` field: a string sets the label, explicit `null` clears it,
and unknown or server-owned fields (such as `revision`) are rejected.

```console
$ curl -i -H "X-Api-Key: $DOCBANK_API_KEY" \
    http://127.0.0.1:43210/api/v1/collections/5ca58787-4608-4f69-8e67-8d5794970f77/label
HTTP/1.1 200 OK
ETag: "1"

{"ingest_id":"5ca58787-4608-4f69-8e67-8d5794970f77","label":"Review set","revision":1,"updated_at":"2026-09-10T09:30:00Z"}

$ curl -sS -X PUT -H "X-Api-Key: $DOCBANK_API_KEY" \
    -H 'Content-Type: application/json' -H 'If-Match: "1"' \
    -d '{"label":"Filed set"}' \
    http://127.0.0.1:43210/api/v1/collections/5ca58787-4608-4f69-8e67-8d5794970f77/label

$ curl -sS -X PUT -H "X-Api-Key: $DOCBANK_API_KEY" \
    -H 'Content-Type: application/json' -H 'If-Match: "2"' \
    -d '{"label":null}' \
    http://127.0.0.1:43210/api/v1/collections/5ca58787-4608-4f69-8e67-8d5794970f77/label
```

Labels are case-sensitive NFC strings of 1 to 256 UTF-8 bytes with no control
characters and at least one non-whitespace character, keeping intentional
surrounding spaces. Non-null labels are unique across all retained label
records, including empty collections, so trash and restore cannot transfer a
name between runs. Clearing keeps the revision fence. A stale revision returns
`412 stale_revision`, a collision returns `409 exists`, and an invalid name
returns `422 invalid_collection_label`.

Permanent audit authority currently records ingest and membership changes but
has no event for label mutation. Because of that, once audit is enabled, label
PUTs fail closed with `409 audit_mutation_unsupported`, including clears and
no-op requests, while existing labels stay readable and portable. Backup
readers that predate collection labels or the `ingest_observe` audit kind
reject that added authority instead of silently dropping it.

## Addendum: `POST /uploads`

`POST /uploads?parent_id=<id>&name=<filename>` accepts exactly one
`multipart/form-data` file field named `file`. Supply exactly one destination selector:
either stable `parent_id` or absolute `parent_path`. Any missing path
directories publish in the document's content transaction after the complete
upload verifies. A rejected upload leaves no destination directories or ingest
record. The multipart filename must match the normalized `name` query
value so the envelope and requested tree entry cannot describe different files.

Two request headers declare the expected identity of the **file part**, not the
multipart envelope:

- `X-Docbank-Blob-Hash`: canonical lowercase hexadecimal SHA-256.
- `X-Docbank-Blob-Size`: raw byte length, up to Docbank's 64 TiB
  logical-object admission ceiling.

The server streams the file once through Kit's durable writer and independently
computes both values. Only after the values match, the closing multipart
boundary verifies, and no extra parts remain does one metadata transaction grant
blob authority and create the node. `201` with `status: "added"` marks a new
node and its initial `content_create` version; repeating the same name, hash,
and parent converges to that stable node with `200` and `status: "skipped"`.
The receipt always returns the server's `computed_hash`, `computed_size`, and
the node's ID and revision. Clients compare the values themselves.

URL imports can include `X-Docbank-URL-Fetch`, a version-1 JSON descriptor with
`version`, `requested_url`, `final_url`, and UTC `fetched_at`, plus optional
`etag` and `last_modified` holding base64-encoded header bytes. Unsupported
versions, unknown fields, and invalid descriptors fail before content
preparation. The daemon consumes the supplied bytes and evidence without
fetching the URL; content, missing directories, the ingest run, and its URL
origin publish together after envelope verification. URL retries match the
exact requested URL and equal current bytes within the destination, even after
a rename, while distinct requested URLs keep separate documents even when their
bytes match. A skip adds no ingest record or provenance. Readers expose
supported evidence as `url_fetch` on provenance and collections while keeping
the raw source description for metadata export and unsupported stored formats.

Uploads are file-granular: send independent requests (concurrently when useful)
so one failure never leaves another file's outcome ambiguous and each file can
retry on its own. Different content under the same requested name follows
normal ingest suffixing rather than overwriting an existing document.

Objects up to 64 MiB are eligible for packing, while larger accepted objects
stay loose with the same content-hash authority, verified streaming, backup,
and restore contracts. Because of that, the 64 TiB admission limit does not
mean a single object moves into a pack.

A digest or size disagreement returns `422 digest_mismatch` or
`422 size_mismatch` and grants no new `blobs` row or node authority. Because
physical bytes publish before metadata by design, a rejected stream can leave an
authority-free loose object that the normal GC untracked-file scan cleans up.
Malformed envelopes and extra parts also fail before authority. Request bodies
cap at the declared size plus bounded multipart overhead. The route is exempt
from the ordinary timeout, so a large upload follows client cancellation rather
than a one-minute deadline.

## Addendum: folder push

`GET /push/sources?push_name=<name>&after=<source_ref>&limit=<1-1000>` pages
through one push name's cursors in `source_ref` byte order, with `limit`
defaulting to 1000. Each item returns `source_ref`, `node_id`, `hash`, `size`,
and `trashed`, where `hash` and `size` record the last accepted source
observation (which can differ from the node's current head after an independent
edit). `next` gives the `after` value for the next page and is absent on the
last page. The read grants no authority. The client reads every page at
startup, checking its key and push name before reading any file, and reads them
again in watch mode at the start of each scan that has files to push.

Both push routes reject a `push_name` that matches a `[[watch]]` still
configured on the daemon with `409 push_name_in_use`, because both producers
would otherwise version the same source identities.

Before any push observation exists, the listing includes the matching
daemon-owned watch cursor for the same name and relative path. Its last
accepted hash and size count as already pushed; an unchanged first push sends
no upload or provenance mutation. Changed bytes version that same node
and record a push observation, which takes precedence afterward. The cursor
survives metadata backup and restore. For the transition steps, see
[switch-over steps](../usage/pushing.md#switch-from-a-daemon-owned-watch).

`POST /push/uploads` shares the multipart envelope, hash and size declarations,
admission limits, maintenance gate, and timeout exemption of
[`POST /uploads`](#addendum-post-uploads), and also requires:

- `push_name`: 1 to 64 lowercase ASCII letters, digits, `-`, `_`, or `.`.
- `source_ref`: a canonical source-relative slash path of at most 4096 bytes.
  Each component must be a valid virtual name; empty, absolute, and parent
  traversal paths are rejected.
- `parent_path`: the absolute virtual directory for a new identity's node.
- `run_id`: a UUIDv4 the client chooses for one scan. Every file accepted with
  the same `run_id` joins one operational ingest run (one import collection).
  A `run_id` that names another source's run returns `409`.

Optional `modified_at` carries the original source modification time in
canonical UTC RFC3339Nano. `parent_path` and `name` place only a new node, and
the multipart filename must match `name`. Missing directories along
`parent_path` are created in the same transaction as that node, and only when a
node is created. An existing identity keeps its node wherever it now lives, so
a moved node or trashed former parent does not affect it. No server-side source
path is opened. Using a distinct route prevents older daemons from ignoring
push identity fields. Responses use `PushUploadReceipt`, while ordinary uploads
keep `UploadReceipt`.

Only verified payloads with a valid closing envelope reach the store. One
transaction resolves the source identity and commits content versions and
operational `push` provenance with exact version bindings. A new identity
always gets its own exact-name node and returns `201 added` even when another
document has the same bytes, while existing name collisions fail. Known
unchanged sources return `200 skipped`, and changed sources follow their
existing node and return `200 updated` when a new content version is created. A
changed source whose bytes already match the node's head records its new
observation and returns `200 skipped` without adding a version. Mapped trash
returns `409`.

Each source's observations stay strictly ordered by their run's start time.
When a shared run does not start after a source's previous observation (for
example, after the wall clock steps backward or when one run sees a source
twice), that observation gets its own run instead.

Every upload success returns the independently computed `computed_hash` and
`computed_size` along with the resulting node, and the client verifies the
receipt before reporting success. A retry after a lost response resolves the
committed source and returns `200 skipped`. Independent edits never overwrite
the cursor's last accepted source digest. For the operator workflow, see
[Push a folder](../usage/pushing.md).

`GET /api/daemon/key-challenge?nonce=<64 hex>` needs no API key and returns
`{"proof": ...}`, an HMAC-SHA256 of the nonce under the daemon API key with its
own domain separator. The push client runs this challenge on every new
connection and sends the API key and document bytes only after the proof
verifies. The challenge never carries the key, and its proof cannot act as a
credential.

## Addendum: `PUT /nodes/{id}/content`

Content replacement accepts raw bytes instead of multipart. First read the file
node and send its revision in `If-Match`, then declare the raw body's canonical
SHA-256 and byte count in `X-Docbank-Blob-Hash` and `X-Docbank-Blob-Size`.
`Content-Type` is normalized and stored on the new version, defaulting to
`application/octet-stream` when omitted.

The daemon streams the body into durable authority-free storage and computes
the identity independently. Only exact agreement lets one metadata transaction
create a `content_replace` version, advance the node's current pointer, and
increment its revision. The old head stays an immutable history and GC root
until you prune that version. Success returns the new ETag plus a receipt with
the node, new version, `computed_hash`, and `computed_size`; compare every
field against your request before accepting success.

Send `Expect: 100-continue` for large writes. The daemon checks the target kind
and revision before its first body read to avoid wasting bandwidth, then
repeats those checks in the committing metadata transaction, which is the only
check that grants authority.

Stale revisions return `412 stale_revision`, missing preconditions return
`428 precondition_required`, and identity disagreements return
`422 digest_mismatch` or `422 size_mismatch`. A failed operation grants no new
catalog authority, though a fully written loose object can remain
authority-free until GC. Because the body can be binary, this route is exempt
from the raw JSON text validator and uses its byte count and digest as the
lossless boundary instead. Cancellation propagates through physical writing and
prevents the metadata transaction.

## Addendum: `POST /nodes/{id}/revert`

Reversion accepts JSON `{"source_version_id":"<uuid>"}` and requires the target
node's revision in `If-Match`. The source must be an immutable version of that
same node and cannot be its current version. A successful transaction creates a
distinct `content_revert` row, copies the source's blob hash, size, and media
type into it, records `source_version_id`, advances the current pointer, and
increments the node revision.

This operation is metadata-only: it neither streams nor copies the source blob,
whether loose or packed, and every existing version stays a reachability root
until version pruning selects it. The receipt returns `node`, the new
`version`, and `source_version`, and the ETag carries the resulting revision;
cross-check all four authorities rather than relying on HTTP 200 alone.

A stale target returns `412 stale_revision`, a source from another node returns
`422 version_node_mismatch`, selecting the current head returns
`422 version_already_current`, and an unknown source returns `404 not_found`.

## Addendum: `POST /nodes/{id}/versions/prune`

Version pruning releases selected non-current history without changing current
content. Every request requires the inspected node revision in `If-Match` and
picks exactly one selector: `version_ids` (at most 1,000 canonical UUIDs),
`keep_newest`, `older_than`, or `all_prior`. Requests default to a dry run;
pass `"run":true` to execute the reported class of operation under the same
node revision precondition.

Ordinary selectors keep revert-source dependencies and report them separately.
When the current head is a revert, `all_prior` can first install a same-byte,
source-free checkpoint so the complete older graph can be removed safely. A
successful run advances the node revision once when it deletes history and
leaves it unchanged for an empty selection. Deleted version IDs stop resolving.

An `older_than` selector computes and returns its cutoff on each request. While
the node ETag guards content-graph changes, wall-clock aging does not advance
the revision, so a later run can include versions that crossed the age boundary
after a preview. For an exact replay, pass the preview's candidate IDs through
`version_ids`.

The receipt separates logical history bytes from physical consequences: shared
blobs stay reachable, authority-free loose blobs wait for GC, and dead packed
payload waits for GC followed by repack. Loose and packed counts can overlap
when one blob has both representations across stores; the receipt reports that
overlap as `mixed_blobs_pending_maintenance`, and its physical byte totals
cover all affected authoritative locations. Pruning itself does not claim
physical space reclamation.

## Addendum: tags

Tag definitions use stable server-generated UUIDv4 identities, mutable unique
NFC-normalized names, and a revision covering both the definition and its
assignment set. `POST /tags`, `GET /tags`, `GET /tags/by-name`, and
`GET|PATCH|DELETE /tags/{tag_id}` cover the definition lifecycle. Tag
definitions store exact assignment counts, including live and trashed nodes.
Membership changes, purges, and metadata restores update those counts in the
same transaction. Single-tag lookups read the stored count without scanning
assignments. `GET /nodes/{id}/tags` and `GET /tags/{tag_id}/nodes` provide
bounded forward and reverse listings where the page, total, and assignment
counts share one query snapshot. Reverse results include a path only for live
nodes. `live_only=true` returns a bounded live projection and `omitted_trashed`
count from one metadata snapshot, letting interactive browsers display live
results without assembling paths and trash states across pages.

`PUT|DELETE /nodes/{id}/tags/{tag_id}` assign and unassign under the required
node `If-Match` revision, returning the resulting node and tag, `changed`, and
the resulting node ETag. Repeating the requested state returns `changed: false`
without advancing either revision, while a real assignment change advances both
the node and the tag once. Single-tag definition responses carry the tag ETag,
and `PATCH|DELETE /tags/{tag_id}` require it in `If-Match`. Renaming advances
the tag and every assigned node. Deleting checks the current tag revision
before removing the full assignment set and advancing each assigned node,
without ever removing nodes or document bytes.

Path-oriented clients can call `PUT|DELETE /path/tags/{tag_id}` with
`{"path":"/..."}` to resolve the live path and update the assignment in one
SQLite transaction. That avoids a race between a separate path lookup and an
ID-addressed call, because moving an ancestor changes a descendant's path
without changing the descendant's revision.

Ingest provenance is filesystem-shaped: each import records the source's
original path and mtime in the store's `provenance` table. Authenticated
clients can also append a post-ingest fact with a source kind, description,
opaque original path, optional mtime, and optional supersession, checked
against the node's `If-Match` revision. Appending records evidence without ever
opening the supplied path or looking up a node by content hash.

## Maintenance gate

Because of the reachability-then-delete race described in
[Ownership and concurrency](locking.md), `gc --run`, `trash empty`, and
`verify` need the vault quiescent while they run. The daemon holds its
exclusive vault lock for its entire lifetime rather than per request, so an
in-process `sync.RWMutex`-shaped gate serializes maintenance against regular
mutations. Ordinary mutating handlers (`PATCH`, trash, restore, create, ingest)
take the read side and can run concurrently with each other, while maintenance
handlers take the write side. Once maintenance is running or waiting for
in-flight mutations to finish, any new HTTP mutation fails immediately with
`503 maintenance_busy` instead of turning into a long wait that clients cannot
tell apart from other delays. Maintenance that is still waiting for an active
backup capture does not refuse mutations, because it holds nothing they need. Daemon-owned background jobs keep blocking gate
semantics so a transient maintenance run never permanently fails a watcher or
extraction worker. Maintenance routes are exempt from the per-request timeout
because `gc` and `verify` can run long on a large vault.

Backup creation takes the mutation-exclusive side only during Kit's freeze
window. Once Docbank's deferred read transaction is pinned, ordinary mutations
resume into SQLite's WAL while verified metadata and blob streams are captured.
The backup holds a separate shared preservation lease until capture ends, and
maintenance takes that lease exclusively so GC cannot delete a loose blob or
catalog mapping still referenced by the pinned snapshot. The create route is
timeout-exempt, and cancellation still propagates through Kit to prevent
publishing a snapshot manifest.

## MCP is a daemon client

`docbank mcp` runs as a separate process and listener rather than a second path
into the vault. It uses the same daemon discovery and authenticated HTTP client
as the CLI, so the daemon stays the sole standalone owner of SQLite, blob,
pack, rendition, processing, and lock authority. MCP has no direct-vault or
remote-daemon mode.

The MCP boundary implements exactly protocol `2026-07-28` through the official Go SDK
v1.8.0. Stdio is newline framed, and its optional HTTP transport is stateless,
POST-only, and separately authenticated, with loopback as the usual listen
address. The MCP bearer resolves from a named credential binding and cannot
equal the daemon's configured, ephemeral, or runtime-discovered API key; the
daemon API key is never accepted as an inbound MCP credential.

MCP exposes 43 bounded read tools, including photo inspection, plus opt-in
write tools for processing enqueue, package and Bates work, photo mutation,
exports, reports, and kata citations. Enqueue keeps the daemon's existing consent and
plan-fingerprint checks and cannot grant consent or replay an ambiguous start.
Rendition resources bind the stable vault, node, content-version, and
attachment tuple and expose only bounded windows of active sanitized Markdown,
never source bytes or host paths.

Setting an explicit network listen IP opts into trusting that network with
plaintext MCP credentials and results. Host checks accept loopback, a concrete
listen IP, and explicit allowed authorities, and matching-origin checks remain
mandatory whenever an Origin header is present. This fixed HTTP bearer is not
MCP OAuth: the listener has no OAuth metadata, authorization-server discovery,
client registration, scopes, or refresh, and no GET streams, sessions,
resumption, prompts, roots, sampling, elicitation, or tasks. For the complete
catalog and limits, see [Model Context Protocol](../usage/mcp.md).

## Auth

Send the key in `X-Api-Key` or `Authorization: Bearer <key>`, which the daemon
compares in constant time against its effective key. The daemon always has a
key: if `[server] api_key` is unset, it generates a fresh one at startup and
writes it inside owner-private `$DOCBANK_HOME` in the same runtime record the
CLI uses for discovery. Only the vault's owner can read it, and it is never
logged. Same-user discovery proves daemon ownership before sending credentials.

Binds default to loopback. Setting an explicit non-loopback IP requires a
configured key and opts into plain HTTP across the selected network. The
operator must provide a trusted network or encrypted transport (see
[Configuration](../configuration.md#bind-validation)). Host checks run before
authentication and accept loopback, the concrete bind IP, the dedicated browser
origin, and explicit `allowed_hosts`; wildcard listeners never accept arbitrary
names. Server-path ingest stays loopback-peer only.

These routes are auth-exempt: `/health`, `/api/ping`, `/docs`, the OpenAPI
documents, the `/schemas` documents, `/api/daemon/challenge`,
`/api/daemon/key-challenge`, `/api/daemon/web-auth`,
`/api/daemon/web-auth/login`, `/api/daemon/web-download/file`,
`/api/daemon/web-upload`, `/photos/hidden`, and the static web application at
`/`, `/searches`, `/productions`, `/imports`, `/trash`, `/photos`,
`/photos/people`, `/photos/map`, `/photos/map/{set_id}`, `/photos/albums`,
canonical `/photos/albums/{id}` pages, and `/assets/`. Everything else,
including the shutdown route, requires the key.

## Error mapping

Errors use RFC 7807 problem-JSON with a `code` extension, a machine-readable
string you can branch on instead of parsing `detail`. Query errors can also
include a `position` span:

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
| `backup_repository_owner_conflict` | 409 | the owner marker names another vault |
| `backup_repository_claim_required` | 409 | unmarked history, or another installation of this vault, requires an explicit claim before capture |
| `backup_claim_changed` | 409 | the repository changed after the claim preview |
| `audit_mutation_unsupported` | 409 | the audited vault does not yet record this logical mutation class |
| `audit_already_enabled` | 409 | an initial-scope preview cannot execute because audit authority was enabled concurrently |
| `audit_scope_overlap` | 409 | the proposed scope shares a live or retained-trash member with permanent protection |
| `audit_scope_limit` | 409 | the vault already has the maximum 1,000 permanent scopes representable by terminal evidence |
| `audit_preview_stale` | 409 | the one-use enrollment preview expired, was consumed, came from another daemon, or no longer matches the vault |
| `audit_acknowledgment_required` | 422 | enrollment execution omitted the permanent-retention acknowledgment |
| `audit_not_enrolled` | 422 | the selected node exists but is outside every permanent audit scope |
| `cursor_expired` | 422 | the listing cursor expired, or a people cursor is invalid or belongs to another query, ordering, projection, or Hidden scope; restart from the first page |
| `invalid_audit_cursor` | 422 | the history cursor is malformed or belongs to another stable node or scope |
| `invalid_batch_move` | 422 | a batch has no moves, too many moves, ambiguous selectors, or an invalid final-state plan |
| `invalid_collection_label` | 422 | a collection label violates the canonical UTF-8, NFC, length, control-character, or non-whitespace policy |
| `stale_revision` | 412 | `store.ErrStaleRevision`: `If-Match` didn't match the current revision |
| `provenance_mismatch` | 409 | the requested predecessor is missing, belongs to another node, is already superseded, or is an operational ingest fact |
| `invalid_provenance_time` | 422 | optional `original_mtime` parses as RFC3339 but is not canonical UTC RFC3339Nano (a value that is not a date-time at all fails schema validation as `validation` instead) |
| `invalid_saved_query` | 422 | saved name, description, kind, payload, or patch violates the saved-definition contract |
| `hidden_locked` · `hidden_passcode` | 403 | missing/expired Hidden unlock or incorrect passcode |
| `hidden_not_configured` · `hidden_configured` | 409 | passcode setup is absent or already exists |
| `invalid_hidden_passcode` | 422 | passcode length is outside 1–1,024 bytes |
| `hidden_lockout` | 429 | five-minute lockout, with `Retry-After` |
| `invalid_photo_asset` | 422 | asset kind, role, display pointer, sidecar target, or graph state is invalid |
| `invalid_photo_album` | 422 | album name, member cover, explicit asset selection, or mutually exclusive selection fields are invalid |
| `photo_snapshot_export_unsupported` | 422 | a Photos snapshot cannot use the generic export API because it would omit related RAW and sidecar files; export it from Photos |
| `photo_node_not_eligible` | 422 | the selected node is not a live eligible file |
| `photo_node_owned` | 409 | the selected node already belongs to another photo asset |
| `invalid_query` | 422 | invalid or unsupported expression, missing reference, or query compilation bound exceeded |
| `invalid_cursor` | 400 | malformed, tampered, wrong-direction, or otherwise invalid snapshot cursor |
| `snapshot_gone` | 410 | snapshot is missing, expired, revoked, owned by another session, or lost with its daemon |
| `snapshot_member_hash` | 400 | `store.ErrSnapshotMemberHash`: supplied member hash doesn't match the snapshot |
| `photo_snapshot_member_not_in_library` | 412 | a frozen photo is missing, hidden, excluded, or trashed when a `photos: true` saved-query run commits, its results are opened, or its album is created |
| `snapshot_capacity` | 429 | bounded snapshot cache admission could not reserve the requested rows or serialized bytes |
| `snapshot_busy` | 429 | both bounded snapshot builders are already occupied |
| `snapshot_too_large` | 413 | one materialization exceeded its row, per-row, or serialized-size bound |
| `snapshot_unavailable` | 503 | query snapshot exceeded its resource budget or its build was canceled |
| `invalid_profile` | 422 | requested processing profile or coverage selection is invalid |
| `invalid_saved_query_run` | 422 | saved-run identity, revision, or execution request is invalid |
| `not_dir` / `not_file` / `invalid_name` / `invalid_tag` / `not_trashed` / `is_root` | 422 | `store.ErrNotDir` / `ErrNotFile` / `ErrInvalidName` / `ErrInvalidTag` / `ErrNotTrashed` / `ErrIsRoot` |
| `search_query_required` | 422 | blank search without a PDF, tag, or modification-time filter |
| `validation` | 400, 415, or 422 | malformed request (bad `If-Match`, paths, media type, multipart envelope, or generated validation) |
| `precondition_required` | 428 | required `If-Match` header missing |
| `host_forbidden` | 403 | Host is outside the configured authority allowlist |
| `loopback_only` | 403 | server-path ingest, preflight, backup restore, or explicit backup `repo` called by a non-loopback peer |
| `digest_mismatch` / `size_mismatch` | 422 | uploaded file bytes disagree with the required declaration; no node/blob authority committed |
| `too_large` | 413 | upload exceeded its declared size plus bounded multipart overhead |
| `maintenance_busy` | 503 | exclusive vault maintenance is running or waiting for in-flight mutations; retry the mutation after it finishes |
| `pack_retirement_deferred` | 503 | repack authority committed but an old source pack remains physically locked; release the lock, then run `storage pack` reconciliation |
| `unauthorized` | 401 | missing or invalid API key; bad shutdown token |
| `web_session_read_only` | 403 | a daemon-issued browser session attempted an endpoint outside its attenuated allowlist |
| `web_unavailable` | 503 | this daemon is not serving compiled web assets |
| `internal` | 500 | unmapped error (still surfaced with a message, because this is a single-user local daemon, not a hardened multi-tenant service) |

## Non-goals

- **No server-side rendering.** The kit-ui application is static public code
  that receives a daemon-lifetime attenuated session from `docbank web`. Reads,
  verified-download preparation, and digest-checked file upload stay ordinary
  authenticated API requests, and the master API key never enters the browser.
- **No multi-user model.** One vault has one master authority. Browser sessions
  are attenuated local capabilities, not accounts, and sharing is out of scope
  for v1.
- **No MCP endpoint inside `docbank daemon run`.** `docbank mcp` stays a
  bounded client process with its own transport and credential boundary.
- **No remote-daemon mode or `[remote]` configuration.**
