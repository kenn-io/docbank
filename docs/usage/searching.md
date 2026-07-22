---
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

PDF text layers, office formats, and OCR are not yet available. Their absence
never changes name-search results or document authority.

Next: organize documents beyond paths with
[Organizing & Tagging](organizing.md), or see every search flag in the
[CLI Reference](../cli-reference.md).
