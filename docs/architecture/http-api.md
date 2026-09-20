---
last_edited: 2026-09-16
title: HTTP API
description: The agent-first HTTP API — filesystem-shaped endpoints, revision preconditions, and the daemon's error contract.
---

# HTTP API

The HTTP API lets clients browse, retrieve, file, and reorganize documents.
`docbank daemon run` serves it, and every CLI data command uses it. CLI commands
cannot open the vault directly.

This page owns the wire contract: routes, authentication, preconditions, content
verification, and errors. Use [Agent Guide](../agents.md) for a task-oriented
starting point.

| Reader question | Contract |
| --- | --- |
| Which operation should I call? | [Endpoint map](#shape) |
| How do I process and search a document version? | [Document processing](#document-processing) |
| How do I reject a stale write? | [Revisions and `If-Match`](#concurrency-resource-revisions-and-if-match) |
| When may I trust downloaded bytes? | [Content verification](#content-identity-and-verification-evidence) |
| Which credentials does a request need? | [Authentication](#auth) |
| How do I handle a failed request? | [Error mapping](#error-mapping) |

## Mailbox imports and explicit EML transfers

Mailbox routes live under `/api/v1/mailbox`. Owner identity comes from the
authenticated vault principal, never from a supplied owner field.

| Route | Contract |
| --- | --- |
| `POST /containers` | Declare stable `id`, `sha256`, `size`, and `format` (`mbox` or `zip`); JSON is bounded to 1 MiB. |
| `PUT /containers/{id}/chunks/{index}` | Stream one ordered 64 MiB chunk (short final chunk allowed), declaring `X-Docbank-Blob-Hash` and `X-Docbank-Blob-Size`; response repeats the verified identity. |
| `GET /containers/{id}` · `POST /containers/{id}/seal` · `DELETE /containers/{id}` | Inspect, verify the complete source and seal it, or abort an incomplete upload. Sealed sources cannot be aborted. |
| `POST /containers/{id}/preview` | Supply explicit `dialect`; inspect up to three messages, reading at most 16 MiB of expanded message data, and 100 entry descriptors, with total `entry_count` and `has_more`. |
| `POST /jobs` | Supply `id`, `container_id`, `container_sha256` and `settings.destination_id`. Dialect defaults to `mboxrd`; recipe and label-to-tag mappings are optional. The response includes normalized settings and the parser/MIME recipe identity. |
| `GET /jobs` · `GET /jobs/{id}` | Inspect durable work; lists return newest first and use `after` job ID and `limit` (maximum 100). |
| `GET /jobs/{id}/occurrences` | Read up to 100 receipts after the `after` ordinal. |
| `GET /jobs/{id}/events` | NDJSON `progress`, `result` or `error` events; disconnecting does not cancel the job. |
| `POST /jobs/{id}/cancel` · `POST /jobs/{id}/resume` | Cancel, or supply the exact prior `request` and boolean `continuation` to resume/continue. |
| `POST /archives` | Register `id` and `description` for an application-independent source archive. |
| `POST /transfers` | Stream EML bytes; `X-Docbank-Transfer` is base64url-encoded JSON containing archive ID, stable reference, hash, size, settings, destination ID, name and optional expected revision. |

Container manifests hash the canonical JSON object with ordered fields
`sha256`, `size`, `chunks`; each chunk has `index`, `sha256`, `size` in that
order. Clients can independently verify the sealed `manifest_sha256` against
their source and ordered chunks. Browser byte uploads use the existing
ownership-proved WebSocket channel, not raw HTTP chunk writes. Browser
credentials cannot register external archives or transfer explicitly identified
EML. No mailbox endpoint accepts an arbitrary daemon-host path.

Conflicting immutable declarations, changed resume settings and changed
targets return 409; invalid input and capacity limits return 422; oversized
request bodies return 413; unexpected server failures return 500.
Preview samples messages; the background job verifies all ZIP entries before
publishing and retains their hashes for resume. See [Mailbox archives](../usage/importing.md#mailbox-archives)
for exact retention, capacity and retry guarantees.

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
| `GET /formats/capabilities?family=&format=&extension=` | read the running binary's per-format capability snapshot | Implemented |
| `GET /nodes/{id}/children` | list a directory, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/content` | stream document bytes with catalog identity and a computed digest trailer | Implemented |
| `PUT /nodes/{id}/content` | replace raw content under revision, size, and digest preconditions — see [addendum](#addendum-put-nodesidcontent) | Implemented |
| `POST /nodes/{id}/revert` | create a new head from a prior version of the same file | Implemented |
| `GET /nodes/{id}/versions` | list immutable content versions newest-first, paginated (`limit`/`offset`) | Implemented |
| `GET /nodes/{id}/provenance` | inspect immutable ingest-origin facts newest-first, paginated (`limit`/`offset`) | Implemented |
| `POST /nodes/{id}/provenance` | append an immutable origin fact under the node revision | Implemented |
| `GET /versions/{version_id}` · `GET /versions/{version_id}/content` | inspect or stream one immutable version by stable UUID | Implemented |
| `GET\|POST /versions/{version_id}/email` | read or synchronously ensure canonical email metadata for one immutable version | Implemented |
| `GET /versions/{version_id}/email/generations/{generation_id}` | read one immutable email generation attached to the exact version | Implemented |
| `GET /versions/{version_id}/email/generations/{generation_id}/parts/{part_path}/{role}` | stream one verified raw-header, decoded-payload, or UTF-8 body artifact | Implemented |
| `GET /content-references?sha256=&limit=&offset=` | find every stable node/version pair retaining a content hash | Implemented |
| `GET /duplicates?limit=&offset=` | list live current documents sharing content, with exact counts and bounded reference previews | Implemented |
| `GET\|POST /tags` · `GET /tags/by-name` · `GET\|PATCH\|DELETE /tags/{tag_id}` | list, resolve, create, rename, or delete stable tag definitions | Implemented |
| `GET /nodes/{id}/tags` · `GET /tags/{tag_id}/nodes` · `PUT\|DELETE /nodes/{id}/tags/{tag_id}` · `PUT\|DELETE /path/tags/{tag_id}` | inspect and change tag assignments | Implemented |
| `GET\|POST /saved-queries` · `GET\|PATCH\|DELETE /saved-queries/{saved_query_id}` | list, create, inspect, edit, or delete named query and literal highlight definitions | Implemented |
| `POST /saved-queries/{saved_query_id}/runs` | execute one revision-fenced saved query and return its durable run receipt with the first snapshot page | Implemented |
| `POST /queries/parse` | validate complete QueryV1 intent and resolve dependency revisions without executing a search | Implemented |
| `POST /workspace/queries` | execute complete QueryV1 intent and return the first page of one exact bounded result snapshot | Implemented |
| `POST /workspace/queries/{id}/pages` | read another page from an existing query snapshot with its opaque cursor | Implemented |
| `POST /audit/preview` · `POST /audit/enable` · `GET /audit/status` | review permanent first-scope retention, enable the exact reviewed plan, and inspect authority or membership | Implemented |
| `GET /audit/history?path=&node_id=&limit=&cursor=` | read one audited node's canonical newest-first event timeline with a stable continuation cursor | Implemented |
| `GET /audit/scopes/{scope_id}/history?limit=&cursor=` | read canonical newest-first events across every member of one permanent scope | Implemented |
| `POST /audit/verify` | independently replay audit authority, optionally prove recorded evidence is an exact prefix, and re-hash every protected blob | Implemented |
| `POST /nodes/{id}/verify` | re-hash one file, bound to an inspected node revision | Implemented |
| `GET /search?q=&tag_id=&mime_type=&under_node_id=&modified_since=&modified_before=&limit=` | bounded name and extracted-content search (FTS5), optionally restricted by stable tag identity, current base media type, descendants of a live directory, and current node modification time, with match source and explicit `truncated` status | Implemented |
| `GET /processing/profiles` · `POST /processing/plans` | list executable profiles / preview one exact source version and its provider disclosures | Implemented |
| `POST /processing/jobs` · `GET /processing/jobs/{id}` | run a reviewed plan with streamed job identity / read aggregate status | Implemented |
| `POST /processing/consent/grants` · `POST /processing/consent/revocations` | grant reviewed profile consent / revoke this operator's processing consent | Implemented |
| `GET /renditions/{attachment_id}` · `POST /renditions/select` | stream retained sanitized Markdown by attachment or exact source selector | Implemented |
| `GET /coverage?profile=&vault_uid=&content_version_id=` · `POST /search` | inspect separate rendition/embedding coverage / search an authorized source-version set | Implemented |
| `POST /search/similar` | stored-vector neighbors | Implemented |
| `POST /derivatives/purge-plans` · `POST /derivatives/purge-jobs` | preview / run a live derivative purge without changing immutable backups | Implemented |
| `POST /nodes` · `POST /path/mkdir` | create a directory beneath a stable parent ID or at one exact virtual coordinate | Implemented |
| `POST /ingest` · `POST /ingest/stream` · `POST /ingest/preflight` | import with JSON or streamed progress / inventory server-side paths — see [addendum](#addendum-post-ingest-post-ingeststream-and-post-ingestpreflight) | Implemented |
| `GET /collections` · `GET /collections/{id}` · `GET /collections/{id}/members` · `GET\|PUT /collections/{id}/label` | browse live ingest-run membership and inspect, set, or clear its revision-fenced label | Implemented |
| `GET /collections/{id}/quality` | inspect bounded document distributions and processing coverage for one collection | Implemented |
| `POST /uploads?parent_id=&name=` | stream one digest-checked remote file — see [addendum](#addendum-post-uploads) | Implemented |
| `PATCH /nodes/{id}` | move and/or rename, including resolving an absolute `dest_path` transactionally | Implemented |
| `POST /path/move` · `POST /path/trash` | move / trash by virtual path, resolved and mutated in one store transaction | Implemented |
| `POST /batch/move` | validate and apply up to 1,000 moves as one final-state transaction | Implemented |
| `POST /batch/tags` · `POST /batch/tags/preview` | atomically assign/remove one tag on a revision-fenced selected set, or inspect its exact membership | Implemented |
| `POST /nodes/{id}/trash` · `POST /nodes/{id}/restore` | soft delete / recover | Implemented |
| `GET /trash` · `POST /trash/empty` `{run, older_than}` | list (optionally paginated) / report or hard-delete trash roots | Implemented |
| `POST /gc` `{run}` · `POST /verify` | reclaim unreachable blobs / validate metadata and re-hash all blobs | Implemented |
| `GET /storage` · `POST /storage/pack` · `POST /storage/repack` | inspect usage / pack loose blobs / compact sparse packs | Implemented |
| `GET /jobs` | inspect daemon-owned background tasks and terminal failures | Implemented |
| `GET /watches` | inspect effective watched-inbox configuration and runner state | Implemented |
| `POST /backup/init` · `POST /backup/snapshots` · `POST /backup/snapshots/stream` · `GET /backup/snapshots` | initialize a repository / create with JSON or streamed progress / list snapshots | Implemented |

### Format capability coverage

`GET /formats/capabilities` returns the flat `format-coverage/v1` object:
`contract_version`, `formats`, `pending`, and `generated_by` are top-level
members. Supplying `format` or `extension` also adds `lookup`. `family` filters
returned format rows, while lookup still resolves against the full runtime
snapshot. `generated_by.catalog_rows` continues to describe that complete
source catalog.

`family` and `format` are limited to 64 bytes; `extension` is limited to 16.
Use either `format` or `extension`. Supplying both returns
`422 invalid_format_query`. Pending and unknown queries return status 200 with
`lookup.match` set to `pending` or `unknown_format`. See
[Format Coverage](format-coverage.md) for the capability and state contract.

Search accepts an optional `q` query. An omitted, empty, or whitespace-only
query requires `tag_id`, `modified_since`, or `modified_before`. This is a
request rule; `limit` bounds the response size, not the database work.
Filter-only hits include live files and directories except the vault root.
They are ordered by current `modified_at` descending, then name and ID, and
identify their source with `match: "filter"`. `mime_type` and `under_node_id`
narrow a page but cannot anchor a blank query alone. A blank unanchored query
returns `422 search_query_required`.

Tag selection uses the tag index; time-window selection has a live-node index
matching the result ordering. Combined filters can still require substantial
work before reaching `limit`. The normal `limit` and `truncated` contract
remains in force, without a cursor. If `truncated` is true, the page is
incomplete. Narrowing time bounds cannot split a group with identical
modification timestamps, such as nodes restored together.

### Document processing

The routes below use the normal [API authentication](#auth). Plan and grant
requests use an exact source selector and a reviewed plan fingerprint instead
of `If-Match`. Browser sessions can use the built-in processing, consent,
coverage, search, and attachment-read routes; selector reads and derivative
purge require the master API credential.

`GET /processing/profiles` returns an array of executable profiles with `name`,
`fingerprint`, `rendition`, and `embedding_bindings`. An empty array means no
profiles are executable. Choose one of these names; the default configuration
has none.

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
and an executable profile. Node IDs are positive integers; version IDs are
canonical UUIDv4 values. Profile names have 1–128 characters, start with a
lowercase letter, and contain only lowercase letters, digits, `_`, or `-`.

The response includes `fingerprint`, `vault_uid`, `selector`,
`profile_fingerprint`, `flow`, `disclosed_classes`, `retained_classes`,
`estimate`, `consent_required`, `consent_state`, and `backup_consequence`.
Each flow identifies the provider, capability, trust boundary, input classes,
any filename disclosure, and a `runtime_disclosure` object. Review it before
granting consent or starting work. The runtime disclosure contains:

| Field | Meaning |
|-------|---------|
| `immediate_processor`, `ultimate_processor` | The adapter or processor receiving the input and the processor it ultimately uses |
| `endpoint`, `deployment` | The configured destination and deployment identity; a local provider reports `in-process` |
| `model`, `model_revision`, `vector_space` | Model and vector-space identities, when applicable |
| `metadata_classes` | Metadata disclosed alongside the content or query |
| `retained_artifact_roles` | Artifacts retained for this hop; query embedding retains none |

These values enter the plan fingerprint. Consent state is advisory and does not
authorize a provider call by itself.

#### Remote recording references

`POST /api/v1/media/sources` also accepts a JSON remote recording reference.
`reference_url` is required and write-only. It is the protected acquisition
input, including any occurrence-specific query. `canonical_url` is optional,
write-only, and limited to 8,192 bytes. It is the caller's sanitized identity
URL. It must be an absolute HTTP(S) URL without userinfo. Docbank lowercases
the scheme and hostname, converts Unicode domains to punycode, and removes
the default port and fragment. An empty path becomes `/`. A trailing dot in
the hostname stays distinct. Other path and query encoding stays unchanged.
These rules determine the permanent source identity.

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
branch. The source identity is the Cap video ID, so these routes identify one
source. A present `canonical_url` must name the same registered origin and video ID;
otherwise the request returns `422 invalid_media_plan`. A nonempty
`provider_hint` must equal `cap.self-hosted`; a different hint also returns
`422 invalid_media_plan`. A credential binding must be empty or the binding
registered for that origin. A foreign binding
returns `422 credential_cross_origin`.

`GET /api/v1/media/origins` reports `adapter_contract`,
`deployment_revision`, `probe_state`, and `probed_at` when a registered
origin has a probe. Probe results cover the configured network and TLS policy
at daemon startup; they do not refresh during the process lifetime.
`deployment_revision` is the operator's configured value, not a remotely
verified version. Recognized self-hosted Cap references remain
`access_required`, and `acquire: true` returns `503 capability_unavailable`.

Registration affects new submissions only. A generic source retained before
registration remains separate, and its manual import remains unchanged.

Outside registered self-hosted origins, the canonical path does not resolve
DNS, follow redirects, read credentials, or download a recording.
`provider_hint` is a bounded replay value and does not prove a provider.
A fresh canonical submission returns
`outcome: "unsupported"` with a pending occurrence. A submission that omits
`canonical_url` uses its configured origin policy and returns
`access_required`. Acquisition planning uses the configured origin and
`reference_url`; `canonical_url` does not affect its plan.

The canonical URL selects the provider identity:

- **Cap Cloud.** A `canonical_url` on `https://cap.so` or `https://www.cap.so`
  whose path is exactly `/s/<id>`, `/embed/<id>`, or the documented SDK
  `/dev/<id>` route uses the `cap` provider identity, keyed by that video ID.
  Either host, either route, and any query or fragment select the same source.
  The ID is case-sensitive and must not be percent-encoded. The submission
  returns `outcome: "unsupported"` whether or not `acquire` is set, because Cap
  documents no download route for received links. Import the file with the
  artifact route described below.
- **Loom.** A `canonical_url` on `https://loom.com` or
  `https://www.loom.com` whose path is exactly `/share/<id>` or
  `/embed/<id>` uses the `loom` provider identity. The route is part of the
  source key because Loom does not document equality between share and embed
  references. Queries and fragments do not affect identity. Loom returns
  `outcome: "unsupported"`, even with `acquire: true`, and uses no network
  access. Import the caller's exact MP4 and SRT files with the artifact route.
- **Any other URL.** The generic `url` identity uses the whole canonical URL.
  `acquire: true` returns `503 capability_unavailable`.

Docbank recognizes Cap links from the URL alone. It does not check whether a
Cap video is public, private, or password-protected, and it does not contact
Cap. Other Cap paths keep the generic `url` identity.

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

To add a local original, send exactly the two multipart parts required by the
artifact route, with metadata `kind: "media"`. WAV and MP3 keep their existing
rules. A remote recording may use an MP4 named `.mp4` with `video/mp4`. MP4
files are limited to 20 MiB, 2,088,960 coded pixels, 300,000 milliseconds,
and 18,000 frames. The metadata's filename and media type must agree with the
inspected bytes. The service checks the declared size and SHA-256 before
publication.

The store publishes the verified original and binds it to the selected visible
occurrence in one transaction. A pending occurrence receives a new source
version, while an existing exact version can be reused without moving the
source head. A changed original for a bound occurrence returns
`409 source_conflict`. A caption or transcript must follow the original. A
caption with `application/x-subrip` stays a retained input until the caller
selects the `supplied-captions` profile. That profile publishes timed
`media-transcript/v1` evidence and supports lexical and auto search. The retained
`origin` label does not restrict caption processing: imported files produce
supplied evidence, just as imported transcripts do. See the
[caption timing limits](../document-understanding.md#supplied-caption-timing-limits).
Semantic and hybrid search are not configured for supplied captions. A
transcript requires an explicit processing plan, consent, and retry before it
can produce `transcribed` coverage.

Media retry selects the caller's newest visible occurrence for the source.
It cannot target an older occurrence. To process an older recording, use the
ordinary processing API with that recording's node and current content version.

Status and list responses select the source version bound to the visible
occurrence they report. They show `content_available` and `unprocessed` after
the original is retained, then use processing receipts for that same source
version. A failed retry can leave an earlier successful transcript visible for
the same version. A transcript from an older version cannot cover newer bytes.
Raw URLs and credential bindings never appear in receipts, errors, logs,
renditions, search results, or portable metadata.

#### Processing consent

To grant consent without running document processing, send
`POST /processing/consent/grants` with the plan's exact `selector` and its
`fingerprint` as `plan_fingerprint`. An optional `expires_at` must be a future
RFC3339 timestamp; omitting it grants consent without expiry. The response
returns `plan_fingerprint`, `profile_fingerprint`, and any `expires_at`.

The grant covers this operator's use of the profile across documents and
searches. It includes the profile's document inputs, retained classes, and
`query_text` for providers that support query embedding. It is not limited to
the document used for preview. The daemon uses the `daemon:operator` principal
and `document-processing` scope for these routes.

`POST /processing/consent/revocations` takes no body and returns `revoked_at`.
It revokes this operator's processing grants across all profiles. A new grant
must use a reviewed plan; revocation does not delete existing derivatives.

Semantic and hybrid searches require active `query_text` consent for the
selected binding's provider disclosure. Lexical and auto searches read retained
local text without query embedding or query-text consent. Provider work checks
consent before egress and before publication; having stored vectors alone does
not authorize a query disclosure.

#### Start work and recover its status

`POST /processing/jobs` accepts `selector`, `plan_fingerprint`, and `consent`.
The fingerprint must match the current plan. `consent: true` grants the reviewed
profile consent without expiry before running; `false` relies on existing
active grants. The HTTP API permits both; the CLI requires `--consent`.

Once work is accepted, the response is `200 application/x-ndjson` with
`Cache-Control: no-store`. A complete stream has two records:

1. `sequence: 1`, `type: "job"`, and `job` with the durable `id`, source version,
   profile fingerprint, and any known rendition, attachment, or embedding IDs.
2. `sequence: 2`, `terminal: true`, and either `type: "status"` with `status`, or
   `type: "error"` with the job and `processing_status_unavailable` error.

A terminal status can report failure; HTTP 200 alone does not mean processing
succeeded. The first job receipt does not report a state or phase. Read `state`
and `failure_code` from a status response. Once accepted, work continues under
the daemon lifecycle after the requesting connection closes. Keep the first
job ID if the stream ends early and call `GET /processing/jobs/{id}`. That read
returns `job_id`, `state`, `phase`, `embedding_job_ids`, `completed_bindings`,
and any `failure_code`. Job IDs are lowercase SHA-256 strings.

#### Read a rendition

`GET /renditions/{attachment_id}` reads one active sanitized-Markdown attachment.
Its optional `max_bytes` query accepts 1–67,108,864 and defaults to 67,108,864.
Browser sessions must omit query parameters on this route; setting `max_bytes`
requires the master API credential.
`POST /renditions/select` accepts `selector` and a required `max_bytes` in the
same range, so callers can read without first discovering the attachment ID.
Attachment IDs are lowercase SHA-256 strings.

The response is `text/markdown; charset=utf-8` with `Cache-Control: no-store`.
`X-Docbank-Rendition-Attachment`, `-Build`, `-Artifact`, `-Profile`,
`-Completeness`, and `-Warnings` identify the result. `X-Docbank-Content-Version`,
`X-Docbank-Blob-Hash`, and `X-Docbank-Blob-Size` identify its source version and
complete artifact. Read to completion and compare the byte count and SHA-256
with those headers and the base64 SHA-256 `Content-Digest` trailer before
accepting the bytes. An incomplete stream or missing digest is not verified.

The GET route accepts one `Range: bytes=...` and returns `206` with
`Content-Range`. Artifact identity headers still describe the complete
rendition; the digest trailer covers only the returned range. Invalid or
unsatisfiable ranges return `416 invalid_rendition_range`.
See the [Markdown contract](document-derivatives.md#sanitized-markdown-contract)
for the envelope and body-relative navigation.

#### Similar documents

`POST /api/v1/search/similar` accepts `selector`, optional `binding_id`,
optional `limit` from 1 to 100, and `fence`. The selector identifies a current
live file by `node_id`, `content_version_id`, and configured `profile`. The
normalized fence must contain that version before coverage is inspected.

The report has `state`, `source`, `binding_id`, `coverage`, `results`, and
`truncated`. `state: unavailable` includes `missing_coverage` with kind
`embedding`, binding, profile fingerprint, and source version. It has no
results. `state: ready` can have an empty result list.

Each result carries a rank, score, path, document identity, `blob_hash`,
`duplicate_count`, and one embedding evidence reference. The source node is
excluded; eligible copies with the same bytes share one result. The limit
applies after grouping. L2 scores are negative distances, so higher wire scores
always rank first. Invalid source identity, stale source, storage failures,
and stale index errors remain errors. This read starts no processing and
makes no provider request. Browser sessions may POST this exact path with an
empty query string.

#### Coverage and source-fenced search

`GET /coverage` takes `profile`, `vault_uid`, and repeated `content_version_id`
query parameters. It reports `profile_fingerprint`, aggregate `state`, and
separate `renditions` and `embeddings` classes. Each class reports its name,
required status, state, and complete, unavailable, stale, ineligible, and total
counts, plus `rebuilding` and `previous_generation_serving`. A rebuilding cell
is not also counted as complete; `previous_generation_serving` is the subset of
rebuilding cells whose prior complete result remains available. A queued first
build therefore has `rebuilding: 1` and `previous_generation_serving: 0`.
The rendition and embedding counts come from one catalog snapshot, including
current source visibility and serving evidence. Missing required coverage keeps
the aggregate state `partial` even if another class is rebuilding. This read
does not grant consent or start provider work.

`POST /search` uses JSON, separately from ordinary lexical `GET /search`:

```json
{
  "query": "renewal terms",
  "mode": "lexical",
  "profile": "private",
  "limit": 50,
  "fence": {
    "vault_uid": "22222222-2222-4222-8222-222222222222",
    "content_version_ids": ["11111111-1111-4111-8111-111111111111"]
  },
  "explain": true
}
```

Use the actual vault UUID and 1–4,096 distinct canonical UUIDv4 versions for
both coverage and search. A foreign vault or invalid source fence is rejected.
Search requires nonblank `query` text of at most 8,192 characters, `profile`,
and `mode` (`lexical`, `semantic`, `hybrid`, or `auto`). `limit` defaults to 50
and accepts 1–100. `binding_id` selects the embedding binding; omitting it uses
the profile's first binding. Set it explicitly for semantic/hybrid search when
several are configured. The CLI requires that choice. `auto` uses lexical
retrieval. See [processing consent](#processing-consent) before choosing a mode
that embeds query text.

The response includes `requested_mode`, `actual_mode`, `coverage`,
`degradations`, `results`, `truncated`, and `trace` (`explain: true` populates
the trace). Each result retains its vault, node, and content-version identity
with bounded evidence references. The source fence applies before retrieval;
vector scoring uses only eligible rows from current, live attachments.
Consumers still check visibility immediately before displaying a result.
See [Processing search](../usage/search.md) for the consumer contract.

#### Processing errors and derivative purge

Before a stream starts, failures use the normal `application/problem+json`
envelope. Common processing codes are:

| HTTP status | Code | Meaning |
|-------------|------|---------|
| 428 | `processing_consent_required` | No matching grant; review and grant consent |
| 412 | `processing_consent_expired`, `processing_consent_revoked` | Existing consent cannot authorize this operation |
| 409 | `processing_plan_changed` | Preview the changed source or profile again |
| 409 | `rendition_operator_required` | Processing needs operator intervention |
| 422 | `processing_profile_unavailable`, `foreign_vault`, `version_node_mismatch` | Profile or source identity does not match |
| 422 | `invalid_processing_consent_expiry`, `search_query_required`, `validation` | Invalid expiry, blank query, or request schema violation |
| 422 | `rendition_failed` | Required rendition work failed |
| 404 | `not_found` | Requested node, version, job, or active rendition is absent |
| 503 | `processing_unavailable` | The processing service is not configured |
| 500 | `processing_failed` | An otherwise unclassified processing failure |

After the first job event, inspect the terminal event and recover through the
status route if it is missing or reports `processing_status_unavailable`.

`POST /derivatives/purge-plans` previews `content_version_ids`, `attachment_ids`,
`build_ids`, or `all: true`. Each ID list is bounded to 1,000 entries.
`POST /derivatives/purge-jobs` takes the same selection plus the returned
`plan_fingerprint`. It returns one terminal NDJSON `result` event with a receipt;
partial or deferred cleanup includes an `error` beside that receipt. Stale plans
return `409 derivative_purge_plan_changed`; invalid selections return
`422 invalid_derivative_purge`. Immutable backup copies remain untouched.

### Query compilation preview

`POST /queries/parse` accepts one QueryV1 object and returns its canonical
`query`, `query_fingerprint`, and `dependencies` (`kind`, stable `id`, and
observed `revision`). Definition reads share one read transaction. No result
rows, membership snapshot, or executable SQL are returned. The endpoint is
available to authenticated API clients and browser sessions; browser requests
must use POST without query parameters.

The compiler preserves expression scope and separate structured filters.
Missing references, including negative tag and collection operands, fail with
`422 invalid_query`; expression errors include a `position` object with
half-open UTF-8 byte `offset` and `end`. Backend failures remain server errors.
See [query grammar and limits](../usage/searching.md#preview-a-field-aware-query).
Existing `/search` requests do not gain advanced syntax through this endpoint.

### Exact query snapshots

`POST /workspace/queries` executes one strict QueryV1 payload and returns HTTP
200 with its first page. The request accepts `query`, an optional configured
processing `profile`, `page_size` of 50, 100, or 250 (default 100), and any
explicit subset of `collections`, `tags`, `media_family`, `extension`,
`modified`, `size`, `text_coverage`, and `duplicates` facets.

The response freezes the canonical query, dependency revisions, selected
lexical generation and processing coverage, ordered row metadata, and exact
node/content-version membership observed at creation. It also carries exact
`total` and `total_bytes` values, `member_hash`, `snapshot_fingerprint`,
`snapshot_id`, and creation and expiry times. Later edits, moves, tag changes,
content replacements, or dependency changes do not rewrite an existing
snapshot. Create another snapshot to observe them.

`POST /workspace/queries/{id}/pages` accepts only the required `cursor`. Send
the returned `next_cursor` or `previous_cursor` unchanged; it is bound to the
snapshot, owner, ordering, direction, and page size. The response omits a
directional cursor when no page exists in that direction. An empty snapshot
therefore needs no page request.

Requested facets use the same creation-time read snapshot. A facet omits only
its matching outer structured filter so clients can see alternative values;
expression operands and nested saved-query scope stay in force. `total` counts
distinct documents in that self-excluded population, `missing` counts documents
without a value, and `other` sums value counts omitted from the response. A
document with several tags or collections contributes once to each value, so
those value counts need not sum to `total`.

Each available facet retains its leading 50 values plus selected QueryV1 values
outside that set, including a selected value whose count is zero. Size uses the
fixed `<1 MiB`, 1–10 MiB, 10–100 MiB, 100 MiB–1 GiB, and `>=1 GiB` buckets;
modification time uses UTC calendar-month buckets. Fixed categorical facets
include their defined zero-count buckets. A facet that cannot be computed
within its coverage, member, or time budget returns `available: false` with a
`reason` and omits count fields; it is not a successful zero.

Materialization admits at most 250,000 rows and 64 KiB of serialized data per
row. Across the daemon, the cache admits at most 1,000,000 rows and 512 MiB of
serialized snapshot data, with at most eight handles per authenticated owner
and two builders at once. A build has 30 seconds; all requested facets share a
five-second facet budget. Handles expire after 15 minutes idle or 30 minutes
absolute lifetime. These are cache-admission bounds, not a claim that the
process uses at most 512 MiB of RSS.

Snapshot ownership follows authentication. A browser token can read only its
own handles, and revoking the browser session invalidates them. Master-key
requests share the daemon's master owner. All handles are daemon-lifetime
cache state and disappear on restart or shutdown.

`POST /saved-queries/{saved_query_id}/runs` executes the saved query revision
named by `If-Match`. Its body accepts only `profile`, `page_size`, and `facets`;
it cannot replace the saved QueryV1 payload. The response is
`{run,snapshot}`. `run` durably records the definition revision, fingerprints,
exact totals, expiry, and comparison with the previous run. The accompanying
snapshot rows remain ephemeral: backup and restore retain the receipt but do
not reconstruct rows or silently rerun the query. Run the saved query again
explicitly after `410 snapshot_gone`.

Snapshot execution supports duplicate and text-coverage constraints.
Text-coverage predicates require a configured processing profile. Semantic and
hybrid modes and relevance ordering remain unsupported and return positioned
`422 invalid_query` errors.

### Saved query and highlight definitions

`POST /saved-queries` stores a name, optional description, immutable `kind`,
and one complete structured `payload`. `kind` is either `query` for QueryV1 or
`highlight_set` for an ordered set of literal text/color pairs. The response
adds a stable UUID, canonical payload fingerprint, revision, timestamps, and a
quoted numeric `ETag`. Payloads remain JSON objects on the wire; they are not
base64 strings.

QueryV1 stores these fields. Defaults apply when a field is omitted:

| Field | Accepted value | Default |
|-------|----------------|---------|
| `v` | `1` | `1` |
| `text` | Up to 8,192 Unicode characters, preserved exactly | Empty text |
| `syntax` | `simple` or `advanced` | `simple` |
| `mode` | `lexical`, `semantic`, or `hybrid` | `lexical` |
| `filters` | Object described below | `{}` |
| `sort.field` | `name`, `path`, `modified_at`, `size`, `media_type`, or `relevance` | `name` |
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

Filter sets are sorted and deduplicated when saved. Query text is not trimmed
or rewritten. Unknown fields and duplicate JSON object keys are rejected.
A raw payload may use at most 128 KiB; its normalized encoding may use at most
64 KiB. Nested JSON is limited to 16 levels.

A `highlight_set` payload contains `v: 1` and `terms`. Each term has `text`
and `color`. Supply 1–64 terms with unique literal text, each 1–256 Unicode
characters long. Colors must be lowercase `#rrggbb`. The term order is
preserved, and terms are not regular expressions. See the
[creation examples](../usage/searching.md#save-complete-query-intent-over-http).

Names are unique across both kinds. Names use Unicode NFC normalization and
must contain 1–256 UTF-8 bytes afterward. Blank names and control characters
are rejected. Descriptions allow at most 4,096 UTF-8 bytes; Windows CRLF line
endings normalize to LF on create and update.

An omitted or `null` size bound is unset;
`size_max: 0` preserves an empty-file bound, while `size_min: 0` is equivalent
to no lower bound. A minimum greater than the maximum is rejected.

`GET /saved-queries` returns a consistent name-then-ID-sorted page with
`items`, `total`, `limit`, and `offset`. The default limit is 100, the maximum
is 1,000, and an optional `kind` selects one definition type. The stable-ID
route returns one definition and its current ETag. `PATCH` accepts only
`name`, `description`, and `payload`; omitted fields stay unchanged, `null` and
an empty patch are rejected, and `kind` cannot change. Update and delete both
require `If-Match`. A canonical no-op keeps its revision and timestamp.
Replacing `payload` replaces the whole payload, not individual nested fields.
A stale revision returns `412 stale_revision`. A successful create
returns `201`; update and delete return `200` with the resulting or deleted
definition and its ETag. Saved definitions are included in metadata backup and
restore.

These definition endpoints store intent. They do not execute a query,
translate QueryV1 into the current `/search` query string, read document content, render
highlights, or return result counts. The separate `/runs` endpoint above
executes a saved query. Query text is preserved exactly, including quotes,
parentheses, and Boolean operators.

Once audit authority is enabled for a vault, saved-definition reads remain
available but create, update, and delete return `409
audit_mutation_unsupported`. The audited history format does not yet record
this mutation class, so the server leaves every saved row and revision intact.

Saved definitions are included in current backups. Restoring an older supported
backup upgrades it into the current schema with no saved definitions when none
were present. A backup containing saved-definition authority must be restored
with the release that wrote it or a newer release; downgrade readability is not
promised, and an older reader rejects the unknown authority instead of silently
dropping it.

Root-level, outside `/api/v1` and auth-exempt: `GET /health`, `GET
/api/ping` (daemon discovery), `GET /docs` and the OpenAPI documents,
and `/` plus `/assets/` (the static web application, when `[web] enabled`). A hidden `POST
/api/daemon/shutdown` (not in the OpenAPI document) backs `docbank
daemon stop`; it isn't auth-exempt, so it requires both the API key and
its own shutdown token. The hidden `POST /api/daemon/web-session` exchanges
that master authority for a random daemon-lifetime browser token, an
independent upload-proof secret, and the fresh loopback origin dedicated to
that daemon lifetime, while
`DELETE /api/daemon/web-session` revokes the calling browser session. Those
tokens authenticate only the explicit routes used by the built-in document,
tag-definition/assignment, saved-definition, recoverable-trash, storage, job,
configured-backup, and verified-download workflows; they are intentionally not another general API
credential. Browser file bytes use the
hidden `/api/daemon/web-upload` WebSocket instead. The page verifies a
challenge proof over the upload secret before sending bytes, binds the socket
to one session, and never reconnects it. An ordinary browser token is
explicitly forbidden from `POST /api/v1/uploads`.

`GET /nodes/{id}/children` binds the live directory projection—including its
current canonical path—and the requested child page to one read transaction.
Refresh clients therefore do not combine an earlier directory name with a
later child listing.

IDs are canonical everywhere: every response carries them, and mutating
endpoints address nodes by ID so a rename can't strand a concurrent
client's reference.

For stable-identity moves, `PATCH /nodes/{id}` accepts either the lower-level
`new_parent_id`/`new_name` fields or one absolute `dest_path`; these forms are
mutually exclusive. The latter resolves POSIX-style destination semantics and
checks `If-Match` in the same transaction. `POST /path/move` remains the
coordinate-oriented form when the source path itself is the intended target.

Directory creation has the same identity-versus-coordinate choice.
`POST /nodes` accepts a previously resolved `parent_id`, so a concurrent parent
move does not change which directory receives the child. `POST /path/mkdir`
accepts `{"path":"/projects/2026"}` and resolves the existing parent inside
the creation transaction, so the exact coordinate cannot be redirected between
separate client requests. Both return the new node and its canonical path from
that transaction; neither creates missing ancestors.

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
`GET /backup/snapshots?repo=...` returns
`{repository: {id, path}, items: [...]}` so clients can identify the repository
behind the immutable manifests and pagination can be added later without
changing a top-level array contract. A browser session may use this GET only
without `repo`, which confines the web application to the daemon's configured
repository; backup mutations and arbitrary server-path selection still require
the master API authority.

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

`GET /watches` returns `{items: [...]}` in stable watch-name order. Each item
joins the effective machine-local source, virtual destination, settle and scan
durations, and literal exclusions with its current `watch:<name>` job record.
The job is omitted only when no runner has been registered, which is not an
ordinary live-daemon state. Configuration remains file-owned: this endpoint
does not create, edit, or restart watches.

### Path resolution: a query parameter, not a URL segment

`GET /path` takes the virtual path as `?path=/inbox/doc.pdf`, not a
catch-all URL segment (`/path/{path...}`). Stdlib-mux decoding of a
wildcard segment makes a route ambiguous for names containing
`/`-adjacent percent-encoding; a query parameter has one well-defined
encoding instead. The path must be absolute (leading `/`); `?path=/`
resolves the root. The server applies the store's existing NFC name
normalization and validation and returns `422` for an invalid path.

## Batch tag assignment

`POST /api/v1/batch/tags` accepts a canonical UUIDv4 `operation_id`, a tag UUID
`tag_id`, required boolean `assign`, and `nodes: [{node_id, revision}]`.
The body is limited to 1 MiB and 1–1,000 unique live file or directory IDs with
positive exact revisions. No query, subtree expansion, or `If-Match` header is
used. Invalid structure returns 422, missing or trashed targets return 404,
and a stale target returns 412 (`stale_revision`). Every target, including assignment no-ops,
is validated in the same transaction as all changes and receipt persistence.
Actual changes use the existing canonical audit events, with a separate audit
operation for each changed node. The receipt's operation ID identifies the
batch retry; it is not an audit operation ID.

Success returns 200 with `version: 1`, `operation_id`, `request_digest`,
`tag_id`, `assign`, final `tag_revision`, final `assignment_count`,
`completed_at`, and numerically sorted
`nodes: [{node_id, expected_revision, revision, changed}]`. Every requested
identity appears exactly once. Changed nodes advance by one revision;
unchanged nodes retain their expected revision.

Request order is not semantic. The lowercase SHA-256 request digest covers
UTF-8 text: `docbank-tag-batch-v1` followed by newline, the canonical tag UUID
followed by newline, `1` for assignment or `0` for removal followed by newline,
then one `node_id:revision` line per target in ascending numeric node-ID order.
Every line, including the last, ends in newline; integers use unpadded decimal.
The operation UUID is a separate lookup identity, not part of that digest.

Repeating an operation with the same canonical request returns its original
receipt, even after later edits or deletion. Reusing the operation UUID with
different input returns 409 `batch_tag_operation_conflict`. Receipts are
retained indefinitely without node/tag cascade deletion and contain no names,
paths, or document bodies. Metadata and physical backup/restore retain receipts
present at the backup checkpoint; an earlier backup cannot know later
operations. A historical success does not assert current membership or
recreate a deleted entity. Clients must validate complete receipt identity and
revision outcomes before accepting success.

`POST /api/v1/batch/tags/preview` takes only `tag_id` and the same bounded,
revision-fenced `nodes`. One read snapshot returns `tag_id`, `tag_revision`,
and sorted `nodes: [{node_id, revision, assigned}]`, with no durable changes.
This provides exact selected-set membership without interpreting omissions
from a truncated tag listing. Both routes require authentication. Browser
sessions permit only these exact POST paths without query arguments.

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
| `PATCH /saved-queries/{saved_query_id}`, `DELETE /saved-queries/{saved_query_id}` | required — saved-definition revision |
| `POST /saved-queries/{saved_query_id}/runs` | required — executes exactly the saved definition revision the caller inspected |
| `PUT\|DELETE /nodes/{id}/tags/{tag_id}` | required — target node revision; the tag revision also advances on a real assignment change |
| `POST /path/move`, `POST /path/trash` | none — the path is resolved and mutated inside one store transaction, so there is no separate read for a revision to guard |
| `POST /batch/move` | each path source resolves in the transaction; each stable-ID source carries its own required revision |
| `POST /nodes` (create dir) | none — creation has no prior revision; a name collision is `409` |
| `POST /path/mkdir` | none — the exact parent coordinate resolves inside the creation transaction; a name collision is `409` |
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

File nodes and versions also carry `md5`, an auxiliary checksum of the same
logical bytes for interoperability with tools that key on MD5; it is never an
identity. Single-node and version-detail responses include `source_metadata`
when the daemon has extracted metadata from the original bytes (see
[Source metadata](source-metadata.md)); browser-session reads omit fields the
extractor marked sensitive, while API-key reads receive every field. Ingest
preflight reports `cloud_placeholders`, the files whose bytes a cloud-drive
provider has not materialized locally.

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

`GET /nodes/{id}/provenance` returns the requested file node, its live path when
one exists, and a bounded newest-ingest-first page from one read snapshot. Each
fact carries its canonical SHA-256 identity, active/superseded state, ingest
identity and time, source kind and description, original path and mtime, and an
optional superseded-fact identity. A trashed node remains inspectable by ID but
has no live `path`. The route is observation only: it neither accesses the
source nor changes retention authority.

`POST /nodes/{id}/provenance` accepts `source_kind`, `source_description`,
`original_path`, an optional canonical UTC RFC3339Nano `original_mtime` (for
example, `2026-08-26T12:00:00Z`), and an optional
`supersedes` identity. The caller supplies the node's current revision in
`If-Match`; a successful response is `201`, advances that node revision once,
returns the appended fact and its ETag, and records a generic ingest alongside
the fact. `original_path` is opaque evidence and is never opened. A
supersession must point to an active caller-supplied fact on the same node,
while the old fact stays visible and immutable. Operational CLI and
watched-folder ingest facts cannot be superseded: they keep re-ingest
idempotent. Record a newly learned origin as an additional fact instead.

The encoded request body must be smaller than 1 MiB (1,048,576 bytes). A body
at or above that limit receives `413` before the append runs.

### Email metadata and exact parts

Email files keep their original bytes as the immutable content version. The
built-in decoder adds a separate canonical MIME inventory: ordered raw header
spans, decoded fields including Bcc, message and part structure, alternatives,
diagnostics, and exact SHA-256/size receipts for retained artifacts. It does not
normalize the source, deduplicate messages by Message-ID, or turn attachments
into child documents.

Encoded headers, addresses, and multipart structure use the Go standard library. The recipe
records the Go version and fixed resource limits. A structure rejected by the
parser remains a partial inventory with a diagnostic; this includes malformed
part headers and multipart delimiter or preamble lines longer than 4 KiB.
Original bytes and already inventoried parts remain readable.

`GET /versions/{version_id}/email` returns the selected generation for that
exact version. An eligible version still waiting for the daemon worker returns
`202` with its populated version and `state: "pending"`. An undeclared source
returns `422 email_not_supported`; `POST` with `{}` explicitly attempts it
without changing its stored media type or bytes. A suppressed inventory returns
`409 email_derivative_suppressed`.

Generation and part URLs are immutable. A generation must be attached to the
version in the URL, and a part is selected by its dotted structural path plus
one role: `raw_headers`, `decoded_payload`, or `body_utf8`. Missing roles return
`409 email_part_unavailable`; invalid selectors return `422
invalid_email_part`. Part responses are always downloads with
`application/octet-stream`, `nosniff`, complete version, generation,
attachment, and part identity headers, expected SHA-256 and size, and an actual
`Content-Digest` trailer. Clients must read through verified EOF; a partial or
corrupt stream has no successful digest proof.

These routes require the master API credential. Browser-session credentials
cannot read email metadata or part bytes. There is no browser email viewer,
PDF rendering, attachment indexing, mailbox sync, or MBOX import in this
surface.

`GET /content-references` is the inverse identity lookup. It accepts one
canonical lowercase SHA-256 and returns only logical `content_versions`
references backed by blob-catalog authority; it never infers a match from a
loose file or pack entry alone. Each item contains the complete immutable
version, its current node projection, whether that version is the node's
current head, and a path only while the node is live. Results are bounded and
deterministic: live current references, live history, then trashed references.

`GET /duplicates` discovers hashes shared by at least two live current file
nodes. Its `items`, `total`, and `total_references` describe that population;
historical versions and trash are excluded. `limit` defaults to 50 and accepts
1–100; `offset` is nonnegative. Groups sort by canonical SHA-256, and counts
remain exact on an exhausted page. Counts, references, paths, and collection
labels share one read transaction per request.

Each group contains `sha256`, `size`, `reference_count`,
`representative_node_id`, `references`, and `references_truncated`. At most
16 references are displayed in earliest `(modified_at,node_id)` order. Each
reference wraps the existing content-reference identity in `reference`, plus
`collections` (up to 16 distinct eligible IDs and optional labels),
`collection_count`, and `collections_truncated`. Collection identities sort
by ID; multiple import memberships never multiply document counts. The read
is available to API-key clients and browser sessions. Browser access permits
only GET on the exact route, with optional singleton `limit` and `offset`
parameters. There is no duplicate-deletion operation.

See [duplicate discovery](../usage/searching.md#find-documents-with-identical-content)
for pagination and historical-lookup guidance.

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

`POST /ingest/preflight` takes `{paths: [...], include: [...], exclude: [...]}` and performs a
metadata-only source inventory. It opens no regular-file content and writes no
vault metadata or blobs. Its report includes file/directory/logical-byte totals,
pack-eligible, loose-only, and rejected size classes, exclusion/skip/error
counts, bounded findings, and extension summaries. The route uses the same
absolute-path validation, explicit root-directory-symlink behavior, exclusion
rules, loopback fence, and timeout exemption as the real ingest. Findings are
observations rather than a snapshot lock: sources can still change before
ingest, and metadata-only scanning cannot prove later content readability.
Include and exclude patterns use `/` separators on every platform; a backslash
in a pattern is rejected. Entries filtered by a rule are excluded without
failure, while selected non-regular entries are reported as skipped findings.

`POST /ingest` takes **server-side local paths** — `{paths: [...],
dest: "/inbox", include: [...], exclude: [...], replace: false,
collection_label: "Review set"}` — and returns an `IngestReport` (`ingest_id`,
`added`, `skipped`, `excluded`, per-path `failed` entries), backing `docbank
add`. `collection_label` is optional. Paths must be
**absolute**: the long-lived daemon's working directory is meaningless,
so a relative path is rejected with `422`. The CLI resolves `docbank
add`'s arguments to absolute paths before calling, so the command-line
UX still accepts relative and `cwd`-relative sources. Collisions resolve by the
same suffixing rules as other imports when `replace` is false or omitted.

With `replace: true`, the daemon resolves the exact destination name and
records its node revision before reading source content. Changed bytes create
a new version on that node, while equal hash and size skip without changing
the stored MIME type or version history. A live directory fails before source
I/O. A stale revision or exact-name create race fails the file and never falls
back to a suffix.

`POST /ingest/stream` accepts the same body, including `replace`, and returns
`application/x-ndjson`. A metadata-only `scan` stage establishes advisory file
and byte totals, followed by `ingest` progress for bytes read and file outcomes.
Exactly one `result` carrying `IngestReport` or `error` terminates the stream;
HTTP 200 alone is not success. A write failure or client disconnect cancels the
request context used by traversal, blob writing, and metadata transactions.
Already completed files remain valid and converge on retry, while an
incomplete blob never receives node authority.

One request is one logical ingest run. Its `ingest_id` appears only after at
least one document observation commits. A partial import therefore returns the
run that owns its successful files alongside the individual failures. A scan,
an excluded-only request, or an all-failed request returns no `ingest_id` and
creates no collection or label authority. An initial label collision, or an
initial label attempted after permanent audit enrollment, is reported through
the same per-file failure protocol; other files in the request still follow
the established partial-import behavior.

Repeating a logical filesystem import creates a new run and records the
existing same-content node as a member without creating a content version.
That new provenance advances the node revision, even when `collection_label`
and `replace` are omitted or `replace: true` finds identical bytes. A client
holding the previous ETag must read the node again before writing; the old
`If-Match` returns `412 stale_revision`. Within one run, the same observation
is idempotent. The separate unkeyed
`POST /uploads` retry contract is unchanged: an equal retry returns the
existing node without changing its revision and does not expose an unused run
identifier.

Because they grant "read any daemon-readable local path," `POST /ingest` and
`POST /ingest/stream` are checked per-request against `RemoteAddr` and
**restricted to loopback callers** regardless of bind address or API key — a
non-loopback client gets `403` (`loopback_only`). There is no remote file-upload
capability on this route: remote bytes use `POST /uploads`, while remote access
to the loopback-bound daemon still terminates through the configured SSH/VPN
tunnel.

Each include or exclude pattern uses Go's `path.Match` grammar over a
slash-separated source-relative path. Use `/` separators on every platform;
backslashes are rejected. A pattern without `/` matches a basename
at any depth; a pattern containing `/` matches the relative path. `*` and `?`
do not cross `/`, and `**` has no recursive globstar behavior. An include filters
regular files only, while a matching exclusion wins and prunes a directory's
subtree. The preflight and ingest implementations share this compiler so
reviewed selection and actual selection cannot drift. Patterns must be relative;
invalid syntax and parent traversal are rejected before filesystem access. Use
bracket expressions such as `report[[]1].txt` for literal metacharacters instead
of backslash escaping.
Watched-inbox exclusions are a separate literal contract.

## Addendum: ingest-run collections

`GET /api/v1/collections/{id}/quality` returns a current aggregate receipt with
the collection, `source_fingerprint`, dimensions, zero-byte and mismatch counts,
duplicate-document counts, and descriptive concentrations. Optional `fields`
accepts at most seven distinct comma-separated names: `extension`, `media_type`,
`media_family`, `modified_month`, `size`, `text_coverage`, and `duplicates`.
The default includes all seven. Unknown or repeated fields return 422.

Only quality accepts `profile=<configured name>` and includes
`collection.coverage`. Ordinary list, detail, and members reads do not inspect
processing state. One profile is selected automatically; no profiles
returns `unconfigured`, and multiple without a choice returns `profile_required`.
These unavailable states have null counts. An unknown name returns 422.
Configured coverage includes the selected name and fingerprint, active generation,
and complete, partial, failed, unprocessed, and none counts over current members.
Selection identifies policy, not runtime readiness. Retained active output wins
over a later failed attempt; source hashes alone cannot transfer profile authority.

Quality uses one source snapshot, bounded to 250,000 members, 64 MiB of projected
census data, and five seconds. Size bounds return 413 `quality_too_large`;
interruption or timeout returns 503 `quality_unavailable`, not partial results.
Frequency dimensions retain the top 50 values with missing and other counts.
Duplicate membership is vault-wide current content, counted within this collection.
Concentrations require at least ten members and 80% of the collection.
Each request reads current source data. Extension and MIME buckets follow the
query operand rules; values that cannot name a concrete operand count as missing.
MIME parameters do not affect the bucket, matching the MIME search predicate.

A collection is an immutable ingest-run identity with live document
membership. It is not a folder: moving or renaming a member leaves its run
identity intact. Membership excludes caller-supplied `embedded:` provenance,
superseded provenance, directories, and trashed files. Counts and byte totals
deduplicate nodes within a run and describe each member's current version;
they are browsing observations rather than an export snapshot.

Lists default to 100 results and accept `limit=1..1000` and a nonnegative
`offset`. They are ordered by `started_at` descending, then ingest ID. Member
pages use the same bounds and return current `Node` representations with live
paths. The list hides empty runs. A directly addressed run remains readable
when it has retained label authority, even after its final member is trashed or
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

Labels have their own resource and revision. Read that resource for its ETag,
then send the quoted positive revision in `If-Match`. The PUT body contains
exactly one required `label` member. A string sets the label; explicit `null`
clears it. Unknown fields and server-owned fields such as `revision` are
rejected.

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

Labels are case-sensitive NFC strings of 1–256 UTF-8 bytes, with no control
characters and at least one non-whitespace character. Intentional surrounding
spaces are preserved. Non-null labels are unique across all retained label
records, including empty collections, so trash and restore cannot transfer a
name between runs. Clearing retains the revision fence. A stale fence returns
`412 stale_revision`; a collision returns `409 exists`; an invalid name returns
`422 invalid_collection_label`.

Permanent audit authority currently records ingest and membership changes but
has no event for label mutation. Once audit is enabled, label PUTs therefore
fail closed with `409 audit_mutation_unsupported`, including clears and no-op
requests. Existing labels remain readable and portable. Backup readers that
predate collection labels or the `ingest_observe` audit kind reject that added
authority explicitly instead of silently dropping it.

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
payload awaits GC followed by repack. Loose and packed counts may overlap when
one blob has both representations across stores; the receipt reports that
intersection as `mixed_blobs_pending_maintenance`, and its physical byte totals
cover all affected authoritative locations. Pruning itself does not claim
physical space reclamation.

## Addendum: tags

Tag definitions use stable server-generated UUIDv4 identities, mutable,
unique NFC-normalized names, and a revision covering both the definition and
its assignment set. `POST /tags`, `GET /tags`, `GET /tags/by-name`,
and `GET|PATCH|DELETE /tags/{tag_id}` expose definition lifecycle. `GET
/nodes/{id}/tags` and `GET /tags/{tag_id}/nodes` provide bounded forward and
reverse listings; reverse results include a path only for live nodes.
`live_only=true` returns a bounded live projection and `omitted_trashed` count
from one metadata snapshot, which is suitable for an interactive live browser
without assembling paths and trash states across pages.

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

Ingest provenance is filesystem-shaped: each import records the source's
original path and mtime in the store's `provenance` table. Authenticated clients
can also append a post-ingest fact with a source kind, description, opaque
original path, optional mtime, and optional supersession, fenced by the node's
`If-Match` revision. The append records evidence and never opens the supplied
path or looks up a node by content hash.

## Maintenance gate

`gc --run`, `trash empty`, and `verify` need the vault quiescent while
they run — the same reachability-then-delete race described in
[Ownership & Concurrency](locking.md). Rather than the daemon's exclusive
vault lock (held for the daemon's whole lifetime, not per-request), an
in-process `sync.RWMutex`-shaped gate serializes them against regular
mutations: ordinary mutating handlers (`PATCH`, trash, restore, create,
ingest) take the read side and may run concurrently with each other;
maintenance handlers take the write side. Once maintenance is running or
queued for that write side, a new HTTP mutation fails immediately with
`503 maintenance_busy` instead of becoming an indistinguishable long wait.
Daemon-owned background jobs retain blocking gate semantics so a transient
maintenance run does not permanently fail a watcher or extraction worker.
Maintenance routes are exempt from the per-request timeout since
`gc`/`verify` can legitimately run long on a large vault.

Backup creation uses the mutation-exclusive side only for Kit's freeze window.
Once Docbank's deferred read transaction is pinned, ordinary mutations continue
into SQLite's WAL while verified metadata and blob streams are captured. The
backup retains a separate shared preservation lease until capture ends;
maintenance takes that lease exclusively, so GC cannot delete a loose blob or
catalog mapping still referenced by the pinned snapshot. The create route is
timeout-exempt; cancellation still propagates through Kit and prevents
publication of a snapshot manifest.

## MCP is a daemon client

`docbank mcp` is a separate process and listener, not another path into the
vault. It uses the same daemon discovery and authenticated HTTP client as the
CLI, so the daemon remains the only standalone owner of SQLite, blob, pack,
rendition, processing, and lock authority. There is no direct-vault or
remote-daemon mode behind MCP.

The MCP boundary implements exactly protocol `2026-07-28` through the official
Go SDK v1.7.0. Stdio is newline framed. Its optional HTTP transport is
stateless, POST-only, loopback-only, and separately authenticated; the MCP
bearer is resolved from a named credential binding and cannot equal the
daemon's configured, ephemeral, or runtime-discovered API key. The daemon API
key is never accepted as an inbound MCP credential.

MCP exposes nine bounded read tools plus one optional processing enqueue. The
enqueue preserves the daemon's existing consent and plan-fingerprint checks;
it cannot grant consent or replay an ambiguous start. Rendition resources bind
the stable vault, node, content-version, and attachment tuple and expose only
bounded windows of active sanitized Markdown. They do not expose source bytes
or host paths.

This fixed local HTTP bearer is not MCP OAuth. The listener has no OAuth
metadata, authorization-server discovery, client registration, scopes, or
refresh. It also has no GET streams, sessions, resumption, prompts, roots,
sampling, elicitation, or tasks. See [Model Context Protocol](../usage/mcp.md)
for the complete catalog and limits.

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
`/api/ping`, `/docs`, the OpenAPI documents, and the static web application at
`/` and `/assets/` are auth-exempt; everything else, including the shutdown route,
requires the key.

## Error mapping

Errors are RFC 7807 problem-JSON with a `code` extension, a machine-readable
string clients branch on instead of parsing `detail`. Query errors may also
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
| `audit_mutation_unsupported` | 409 | the audited vault does not yet record this logical mutation class |
| `audit_already_enabled` | 409 | an initial-scope preview cannot execute because audit authority was enabled concurrently |
| `audit_scope_overlap` | 409 | the proposed scope shares a live or retained-trash member with permanent protection |
| `audit_scope_limit` | 409 | the vault already has the maximum 1,000 permanent scopes representable by terminal evidence |
| `audit_preview_stale` | 409 | the one-use enrollment preview expired, was consumed, came from another daemon, or no longer matches the vault |
| `audit_acknowledgment_required` | 422 | enrollment execution omitted the explicit permanent-retention acknowledgment |
| `audit_not_enrolled` | 422 | the selected node exists but is outside every permanent audit scope |
| `invalid_audit_cursor` | 422 | the history cursor is malformed or belongs to another stable node or scope |
| `invalid_batch_move` | 422 | a batch has no moves, too many moves, ambiguous selectors, or an invalid final-state plan |
| `invalid_collection_label` | 422 | a collection label violates the canonical UTF-8, NFC, length, control-character, or non-whitespace policy |
| `stale_revision` | 412 | `store.ErrStaleRevision` — `If-Match` didn't match the current revision |
| `provenance_mismatch` | 409 | the requested predecessor is missing, belongs to another node, is already superseded, or is an operational ingest fact |
| `invalid_provenance_time` | 422 | optional `original_mtime` parses as RFC3339 but is not canonical UTC RFC3339Nano (a value that is not a date-time at all fails schema validation as `validation` instead) |
| `invalid_saved_query` | 422 | saved name, description, kind, payload, or patch violates the saved-definition contract |
| `invalid_query` | 422 | invalid or unsupported expression, missing reference, or query compilation bound exceeded |
| `invalid_cursor` | 400 | malformed, tampered, wrong-direction, or otherwise invalid snapshot cursor |
| `snapshot_gone` | 410 | snapshot is missing, expired, revoked, owned by another session, or lost with its daemon |
| `snapshot_capacity` | 429 | bounded snapshot cache admission could not reserve the requested rows or serialized bytes |
| `snapshot_busy` | 429 | both bounded snapshot builders are already occupied |
| `snapshot_too_large` | 413 | one materialization exceeded its row, per-row, or serialized-size bound |
| `snapshot_unavailable` | 503 | snapshot materialization exceeded its build deadline |
| `invalid_profile` | 422 | requested processing profile or coverage selection is invalid |
| `invalid_saved_query_run` | 422 | saved-run identity, revision, or execution request is invalid |
| `not_dir` / `not_file` / `invalid_name` / `invalid_tag` / `not_trashed` / `is_root` | 422 | `store.ErrNotDir` / `ErrNotFile` / `ErrInvalidName` / `ErrInvalidTag` / `ErrNotTrashed` / `ErrIsRoot` |
| `search_query_required` | 422 | blank search without a tag or modification-time filter |
| `validation` | 400, 415, or 422 | malformed request (bad `If-Match`, paths, media type, multipart envelope, or generated validation) |
| `precondition_required` | 428 | required `If-Match` header missing |
| `loopback_only` | 403 | server-path ingest or preflight called by a non-loopback peer |
| `digest_mismatch` / `size_mismatch` | 422 | uploaded file bytes disagree with the required declaration; no node/blob authority committed |
| `too_large` | 413 | upload exceeded its declared size plus bounded multipart overhead |
| `maintenance_busy` | 503 | exclusive vault maintenance is running or queued; retry the mutation after it finishes |
| `pack_retirement_deferred` | 503 | repack authority committed but an old source pack remains physically locked; release the lock, then run `storage pack` reconciliation |
| `unauthorized` | 401 | missing or invalid API key; bad shutdown token |
| `web_session_read_only` | 403 | a daemon-issued browser session attempted an endpoint outside its explicit attenuated allowlist |
| `web_unavailable` | 503 | this daemon is not serving compiled web assets |
| `internal` | 500 | unmapped error (still surfaced with a message — this is a single-user local daemon, not a hardened multi-tenant service) |

## Non-goals

- No server-side rendering. The kit-ui application is static public code; it
  receives a daemon-lifetime attenuated session from `docbank web`. Reads,
  verified-download preparation, and digest-checked file upload remain ordinary
  authenticated API requests. The master API key never enters the browser.
- No multi-user model: one vault and one master authority. Browser sessions are
  attenuated local capabilities, not accounts. Sharing is out of scope for v1.
- No MCP endpoint inside `docbank daemon run`; `docbank mcp` remains a bounded
  client process with its own transport and credential boundary.
- No remote-daemon mode or `[remote]` configuration.
