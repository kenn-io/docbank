---
title: Document Understanding in Go
description: Normalize extracted documents and use bounded Mistral OCR without opening a Docbank vault.
---

# Document Understanding in Go

Docbank provides reusable Go packages for turning extracted document evidence
into stable text, provenance, and chunks. These packages are independent of a
Docbank vault: importing them does not start a daemon, open storage, or connect
to an existing installation.

Use `go.kenn.io/docbank/document` for provider-neutral normalization. Use
`go.kenn.io/docbank/document/mistral` when an application explicitly chooses
Mistral OCR as its extraction provider.

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

## Package boundary

The dependency direction is deliberate:

```text
application storage and workers
  -> document/mistral (optional provider transport)
  -> document (provider-neutral normalization)
```

Neither public package imports Docbank's vault, database, daemon, or queue.
Local extractors can use `document` without depending on Mistral or any network
transport.
