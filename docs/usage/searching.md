---
title: Searching
description: Ranked, prefix-matching search over document names and verified text content.
---

# Searching

```bash
docbank search insurance
docbank search tax 2026
docbank search report --limit 200
docbank search report --json
```

```
ID   MATCH    PATH
231  name     /taxes/2026/insurance-renewal.pdf
198  content  /taxes/2026/car-insurance-notes.md
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
daemon job reaches it. `docbank jobs` shows whether that worker is running.

PDF text layers, office formats, OCR, and tag/MIME/date/path search filters are
not yet available. Their absence never changes name-search results or document
authority.

Next: organize documents beyond paths with
[Organizing & Tagging](organizing.md), or see every search flag in the
[CLI Reference](../cli-reference.md).
