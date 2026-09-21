---
last_edited: 2026-09-20
title: Document processing search
description: Search only the exact document versions a consumer currently authorizes.
---

# Document processing search

Processing search is separate from ordinary `docbank search`. It searches
retained rendition text and, where configured, vectors for an exact source set.
The caller supplies the daemon's vault UUID and one or more immutable content
version IDs; the CLI obtains the vault UUID and sends repeated
`--source-version` values for you.

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111
```

Processing search requires a mode, executable profile, and at least one source
version. Replace `<configured-profile>` with a name reported by `docbank
processing profiles`. It accepts at most 4,096 unique versions and returns at
most 100 results. `--binding` is optional when the selected profile has one
embedding binding and required when it has several. Lexical and auto modes
reject `--binding`.
It cannot be combined with ordinary tag, media-type, directory, or
modification-time filters.

## Consent before semantic or hybrid search

`semantic` and `hybrid` send query text to the selected embedding provider.
They require an active `query_text` consent grant for that profile and provider
disclosure, even when the document's vectors already exist. Missing consent
returns `processing_consent_required`; expired or revoked consent returns
`processing_consent_expired` or `processing_consent_revoked`. Search does not
grant consent automatically.

Preview `docbank processing plan <path-or-id> --profile <configured-profile> --json`
and review its query-text disclosure. Then either:

- Run the reviewed [processing build](document-processing.md) with
  `--plan-fingerprint <fingerprint-from-plan> --consent`. This grants the
  profile's configured operations and runs its document processing.
- Grant consent without rebuilding through
  [`POST /api/v1/processing/consent/grants`](../architecture/http-api.md#processing-consent).
  Send the plan's `selector` and its `fingerprint` as `plan_fingerprint`;
  supply `expires_at` for a grant that expires.

These grants cover the daemon operator's use of that profile across documents
and searches. A profile with reranking includes a `query_text_and_excerpt`
grant in the same approval. Review the reranking provider and its disclosure
alongside the document and query operations. CLI grants have no expiry.
`lexical` and `auto` use retained local text and do not require query-text
consent or call an embedding provider.

## Rerank search results

Add `--rerank` to send the query and bounded candidate excerpts to the profile's
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
auto modes. Review and grant the plan using the consent steps above; search
never grants consent itself.

Human output shows the reranking outcome and candidate count. `--json` includes
a `reranking` receipt independently of `--explain`: `applied` means reranking
succeeded, `skipped` means there were no candidates, and `degraded` means the
search kept its original ordering after a reranking failure. The configured
`degrade` failure policy returns that original ordering; `fail_closed` returns
`reranking_failed` instead. See the [search response contract](../architecture/http-api.md#coverage-and-source-fenced-search)
for receipt fields and degradation causes.

## Modes and coverage

- **`lexical`** searches canonical segments from retained renditions.
- **`semantic`** searches the selected embedding binding.
- **`hybrid`** combines lexical and semantic candidates.
- **`auto`** uses lexical retrieval and reports `lexical` as the actual mode.

Each search response reports the actual mode, degradation, and the number of
complete documents in the supplied source set. The coverage API separately
reports rendition and embedding state, including complete, unavailable, stale,
ineligible, and rebuilding counts. `previous_generation_serving` counts rebuilding
documents whose previous complete result is still available. A rebuild alone
does not establish that a previous result exists. Missing required coverage keeps
the aggregate state `partial`, even while another result is rebuilding. See the
[coverage contract](../architecture/http-api.md#coverage-and-source-fenced-search)
for the fields and snapshot boundary. A direct-file-only embedding can produce a match without an
excerpt because Docbank does not invent readable evidence that was never
retained.

## Source-fenced consumer contract

The source fence is authority, not a post-filter. Docbank validates the vault
identity and applies the exact content-version set before lexical retrieval,
vector scoring, candidate expansion, and reranking. Only vectors attached to
current versions of live documents inside that fence are eligible. Missing
vector-row authority stops the search. A foreign vault or an empty,
duplicate, malformed, or oversized fence is rejected.

Consumers keep durable source identity as `(vault_uid, node_id,
content_version_id)`. A hit's rendition attachment, rendition build, lexical
segment, embedding set, and vector space identify the evidence used for that
query, but they are not a second document authority. Fetch readable evidence
from Docbank's authenticated rendition endpoint instead of retaining a parallel
chunk or vector corpus.

The consumer must still re-check whether each candidate is visible immediately
before presentation. That final check closes the interval between retrieval and
display when a source version is revoked or otherwise becomes unauthorized.

Processing search does not require a user-facing evidence sidecar. Results carry
bounded evidence references and an authenticated rendition carries its own
identity and navigation metadata; consumers may request those when they need to
explain a result.

For ordinary name and verified plain-text search, see [Searching](searching.md).
