---
title: Embedding Index
description: Build and inspect a disposable semantic-search index from verified current document text.
---

# Embedding index

!!! info "Release availability"

    The embeddings commands are newer than v0.9.0. Build from source to use
    them until the next release is published.

Docbank can encode verified current document text through an optional
OpenAI-compatible endpoint and keep the resulting vectors in a local derived
sidecar:

```bash
docbank embeddings build
docbank embeddings list
```

This is an explicit operation. Adding `[embeddings]` configuration does not
upload text or start embedding work by itself.

## What is encoded

The source set is deliberately narrow:

- the node is a live file;
- the version is that node's current version;
- the plain-text extraction completed successfully and reached verified EOF;
- the content type is already eligible for Docbank's bounded text extraction.

Historical versions, trashed documents, unsupported binary formats, failed or
pending extractions, and text beyond the extraction limit are absent. The
encoder receives text chunks only. It does not receive paths, node or version
IDs, tags, provenance, audit records, or original binary blobs.

Text extraction is asynchronous. `embeddings build` mirrors extraction results
that exist when its scan begins; it does not wait for or perform extraction.
Use `docbank jobs` to inspect `extract:plain-text`, then rerun the build after
new text becomes available.

## Generation and activation

A generation identifies one exact vector space: model name, dimensions, the
Docbank chunking recipe, and optional fingerprint salt. Docbank splits long
text into bounded overlapping chunks, normalizes every finite nonzero vector,
and stores it through Kit's SQLite vector substrate. Byte-identical current
documents share one content digest and are encoded once, avoiding duplicate
endpoint work.

A build first refreshes the current-document mirror and then fills missing or
stale vectors. Re-running the same generation resumes and converges rather than
starting over. When configuration selects a new generation, the prior active
generation remains active until the replacement covers the complete current
mirror. Only then does Docbank activate the replacement.

Human builds show unique-content progress. `--progress plain` emits durable
lines for logs; `--json` suppresses progress and returns only the terminal
report:

```bash
docbank embeddings build --progress plain
docbank embeddings build --json
docbank embeddings list --json
```

Only one build may run at a time. A concurrent request fails with the structured
`embeddings_build_running` conflict rather than racing the sidecar.

## Authority and recovery

`vectors.db` is disposable derived state, not document or metadata authority.
Docbank's JSONL backup and restore contract excludes it. After restore, configure
an encoder and run `docbank embeddings build` to reconstruct it from verified
current text. Deleting a stopped vault's `vectors.db` has the same effect; the
next configured daemon creates an empty sidecar.

Lexical `docbank search` remains available whether embeddings are configured,
built, incomplete, or absent. The current command surface builds and
inventories vector generations; query-time semantic ranking is not yet exposed.

See [Configuration](../configuration.md#embeddings) for endpoint and credential
settings, and [Searching](searching.md) for the currently queryable lexical
search contract.
