---
last_edited: 2026-10-14
title: Automatic and processing search
description: Choose automatic retrieval or an explicit profile and source-version set.
---

# Automatic and processing search

When you run `docbank search renewal terms` without processing options, the CLI
tries hybrid search if a processing profile has a query embedding binding. It
asks the daemon to search all live files, and you do not need profile names or
version UUIDs for this path. The daemon enforces query consent and index
authority.

After the hybrid attempt, the CLI runs an ordinary lexical search and shows
those results if hybrid search falls back. Folders have no content version, so
the hybrid source scope cannot include them. When hybrid search succeeds, the
CLI adds folder name matches from the lexical search instead, reading them after
query embedding, like the revalidated hybrid results. Folders count toward
`--limit` and replace the lowest-ranked hybrid results when the page is full.
If either search omits a match, the report is `truncated`.

The CLI tries profiles and query bindings in ascending name order until one
succeeds. If a failure could differ by binding, it moves on to the next pair.
That includes missing consent, `processing_failed`,
`processing_profile_unavailable`, `stale_index`, or `index_unavailable` from a
binding whose vector index is not built. A failure that applies to every
binding stops the search at once. Automatic search covers the whole live
population, subject to the vector generation size limit, and if every pair
fails, the command reports the first failure reason.

Some requests never try hybrid search and report no degradation:

- No profile has a query embedding binding, or processing is unavailable.
- You pass a tag, directory, MIME, or modification filter, or `--fuzzy`.
- `--limit` is above 100, the processing search maximum.
- The query text is longer than 8,192 Unicode characters, the processing
  maximum.
- You pass `--mode lexical` with no processing options.

Cancellation and authentication failures stop the command immediately. Any
other hybrid failure falls back to lexical results and reports the daemon's
stable problem code as the reason. If a failure has no problem code, such as an
invalid response, the command reports `semantic_search_failed` along with the
error text. Search never grants consent itself.

In human and `--json` output, the CLI writes one `degraded: REASON` line to
stderr so stdout keeps the lexical table or JSON unchanged. In `--agent`
output, the reason appears in the header instead.

The shape of `--json` output depends on which search produced the results.
Lexical search returns the ordinary report with `hits`, while a successful
hybrid search returns the processing report with `actual_mode`, `coverage`, and
`results`, plus a `folders` array of ordinary folder name hits. To tell the two
shapes apart, check for `actual_mode`, or pass `--mode lexical` to always get
the ordinary report.

