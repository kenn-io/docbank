---
last_edited: 2026-09-11
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

## Find documents with identical content

Use authenticated `GET /api/v1/duplicates?limit=50&offset=0` to find live
documents whose current versions share a SHA-256 content identity. A group
requires at least two documents. Shared bytes do not mean the archive entries
are interchangeable: each keeps its own node, version, path, and import
memberships. This read does not delete or merge anything.

The response reports exact group and current-reference totals. Groups sort by
SHA-256; `limit` accepts 1–100 groups and defaults to 50. Increase `offset` to
read the next page. Each page reflects one read snapshot, not a frozen result
set across requests, so concurrent changes can affect later pages.

Each group previews at most 16 references and reports `references_truncated`
when more exist. References sort by earliest current modification time, then
node ID; `representative_node_id` identifies the first. This choice does not
assert which document was originally authored first. Each preview also shows
up to 16 eligible collection identities and their labels, with an exact
`collection_count` and `collections_truncated` flag.

Historical versions and trash do not inflate live duplicate groups. To inspect
all retained references for one hash, use `GET /api/v1/content-references` with
`sha256`, `limit`, and `offset`. That lookup distinguishes current versions,
live history, and trash. Duplicate discovery is a separate HTTP read; it does
not change text-search results or automatically collapse them.

## Save complete query intent over HTTP

Save a named search definition when several clients need to reuse it. The
saved-definition HTTP API stores the intent with the vault's metadata. A
separate run endpoint executes a saved query. Saved definitions and run
receipts survive backup and restore; snapshot rows do not.

