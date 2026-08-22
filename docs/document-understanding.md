---
title: Document Understanding in Go
description: Normalize and prepare documents for search, use bounded Mistral OCR, and embed images and video through Voyage without opening a Docbank vault.
---

# Document Understanding in Go

Docbank provides reusable Go packages for turning extracted document evidence
into stable text, provenance, and chunks. These packages are independent of a
Docbank vault: importing them does not start a daemon, open storage, or connect
to an existing installation.

Use `go.kenn.io/docbank/document` for provider-neutral normalization. Use
`go.kenn.io/docbank/document/embedding` to build deterministic text-embedding
plans, optionally with a pre-embedding distillation step. Use
`go.kenn.io/docbank/document/embedding/eval` to compare retrieval recipes. Use
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

## Prepare text for semantic retrieval

`embedding.BuildEmbeddingPlan` converts a complete `NormalizedDocument` into a
deterministic set of bounded provider inputs. Raw normalized chunks are the
default representation. A recipe may instead select distilled sections or a
combined plan containing both representations.

```go
recipe, err := embedding.NewRecipe(embedding.RecipeConfig{
	Mode:          embedding.RepresentationRaw,
	MaxInputRunes: 8_000,
})
if err != nil {
	return err
}

plan, err := embedding.BuildEmbeddingPlan(
	normalized,
	embedding.DocumentContext{Filename: "quarterly-report.pdf"},
	recipe,
	nil,
)
if err != nil {
	return err
}
```

Context includes bounded filename, title, heading path, and source locator
fields. Every final input is truncated on rune boundaries to the recipe's
provider input cap. Recipe, context, plan, source, and individual input
fingerprints let an application store derived state without making worker
batch size or claim timing part of vector identity.

For optional distillation, configure `Distillation` on a `distilled` or
`combined` recipe and use this flow:

```text
PrepareDistillation(complete normalized document)
  -> application checks separate distillation consent
  -> Distiller.Distill
  -> ValidateDistillate
  -> BuildEmbeddingPlan
```

Partitions contain complete normalized chunks in source order. A distiller's
result must cover every partition exactly once in that order and stays linked
to exact source spans. Derived text is untrusted, non-authoritative evidence;
callers should retain normalized source evidence for display and verification.
Different provider output gets a different content-addressed distillate and
plan identity.

Docbank defines the provider-neutral `Distiller` interface but does not choose
a distillation provider or make network requests. Provider implementations are
responsible for authenticated capability checks, transport bounds, redirect
policy, and per-request consent enforcement. `EgressIdentity` gives
applications separate endpoint-sensitive fingerprints for document
distillation, document embedding, and query embedding. Credentials are not
part of those identities. `VectorSpaceIdentity` separately pins provider,
model revision, dimension, and normalization without an endpoint, allowing an
application to reuse compatible vectors while still requiring fresh consent
when their destination changes.

The shared retrieval helpers make omitted and `auto` search lexical, so a
query is not sent to an embedding provider without explicit `semantic` or
`hybrid` mode. Candidate limits default to 100 and are bounded at 1,000.
`CollectScopedCandidates` requires the backend to apply scope before its
vector cutoff and pages until authoritative exhaustion or a `limit+1`
overflow probe. Reciprocal-rank fusion preserves lexical and semantic signals
and reports overflow from either input lane or the fused union.

`document/embedding/eval` runs versioned public or synthetic corpora through
repeatable retrieval systems. Reports include Recall@5/10/20, nDCG@10, MRR,
critical misses, provider calls and input/output usage, estimated cost, and
latency. Repeated trials retain individual observations and report empirical
minimum, mean, and maximum values instead of hiding hosted-provider variance.
Applications should keep raw as the default unless measured results justify a
different recipe.

## Detect and bound visual attachments

`media.Detect` sniffs JPEG, PNG, WebP, GIF, and MP4 containers and reads only
the metadata needed to bound provider input: dimensions (every authoritative
axis bound for MP4, every frame inside the logical screen for GIF), verified
frame count (including animated WebP and APNG), and MP4 duration as the
longest movie, track, media, or sample-timing value. MP4 video tracks must
carry out-of-band H.264 (`avc1`/`avcC`) or H.265 (`hvc1`/`hvcC`) dimensions
plus `mdhd` and `stts` timing. In-band or missing evidence is malformed rather
than inferred from a smaller container summary. Detection never decodes pixels
or samples and never trusts the declared media type.
`media.Evaluate` applies a policy of byte, pixel, frame, and duration caps
and still, animated, and video toggles, returning a stable reason such as
`too_many_pixels` or `animated_not_allowed` that an application can record.
Under a duration cap, video whose duration cannot be measured is refused.

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
JPEG, PNG, WebP, still GIF, animated GIF, and MP4 documents; text queries;
JPEG, PNG, WebP, and still GIF image queries; text-then-PNG queries;
text-then-media documents probed per format; and mixed-format batches at the
policy limit.
Whether animated GIF, video, or a given query shape may be sent is decided by
recorded probe evidence, not by assumption.

The operator flow is:

```text
WriteProbeFixtures (JPEG, PNG, GIF generated; WebP and MP4 from operator seeds)
  -> ValidateProbeFixtures (local only; no credentials or HTTP)
  -> NewClient(API key) + RunCapabilityProbe
  -> review and retain the CapabilityManifest
```

WebP and MP4 cannot be encoded by the Go standard library, so the operator
supplies synthetic seeds named `image_webp.webp` and `video_mp4.mp4`, plus
contrasting `image_webp_alt.webp` and `video_mp4_alt.mp4` variants of the
same format whose content differs, so pixel contribution is demonstrated
within each format. The
seed directory, destination parent, and published fixture directory must be
owner-private, and fixture generation publishes a complete new directory
atomically rather than updating an existing directory. The
probe stores no media, vectors, or responses; the manifest records only
sanitized pass, reject, and fail observations.

For each production item, the application flow is:

```text
Policy.AuthorizeAll(validated manifest)
  -> application verifies its own consent record against PolicyFingerprint
  -> media.Inspect (eligible under the policy)
  -> Client.EmbedDocuments(inputs, authorizations)
```

`EmbedDocuments` accepts only the probed input shapes, `[media]` and
`[text, media]`. It re-detects every media part and serializes from the
detected metadata so caller metadata cannot misdescribe bytes, requires an
authorization for the part's format and animation state, requires the
format's own interleaved capability for `[text, media]`, requires the batch
capability for more than one input, and refuses requests over the policy's
item and byte limits before allocation. `EmbedQuery` accepts `[text]`,
`[image]`, or `[text, image]`, each needing its own probed capability, with
image queries authorized per format and the combined shape limited to PNG.
Provider responses are strictly decoded: every vector
must carry a unique index and the exact dimension. The client never follows
redirects, so media and credentials cannot leave the pinned endpoint.
Provider bodies never appear in errors; `IsRetryable`, `RetryAfter`, and
`MetricsFromError` support the application's own scheduling.

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

application storage and workers
  -> document/embedding (provider-neutral text planning and retrieval contracts)
  -> document (provider-neutral normalization)
```

No public package imports Docbank's vault, database, daemon, or queue. Local
extractors can use `document` without depending on Mistral or any network
transport, applications can use `document/embedding` without a database or
provider client, and applications can use `document/media` alone to record
image dimensions at ingest without any provider.
