---
last_edited: 2026-09-21
title: Search exports
description: Export dated search counts as CSV, review date evidence, and verify a frozen evidence ZIP.
---

# Search exports

Export search counts for a date range from all live documents or selected
import collections. Each export freezes the current document versions, search
matches, family relationships, date evidence, and processing profile. Later
vault changes do not change that export.

The CSV contains counts. The evidence ZIP contains the calculation inputs and
date evidence needed to check those counts offline. To export original files
or complete retained text, use [document export bundles](export-bundles.md).

## Create an export in the browser

1. Open **Search exports** in the top bar.
2. Choose all documents or select import collections. Select a processing
   profile when more than one is configured. Exporting uses existing text;
   it does not start document processing.
3. Add search expressions and an inclusive start and end date for each row.
   Choose **Simple** or **Advanced** [query syntax](searching.md).
4. Choose the export timezone and coverage policy, then **Create export**.
5. If dates need review, select **Review dates**, inspect the evidence, and
   give a reason for each choice. An interpreted date needs an explicit date
   and source timezone. Select **Create reviewed revision**.
6. Download `search-export.csv` or `search-export.zip` when counts are ready.

**Recent exports** automatically keeps the latest 100 requests and compact
outcomes in the vault. **Use as draft** copies the request without its old date
choices and runs against current data. This history has no naming or deletion
controls. Use [Saved queries and highlights](web.md#saved-queries-and-highlights)
to manage named search definitions.

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

```bash
docbank search-export create --input request.json --output search-export.zip
docbank search-export verify search-export.zip
docbank search-export csv search-export.zip --output search-export.csv
```

`create` uses the daemon and prints coverage. If dates need review, it prints
an export ID and review instructions without writing the ZIP. Inspect the
frozen evidence and submit a JSON array of choices:

```bash
docbank search-export dates <export-id> --limit 50
docbank search-export revise <export-id> --choices choices.json --output reviewed.zip
```

Use `--cursor` with the returned `next_cursor` to continue reading dates.
`create`, `revise`, and `csv` refuse to replace an existing output unless
`--overwrite` is supplied. `verify` and `csv` work offline without a vault;
both verify the packet before accepting its counts.

## Request and reviewed-date format

Version 1 is stored in export history and evidence packets. Required fields
and optional controls are:

| Field | Meaning |
| --- | --- |
| `version` | Must be `1`. |
| `all_documents`, `collection_ids` | Set `all_documents: true` without collection IDs, or `false` with at least one existing collection ID. |
| `profile` | Optional configured processing profile name. A sole configured profile is selected automatically. |
| `timezone` | Required IANA timezone, such as `UTC` or `America/New_York`. `Local` is rejected. Cutoffs are literal calendar dates in this zone. |
| `source_timezone` | Optional source timezone for timestamps that omit one. |
| `numeric_date_order` | Optional `MDY` or `DMY` interpretation of ambiguous numeric dates. Omit to leave ambiguity unresolved. |
| `coverage_mode` | `strict` (default) rejects incomplete search or family coverage in the date ranges. `available_only` retains the gaps in coverage totals. |
| `terms` | 1–128 rows. Each has a unique positive `number`, nonempty `expression` of at most 8,192 Unicode characters, `syntax` (`simple` or `advanced`), and `dates.start` / `dates.end` in `YYYY-MM-DD` form. |
| `date_choices` | Optional evidence-bound choices for captured documents. A reused history request omits these. |

A choice identifies the exact `document` (`node_id`, `version_id`, `sha256`),
`candidate_id`, and `evidence_sha256` returned by the date page. Copy those
identities from that page; do not invent replacement IDs or hashes. Include
a nonempty `reason` of at most 4,096 bytes and one action:

| Action | Additional fields |
| --- | --- |
| `select` | Select the source date as given; omit all `reviewed_*` fields. |
| `interpret` | Supply `reviewed_date` and `reviewed_timezone`. Only ambiguous numeric dates and timestamps without a timezone can be interpreted. The date must agree with the source tokens. An unclassified candidate also needs `reviewed_role`. |
| `reclassify` | Supply `reviewed_role` without replacing the date or timezone. |

Allowed reviewed roles are `document_date`, `created`, `authored`, `sent`,
`captured`, `signed`, `effective`, and `expiry`. A revision keeps the original
observation and expiry and receives a new export ID. A revision
keeps its parent’s reviewed choices; submit the documents whose choices you
want to add or replace.

## Read the counts and coverage

Rows retain the request order. Counts refer to distinct current document
identities, not text occurrences. The first three CSV columns are **Term #**,
**Terms**, and **Date Range**; the remaining columns mean:

| Column | Meaning |
| --- | --- |
| Hits | Documents that match this term and fall within its date range. |
| Hits Plus Family | Date-eligible documents in a family with a hit for this term. |
| Unique Hits | Hits for this term that are not hits for another row. |
| Unique Families | Date-eligible documents in matching families with no competing row hits among those eligible members. Despite the name, this counts documents, not family groups. |
| Unique Hits Plus Family | Date-eligible documents in a family containing a unique hit for this term. |

Families come from retained email parent/child evidence. A document without a
family is a singleton. Family expansion stays within the selected source
scope and each row's date range.

The ZIP retains relationship groups connected to selected documents, including
connected documents outside the selected collections when needed to preserve
grouping. It omits unrelated relationship groups and their warnings. Documents
outside the selected scope do not contribute to counts.

Date selection prefers source evidence appropriate to the document kind,
then labeled document dates, then source metadata and recorded import or vault
dates. Equally preferred conflicting dates require review. Signed, effective,
and expiry dates are retained for review rather than automatically treated as
creation dates. Coarse or unusable dates are not silently made precise.

Coverage reports scoped documents, searchable documents, missing text,
incomplete families, and fallback dates for each row and across the union of
the date ranges. `available_only` can omit documents without a usable selected
date. Missing evidence does not prove that a document has no relevant content.
Keep the coverage receipt with the CSV when sharing counts.

## Evidence and retention limits

The ZIP format marker is `search-export-v1`. Its fixed entries are `hits.csv`,
`manifest.json`, `members.jsonl`, `families.jsonl`, and `dates.jsonl`. The
manifest binds sizes and SHA-256 digests; verification also checks date
selections, hit bits, counts, and coverage against the retained evidence.
The packet includes document identities and date quotes, so review it before
sharing it.

Successful offline verification means **internally consistent**, not **source
verified**. A packet cannot establish that its source vault or source documents
were authentic. It does not include complete source text or rerun searches
against the original documents.

Live export handles and date pages expire 30 minutes after observation and
are lost on daemon restart. Handles belong to the requesting API or browser
session; ending a browser session invalidates its handles. Download the ZIP
before expiry. History receipts survive backup and restore, but neither
history nor a backup of history restores live artifacts or date-review pages.

Each build has a 60-second deadline and shares a 1 GiB accounted memory budget
with other cached exports. Limits include 50,000 documents, 100,000 family
relations, 256 date candidates per document, 16 MiB of inspected text per
document, 512 MiB of inspected text overall, and a 512 MiB ZIP. Exceeding the
date-candidate limit returns a resource-limit error naming the document; it
does not silently truncate the evidence. Narrow the source scope when a vault
exceeds a build limit. At most two builds run concurrently, with 64 cached or
pending exports overall and eight per owner.

See the [HTTP contract](../architecture/http-api.md#search-exports) for endpoints,
authentication, paging, and errors.
