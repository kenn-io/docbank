---
title: Document Understanding in Go
description: Normalize extracted documents, use bounded Mistral OCR, and embed images and video through Voyage without opening a Docbank vault.
---

# Document Understanding in Go

Docbank provides reusable Go packages for turning extracted document evidence
into stable text, provenance, and chunks. These packages are independent of a
Docbank vault: importing them does not start a daemon, open storage, or connect
to an existing installation.

Use `go.kenn.io/docbank/document` for provider-neutral normalization. Use
`go.kenn.io/docbank/document/mistral` when an application explicitly chooses
Mistral OCR as its extraction provider. Use `go.kenn.io/docbank/document/media`
to detect and bound images and video, and `go.kenn.io/docbank/document/voyage`
when an application explicitly chooses Voyage AI for multimodal embeddings.

## Normalize provider output

`document.NormalizeDocument` accepts ordered source units and an opaque
normalization policy. It produces deterministic normalized units, heading
paths, exact source spans, baseline chunks, and checksums.

```go
policy, err := document.NewNormalizePolicy(2_000_000)
if err != nil {
	return err
}

normalized, err := document.NormalizeDocument(document.SourceDocument{
	Family:   "pdf",
	UnitKind: "page",
	Units: []document.SourceUnit{
		{Index: 0, Markdown: "# Quarterly report\n\nRevenue increased."},
	},
}, policy)
if err != nil {
	return err
}
```

The policy's structural values are fixed for its normalization version. The
only caller-selected input is the maximum normalized document size. A future
change to the algorithm or structural values requires a new normalization
version rather than silently changing the meaning of existing checksums.

## Run Mistral OCR safely

Mistral uploads fail closed until an operator has produced and supplied a
validated capability manifest for the configured endpoint, model, and policy.
Docbank ships no live manifest and does not treat documentation as evidence of
provider behavior.

The operator flow is:

```text
WriteProbeFixtures
  -> ValidateProbeFixtures (local only; no credentials or HTTP)
  -> NewClient(API key) + RunCapabilityProbe
  -> review and retain the CapabilityManifest
```

Fixture generation creates 21 formats deterministically. Five legacy formats
require operator-supplied synthetic seeds named `doc`, `ppt`, `xls`, `numbers`,
and `msg`. Fixture and staging directories must be private. The initial
capability contract can authorize at most PDF because it is the only format
with a probe-tested pre-upload unit bound. Other formats may extract during a
probe but remain unauthorized for production uploads.

For each production document, the application flow is:

```text
Policy.Authorize(validated manifest, declared format)
  -> application verifies its own consent record against PolicyFingerprint
  -> Prepare(private staging)
  -> Process(sniffed format must match the authorization)
  -> Release
  -> NormalizeDocument(Result.Document, Policy.NormalizePolicy())
```

`Prepare` copies one input into a private, bounded, immutable staging file.
`Process` reopens and verifies those bytes for every attempt, derives request
options from the policy and authorization, bounds the response, and converts
validated provider output into `document.SourceDocument`. Call `Release` on
every success or failure path.

The importing application remains responsible for credentials, human consent,
spending and scheduling limits, durable manifests, job orchestration,
persistence, and search serving. Those application decisions are intentionally
outside the reusable packages and their policy identity.

## Detect and bound visual attachments

`media.Detect` sniffs JPEG, PNG, WebP, GIF, and MP4 containers and reads only
the metadata needed to bound provider input: dimensions, GIF frame count, and
MP4 duration. It never decodes pixels and never trusts the declared media type.
`media.Evaluate` applies a policy of byte, pixel, and duration caps and
still, animated, and video toggles, returning a stable reason such as
`too_many_pixels` or `animated_not_allowed` that an application can record.

```go
metadata, reason, err := media.Inspect(reader, size, "image/png", media.DefaultPolicy())
if err != nil {
	return err
}
if reason != media.ReasonEligible {
	return recordIneligible(reason)
}
```

The package has no notion of attachment ownership, roles, or hashes; those are
application pre-filters.

## Embed images and video with Voyage

Voyage uploads fail closed until an operator has produced and supplied a
validated capability manifest for the pinned endpoint, model, and dimension.
Docbank ships no live manifest. Each capability is authorized separately:
JPEG, PNG, WebP, still GIF, animated GIF, MP4, text queries, image queries,
text-plus-media documents, and batches at the policy limit. Whether animated
GIF or video may be sent is decided by recorded probe evidence, not by
assumption.

The operator flow is:

```text
WriteProbeFixtures (JPEG, PNG, GIF generated; WebP and MP4 from operator seeds)
  -> ValidateProbeFixtures (local only; no credentials or HTTP)
  -> NewClient(API key) + RunCapabilityProbe
  -> review and retain the CapabilityManifest
```

WebP and MP4 cannot be encoded by the Go standard library, so the operator
supplies synthetic seeds named `image_webp.webp` and `video_mp4.mp4`. The
probe stores no media, vectors, or responses; the manifest records only
sanitized pass, reject, and fail observations.

For each production item, the application flow is:

```text
Policy.AuthorizeAll(validated manifest)
  -> application verifies its own consent record against PolicyFingerprint
  -> media.Inspect (eligible under the policy)
  -> Client.EmbedDocuments(inputs, authorizations)
```

`EmbedDocuments` re-detects every media part so metadata cannot misdescribe
bytes, requires an authorization for the part's format and animation state,
requires the interleaved capability for text-plus-media inputs and the batch
capability for more than one input, and refuses requests over the policy's
item and byte limits before allocation. Provider bodies never appear in
errors; `IsRetryable`, `RetryAfter`, and `MetricsFromError` support the
application's own scheduling.

The importing application remains responsible for credentials, human consent,
spending and scheduling limits, durable manifests, job orchestration, vector
storage, and search serving. Docbank does not compute, store, or serve
embeddings for its own vault.

## Package boundary

The dependency direction is deliberate:

```text
application storage and workers
  -> document/mistral (optional provider transport)
  -> document (provider-neutral normalization)

application storage and workers
  -> document/voyage (optional provider transport)
  -> document/media (provider-neutral detection and eligibility)
```

No public package imports Docbank's vault, database, daemon, or queue. Local
extractors can use `document` without depending on Mistral or any network
transport, and applications can use `document/media` alone to record image
dimensions at ingest without any provider.
