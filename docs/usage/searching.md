---
last_edited: 2026-11-11
title: Searching
description: Ranked, prefix-matching search over document names and verified text content.
---

# Searching

You can search live files and folders by name, current text, tags, or
modification time. To require adjacent words, quote a phrase inside the query,
as in `search '"tax 2026"'`. Without processing options, search automatically
uses hybrid retrieval when a query-capable embedding profile is available and
uses ordinary lexical search otherwise; a failed hybrid attempt falls back with
an explicit degradation reason. For selection rules and scope limits, see
[Automatic and processing search](search.md). By default, search returns at
most 50 results and tells you when more match.

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

To exclude documents whose watched source is known to be gone, add
`--source-present`, or omit it to include those archived documents. This flag
works with ordinary and fuzzy search, requires a query or the usual tag or time
anchor, and cannot be combined with processing-mode flags or `--similar-to`.
HTTP search and the document catalog accept `source_present=true` and echo it
in their responses. Processing search filters accept the same boolean and
search every matching live file without a 4,096-version cap, which applies to
explicit version lists. Empty scopes still run a normal search, including query
and profile validation. These observations do not establish that every unmarked
source exists. To find other documents holding the same bytes, use
`GET /api/v1/duplicates`.

## Find PDFs by recorded evidence

To filter PDFs by recorded evidence, pass a PDF fact expression to
`--pdf-filter`, or select **Query syntax** in the web app and enter the
expression under **Advanced**. You can save and reopen these queries.

```bash
docbank search --pdf-filter 'pdf_last_signature_covers_file:false'
docbank search --pdf-filter 'pdf_embedded_file_count_min:1 OR pdf_has_javascript:true'
docbank search --pdf-filter 'pdf_permissions:"Print"' --under /cases
```

| Field | Operand |
| --- | --- |
| `pdf_encrypted` | `true` or `false` |
| `pdf_last_signature_covers_file` | `true` or `false` |
| `pdf_has_javascript` | `true` or `false` |
| `pdf_has_launch_actions` | `true` or `false` |
| `pdf_signature_field_count` | Exact nonnegative count |
| `pdf_embedded_file_count` | Exact nonnegative count |
| `pdf_permissions` | One exact, case-sensitive permission label |

For an inclusive bound on either count field, append `_min` or `_max`. You can
combine fields with `AND`, `OR`, `NOT`, and parentheses, and quote permission
labels containing spaces. Labels are Print, Modify, Copy text and graphics,
Annotate and fill forms, Fill forms, Extract for accessibility, Assemble,
High-quality print flag, and None.

