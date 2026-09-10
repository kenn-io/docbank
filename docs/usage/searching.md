---
last_edited: 2026-09-09
title: Searching
description: Ranked, prefix-matching search over document names and verified text content.
---

# Searching

Find live files and folders by name, current text, tags, or modification time.
Search returns at most 50 results by default and tells you when more match.

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

## How do words match?

Each whitespace-separated term matches the start of a word. For example,
`insur` finds `insurance-renewal.pdf`. Every term must match.

Docbank searches query text literally. It escapes `AND`, `OR`, quotes, and
parentheses before passing the query to SQLite's FTS5 search index. You do not
need to escape them yourself.

Name matches appear before content-only matches. Within each group, Docbank
uses BM25, a text-relevance score, then name and ID to break ties consistently.
The `MATCH` column tells you which group produced the result.

## Which documents can match?

Search includes live files and directories, excluding the vault root. Trashed
nodes do not appear. Restoring them returns them to search; renames update the
name index immediately.

Text search uses only the current version of each live file. Read retained
older versions with `docbank versions`; ordinary search does not search them.

## How do I narrow the results?

| Filter | What it selects |
|--------|-----------------|
| `--tag <name-or-id>` | Nodes currently assigned to one tag. |
| `--mime-type <type/subtype>` | Files whose current version has that media type. |
| `--under <path-or-id>` | Descendants of one live directory. |
| `--modified-since <timestamp>` | Nodes modified at or after the timestamp. |
| `--modified-before <timestamp>` | Nodes modified before the timestamp. |

The CLI resolves tag names to stable UUIDs before searching. JSON echoes the
selected UUID as `tag_id`. A later tag rename cannot change the tag selected by
that request, and filtering does not change name-before-content ranking.

Media-type matching ignores case and stored parameters. For example,
`text/plain` also matches `text/plain; charset=utf-8`. The filter itself must
contain no parameters. Directories and historical versions never match it.

The directory filter accepts a path or `id:N`. The CLI resolves it to a stable
node ID, which JSON echoes as `under_node_id`. Moving or renaming that
directory does not change which directory the request selects. The directory
itself is excluded from results.

Time filters require absolute RFC3339 timestamps. Docbank normalizes explicit
offsets to UTC and echoes the bounds in JSON. The start is inclusive and the
end is exclusive, so adjacent ranges do not duplicate a boundary result.
These filters use the live node's current `modified_at`, not the source file's
modification time or the age of an older version.

## Can I search with filters alone?

Yes, when you provide `--tag`, `--modified-since`, or `--modified-before`.
These results use newest `modified_at` first and show `filter` in the `MATCH`
column. The usual result limit still applies.

A blank or whitespace-only query with only `--mime-type` or `--under` is
rejected. Add text, a tag, or a time bound before using those filters.

## How do I handle incomplete results?

`--limit` accepts 1–1000. Docbank always reports when the result was truncated.
For scripts, `--json` returns `hits`, `limit`, and `truncated`; `hits` is always
an array, including when nothing matches. The limit caps response size, not
work inside the database.

Search has no continuation cursor. When `truncated` is true, increase the
limit or narrow the query. Time ranges alone cannot recover every result when
more than 1,000 nodes share one modification timestamp, such as after a large
restore.

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
The worker reads the stored content and checks its hash, whether Docbank stores
it as an individual file or inside a pack. Text becomes searchable only after
that complete read passes verification.

Extraction is bounded to 16 MiB per blob. Larger documents, invalid UTF-8, and
text containing NUL bytes remain stored and readable but are not body-indexed.
Newly ingested or replaced content may take a few seconds to appear while the
daemon job reaches it. A transient open, read, or verification error leaves the
item queued and is retried on a bounded delay; it does not become a permanent
extraction failure. `docbank jobs` shows whether that worker is running.

## Which text is not searched?

Docbank does not extract PDF text layers, office-document text, or text from
images through optical character recognition (OCR). You can still find these
files by name and read their stored bytes.

Next: organize documents beyond paths with
[Organizing & Tagging](organizing.md), or see every search flag in the
[CLI Reference](../cli-reference.md).