Empty lexical and automatic hybrid reports can include
[`hints`](searching.md#why-did-a-text-search-find-nothing), which other output
modes print to stderr.

After semantic retrieval, the daemon revalidates the complete report against
current live versions, including unembedded lexical matches. It removes any
match trashed or replaced during query embedding, refreshes moved paths, and
removes a file-name match when the file is renamed; a tag edit keeps the match.
Removed candidates make the report `truncated`, and the surviving result ranks
and coverage reflect this final check. This revalidation runs for both
automatic and explicit semantic or hybrid searches, even when expansion and
reranking are disabled.

## Choose exact versions and a profile

Explicit processing search searches only the source set you name, using
rendition text, which is text derived from a document version, and vectors
where configured. The CLI obtains the daemon's vault UUID and sends your
repeated `--source-version` values for you:

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111
```

To run an explicit processing search, provide a mode, an executable profile,
and at least one source version. Replace `<configured-profile>` with a name
reported by `docbank processing profiles`. Each search accepts at most 4,096
unique versions and returns at most 100 results. You can omit `--binding` when
the selected profile has one embedding binding, and you must pass it when the
profile has several. Lexical and auto modes reject `--binding`, and processing
search cannot be combined with ordinary tag, media-type, directory, or
modification-time filters.

## Consent before semantic or hybrid search

Both `semantic` and `hybrid` modes send your query text to the selected
embedding provider. Even when a document's vectors already exist, these modes
require an active `query_text` consent grant for that profile and provider
disclosure. Missing consent returns `processing_consent_required`, while
expired or revoked consent returns `processing_consent_expired` or
`processing_consent_revoked`. Search never grants consent automatically.

If a similar search reports a missing source embedding:

1. Run the displayed `docbank processing plan` command and review the
   disclosure.
2. Follow the [processing build instructions](document-processing.md) to
   process the document.
3. Retry the search.

To grant consent for a profile, preview
`docbank processing plan <path-or-id> --profile <configured-profile> --json`
and review its query-text disclosure. Then choose one of these options:

- Run the reviewed [processing build](document-processing.md) with
  `--plan-fingerprint <fingerprint-from-plan> --consent`. This grants the
  profile's configured operations and runs its document processing.
- Grant consent without rebuilding through
  [`POST /api/v1/processing/consent/grants`](../architecture/http-api.md#processing-consent).
  Send the plan's `selector` and its `fingerprint` as `plan_fingerprint`, and
  supply `expires_at` if you want the grant to expire.

These grants cover the daemon operator's use of that profile across documents
and searches, and CLI grants have no expiry. If the profile includes reranking,
the same approval covers a `query_text_and_excerpt` grant. Review the reranking
provider and its disclosure alongside the document and query operations.
Explicit processing `lexical` and `auto` modes use text already stored locally;
they do not require query-text consent or call an embedding provider.

## Rerank search results

To send your query and candidate excerpts to the profile's
[configured reranking provider](../configuration.md#search-reranking), add
`--rerank`:

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111 \
  --rerank
```

Reranking works with every search mode and runs only when you request it. It
requires the profile's active `query_text_and_excerpt` grant, including in
lexical and auto modes. Review and grant the plan using the consent steps
above; search does not grant consent itself.

Human output shows the reranking outcome and candidate count, while `--json`
includes a `reranking` receipt whether or not you pass `--explain`. In the
receipt, `applied` means reranking succeeded, `skipped` means there were no
candidates, and `degraded` means the search kept its original ordering after a
reranking failure. Under the configured `degrade` failure policy, Docbank
returns that original ordering; under `fail_closed`, it returns
`reranking_failed` instead. For the receipt fields and degradation causes, see
the [search response contract](../architecture/http-api.md#coverage-and-source-fenced-search).

## Modes and coverage

- **`lexical`.** Searches canonical segments from renditions.
- **`semantic`.** Searches the selected embedding binding.
- **`hybrid`.** Combines lexical and semantic candidates.
- **Explicit `auto`.** Uses lexical retrieval and reports `lexical` as the actual mode.

When readable text is available, semantic-only results include the matched
chunk as their excerpt. A direct-file-only embedding can produce a match
without an excerpt, because Docbank kept no readable text to quote.

HTTP and embedded callers can
[prefer content matches over filenames](../architecture/http-api.md#coverage-and-source-fenced-search)
by passing `content_first: true` or `ContentFirst: true`.

Each search response reports the actual mode, degradation, and the number of
complete documents in the supplied source set. Separately, the coverage API
reports rendition and embedding state, including complete, unavailable, stale,
ineligible, and rebuilding counts. `previous_generation_serving` counts
rebuilding documents whose previous complete result is still available, and a
rebuild alone does not establish that a previous result exists. Missing
required coverage keeps the aggregate state `partial`, even while another
result is rebuilding. For the fields and snapshot boundary, see the
[coverage contract](../architecture/http-api.md#coverage-and-source-fenced-search).

## Source-fenced consumer contract

Instead of a source fence, callers may send `scope: {"kind":"all_live"}` with
optional metadata filters. Docbank selects the current live files on the server
and applies that scope throughout retrieval and reranking.

A source fence consists of the vault identity and content-version set that the
caller supplies. Docbank enforces the fence before retrieval rather than
filtering results afterward. It validates the vault identity and applies the
content-version set before lexical retrieval, vector scoring, candidate
expansion, and reranking. Only vectors attached to current versions of live
documents inside that fence are eligible. Missing vector-row authority stops
the search, and Docbank rejects a foreign vault or an empty, duplicate,
malformed, or oversized fence.

Store a source's identity as `(vault_uid, node_id, content_version_id)`. A
hit's rendition attachment, rendition build, lexical segment, embedding set,
and vector space identify the evidence used for that query, not a second
identity for the document. Fetch readable evidence from Docbank's authenticated
rendition endpoint instead of keeping a parallel chunk or vector corpus.

You must still re-check whether each candidate is visible immediately before
presentation. That final check closes the interval between retrieval and display
when a source version is revoked or otherwise becomes unauthorized.

Processing search does not require a user-facing evidence sidecar. Results
carry evidence references, and authenticated renditions carry their own
identity and navigation metadata. Consumers may request those authenticated
renditions when they need to explain a result.

For ordinary name and verified plain-text search, see [Searching](searching.md).
