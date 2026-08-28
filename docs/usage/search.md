---
last_edited: 2026-08-28
title: Document processing search
description: Search only the exact document versions a consumer currently authorizes.
---

# Document processing search

Processing search is separate from ordinary `docbank search`. It searches
retained rendition text and, where configured, vectors for an exact source set.
The caller supplies the daemon's vault UUID and one or more immutable content
version IDs; the CLI obtains the vault UUID and sends repeated
`--source-version` values for you.

```bash
docbank search "renewal terms" \
  --mode hybrid \
  --profile <configured-profile> \
  --source-version 11111111-1111-4111-8111-111111111111
```

Processing search requires a mode, executable profile, and at least one source
version. Replace `<configured-profile>` with a name reported by `docbank
processing profiles`. It accepts at most 4,096 unique versions and returns at
most 100 results. `--binding` is optional when the selected profile has one
embedding binding, required when it has several, and unused for lexical search.
It cannot be combined with ordinary tag, media-type, directory, or
modification-time filters.

## Modes and coverage

- **`lexical`** searches canonical segments from retained renditions.
- **`semantic`** searches the selected embedding binding.
- **`hybrid`** combines lexical and semantic candidates.
- **`auto`** chooses the available retrieval path and reports the actual mode
  and any degradation.

Each response reports coverage for the supplied source set: rendition and
embedding state, complete, unavailable, stale, and ineligible counts, plus the
actual mode. A direct-file-only embedding can produce a match without an
excerpt because Docbank does not invent readable evidence that was never
retained.

## Source-fenced consumer contract

The source fence is authority, not a post-filter. Docbank validates the vault
identity and applies the exact content-version set before lexical retrieval,
ANN retrieval, candidate expansion, and reranking. A foreign vault or an empty,
duplicate, malformed, or oversized fence is rejected.

Consumers keep durable source identity as `(vault_uid, node_id,
content_version_id)`. A hit's rendition attachment, rendition build, lexical
segment, embedding set, and vector space identify the evidence used for that
query, but they are not a second document authority. Fetch readable evidence
from Docbank's authenticated rendition endpoint instead of retaining a parallel
chunk or vector corpus.

The consumer must still re-check whether each candidate is visible immediately
before presentation. That final check closes the interval between retrieval and
display when a source version is revoked or otherwise becomes unauthorized.

Processing search does not require a user-facing evidence sidecar. Results carry
bounded evidence references and an authenticated rendition carries its own
identity and navigation metadata; consumers may request those when they need to
explain a result.

For ordinary name and verified plain-text search, see [Searching](searching.md).
