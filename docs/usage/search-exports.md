---
last_edited: 2026-10-31
title: Search exports
description: Export search counts for a date range as CSV, review date evidence, and verify the frozen evidence ZIP.
---

# Search exports

You can export search counts for a date range across all live documents,
selected import collections, or specific document versions you pick in the
workspace. Every export freezes the current document versions, search matches,
family relationships, date evidence, and processing profile at the time you run
it, and later changes to your vault do not affect it.

The CSV gives you the counts, while the evidence ZIP holds the calculation
inputs and date evidence you need to check those counts offline. To export
original files or complete retained text instead, use
[document export bundles](export-bundles.md).

## Create an export in the browser

1. Open **Searches → Run report**. To report on specific files instead, select
   them on the displayed workspace or query-result page and choose
   **Report selected documents**.
2. Under **Options**, choose **All documents** or **Selected imports**. If you
   started from a workspace selection, the drawer opens with a fixed
   **Selected documents (N)** scope that covers only those displayed versions,
   without pulling in other query matches or attachments. When more than one
   processing profile is configured, enter the profile's name in the
   **Processing profile** field. Exporting reads existing text and does not
   start document processing.
3. Add your search expressions with an inclusive start and end date for each
   row, and choose **Simple** or **Advanced** [query syntax](searching.md).
4. If needed, adjust the report timezone and coverage under **Options**, then
   select **Create report**.
5. If any dates need review, select **Review dates**, inspect the evidence, and
   enter a reason for each choice. When you interpret a date, provide both a
   date and a source timezone. When you finish, select
   **Create report with these date choices**.
6. Once counts are ready, download `search-export.csv` or `search-export.zip`.

The vault automatically keeps the latest 100 requests and their compact
outcomes under **Recent reports**. You can select **Use as draft** to copy an
earlier request without its previous date choices. Drafts for selected
documents keep their exact version identities even if the workspace refreshes,
and a rename or move alone does not invalidate that content identity.

