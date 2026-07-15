---
title: Searching
description: Full-text search over the vault with FTS5.
---

# Searching

```bash
docbank search insurance
docbank search tax 2026
docbank search report --limit 200
```

```
ID   PATH
231  /taxes/2026/insurance-renewal.pdf
198  /taxes/2026/car-insurance.pdf
```

## Semantics

- **Prefix matching.** Every whitespace-separated term matches word
  prefixes: `insur` finds `insurance-renewal.pdf`. Multiple terms must
  all match.
- **Operator-safe.** Query text is escaped before reaching FTS5, so
  `AND`, `OR`, quotes, and parentheses in a query are searched for
  literally, never interpreted. Any string is a safe query.
- **Ranked and bounded.** Results are ordered by BM25 relevance, with
  deterministic name/ID tie-breaks. The default limit is 50; `--limit`
  accepts 1–1000, and truncation is always reported.
- **Live nodes only.** Trashed documents don't appear; restore returns
  them to the index. Renames update the index immediately.

## What's indexed today

Docbank indexes **node names** only. That already covers the common
"where did I file that" lookup, since document filenames tend to carry
their subject.

Document-body extraction, content search, and tag/MIME/date/path filters are
not available in the current CLI or HTTP API.
