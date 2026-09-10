---
title: Document Understanding in Go
description: Normalize and prepare documents for search, use bounded Mistral OCR, and embed images and video through Voyage without opening a Docbank vault.
---

# Document Understanding in Go

Use Docbank's Go packages to prepare documents for text search, OCR, or
embeddings in your own application. OCR extracts text from document images.
Embeddings represent text or media as numeric vectors for similarity search.
Importing these packages does not start a daemon or open a vault.

| Your application needs to… | Package under `go.kenn.io/docbank/` |
|----------------------------|-------------------------------------|
| Normalize extracted text and prepare text chunks | `document` |
| Identify provider destinations and define retrieval policy | `document/embedding` |
| Compare retrieval approaches | `document/embedding/eval` |
| Extract text with Mistral OCR | `document/mistral` |
| Detect media formats and enforce input limits | `document/media` |
| Embed images and video with Voyage AI | `document/voyage` |

The examples below describe reusable package contracts. For daemon-owned jobs,
see [embedding worker configuration](configuration.md#embedding-workers-and-credentials).

## Normalize provider output

`document.NormalizeDocument` turns ordered source units, such as pages, into
consistent text. The result includes heading paths, chunks, checksums, and
source spans that locate each result in the original input. The same input and
policy produce the same result.

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
Source family and unit-kind identifiers must be valid UTF-8 without control
characters so they remain stable in fingerprints and provider-facing locators.
Version 3 binds truncation into unit, chunk, and document checksums. A complete
shorter document therefore cannot share an identity with a longer document
whose retained evidence or remaining units were truncated by a normalization
bound.

## Build evidence from a supplied transcript

Applications that already have transcript text can pass it through the same evidence and rendition contracts as other document families:

```go
evidencePolicy, err := document.NewEvidencePolicy(100_000)
if err != nil {
	return err
}
evidence, artifact, err := document.BuildTranscriptEvidenceV1(
	document.SuppliedTranscript{Provider: "beeper", Text: transcript}, evidencePolicy,
)
if err != nil {
	return err
}
_ = artifact // retain the provider transcript with the source record
```

Use a lowercase identifier for the provider. The `supplied-transcript/v1`
JSON artifact keeps the exact provider name and transcript text. Normalized
evidence and the rendition—the derived document result—still apply their
Unicode, Markdown, and character limits.

The evidence uses the `audio` family, a generic unit, and
`degraded_provenance` completeness. Supplied text has no timing or speaker
information. Your application must associate it with the audio source and
source version. This function does not inspect audio, run speech recognition,
verify the provider's identity, or create ingestion and search records.

## Run Mistral OCR safely

Mistral uploads are refused until an operator supplies a validated capability
manifest for the configured endpoint, model, and policy. A capability manifest
records what a live provider probe demonstrated. Docbank supplies no live
manifest; provider documentation alone does not authorize uploads.

The operator prepares that evidence in this order:

1. Call `WriteProbeFixtures` to create synthetic test documents.
2. Call `ValidateProbeFixtures` locally, without credentials or HTTP.
3. Create `NewClient(API key)` and call `RunCapabilityProbe`.
4. Review and retain the resulting `CapabilityManifest`.

Fixture generation creates 21 formats deterministically. Five legacy formats
require operator-supplied synthetic seeds named `doc`, `ppt`, `xls`, `numbers`,
and `msg`. Fixture and staging directories must be private. The initial
capability contract can authorize at most PDF because it is the only format
with a probe-tested pre-upload unit bound. Other formats may extract during a
probe but remain unauthorized for production uploads.

For each production document:

1. Call `Policy.Authorize(validated manifest, declared format)`.
2. Check your application's consent record against `PolicyFingerprint`.
3. Call `Prepare` with private staging storage.
4. Call `Process`; the detected format must match the authorization.
5. Call `Release` on success or failure.
6. Call `NormalizeDocument(Result.Document, Policy.NormalizePolicy())` on a
   successful result.

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

`document.BuildEmbeddingInputs` converts canonical normalized evidence into a
sealed generation of provider-ready document inputs. It never reads the
retained Markdown, so YAML frontmatter, checksums, build identifiers, and
navigation metadata cannot enter embedding text.

The processing profile's `EmbeddingChunkPolicyV1` declares how to build
chunks. It records the tokenizer name and revision, content token budget,
overlap, formatter, truncation policy, and attachment-context rules. A
tokenizer determines the token units that the model counts.

`document.NewInputPolicy` combines that declaration with:

- A `document.Tokenizer` implementation reporting the declared identity.
- The binding's fixed model-input contract.
- The lexical evidence fingerprint, which identifies the text evidence.
- The provider's hard token and byte limits for the complete rendered input.

The resulting policy belongs to one rendition-chunk binding.

```go
policy, err := document.NewInputPolicy(binding, tokenizer, lexicalFingerprint, nil)
if err != nil {
	return err
}

generation, err := document.BuildEmbeddingInputs(evidence, policy, document.GenerationLimits{
	MaxInputs:              10_000,
	MaxTotalContentTokens:  4_000_000,
	MaxTotalRenderedTokens: 5_000_000,
	MaxTotalContentBytes:   64 << 20,
	MaxTotalRenderedBytes:  80 << 20,
	MaxFittingWorkTokens:   50_000_000,
	MaxFittingWorkBytes:    1 << 30,
})
if err != nil {
	return err
}
```

Page, heading, region, and table boundaries are preferred before token
splitting. The configured token budget applies to content only; the document
role envelope and any declared attachment context are rendered on top of it,
and the complete rendered input is re-counted against the provider limits.
Overlap is measured in exact emitted tokens, and every input keeps its heading
path and the source span it was reconstructed from.

Generation limits can reject a build but cannot change its output. They do
not enter the fingerprints. All other policy values enter the generation's
policy fingerprint. If declared, `AttachmentContextSnapshot` also binds the
exact attachment titles and context to the generation's identity.

Use `MarshalEmbeddingInputGeneration` to produce the canonical byte form.
`DecodeEmbeddingInputGeneration` accepts only that form under explicit caller
limits. It rejects forged totals, policy fingerprints, and checksums.
`ToEmbeddingInputs` accepts only the exact model-input contract used to build
the generation.

`EgressIdentity` gives applications separate endpoint-sensitive fingerprints
for document embedding and query embedding. Credentials are not part of those
identities. `VectorSpaceIdentity` separately pins provider, model revision,
dimension, and normalization without an endpoint, allowing an application to
reuse compatible vectors while still requiring fresh consent when their
destination changes.

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

`media.Detect` identifies JPEG, PNG, WebP, GIF, and MP4 from their bytes. It
does not trust the declared media type or decode pixels and samples. It reads
the metadata needed to enforce input limits:

- Dimensions, including every declared axis bound for MP4 and every GIF frame
  within the logical screen.
- Verified frame counts, including animated WebP and APNG.
- MP4 duration, using the longest movie, track, media, or sample-timing value.

MP4 video tracks must contain out-of-band H.264 (`avc1`/`avcC`) or H.265
(`hvc1`/`hvcC`) dimensions, plus `mdhd` and `stts` timing. The detector rejects
in-band or missing evidence as malformed. It does not substitute a smaller
container summary.

`media.Evaluate` checks byte, pixel, frame, and duration caps, plus the policy's
still-image, animation, and video settings. It returns stable reasons such as
`too_many_pixels` or `animated_not_allowed`. If the policy caps duration,
video with an unmeasurable duration is refused.

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

The operator prepares a manifest in this order:

1. Call `WriteProbeFixtures` to generate JPEG, PNG, and GIF fixtures and copy
   operator-supplied WebP and MP4 seeds.
2. Call `ValidateProbeFixtures` locally, without credentials or HTTP.
3. Create `NewClient(API key)` and call `RunCapabilityProbe`.
4. Review and retain the resulting `CapabilityManifest`.

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

For each production item:

1. Call `Policy.AuthorizeAll(validated manifest)`.
2. Check your application's consent record against `PolicyFingerprint`.
3. Call `media.Inspect` and require the item to be eligible under the policy.
4. Call `Client.EmbedDocuments(inputs, authorizations)`.

`EmbedDocuments` accepts only the probed input shapes `[media]` and
`[text, media]`. Before allocating a request, it enforces item and byte limits.
It also:

- Detects every media part again and serializes from the detected metadata.
- Requires authorization for each part's format and animation state.
- Requires that format's interleaved capability for `[text, media]`.
- Requires the batch capability for more than one input.

`EmbedQuery` accepts `[text]`, `[image]`, or `[text, image]`. Each shape needs
its own probed capability. Image authorization is per format; combined text and
image queries support PNG only.

Every response vector must have a unique index and the exact dimension.
The client does not follow redirects. Provider response bodies do not appear
in errors. Use `IsRetryable`, `RetryAfter`, and `MetricsFromError` when
scheduling retries.

The importing application remains responsible for credentials, human consent,
spending and scheduling limits, durable manifests, job orchestration, vector
storage, and search serving. Importing this library does not enable a vault
workflow. The daemon's separate retained-job requirements are documented in
[Configuration](configuration.md#embedding-workers-and-credentials).

## Convert CSV locally for PDF OCR

Use `document/csvpdf` to convert a declared `text/csv` source into a PDF for
the existing PDF OCR path. Conversion runs locally and never uploads content
or looks up credentials.

The converter verifies the source byte count and SHA-256, parses records,
labels each cell, and counts PDF pages before returning a result. It closes
`ocr.Source.Content` on every success and failure path.

```go
policy, err := csvpdf.NewPolicy(csvpdf.DefaultLimits())
if err != nil {
    return err
}
converted, err := csvpdf.Convert(ctx, originalCSV, policy)
if err != nil {
    return err
}
receipt := converted.Receipt()
pdfSource, err := converted.Source()
if err != nil {
    return err
}
// Retain receipt with the source and pass pdfSource to the authorized PDF processor.
```

The defaults are also hard ceilings. `NewPolicy` accepts positive, tighter
limits.

| Limit | Maximum |
|-------|---------|
| Source bytes | 1 MiB |
| PDF bytes | 10 MiB |
| Records | 10,000 |
| Cells | 50,000 |
| Bytes per cell | 64 KiB |
| Generated pages | 100 |

The embedded Go Mono font covers a subset of Latin, Greek, Cyrillic, and
common punctuation or symbols. Conversion rejects missing glyphs, characters
outside the Unicode basic multilingual plane, combining marks, shaping
scripts, formatting controls, and control characters other than cell newlines.
Use precomposed accented characters such as `é`.

CSV quoting, blank lines, and CRLF normalization follow Go's `encoding/csv`.
The output preserves empty cells, variable-width records, and quoted newlines.
Long cells continue onto later pages. Formulas remain literal text.

Keep the receipt with the source. It records original and generated hashes and
byte counts, the converter version, the policy fingerprint, and the mapping
from generated pages to original records and cells. Span identifiers start at
one. A record may span several pages. To cite a CSV cell from PDF OCR output,
use this mapping to translate the generated page reference.

The receipt does not authorize upload. Your application must:

1. Verify the PDF capability manifest and consent.
2. Choose PDF byte and page limits large enough for the conversion.
3. Retain the conversion policy with that consent.

This Go API does not add CSV OCR to the daemon or CLI.

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
