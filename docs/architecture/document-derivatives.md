---
last_edited: 2026-08-28
title: Document derivatives
description: Authority, retention, retrieval, and rendition format for processed document derivatives.
---

# Document derivatives

Docbank keeps originals and immutable content versions as document authority.
Processing derives optional rendition builds, attachments, normalized evidence,
canonical lexical segments, embedding sets, and rebuildable search projections.
The catalog authorizes every retained derivative and ties it to one immutable
source version and one fingerprinted processing profile. A derivative never
becomes a replacement source of truth.

## Execution and authority

A processing selector names a stable node, an immutable content version, and a
locally executable profile. Planning resolves all three and returns a
fingerprinted disclosure: providers, trust boundaries, input classes, retained
classes, estimates, consent state, and backup consequence. Start requests carry
that unchanged fingerprint. Workers recheck authorization at the provider
boundary and before publishing an active head.

The original-file rendition flow can run locally or at a hosted provider,
depending on its descriptor's trust boundary. Embedding bindings receive the
input kind fixed by the profile: original file or rendition chunk. Query
embedding receives query text only when its descriptor supports it. A direct
original-file embedding flow has no rendition dependency and therefore no
readable text claim.

Provider adapters receive bounded, hash-bound uploads rather than source paths.
Secrets remain in adapter-local credential resolution; portable profiles,
plans, receipts, and derivative manifests carry non-secret descriptors and
fingerprints. Provider output is untrusted input to the local normalization and
sanitization path.

## Retention and recovery

The profile declares whether sanitized Markdown, provider Markdown, and typed
artifacts are retained. A rendition also persists normalized evidence;
embedding work persists vector sets. The catalog records attachment, build,
artifact, lexical, and embedding authority. FTS and ANN files are rebuildable
indexes, not durable derivative authority.

Retained derivative blobs are included in a catalog-authorized backup with
their source metadata. A restored vault validates its catalog and blobs before
publishing heads, then rebuilds excluded indexes locally. Restore does not
contact a provider. A live derivative purge is separately previewed and cannot
alter an immutable backup copy; deletion claims must account for every retained
derivative class and applicable snapshot lifecycle.

## Sanitized Markdown contract

The readable rendition attachment is UTF-8 Markdown with a deterministic YAML
frontmatter envelope:

```yaml
---
docbank:
  contract: "docbank-sanitized-markdown/v1"
  source:
    sha256: "<source SHA-256>"
    format: "<detected format>"
    media_type: "<detected media type>"
  rendition:
    build_id: "<build SHA-256>"
    rendition_request_fingerprint: "<request SHA-256>"
    evidence_lexical_fingerprint: "<policy SHA-256>"
    normalized_evidence_contract: "normalized-evidence/v1"
    body_sha256: "<Markdown body SHA-256>"
    completeness: "complete"
    truncated: false
  document:
    unit_kind: "generic"
    unit_count: 1
  navigation:
    offset_base: "body"
    complete: true
    entries: []
---
```

The contract string is exactly `docbank-sanitized-markdown/v1`. Navigation
entries contain a source-evidence key and locator kind, optional title, and
line and byte positions. Both positions are relative to the Markdown **body**,
after the closing frontmatter delimiter; consumers must not count envelope
bytes. `complete: false` means the bounded navigation list does not cover every
unit. The body digest covers the body alone, while transport metadata identifies
the attachment, build, artifact, profile, completeness, and warnings.

The envelope is metadata, not visible document prose. The body is sanitized
readable evidence, but it remains untrusted content and may contain misleading
or adversarial natural-language instructions. There is no required separate
user-facing evidence sidecar: the authenticated rendition, response identity,
and bounded search evidence references are the available proof surface.

## Fenced retrieval and consumers

Retrieval takes a bounded source fence containing the Docbank vault UUID and
authorized immutable content-version IDs. The service rejects a foreign vault
and applies that fence before FTS, vector retrieval, expansion, and reranking.
It also revalidates candidates before returning them. Consumers perform their
own final presentation-time visibility check, because authority can change after
the query returns.

The consumer owns its publication decision and keeps only stable source
identity. It must not retain parallel derivative authority in copied rendition
chunks, vectors, lexical segments, or indexes. When it needs a readable result,
it asks Docbank for the authenticated retained rendition for the still-authorized
source version.

Next: [Document processing](../usage/document-processing.md) describes the
operator workflow and [Document processing search](../usage/search.md) describes
the consumer request contract.