When you rerun a selected-document draft, Docbank checks those versions again.
If any have changed or disappeared, select **Refresh selection** in the drawer,
review the updated versions, and pick which current versions to include.
Missing and trashed documents are excluded. Select **Use this selection**, then
create the report again. **Recent reports** has no naming or deletion controls,
and if you want to manage named search definitions, use
[Saved queries and highlights](web.md#saved-queries-and-highlights).

![Search-count request scoped to selected document versions](https://docbank.ai/assets/generated/selected-report-draft.png)

![Recent report requests available to reopen as drafts](https://docbank.ai/assets/generated/web-recent-exports.png)

## Create an export from the CLI

Save this version 1 request as `request.json`:

```json
{
  "version": 1,
  "all_documents": true,
  "timezone": "UTC",
  "coverage_mode": "strict",
  "terms": [
    {
      "number": 1,
      "expression": "agreement",
      "syntax": "simple",
      "dates": {"start": "2024-01-01", "end": "2026-12-31"}
    }
  ]
}
```

Then create, verify, and extract counts from the export:

```bash
docbank report create --input request.json --output search-export.zip
docbank report verify search-export.zip
docbank report csv search-export.zip --output search-export.csv
```

The `create` command talks to the daemon and prints coverage. If any dates need
review, it prints an export ID and review instructions instead of writing the
ZIP. You can inspect the frozen evidence and submit a JSON array of choices:

```bash
docbank report dates <export-id> --limit 50
docbank report revise <export-id> --choices choices.json --output reviewed.zip
```

To read more dates, pass the returned `next_cursor` value with `--cursor`. The
`create`, `revise`, `download`, and `csv` commands refuse to overwrite an
existing file unless you pass `--overwrite`. Both `verify` and `csv` work
offline without a vault, and each checks the packet before accepting its counts.

## Inspect or recover an existing export

If you have a report ID, you can read its live summary or save its verified
evidence ZIP:

```bash
docbank report show <report-id>
docbank report show <report-id> --json
docbank report download <report-id> --output recovered.zip
docbank report csv recovered.zip --output recovered.csv
```

`show` returns counts, coverage, and expiry. If a report is still waiting for
date review, it has no counts or download yet, and you can resolve its evidence
with `dates` and `revise`. The `download` command saves the existing ZIP
without creating a new report, taking another slot, or extending the expiry. It
accepts relative paths and refuses to replace an existing file unless you pass
`--overwrite`. It does not download CSV directly; extract the CSV offline from
the saved ZIP.

To list recorded runs along with their original requests, check the report
history:

```bash
docbank report history --offset 0 --limit 20
docbank report history --offset 0 --limit 20 --json
```

History covers the whole vault, including reports created in the browser. A
recorded entry does not guarantee that the report is still live or belongs to
the CLI. Both `show` and `download` require a live handle owned by the CLI's
API key, which CLI and MCP share, and neither command recreates an expired or
unavailable report from history.

History stores the latest 100 receipts. Offsets range from 0 to 100, and limits
range from 1 to 50 with a default of 20. In JSON output, you get the stored
`{items, total}` page, including full request selections. A page can return
fewer items than you asked for to stay within its byte limit. Advance your
offset by the number of returned items rather than by your requested limit.
Human-readable output prints the next offset whenever more receipts remain.

Once `create` or `revise` receives a valid summary, any later error keeps the
new report ID, and a revision error names the child report. While the handle is
still live, use `show` to inspect the result and `download` to retry delivery.
If the handle expires or disappears after a restart, retrying it cannot recover
the report, and you need to create a new one.

If delivery does not finish cleanly, check the destination path first.
Docbank may have already published the verified file before hitting a sync or
staging-cleanup error. Verify any existing ZIP before you decide to retry or
overwrite it. If printing the final status fails after a successful save, the
error tells you the file was saved.

Recovery depends on having a known ID. If a `create` or `revise` reply is lost
before the CLI receives a valid summary, the CLI cannot reliably identify the
result. History can help you investigate, though it cannot prove which call
created a given receipt, and repeating `create` or `revise` can consume another
slot.

## Release a report after review

When you no longer need a live report, free its slot:

```bash
docbank report release <report-id>
docbank report release <report-id> --json
```

Releasing a report discards that handle's packet and date-review evidence, even
if the report is still awaiting review, and history cannot reconstruct that
evidence. Your saved files, source documents, history receipts, and any already
published child reports stay intact, and each child keeps its original expiry.
Shared evidence remains in memory while another report or in-flight operation
uses it, so freeing a slot does not guarantee that all global build or memory
capacity becomes available.

If a download is still active, release returns `409 report_retained`. Wait for
the download to close and retry manually, as the command does not retry on its
own. Even running `download && release` can need a retry because the server
closes its reader after sending the final bytes.

If you get an uncertain reply, run `docbank report show <report-id>` to check
the state:

- A successful read means the handle is still live.
- `410 report_unavailable` means you no longer have a live handle for that ID,
  though it does not tell you which call removed it.
- `503 report_unavailable` means reporting itself is unavailable and says nothing
  about whether the release succeeded.

Calling release again on an absent handle returns `410`, not success. If
printing the result fails after a successful release, the error tells you the
report was released.

In JSON mode, a successful release returns
`{"report_id":"<report-id>","released":true}`. Invalid IDs or wrong argument
counts exit with code 2, and release errors exit with code 1. If you run a
development build against an older daemon that lacks this endpoint, restart the
daemon explicitly.

## Request and reviewed-date format

Export history and evidence packets store requests in version 1 format. Here
are the required fields and optional controls:

| Field | Meaning |
| --- | --- |
| `version` | Must be `1`. |
| `all_documents`, `collection_ids`, `selected_documents` | Choose exactly one scope: `all_documents: true`, a nonempty collection ID list, or the selected-document object below. Use `all_documents: false` for either list scope. |
| `profile` | Configured processing profile name. Required when several profiles exist. Omission returns `invalid_profile`. A sole configured profile is selected automatically. |
| `timezone` | Required IANA timezone, such as `UTC` or `America/New_York`. `Local` is rejected. Cutoffs are literal calendar dates in this zone. |
| `source_timezone` | Optional source timezone for timestamps that omit one. |
| `numeric_date_order` | Optional `MDY` or `DMY` interpretation of ambiguous numeric dates. Omit to leave ambiguity unresolved. |
| `coverage_mode` | `strict` (default) rejects incomplete search or family coverage in the date ranges. `available_only` retains the gaps in coverage totals. |
| `terms` | 1–128 rows. Each has a unique positive `number`, nonempty `expression` of at most 8,192 Unicode characters, `syntax` (`simple` or `advanced`), and `dates.start` / `dates.end` in `YYYY-MM-DD` form. |
| `date_choices` | Optional evidence-bound choices for captured documents. A reused history request omits these. |

To target specific documents, copy each current file's node ID, version ID, and
content hash from the workspace or API, and replace the example request's
all-document scope with a selection like this synthetic one:

```json
{
  "all_documents": false,
  "selected_documents": {
    "documents": [{
      "node_id": 42,
      "version_id": "20000000-0000-4000-8000-000000000002",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }]
  }
}
```

A selection needs from 1 to 50,000 distinct positive node IDs, canonical
lowercase UUIDv4 version IDs, and 64-character lowercase hexadecimal hashes,
and the entire request must fit within 8 MiB. Historical versions and selected
pages are not supported. Omitting `selected_documents` or setting it to `null`
means no selected scope, while an object with a missing, `null`, or empty
`documents` list is invalid.

Wrong JSON types or missing required fields return the API's ordinary schema
errors. The application errors below apply after HTTP schema validation:

- Malformed or mixed scopes, duplicate nodes, invalid identity values, or a
  mismatched hash for a current version return `422 invalid_report_request`.
- Missing, trashed, or replaced versions return `409 report_selection_changed`
  without listing the conflicting members. Even a new version with identical
  bytes counts as a conflict.
- Selecting more than 50,000 identities returns `413 report_limit`, while
  exceeding the 50,000-collection limit returns `422`.

Each date choice references the `document` fields `node_id`, `version_id`, and
`sha256`, along with the `candidate_id` and `evidence_sha256` from the date
page. Copy those values directly from the page rather than inventing
replacements. Include a nonempty `reason` of at most 4,096 bytes and choose one
action:

| Action | Additional fields |
| --- | --- |
| `select` | Select the source date as given. Omit all `reviewed_*` fields. |
| `interpret` | Supply `reviewed_date` and `reviewed_timezone`. Only ambiguous numeric dates and timestamps without a timezone can be interpreted. The date must agree with the source tokens. An unclassified candidate also needs `reviewed_role`. |
| `reclassify` | Supply `reviewed_role` without replacing the date or timezone. |

Allowed values for `reviewed_role` are `document_date`, `created`, `authored`,
`sent`, `captured`, `signed`, `effective`, and `expiry`. Each revision gets a
new export ID while keeping the original observation, expiry, and its parent's
reviewed choices. You only need to submit the documents whose choices you want
to add or replace.

## Read the counts and coverage

CSV rows stay in the same order as your request, and every count measures
distinct current document identities rather than individual text matches. The
first three columns are **Term #**, **Terms**, and **Date Range**, and the
remaining columns report:

| Column | Meaning |
| --- | --- |
| Hits | Documents that match this term and fall within its date range. |
| Hits Plus Family | Date-eligible documents in a family with a hit for this term. |
| Unique Hits | Hits for this term that are not hits for another row. |
| Unique Families | Date-eligible documents in matching families with no competing row hits among those eligible members. The web app labels this count **Documents in unique families**. The CSV keeps the heading **Unique Families**. |
| Unique Hits Plus Family | Date-eligible documents in a family containing a unique hit for this term. |

Document families come from stored email parent and child evidence, while a
document without a family is a singleton. Family expansion stays within your
selected source scope and each row's date range.

The ZIP includes every relationship group connected to your selected documents.
When needed to preserve grouping, it also includes connected documents outside
the selected scope. For example, an unselected attachment can link two selected
parents; it appears in the ZIP as a relationship reference, without member
rows, text bindings, or date evidence. Unrelated relationship groups and their
warnings are left out, and documents outside the selected scope never
contribute to counts.

A trashed attachment cannot connect a family in a fresh report, and its
published parent has incomplete family coverage. If both the parent and
attachment versions are still current, restoring the attachment from trash
restores that link for future reports. Backup and restore preserve published
relationships and trash state, while an already captured report keeps its
original family evidence.

When picking a date for a document, Docbank prefers source evidence suited to
the document kind, then labeled document dates, and finally source metadata and
recorded import or vault dates. If two equally preferred dates conflict, they
require review. Docbank keeps signed, effective, and expiry dates for review
instead of automatically treating them as creation dates, and it never silently turns coarse
or unusable dates into precise ones.

The coverage summary reports scoped documents, searchable documents, missing
text, incomplete families, and fallback dates for each row as well as across
the union of all date ranges. Incomplete-family coverage counts selected,
date-eligible documents rather than distinct family groups. In `available_only`
mode, documents without a usable selected date can be omitted from coverage
while staying in the packet as members. **Selected documents (N)** shows your
input population, while `scoped` counts only documents that have a usable date
inside at least one term's date range. Missing evidence does not prove that a
document lacks relevant content. Keep the coverage receipt alongside the CSV
when you share counts.

## Evidence and retention limits

Each ZIP uses the format marker `search-export-v1` and contains five fixed
entries: `hits.csv`, `manifest.json`, `members.jsonl`, `families.jsonl`, and
`dates.jsonl`. The manifest records file sizes and SHA-256 digests, and
verification also checks date selections, hit bits, counts, and coverage against the
packet's evidence. For a selected-document scope, the packet members must match
your requested identities exactly; any omitted, extra, duplicate, or
substituted member fails verification. Because the packet includes document
identities and date quotes, review it before sharing it.

Passing offline verification means the packet is **internally consistent**, not
**source verified**. A packet cannot prove that its source vault or original
documents were authentic, and it neither includes complete source text nor
reruns searches against the originals.

Live export handles and date pages expire 30 minutes after observation and are
lost if the daemon restarts. Handles are tied to the API or browser session
that created them, and ending a browser session invalidates its handles.
Download the ZIP before it expires. CLI and MCP share eight live report slots,
and each revision uses another slot. Every descendant expires 30 minutes after
the original observation. Downloading a ZIP does not free its slot. CLI and MCP
clients can release handles they no longer need, while browser sessions cannot.

History receipts survive backup and restore, though neither history nor a
backup of history restores live artifacts or date-review pages. Replacing,
trashing, or pruning source documents does not invalidate an already captured
report or extend its lifetime, and history stays readable after source deletion
and restore. To rerun a saved selection, its versions must be live and current
again.

After restoring a vault, configure the same portable processing profile before
you rerun its saved requests. The
[backup](backup.md#where-does-restored-content-go) excludes `config.toml`, and
you should preserve the restored target's storage settings when adding the
profile. History does not keep date choices, so a fresh report can require date
review again.

Large selections add up in stored history: 100 requests near the 8 MiB ceiling
can reach almost 800 MiB of request JSON, which metadata export also carries.
History pages stop at 16 MiB, and automated callers must advance their offset
by the number of returned items, which can be smaller than the requested limit.

Each export build has a 60-second deadline and shares a 1 GiB accounted memory
budget with other cached exports. At most two builds run at once, with up to 64
cached or pending exports globally and eight per owner. Individual builds are
bounded to:

- 50,000 documents
- 100,000 family relations
- 256 date candidates per document
- 16 MiB of inspected text per document and 512 MiB of inspected text overall
- 512 MiB for the final ZIP

If a document exceeds the date-candidate limit, Docbank returns a
resource-limit error naming that document instead of silently truncating its
evidence. If your vault exceeds a build limit, narrow the source scope.

Downloads of completed CSV and ZIP files are exempt from the server's 60-second
request timeout, though canceling from the client still stops the transfer.

Local MCP clients can select current documents, review frozen dates, and save
the same verified ZIP with `docbank mcp --allow-report-writes`. See the
[MCP report workflow](mcp.md#frozen-search-reports) for discovery, message
limits, and shared handle capacity. Date pages return a continuation cursor
whenever the next complete evidence item cannot fit, and quotes are never
truncated. HTTP clients can request smaller pages with `max_bytes`.

For endpoints, authentication, paging, and error details, see the
[HTTP contract](../architecture/http-api.md#search-exports).