Background extraction may still be recording facts after an import or upgrade.
Only recorded observations match, and an unknown observation matches neither
`false` nor zero. `NOT` excludes matching documents and can include documents
with unknown observations. Only the active evidence for the current file bytes
counts. Signature coverage describes the declared byte range, without
authenticating the signer. For the facts' limits, see
[PDF evidence](../architecture/source-metadata.md#pdf-evidence).

If you want only files labeled as PDFs, narrow the result type with
`--mime-type application/pdf`, or `mime:application/pdf` in Query syntax.
Evidence belongs to the bytes, so an alias labeled with a different MIME type
can share the same facts. `NOT` can also include non-PDF files with unknown
observations; it excludes folders.

In the CLI, `--pdf-filter` accepts only PDF fields. You can use it to anchor a
search without text and combine it with tag, MIME, folder, time, and fuzzy
filters. It keeps automatic search lexical and rejects explicit
processing-search options and `--similar-to`. HTTP search accepts `pdf_filter`
and echoes the expression in its response.

Ordinary lexical output looks like this:

```
SELECTOR   MATCH    PATH
id:231     name     /taxes/2026/insurance-renewal.pdf
id:198     content  /taxes/2026/car-insurance-notes.md
```

[Photos search](photos.md#save-and-freeze-a-photo-search) supports saved smart
sets, frozen albums, and authored captions and credits.

QueryV1 and saved queries also support
[photo quality filters](photos.md#quality-signals). For example, in advanced
search, `brightness_min:0.4 color_blue_min:0.5` selects bright, blue-leaning
photos, while `unevaluated:true` selects photos with missing measurements.

## How do words match in lexical search?

Each prepared word matches the start of a word along with its regular forms.
For example, `insur` finds `insurance-renewal.pdf`, and `invoices` finds
`invoice`. Docbank drops common words when content words remain, and every
remaining word and quoted phrase must match in the same name or text segment.
Quoted phrases require adjacent words in order.

Docbank does not interpret unquoted `AND`, `OR`, `NOT`, or parentheses as
Boolean operators. Use balanced quotes to delimit phrases.

Name matches appear before content-only matches. Within each group, Docbank
ranks results with BM25, a text-relevance score, and then breaks ties
consistently by name and ID. The `MATCH` column tells you which group produced
each result.

## What if I remember a misspelled filename or path?

To append filename and path matches after ordinary name and content hits, add
`--fuzzy`. All existing filters still apply.

```bash
docbank search invocie --fuzzy --under /cases/acme
docbank search cases/acme/invocie --fuzzy
docbank search /cases/acme/invoice --fuzzy --json
```

Supplemental matching requires ASCII query text. Compared names may still
contain non-ASCII spaces, punctuation, and symbols, such as the narrow no-break
space in macOS screenshot names, an en dash, or a curly apostrophe, while names
with a non-ASCII letter or digit, a combining accent, or a private-use
character are skipped. For slash queries, the same rule applies to every
compared component; extra unmatched ancestors may contain any characters and
keep their original spelling. Matching folds ASCII letters and splits on every
other character, and each query word matches a word in its component using a
clean prefix or the allowed edit. The entire query may contain one insertion,
deletion, substitution, or adjacent transposition. An edited word must be at
least five characters long, its first character must match, and edited
characters cannot be digits. For example, `invocie` finds `invoice.pdf`,
whereas `mian` does not find `main.go`, and year changes are not guessed.

Without a slash, the supplemental matcher checks the basename. Slash queries
match consecutive path components ending in the node's own name. A leading
slash requires the sequence to begin at the vault root, while other slash
queries may have extra ancestors. An ancestor-only query does not list
descendants. Backslash is treated as punctuation on every platform, and glob,
regular-expression, and boolean syntax are not added.

Queries outside the supplemental syntax still run ordinary search, and the
receipt reports `not_run` with `query_ineligible`. That covers:

- Non-ASCII query text, since Unicode typo matching is not supported.
- Queries with more than 256 characters, 16 words, 64 characters in one word,
  or eight path components.
- An empty, repeated-slash, trailing-slash, `.`, or `..` component, or a
  component without a letter or digit.
- Basename queries without a word of five characters; short slash queries can
  still match exact paths.

Ordinary hits keep their original order and source, while supplemental hits
show `fuzzy_name` or `fuzzy_path` in `MATCH`, including zero-edit path matches.
Supplemental zero-edit hits appear before one-edit hits. Ties prefer whole
words, then less unmatched word suffix, then full path and node ID.

Fuzzy search requires query text even when you pass tag or date filters, and
you cannot combine it with processing search, similarity, explanations, or
reranking.

## How complete is fuzzy discovery?

Fuzzy discovery checks at most 2,048 scoped candidates in node-ID order and
shares a 200 ms cancellation budget across snapshot acquisition, scope
validation, SQL, matching, and path resolution. Ordinary search runs first and
keeps its current behavior. Supplemental names are capped at 512 characters,
ancestry at 64 nodes including the root, and output paths at 4,096 UTF-8 bytes.
A candidate past the ancestry or path bound counts toward `path_limit` only if
the path components already read could still match a slash query. Fuzzy
matching reads no archive blobs or source directories, and moving files does
not require rebuilding a separate path index.

Only `--fuzzy` reports include a `fuzzy` coverage receipt, and its `reasons`
field is always an array, including when there are no results:

```json
{"status":"limited","reasons":["candidate_limit"]}
```

| Status | Meaning |
|--------|---------|
| `complete` | All scoped candidates under the documented matching rules were checked; `reasons` is empty. |
| `limited` | A candidate, name, path, or time bound prevented complete coverage. |
| `not_run` | Ordinary results filled the page, or the query is outside the supplemental domain. |

Reasons sort alphabetically: `candidate_limit`, `name_limit`, `path_limit`,
`query_ineligible`, `result_limit`, and `time_budget`. A timeout discards the
entire supplemental batch so partial scoring cannot change its ranking, while
ordinary hits still return. Canceling the request itself remains an error.

You can receive an incomplete receipt alongside zero hits and
`truncated: false`. `truncated` still means a verified match was omitted by the
result limit, and human output prints an incomplete-search notice. To recover a
candidate missed by a broad search, use a more specific query or narrow the
scope with `--under`. If ordinary hits leave no room for supplemental
matches, increase `--limit`.

## Which documents can match?

Search covers live files and directories, excluding the vault root. Hybrid
search ranks current versions of live files and then appends directory name
matches from ordinary search. Trashed nodes do not appear until you restore
them, and renames update the name index immediately.

Text search checks only the current version of each live file; ordinary search
does not cover older versions. To read older versions, use
`docbank document versions`.

## How do I narrow the results?

Adding any of these filters selects ordinary lexical search even when hybrid
search is available, and `--mode lexical` alone also accepts them.

| Filter | What it selects |
|--------|-----------------|
| `--tag <name-or-id>` | Nodes currently assigned to one tag. |
| `--mime-type <type/subtype>` | Files whose current version has that media type. |
| `--under <path-or-id>` | Descendants of one live directory. |
| `--modified-since <timestamp>` | Nodes modified at or after the timestamp. |
| `--modified-before <timestamp>` | Nodes modified before the timestamp. |

Before searching, the CLI resolves tag names to stable UUIDs and echoes the
selected UUID in JSON as `tag_id`. A later tag rename cannot change the tag
selected by that request, and filtering does not change name-before-content
ranking.

In photo queries, free-text searches match authored captions, every tag name
assigned to the photo, and pending suggested text. A phrase matches inside one
caption, tag, or suggested field and never across two.

Media-type matching ignores case and stored parameters. For example,
`text/plain` also matches `text/plain; charset=utf-8`. Do not include
parameters in the filter itself. Directories and historical versions never
match a media-type filter.

The directory filter accepts either an absolute virtual path or `id:N`. When
you pass a path, the daemon resolves it during the search and JSON echoes both
the authored `under_path` and the resolved stable `under_node_id`. Paths are
case-exact and at most 16,384 UTF-8 bytes; a file path fails as `not_dir`, and
a missing or trashed folder fails as `not_found`. Passing `id:N` sends
`under_node_id`, which keeps selecting that directory after a move or rename.
The directory itself is excluded from results. HTTP and MCP callers may send
either `under_path` or `under_node_id`, and sending both fails validation
before retrieval or a provider call.

Time filters require absolute RFC3339 timestamps. Docbank normalizes offsets to
UTC and echoes the bounds in JSON. The start is inclusive and the end is
exclusive, so adjacent ranges do not duplicate a boundary result. Both filters
use the live node's current `modified_at`, not the source file's modification
time or the age of an older version.

## Find documents and photos by person

In QueryV1 advanced expressions, you can filter by person with
`person:"Ada Lovelace"` or `person:<person-id>`, while structured queries use
`filters.person_ids`. Names match exactly after Unicode folding. Duplicate
names require an ID, and merged IDs resolve to the surviving person.

Document queries include current published person links and direct assertions,
while stale published links stay out until rebuilt. Photo queries match
depicted-person assertions across the asset's live image and RAW members using
each member's current version, and they keep Hidden photos locked.

## Find documents by person and role

With advanced QueryV1 expressions, you can select what a person did on a
current document version. Use a quoted exact name or a person ID, and combine
the operands with `AND`, `OR`, `NOT`, and other filters. Photo queries reject
role operands and `filters.people`; use `person:` or `filters.person_ids` for
depicted people instead.

| Operand | Document roles |
| --- | --- |
| `author:` | Author |
| `from:` | Sender or organizer |
| `to:` | Recipient, copied or attendee; full access also includes blind copy |
| `cc:` | Copied |
| `bcc:` | Blind copy; API-key access only |
| `custodian:`, `participant:`, `organizer:`, `attendee:` | The named role |
| `between:(A, B)` | Both people have a non-custodian role on the same version |
| `mentions:` | Text contains the person's display name or a name alias |

For example, `from:"Mara Okonkwo-Fell"`, `custodian:"Mara Okonkwo-Fell"`, and
`between:("Mara Okonkwo-Fell", "Ines Albarrán")` are ordinary queries that you
can save and export. `between:` accepts exactly two distinct people separated
by a comma or whitespace, and invalid pairs report
`between: needs two different people`. `mentions:` searches all distinct names
as literal phrases and labels a matched result `Mentioned`.

Browser searches refuse `bcc:` and structured `blind_copy` role filters, and
their other role searches omit Bcc evidence. API-key and MCP searches retain
full evidence. `person:` keeps matching any role.

While people evidence is incomplete, a version matches neither a role search
nor its `NOT` unless an operator assertion establishes the role, though an
independent `OR` branch can still match it. To inspect pending and failed work,
use the document event coverage report. The People facet is unavailable while
any document in its population has incomplete people evidence, including
pending, failed, or unavailable processing.

In structured queries, you can use
`"people":[{"person_id":"00000000-0000-4000-8000-000000000001","roles":["sender","organizer"]}]`
inside `filters`. Each entry is required, while its roles act as alternatives.
The People facet uses safe evidence for every caller and ignores only root
`filters.people` entries when counting. Result roles, facet names, and counts
are frozen with the snapshot, and later renames or republishing do not change
them. PostgreSQL search is not yet qualified.

In the web app, frozen results show chips beside each name for roles named in
positive query operands or people filters that those people hold on the
document, such as `Sender` or `Cc`. A chip can appear even when its query
branch did not select the result, and `Mentioned` marks text that contains a
name from a positive `mentions:` operand. The **People** filter group lists up
to 50 people with their counts and roles. Selecting a person adds a
`filters.people` entry with the roles shown, and selecting them again removes
only that person's entries.

## Can I search with filters alone?

Yes, when you provide `--tag`, `--modified-since`, `--modified-before`, or
`--pdf-filter`. These searches sort by newest `modified_at` first, show
`filter` in the `MATCH` column, and follow the usual result limit.

Docbank rejects a blank or whitespace-only query that includes only
`--mime-type` or `--under`. Add text, a tag, or a time bound before using
those filters.

## How do I handle incomplete results?

`--limit` accepts values from 1 to 1000, and Docbank always reports when
results are truncated. For scripts, ordinary lexical `--json` returns `hits`,
`limit`, and `truncated`, while a successful hybrid search returns the
processing report with `actual_mode`, `results`, coverage, and `folders`
instead. For details, see [Automatic and processing search](search.md). On a
lexical fallback, the CLI writes `degraded: REASON` to stderr and keeps stdout
JSON unchanged.

In ordinary lexical JSON, `hits` is always an array, including when nothing
matches. The limit caps response size rather than work inside the database.

Search has no continuation cursor. When `truncated` is true, increase the limit
or narrow the query. Time ranges alone cannot recover every result when more
than 1,000 nodes share one modification timestamp, such as after a large
restore.

## Find documents with identical content

To find live documents whose current versions share a SHA-256 content identity,
call authenticated `GET /api/v1/duplicates?limit=50&offset=0`. A group requires
at least two documents. Shared bytes do not make the archive entries
interchangeable, and each keeps its own node, version, path, and import
memberships. This read does not delete or merge anything.

The response reports exact group and current-reference totals, with groups
sorted by SHA-256. `limit` accepts 1 to 100 groups and defaults to 50; increase
`offset` to read the next page. Each page reflects one read snapshot rather
than a frozen result set across requests, so concurrent changes can affect
later pages.

Each group previews at most 16 references and reports `references_truncated`
when more exist. References sort by earliest current modification time and then
node ID, with `representative_node_id` identifying the first entry. This choice
does not assert which document was originally authored first. Each preview also
shows up to 16 eligible collection identities and their labels, along with an
exact `collection_count` and `collections_truncated` flag.

Historical versions and trash do not inflate live duplicate groups. To inspect
all retained references for one hash, call `GET /api/v1/content-references` with
`sha256`, `limit`, and `offset`. The lookup distinguishes current versions, live
history, and trash. Duplicate discovery is a separate HTTP read and does not
change text-search results or automatically collapse them.

## Export search counts

To compare date-scoped search terms in an eight-column CSV and keep a frozen
evidence ZIP, use [Search exports](search-exports.md). Recent export history
keeps reusable requests, while named saved queries are a separate workflow.

## Photo query predicates

Saved QueryV1 definitions can include photo kind, case-insensitive complete
camera or lens make or model, exact location labels through `location:` or
`filters.locations`, safe integer ISO bounds, capture dates, decimal-string GPS
boxes, asset UUID sets, and album UUID sets through `set:` or
`filters.set_ids`. Existing tag, text, and collection predicates combine
through the same compiler. For the complete field and sort contract, see
[Browse photo assets over HTTP](photos.md#browse-photo-assets-over-http).

Photos evaluates photo metadata predicates against the selected display file,
while ordinary file predicates can still match individual members. Document
snapshots use each document's own metadata and support these predicates, though
`capture_time` and `import_time` sorting is available only in Photos. Album
browsing also accepts `added_time` with an explicit album scope. A document
snapshot request naming any of these fields returns an error.

## Save complete query intent over HTTP

When several clients need to reuse a search, save it as a named definition
through the saved-definition HTTP API. Docbank stores the intent with the
vault's metadata, and a separate run endpoint executes a saved query. Saved
definitions and run receipts survive backup and restore, whereas snapshot rows
do not. You can also manage saved definitions in the
[web management screen](web.md#saved-queries-and-highlights).

A query payload uses `QueryV1`, a JSON object with version `v: 1`, search text,
filters, and optional syntax, mode, and sort choices. A saved mode such as
`hybrid` describes intent, and query snapshots reject it.
[Explicit processing search](search.md#choose-exact-versions-and-a-profile)
requires a named processing profile and a source-version fence. For accepted
fields and limits, see the
[payload reference](../architecture/http-api.md#saved-query-and-highlight-definitions).

The examples below use `DOCBANK_URL` and `DOCBANK_API_KEY` from the
[agent connection setup](../agents/integration.md#give-an-independent-client-a-stable-endpoint).
First, create a query with a unique name:

```bash
curl --fail-with-body --silent --show-error -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"name":"Quarterly reports","kind":"query","payload":{"v":1,"text":"quarterly report","filters":{"paths":["/reports"]}}}' \
  "$DOCBANK_URL/api/v1/saved-queries"
```

Keep the returned `id` and `ETag`, which is a quoted revision number such as
`"1"`. To read the definition, pass its ID:

```bash
curl --fail-with-body -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>"
```

To edit the definition, send the ETag from your last read as `If-Match`,
replacing the example ID and revision with the values your daemon returned:

```bash
curl --fail-with-body -X PATCH \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "1"' \
  -H 'Content-Type: application/json' \
  --data '{"description":"Reports for quarterly review"}' \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>"
```

Only `name`, `description`, and `payload` can change, and any supplied
`payload` replaces the complete payload. If your revision is stale, the daemon
returns `412 stale_revision`; read the definition again and reconsider the
edit. To delete a definition, send `DELETE` to the same URL with its current
`If-Match`.

To list definitions, call
`GET /api/v1/saved-queries?kind=query&limit=100&offset=0`, or omit `kind` to
list both kinds of definition. The response includes `items`, `total`, `limit`,
and `offset`, and each definition has a SHA-256 fingerprint of its normalized
payload, a revision, and UTC timestamps.

### How do I save a highlight set?

A highlight set is an ordered list of literal text and colors. To save one,
call the same endpoint with `kind: "highlight_set"`:

```bash
curl --fail-with-body -i \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"name":"Review terms","kind":"highlight_set","payload":{"v":1,"terms":[{"text":"invoice","color":"#ffff00"},{"text":"due date","color":"#aaffaa"}]}}' \
  "$DOCBANK_URL/api/v1/saved-queries"
```

Include 1 to 64 unique terms, each 1 to 256 Unicode characters long, and format
colors as lowercase `#rrggbb`. Terms are literal text rather than regular
expressions, and Docbank preserves their order. You can read, edit, and delete
the set by its returned ID using the same revision rules as a query.

### What are the saved-definition limits?

You can manage saved definitions in the web application or over HTTP; the CLI
and TUI have no matching management command or screen. Creating, reading,
updating, or deleting a definition does not execute saved queries, render
highlights, or return result counts. To execute a saved query, use the
saved-query run endpoint.

After permanent audit history is enabled anywhere in the vault, create, update,
and delete return `409 audit_mutation_unsupported`, while listing and reading
still work. Review [Permanent audited history](audited-history.md) before
enabling it in a vault that needs editable saved definitions.

## Run an exact query over HTTP

When you need the complete QueryV1 contract, exact totals, and stable pages
while the live vault keeps changing, create a daemon-lifetime snapshot:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"query":{"v":1,"syntax":"advanced","text":"name:(budget OR forecast) AND NOT extension:tmp","filters":{"paths":["/projects/example"]},"sort":{"field":"name","direction":"asc"}},"page_size":50,"facets":["extension","tags"]}' \
  "$DOCBANK_URL/api/v1/workspace/queries"
```

The response contains the first rows, exact `total` and `total_bytes`, and a
`snapshot_id`. When `next_cursor` is present, send it back unchanged with the
returned snapshot ID to fetch the next page:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"cursor":"<next-cursor>"}' \
  "$DOCBANK_URL/api/v1/workspace/queries/<snapshot-id>/pages"
```

Do not rebuild a cursor or add query, filter, facet, or page-size overrides to
a page request. A missing `next_cursor` means there is no next page. Rows,
ordering, totals, and original node and content versions stay fixed even if the
vault changes after creation.

To execute a saved query, read its current definition, keep its ETag, and run
that revision. The request body can change execution options, but it cannot
replace the saved QueryV1 payload:

```bash
curl --fail-with-body --silent --show-error \
  -H "X-Api-Key: $DOCBANK_API_KEY" \
  -H 'If-Match: "<saved-query-revision>"' \
  -H 'Content-Type: application/json' \
  --data '{"page_size":100,"facets":["collections","text_coverage"]}' \
  "$DOCBANK_URL/api/v1/saved-queries/<saved-query-id>/runs"
```

The `{run,snapshot}` response includes both a durable receipt comparing this
run with the previous one and the first ephemeral snapshot page. The receipt
proves what ran and its exact totals, though it cannot restore the rows. If
paging returns `410 snapshot_gone`, create a new workspace snapshot or run the
saved query again, then use only the new snapshot's cursors.

Snapshot execution supports duplicate and text-coverage constraints; supply a
configured `profile` when the query uses text coverage. Semantic and hybrid
modes and relevance ordering remain unsupported. The web query editor runs a
validated draft through this workspace endpoint and renders its exact totals,
facets, frozen authority, and forward and backward pages. For details, see
[Work with a frozen query](web.md#work-with-a-frozen-query).

## Preview a field-aware query

In the web application, choose **Edit query** for expression editing,
structured facet summaries, syntax help, and positioned server errors. You can
open a saved query in the same editor, and you can start a new
collection-scoped draft from an import collection. Text, facets, mode, and sort
stay together when saved or kept in the URL, and closing the editor does not
discard the draft.

From collection quality, you can also open a scoped draft from selected
distribution values. For example, selecting the `pdf` and `txt` extensions
creates `(extension:"pdf" OR extension:"txt")` with the collection identity kept
as a structured filter. Suggestions require an explicit action and leave live
search unchanged.

The editor never sends an unsupported query through ordinary live search with
constraints removed. After validation, **Run query** creates a new frozen
snapshot from the complete draft, and draft edits do not alter the accepted
snapshot until another run succeeds. Closing the editor keeps the draft in the
current session; save a named definition before reloading or leaving it.

Over HTTP, `POST /api/v1/queries/parse` validates a QueryV1 expression and
resolves its references, returning `query`, `query_fingerprint`, and
`dependencies`, where each dependency includes a `kind`, stable `id`, and
observed `revision`. It does not return search results, change saved
definitions, or expose SQL. The CLI and `/search` keep the simple search
behavior described earlier.

For example, submit this JSON to preview a name expression with a separate
size filter:

```json
{
  "text": "name:(budget OR forecast) AND NOT extension:tmp",
  "syntax": "advanced",
  "filters": {"size_min": 100}
}
```

The response keeps the complete entered text without removing expression fields
or copying them into hidden filters.

Advanced syntax supports exact terms, quoted phrases, a trailing `*` for
prefix matching, parentheses, and uppercase `AND`, `OR`, and `NOT`. Whitespace
between operands means `AND`, and precedence is `NOT`, then `NEAR`, then `AND`,
then `OR`. Use a backslash to escape the next character, so `note\:draft` is
literal text and `\AND` is not an operator. Simple syntax treats these
operators as literal prefix terms.

An expression such as `alpha NEAR/5 beta` requires the two terms or phrases to
occur within five tokens in the same indexed source, while bare `NEAR` uses
ten. Distances range from zero to 1,000, and Docbank rejects chained NEAR
expressions and Boolean operands inside NEAR. To restrict the match to the
filename, use `name:(alpha NEAR/0 beta)`.

| Field | Meaning |
| --- | --- |
| `name` | Filename text, including phrases and prefixes |
| `path` | Exact case-sensitive virtual path or its descendants; quote paths containing spaces |
| `tag`, `collection`, `saved` | Exact name or stable UUID; grouped Boolean choices are supported, prefixes and NEAR are not |
| `mime`, `extension`, `media_family` | Current MIME essence, filename extension, or shared media family |
| `modified_after`, `modified_before` | Inclusive lower and exclusive upper RFC3339 time bounds; quote timestamps |
| `size_min`, `size_max` | Inclusive nonnegative byte bounds |

Reference names are case-sensitive and Unicode-normalized. A canonical UUID
selects an ID without falling back to a name, and unknown references fail even
under `NOT`. A saved reference expands as a grouped expression with its own
filters, so `name:budget OR saved:Review` does not apply Review's filters to
the name branch. Nested field overrides such as `name:(tag:urgent)` are not
supported.

Structured filter dimensions combine with `AND`, values within one include list
combine with `OR`, and an exclude list removes that union. Paths act as subtree
constraints rather than wildcard patterns, collection unions do not duplicate
files, and time comparisons keep nanosecond precision.

Both preview and snapshot execution support duplicate and text-coverage
constraints, and snapshot execution requires a configured processing profile
for coverage predicates. Semantic or hybrid mode and relevance ordering return
errors instead of being ignored.

Expression errors return `422 invalid_query` with `position.offset` and
`position.end`, a half-open UTF-8 byte span in the submitted text, while
database failures remain server errors. Preview limits include 8,192 text code
points, 512 parsed nodes, 32 levels of syntactic nesting, and 16 nested saved
references, with additional bounds on larger expansions and compiled
predicates. Dependency revisions describe this preview, not a permanent result
snapshot.

## Text extraction

The daemon's `extract:plain-text` background job indexes UTF-8 content whose
media type is `text/*`, `application/json`, `application/x-ndjson`,
`application/jsonl`, `application/yaml`, or `application/x-tex`. This covers
plain text, Markdown, CSV, JSON, JSONL, YAML, and TeX. The worker reads the
stored content and verifies its hash, whether Docbank stores it as an
individual file or inside a pack, and text becomes searchable only after that
complete read passes verification.

A separate `extract:email` job handles files imported as `message/rfc822`. It
preserves the raw message and canonical MIME inventory, then indexes only the
selected outer body. Alternate bodies, nested forwarded messages, headers, and
attachments do not leak into ordinary search. Docbank converts HTML to bounded
visible text before publication, and an empty, unsupported, encrypted, or
over-limit body is recorded in metadata without contributing a search hit.

At new import, Docbank declares `.eml`, `.EML`, and mixed-case variants as
`message/rfc822` regardless of the host MIME registry. Existing content with
another stored media type is never reclassified from its filename. An embedded
or master-API caller can explicitly have Docbank decode and publish the email
metadata for that content without changing the original media type, hash, or
bytes.

Applications can
[publish email attachments as ordinary documents](../embedding.md#publish-email-attachment-documents).
Each attachment keeps a relation to its parent version and MIME part. Published
CSV and other supported text files enter normal text extraction, while other
formats require a configured processing provider and consent. Attachment text
matches the child document rather than the parent email's body.

Extraction is capped at 16 MiB per blob. Larger documents, invalid UTF-8, and
text containing NUL bytes remain stored and readable, but Docbank does not
body-index them. Newly ingested or replaced content may take a few seconds to
appear while the daemon job reaches it. A transient open, read, or verification
error leaves the item queued for retry on a bounded delay instead of becoming a
permanent extraction failure. In `docbank jobs`, the worker shows as idle while
it waits for work and running while it processes an item.

## Which text is not searched?

The daemon indexes PDF text layers locally when files are added and after a
restart for existing files. The built-in `docbank-pdf-text` profile accepts
PDFs up to 500 MiB and 5,000 pages. When no profile is configured, the **Text**
tab selects the built-in profile and shows its retained page text, while a
configured profile keeps its own text and processing state.

Office-document text, unpublished email attachments, and text from images
through optical character recognition (OCR) require explicit processing. You
can still find these files by name and read their stored bytes; see
[document processing](document-processing.md).

Ordinary search in the CLI, HTTP API, web app, and TUI matches words in names
and indexed text. Processing search uses the separate workflow below, and
saving search choices in QueryV1 does not execute either workflow.

## Why did a text search find nothing?

When a search returns no matches and PDFs in scope have pages without a text
layer, Docbank reports `needs_ocr`. For options, see
[OCR](document-processing.md#ocr).

An empty CLI text search keeps its usual output and prints hints to stderr
listing how many files in the search scope need a processing profile, how many
text files are still being indexed, and, when a lexical search has more than
one prepared term, that one name or text segment must contain every term.
`--json` carries them in a `hints` array of `code` and `message`.

## Processing search

Automatic CLI search resolves current live versions for hybrid retrieval, while
explicit processing search selects the profile and source versions you name.
That explicit form requires a mode, a profile, and an authorized source set.
For modes, coverage, and the consumer contract, see
[Automatic and processing search](search.md).

Next, organize documents beyond paths with
[Organizing and tagging](organizing.md), or see every search flag in the
[CLI reference](../cli-reference.md).
