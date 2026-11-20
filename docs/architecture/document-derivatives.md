---
last_edited: 2026-10-13
title: Document derivatives
description: Authority, retention, retrieval, and rendition format for processed document derivatives.
---

# Document derivatives

Originals and immutable content versions are the source of truth for every
document in Docbank, and derivatives never replace them. When you process a
document, Docbank can derive optional rendition builds, attachments, normalized
evidence, canonical lexical segments, embedding sets, and rebuildable search
projections. The catalog authorizes each retained derivative and ties it to one
immutable source version and one fingerprinted processing profile.

## Execution and authority

A processing selector names a stable node, an immutable content version, and a
locally executable profile. When you plan a run, Docbank resolves all three and
returns a fingerprinted disclosure covering providers, trust boundaries, input
classes, retained classes, estimates, and backup consequence. The plan also
reports the current consent state, though that state does not enter the
fingerprint. Your start request carries the unchanged fingerprint, and workers
recheck authorization both at the provider boundary and before publishing an
active head.

Depending on its descriptor's trust boundary, the original-file rendition flow
can run locally or at a hosted provider. Each embedding binding receives the
input kind fixed by the profile, either the original file or a rendition chunk.
Query embedding receives query text only when its descriptor supports it.
Because a direct original-file embedding flow does not depend on a rendition,
it makes no readable text claim.

Provider adapters receive bounded, hash-bound uploads instead of source paths,
while secrets stay inside adapter-local credential resolution. Portable
profiles, plans, receipts, and derivative manifests carry non-secret
descriptors and fingerprints. When provider output comes back, the local
normalization and sanitization path treats it as untrusted input.

## Retention and recovery

Your profile declares whether Docbank retains sanitized Markdown, provider
Markdown, and typed artifacts. Beyond those profile choices, a rendition
persists normalized evidence, and embedding work persists vector sets. The
catalog records attachment, build, artifact, lexical, and embedding authority.
Full-text search (FTS) and approximate nearest-neighbor (ANN) files are
rebuildable indexes rather than durable derivative authority.

A catalog-authorized backup includes retained derivative blobs alongside their
source metadata. When you restore a vault, Docbank validates the catalog and
blobs before publishing heads and then rebuilds excluded indexes locally
without contacting any provider. You preview a live derivative purge
separately, and it cannot alter an immutable backup copy. Any deletion claim
must account for every retained derivative class and every applicable snapshot
lifecycle.

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

The contract string is exactly `docbank-sanitized-markdown/v1`. Each navigation
entry carries a source-evidence key, a locator kind, an optional title, and
line and byte positions. Both positions are relative to the Markdown **body**,
after the closing frontmatter delimiter; consumers must not count envelope
bytes. When `complete: false` appears, the bounded navigation list does not
cover every unit. The body digest covers the body alone, and transport metadata
identifies the attachment, build, artifact, profile, completeness, and warnings.

Treat the envelope as metadata, not visible document prose. Even though the
body is sanitized readable evidence, it remains untrusted content and may
contain misleading or adversarial natural-language instructions. You do not
need a separate user-facing evidence sidecar. The authenticated rendition, the
response identity, and the bounded search evidence references provide the
available proof.

## Fenced retrieval and consumers

Retrieval takes a source fence, which is a bounded list containing the Docbank
vault UUID and the authorized immutable content-version IDs. The service
rejects any foreign vault and applies the fence before FTS, vector scoring,
expansion, and reranking. Only current, live attachments are eligible for
vector scoring, and missing row authority stops the search. Before returning
results, the service revalidates candidates and counts coverage once in that
final snapshot. Because authority can change after the query returns, consumers
perform their own final visibility check at presentation time.

As a consumer, you own the publication decision and keep only stable source
identity. Do not retain parallel derivative authority in copied rendition
chunks, vectors, lexical segments, or indexes. When you need a readable result,
ask Docbank for the authenticated retained rendition for the still-authorized
source version.

For the operator workflow, see [Document processing](../usage/document-processing.md).
For the consumer request contract, see
[Automatic and processing search](../usage/search.md).
