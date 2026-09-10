---
last_edited: 2026-09-09
title: Searching
description: Ranked, prefix-matching search over document names and verified text content.
---

# Searching

```bash
docbank search insurance
docbank search tax 2026
docbank search return --tag taxes
docbank search report --mime-type application/pdf
docbank search receipt --under /taxes/2026
docbank search report --modified-since 2026-01-01T00:00:00Z
docbank search report --modified-before 2026-04-01T00:00:00Z
docbank search --modified-since 2026-01-01T00:00:00Z --modified-before 2026-04-01T00:00:00Z
docbank search --tag taxes
docbank search report --limit 200
docbank search report --json
```

```
SELECTOR   MATCH    PATH
id:231     name     /taxes/2026/insurance-renewal.pdf
id:198     content  /taxes/2026/car-insurance-notes.md
```

## Semantics

- **Prefix matching.** Every whitespace-separated term matches word
  prefixes: `insur` finds `insurance-renewal.pdf`. Multiple terms must
  all match.
- **Operator-safe.** Query text is escaped before reaching FTS5, so
  `AND`, `OR`, quotes, and parentheses in a query are searched for
  literally, never interpreted. Any string is a safe query.
- **Ranked and bounded.** Name matches retain their BM25 order and appear
  first. Content-only matches follow in their own BM25 order; both groups use
  deterministic name/ID tie-breaks. The default limit is 50; `--limit` accepts
  1–1000, and truncation is always reported.
- **Bounded filter pages.** The query may be omitted when `--tag`,
  `--modified-since`, or `--modified-before` supplies an anchor. Filter-only
  results are ordered by `modified_at` descending and each hit reports
  `filter` in the `MATCH` column. The default limit and truncation behavior
  are unchanged. A blank query with only `--mime-type` or `--under` is rejected;
  those options narrow an anchored search but don't anchor one. Blank includes
  whitespace-only queries. Results include live files and directories, excluding
  the vault root. The limit bounds the response size, not database work.
- **Truncation is not pagination.** If `truncated` is true, the page is incomplete.
  Increasing the limit or narrowing filters may help, but time bounds cannot
  split results with identical modification timestamps. For example, restoring
  more than 1,000 nodes together can leave some unreachable through time bounds
  alone, even at the maximum limit. Search has no continuation cursor.
- **Live nodes only.** Trashed documents don't appear; restore returns
  them to the index. Renames update the index immediately.
- **Current content only.** Retained prior versions stay available through
  `docbank versions`, but ordinary search matches the current selected version
  of each live file.
- **Stable tag filtering.** `--tag <name-or-id>` requires one current tag
  assignment without changing name-before-content ranking. The CLI resolves a
  tag name to its stable UUID before searching; JSON echoes that UUID as
  `tag_id`, so a later rename cannot change what the request meant.
- **Current media-type filtering.** `--mime-type <type/subtype>` requires the
  current file version to have that base media type. The filter is
  case-insensitive and ignores stored parameters, so `text/plain` also matches
  `text/plain; charset=utf-8`. The request itself must be parameter-free;
  directories and historical versions never match it.
- **Stable directory scoping.** `--under <path-or-id>` restricts results to
  descendants of one live directory. The CLI resolves a path or `id:N`
  selector to its stable node ID before searching; JSON echoes that ID as
  `under_node_id`. The selected directory itself is not a result, and moving or
  renaming it does not change its identity.
- **Current modification time.** `--modified-since <timestamp>` includes nodes
  modified at or after an absolute RFC3339 timestamp;
  `--modified-before <timestamp>` excludes that timestamp and everything after
  it. Together they form a half-open interval, so adjacent searches do not
  duplicate a boundary result. Inputs with an explicit offset are normalized
  to UTC and echoed in JSON. The bounds apply to the live node's current
  `modified_at`, not filesystem provenance or the age of retained versions.

For scripts, `--json` returns `hits`, `limit`, and `truncated` without table
formatting. `hits` is always an array, including when nothing matches.

## Save complete query intent over HTTP

The saved-query API keeps a named QueryV1 definition without running it. The
payload carries the full expression and structured filters, so Boolean syntax
is not flattened into the current `docbank search` flags. There is no saved
query CLI or web management screen yet.

This example creates a synthetic definition, reads its ETag, and updates it
under that revision. It expects `DOCBANK_URL`, `DOCBANK_API_KEY`, and `jq`:

```bash
created=$(mktemp)
headers=$(mktemp)

curl --fail-with-body --silent --show-error \
  -D "$headers" -o "$created" \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Synthetic review search",
    "description": "Complete saved intent",
    "kind": "query",
    "payload": {
      "v": 1,
      "text": "status:open AND (owner:me OR owner:team)",
      "syntax": "advanced",
      "mode": "hybrid",
      "filters": {"paths": ["/records"], "extensions": ["md", "txt"]},
      "sort": {"field": "modified_at", "direction": "desc"}
    }
  }' \
  "$DOCBANK_URL/api/v1/saved-queries"

saved_id=$(jq -r .id "$created")
etag=$(awk 'tolower($1) == "etag:" {sub("\\r$", "", $2); print $2}' "$headers")

curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/saved-queries/$saved_id" | jq .

curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H "Content-Type: application/json" \
  -H "If-Match: $etag" \
  -X PATCH \
  -d '{"description":"Reviewed synthetic definition"}' \
  "$DOCBANK_URL/api/v1/saved-queries/$saved_id" | jq .

rm "$created" "$headers"
```

The response payload is canonical structured JSON and includes its SHA-256
fingerprint, revision, and UTC timestamps. Use `GET
/api/v1/saved-queries?kind=query&limit=100&offset=0` to list definitions.
Create, update, and delete return `409 audit_mutation_unsupported` after audit
authority has been enabled; listing and reading still work. Saving either a
query or a literal highlight set does not execute a search or inspect document
content through these endpoints.

## Text extraction

The daemon's `extract:plain-text` background job indexes UTF-8 content whose
media type is `text/*`, `application/json`, `application/x-ndjson`, or
`application/jsonl`. This covers plain text, Markdown, CSV, JSON, and JSONL.
The source blob is read through Docbank's verified loose/packed interface, and
text becomes searchable only after the complete stream reaches verified EOF.

Extraction is bounded to 16 MiB per blob. Larger documents, invalid UTF-8, and
text containing NUL bytes remain stored and readable but are not body-indexed.
Newly ingested or replaced content may take a few seconds to appear while the
daemon job reaches it. A transient open, read, or verification error leaves the
item queued and is retried on a bounded delay; it does not become a permanent
extraction failure. `docbank jobs` shows whether that worker is running.

PDF text layers, office formats, and OCR are unsupported. Their absence never
changes name-search results or document authority.

Next: organize documents beyond paths with
[Organizing & Tagging](organizing.md), or see every search flag in the
[CLI Reference](../cli-reference.md).
