---
last_edited: 2026-10-13
title: Automatic and processing search
description: Choose automatic retrieval or an explicit profile and source-version set.
---

# Automatic and processing search

With no processing options, `docbank search renewal terms` automatically tries
hybrid search when a query-capable embedding profile is configured. The CLI
resolves the current versions of all live files through the daemon; you do not
need profile names or version UUIDs for this path. The daemon enforces query
consent and index authority. If the semantic leg is unavailable, search returns
ordinary lexical results and reports a stable degradation reason.

Profiles and query bindings are tried in ascending name order until one
succeeds. If all fail, the command reports the first candidate's failure reason.
With no
configured embeddings, ordinary lexical search runs without degradation. With
embeddings but no query encoder, the reason is `query_embedding_unavailable`.
An empty file scope returns lexical search with `empty_source_scope`; more than
4,096 live versions returns lexical search with `scope_too_large`. The CLI never
silently truncates the semantic population. Limits above 100 use lexical search
with `limit_requires_lexical`, preserving the ordinary 1–1000 range.
Queries longer than 8,192 Unicode characters use lexical search with
`query_requires_lexical`, since processing requests have that length limit.

After semantic retrieval, the daemon revalidates the complete report against
current live versions, including unembedded lexical matches. A match trashed or
replaced during query embedding is removed; moved ancestor paths are refreshed.
A both-lane match whose node revision drifts is removed instead of failing the
report, including when a tag edit leaves its path and content version unchanged.
Removed candidates make the report `truncated`, and surviving result ranks and coverage
reflect the final check. This applies to automatic and explicit semantic/hybrid
search, including when expansion and reranking are disabled.

Tag, directory, MIME, and modification filters always use ordinary lexical
search. With configured embeddings, this reports `filters_require_lexical`.
Use `--mode lexical` alone to select ordinary lexical search explicitly.
Authentication, transport, cancellation, and malformed-response errors remain
errors. Provider/index failures and missing, expired, or revoked consent fall
back with the daemon's stable problem code. Search never grants consent itself.

`--json` retains the report types already exposed by the CLI: ordinary lexical
search returns `hits`, while successful automatic hybrid search returns the
processing report with `results`, actual mode, and coverage. Consumers should
check which report shape was returned. A lexical fallback writes one plain
`degraded: REASON` line to stderr and leaves stdout JSON unchanged.

## Choose exact versions and a profile

Explicit processing search uses rendition text (text derived from a document
version) and, where configured, vectors. It searches only the source set you
name. The CLI obtains the daemon's vault UUID and sends repeated
`--source-version` values for you:

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111
```

Explicit processing search requires a mode, executable profile, and at least one source
version. Replace `<configured-profile>` with a name reported by `docbank
processing profiles`. A search accepts at most 4,096 unique versions and
returns at most 100 results. `--binding` is optional when the selected profile
has one embedding binding and required when it has several. Lexical and auto
modes reject `--binding`. Processing search cannot be combined with ordinary
tag, media-type, directory, or modification-time filters.

## Consent before semantic or hybrid search

`semantic` and `hybrid` send query text to the selected embedding provider.
They require an active `query_text` consent grant for that profile and provider
disclosure, even when the document's vectors already exist. Missing consent
returns `processing_consent_required`. Expired or revoked consent returns
`processing_consent_expired` or `processing_consent_revoked`. Search does not
grant consent automatically.

Preview `docbank processing plan <path-or-id> --profile <configured-profile> --json`
and review its query-text disclosure. Then either:

- Run the reviewed [processing build](document-processing.md) with
  `--plan-fingerprint <fingerprint-from-plan> --consent`. This grants the
  profile's configured operations and runs its document processing.
- Grant consent without rebuilding through
  [`POST /api/v1/processing/consent/grants`](../architecture/http-api.md#processing-consent).
  Send the plan's `selector` and its `fingerprint` as `plan_fingerprint`.
  Supply `expires_at` for a grant that expires.

These grants cover the daemon operator's use of that profile across documents
and searches. A profile with reranking includes a `query_text_and_excerpt`
grant in the same approval. Review the reranking provider and its disclosure
alongside the document and query operations. CLI grants have no expiry.
Explicit processing `lexical` and `auto` use text already stored locally. They do not require
query-text consent or call an embedding provider.

## Rerank search results

Add `--rerank` to send the query and candidate excerpts to the profile's
[configured reranking provider](../configuration.md#search-reranking):

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111 \
  --rerank
```

Reranking works with every search mode and runs only when requested. It needs
the profile's active `query_text_and_excerpt` grant, including in lexical and
auto modes. Review and grant the plan using the consent steps above. Search
does not grant consent itself.

Human output shows the reranking outcome and candidate count. `--json` includes
a `reranking` receipt whether or not you pass `--explain`. In the receipt,
`applied` means reranking succeeded, `skipped` means there were no candidates,
and `degraded` means the search kept its original ordering after a reranking
failure. The configured `degrade` failure policy returns that original
ordering. `fail_closed` returns `reranking_failed` instead. See the
[search response contract](../architecture/http-api.md#coverage-and-source-fenced-search)
for receipt fields and degradation causes.

## Modes and coverage

- **`lexical`** searches canonical segments from renditions.
- **`semantic`** searches the selected embedding binding.
- **`hybrid`** combines lexical and semantic candidates.
- **Explicit `auto`** uses lexical retrieval and reports `lexical` as the actual mode.

HTTP and embedded callers can [prefer content matches over filenames](../architecture/http-api.md#coverage-and-source-fenced-search)
with `content_first: true` or `ContentFirst: true`.

Each search response reports the actual mode, degradation, and the number of
complete documents in the supplied source set. The coverage API separately
reports rendition and embedding state, including complete, unavailable, stale,
ineligible, and rebuilding counts. `previous_generation_serving` counts rebuilding
documents whose previous complete result is still available. A rebuild alone
does not establish that a previous result exists. Missing required coverage keeps
the aggregate state `partial`, even while another result is rebuilding. See the
[coverage contract](../architecture/http-api.md#coverage-and-source-fenced-search)
for the fields and snapshot boundary. A direct-file-only embedding can produce
a match without an excerpt, because Docbank kept no readable text to quote.

## Source-fenced consumer contract

The source fence is the vault identity and content-version set the caller
supplies. Docbank enforces it before retrieval, not as a filter on results. It
validates the vault identity and applies the content-version set before lexical
retrieval, vector scoring, candidate expansion, and reranking. Only vectors
attached to current versions of live documents inside that fence are eligible.
Missing vector-row authority stops the search. A foreign vault or an empty,
duplicate, malformed, or oversized fence is rejected.

Consumers store a source's identity as `(vault_uid, node_id,
content_version_id)`. A hit's rendition attachment, rendition build, lexical
segment, embedding set, and vector space identify the evidence used for that
query. They are not a second identity for the document. Fetch readable
evidence from Docbank's authenticated rendition endpoint instead of keeping a
parallel chunk or vector corpus.

The consumer must still re-check whether each candidate is visible immediately
before presentation. That final check closes the interval between retrieval and
display when a source version is revoked or otherwise becomes unauthorized.

Processing search does not require a user-facing evidence sidecar. Results
carry evidence references, and an authenticated rendition carries its own
identity and navigation metadata. Consumers may request those when they need to
explain a result.

For ordinary name and verified plain-text search, see [Searching](searching.md).