A query payload uses `QueryV1`: a JSON object with version `v: 1`, search text,
filters, and optional syntax, mode, and sort choices. A saved mode such as
`hybrid` describes intent; it does not enable that mode in `docbank search`.
See the [payload reference](../architecture/http-api.md#saved-query-and-highlight-definitions)
for accepted fields and limits.

The examples below use `DOCBANK_URL` and `DOCBANK_API_KEY` from the
[agent connection setup](../agents/integration.md#give-an-independent-client-a-stable-endpoint).
Create a query with a unique name:

```bash
curl --fail-with-body --silent --show-error -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"name":"Quarterly reports","kind":"query","payload":{"v":1,"text":"quarterly report","filters":{"paths":["/reports"]}}}' \
  "$DOCBANK_URL/api/v1/saved-queries"
```

Keep the returned `id` and `ETag`. The ETag is a quoted revision number, such
as `"1"`. Use the ID to read the definition:

```bash
curl --fail-with-body -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>"
```

To edit it, send the ETag from your last read as `If-Match`. Replace the
example ID and revision with those returned by your daemon:

```bash
curl --fail-with-body -X PATCH \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "1"' \
  -H 'Content-Type: application/json' \
  --data '{"description":"Reports for quarterly review"}' \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>"
```

Only `name`, `description`, and `payload` can change. A supplied `payload`
replaces the complete payload. A stale revision returns `412 stale_revision`;
read again and reconsider the edit. To delete a definition, send `DELETE` to
the same URL with its current `If-Match`.

List definitions with `GET /api/v1/saved-queries?kind=query&limit=100&offset=0`.
The response includes `items`, `total`, `limit`, and `offset`. Omit `kind` to
list both kinds of definition. Each definition has a SHA-256 fingerprint of
its normalized payload, a revision, and UTC timestamps.

### How do I save a highlight set?

A highlight set is an ordered list of literal text and colors. Save one through
the same endpoint with `kind: "highlight_set"`:

```bash
curl --fail-with-body -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"name":"Review terms","kind":"highlight_set","payload":{"v":1,"terms":[{"text":"invoice","color":"#ffff00"},{"text":"due date","color":"#aaffaa"}]}}' \
  "$DOCBANK_URL/api/v1/saved-queries"
```

Use 1–64 unique terms, each 1–256 Unicode characters long. Colors must use
lowercase `#rrggbb`. Terms are literal text, not regular expressions. Order is
preserved. Read, edit, and delete the set by its returned ID, using the same
revision rules as a query.

### What are the saved-definition limits?

The web application manages saved definitions. The CLI and TUI have no matching
management command or screen. Definition CRUD does not execute saved queries,
render highlights, or return result counts; use the saved-query run endpoint
for execution.

Once permanent audit history is enabled anywhere in the vault, create, update,
and delete return `409 audit_mutation_unsupported`. Listing and reading still
work. See [Permanent audited history](audited-history.md) before enabling it
in a vault that needs editable saved definitions.

## Run an exact query over HTTP

Create a daemon-lifetime snapshot when you need the complete QueryV1 contract,
exact totals, and stable pages while the live vault keeps changing:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"query":{"v":1,"syntax":"advanced","text":"name:(budget OR forecast) AND NOT extension:tmp","filters":{"paths":["/projects/example"]},"sort":{"field":"name","direction":"asc"}},"page_size":50,"facets":["extension","tags"]}' \
  "$DOCBANK_URL/api/v1/workspace/queries"
```

The response contains the first rows, exact `total` and `total_bytes`, and a
`snapshot_id`. When `next_cursor` is present, send it back unchanged with the
returned snapshot ID:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"cursor":"<next-cursor>"}' \
  "$DOCBANK_URL/api/v1/workspace/queries/<snapshot-id>/pages"
```

Do not rebuild a cursor or add query, filter, facet, or page-size overrides to
a page request. A missing `next_cursor` means there is no next page. Rows,
ordering, totals, and original node/content versions remain fixed even if the
vault changes after creation.

To execute a saved query, first read its current definition and keep its ETag.
Then run exactly that inspected revision; the request body can change execution
options but cannot replace the saved QueryV1 payload:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "<saved-query-revision>"' \
  -H 'Content-Type: application/json' \
  --data '{"page_size":100,"facets":["collections","text_coverage"]}' \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>/runs"
```

The `{run,snapshot}` response includes a durable receipt comparing this run
with the previous one and the first ephemeral snapshot page. A receipt proves
what ran and its exact totals; it cannot restore the rows. If paging returns
`410 snapshot_gone`, explicitly create a new workspace snapshot or run the
saved query again, then use only the new snapshot's cursors.

Snapshot execution supports duplicate and text-coverage constraints. Supply a
configured `profile` when the query uses text coverage. Semantic and hybrid
modes and relevance ordering remain unsupported. The web query editor runs a
validated draft through this workspace endpoint and renders its exact totals,
facets, frozen authority, and forward/backward pages. See
[Work with a frozen query](web.md#work-with-a-frozen-query).

## Preview a field-aware query

In the web application, choose **Edit query** for expression editing, structured
facet summaries, syntax help, and positioned server errors. A saved query can
open in the same editor; an import collection can start a new collection-scoped
draft. Text, facets, mode, and sort remain together when saved or kept in the
URL. Closing the editor does not discard the draft.

Collection quality can also open a scoped draft from selected distribution
values. For example, selecting `pdf` and `txt` extensions creates
`(extension:"pdf" OR extension:"txt")` with the collection identity retained
as a structured filter. Suggestions require an explicit action and leave live
search unchanged.

The editor never sends an unsupported query through ordinary live search with
constraints removed. After validation, **Run query** creates a new frozen
snapshot from the complete draft. Draft edits do not alter the accepted
snapshot until another run succeeds; closing or reloading the tab preserves
the draft separately.

`POST /api/v1/queries/parse` validates a QueryV1 expression and resolves its
references. It returns `query`, `query_fingerprint`, and `dependencies`, each
with a `kind`, stable `id`, and observed `revision`. It does not return search
results, change saved definitions, or expose SQL. The CLI and `/search` retain
the simple search behavior described above.

For example, submit this JSON to preview a name expression with a separate
size filter:

```json
{
  "text": "name:(budget OR forecast) AND NOT extension:tmp",
  "syntax": "advanced",
  "filters": {"size_min": 100}
}
```

The response retains the complete entered text. Expression fields are not
removed from it or copied into hidden filters.

Advanced syntax supports exact terms, quoted phrases, a trailing `*` for
prefix matching, parentheses, and uppercase `AND`, `OR`, and `NOT`. Whitespace
between operands means `AND`. Precedence is `NOT`, then `NEAR`, then `AND`,
then `OR`. A backslash escapes the next character: `note\:draft` is literal
text, and `\AND` is not an operator. Simple syntax treats these operators as
literal prefix terms, as the CLI does.

`alpha NEAR/5 beta` requires the two terms or phrases to occur within five
tokens in the same indexed source. Bare `NEAR` uses ten; distances range from
zero to 1,000. Chained NEAR expressions and Boolean operands inside NEAR are
rejected. `name:(alpha NEAR/0 beta)` restricts the match to the filename.

| Field | Meaning |
| --- | --- |
| `name` | Filename text, including phrases and prefixes |
| `path` | Exact case-sensitive virtual path or its descendants; quote paths containing spaces |
| `tag`, `collection`, `saved` | Exact name or stable UUID; grouped Boolean choices are supported, prefixes and NEAR are not |
| `mime`, `extension`, `media_family` | Current MIME essence, filename extension, or shared media family |
| `modified_after`, `modified_before` | Inclusive lower and exclusive upper RFC3339 time bounds; quote timestamps |
| `size_min`, `size_max` | Inclusive nonnegative byte bounds |

Reference names are case-sensitive and Unicode-normalized. A canonical UUID
selects an ID, without falling back to a name. Unknown references fail even
under `NOT`. A saved reference expands as a grouped expression with its own
filters, so `name:budget OR saved:Review` does not apply Review's filters to
the name branch. Nested field overrides such as `name:(tag:urgent)` are not
supported.

Structured filter dimensions combine with `AND`. Values within one include
list combine with `OR`; an exclude list removes that union. Paths are subtree
constraints, not wildcard patterns. Collection unions do not duplicate files.
Time comparisons retain nanosecond precision. Preview and snapshot execution
support duplicate and text-coverage constraints; snapshot execution requires a
configured processing profile for coverage predicates. Semantic/hybrid mode
and relevance ordering return explicit errors instead of being ignored.

Expression errors return `422 invalid_query` with `position.offset` and
`position.end`: a half-open UTF-8 byte span in the submitted text. Database
failures remain server errors. Preview limits include 8,192 text code points,
512 parsed nodes, 32 levels of syntactic nesting, and 16 nested saved
references. Larger expansions and compiled predicates have additional bounds.
Dependency revisions describe this preview, not a permanent result snapshot.

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

The daemon does not automatically extract PDF text layers, office-document
text, or text from images through optical character recognition (OCR). You
can still find these files by name and read their stored bytes. The
[document processing libraries](../document-understanding.md) provide additional
processing options for applications; adding a file does not start them.

The CLI, HTTP search endpoint, web application, and TUI use lexical search:
matching words in names and indexed text. They do not expose semantic search
by meaning, hybrid search that combines both methods, query expansion, or
reranking. Saving those choices in QueryV1 does not run them.

Next: organize documents beyond paths with
[Organizing & Tagging](organizing.md), or see every search flag in the
[CLI Reference](../cli-reference.md).
