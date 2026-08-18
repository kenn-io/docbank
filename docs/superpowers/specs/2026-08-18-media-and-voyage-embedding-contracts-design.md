# Media detection and Voyage multimodal embedding contracts

**Status:** Approved design
**Date:** 2026-08-18
**Kata:** `0jnq` (child of `rz07`)

## Summary

Docbank becomes the reusable home for the provider-facing half of visual
attachment understanding, in the same shape it already provides for text
documents through `document` and `document/mistral`:

- `go.kenn.io/docbank/document/media` — provider-neutral detection of still
  images, animated images, and video from bytes, with bounded eligibility
  evaluation and stable machine-readable reasons.
- `go.kenn.io/docbank/document/voyage` — bounded, stateless Voyage AI
  multimodal embedding transport that fails closed until an operator has run
  the authenticated capability probe and supplied its validated manifest.

The importing application keeps everything that is about *its* archive:
attachment provenance and roles, human consent records, orchestration, claims
and retries, vector storage, search serving, budgets, and schedules.

The first consumer is a message archive that wants "find the screenshot,
photo, diagram, or short video" search over attachments. The design was
extracted from that application's working implementation; nothing here is
speculative provider behavior.

## Goals

- One owner for media sniffing, media caps, Voyage request shape, retry
  policy, error classification, and response validation, so a Docbank change
  cannot silently leave an application behind.
- Capability gating by recorded evidence, not compile-time constants. Whether
  animated GIF, MP4, or image queries may be sent is decided by an operator's
  authenticated probe manifest, exactly as PDF authority is decided for
  Mistral.
- A policy identity (`Fingerprint`) the application can bind its consent
  record to.
- Neither package imports the Docbank vault, database, daemon, or queue.
- Neither package computes, stores, or serves vectors on Docbank's behalf.
  Docbank still does not treat semantic embeddings as part of its own
  retrieval contract.

## Out of scope

- Retention and training posture fields for Voyage. Docbank cannot pin them
  the way Mistral's are pinned; the application's consent disclosure carries
  that text.
- Private staging (spool). Eligible media is at most 20 MiB and is handled in
  memory; there is no 500 MiB document case here.
- Message or document context assembly, owner identity, publication revision
  hashing, and any orchestration.
- Vector storage backends and search.
- OCR of images, transcription, transcoding, thumbnails, or frame extraction.

## `document/media`

### Detection

```go
func Detect(reader io.ReaderAt, size int64, declaredMediaType string) (Metadata, error)
func DetectBytes(data []byte, declaredMediaType string) (Metadata, error)
```

Detection sniffs bytes; the declared media type is recorded and compared but
never trusted. Supported formats and what is read:

| Format | Signature | Metadata |
| --- | --- | --- |
| JPEG | `FF D8 FF` | width, height (stdlib `image.DecodeConfig`) |
| PNG | `89 50 4E 47 0D 0A 1A 0A` | width, height (stdlib) |
| WebP | `RIFF....WEBP` | width, height from `VP8X`, `VP8L`, or `VP8 ` |
| GIF | `GIF87a` / `GIF89a` | logical-screen width, height; frame count by walking image descriptors without decoding pixels; `Animated` when frames > 1 |
| MP4 | `ftyp` at offset 4 | width, height from `tkhd`; duration from `mvhd` (v0 and v1, overflow-safe) |

`Metadata` carries `Format` (`jpeg`, `png`, `webp`, `gif`, `mp4`), `Kind`
(`image` or `video`), `MediaType`, `DeclaredMediaType`, `Width`, `Height`,
`FrameCount`, `DurationMS`, `Animated`, and `Size`.

Detection failures are typed: `ErrUnsupportedMedia` for unknown or explicitly
non-media signatures (PDF, Ogg, ID3 and anything else), `ErrMalformedMedia`
when a recognized container cannot yield dimensions.

### Eligibility

```go
type Policy struct {
	MaxBytes       int64
	MaxPixels      int64
	MaxDurationMS  int64
	AllowStill     bool
	AllowAnimated  bool
	AllowVideo     bool
}
func DefaultPolicy() Policy
func Evaluate(metadata Metadata, policy Policy) Reason
```

`Reason` is a stable string enum: `eligible`, `unsupported_media_type`,
`malformed_media`, `too_large`, `too_many_pixels`, `too_long`,
`still_not_allowed`, `animated_not_allowed`, `video_not_allowed`. Pixel
limits apply per axis and to the product. Defaults are 20 MiB, 16 000 000
pixels, and no duration cap.

`Inspect(reader, size, declaredMediaType, policy)` combines detection and
evaluation and returns `(Metadata, Reason, error)`; a detection error becomes
the matching reason rather than an error, so callers can persist stable
outcomes. `InspectBytes` is the in-memory form and is what a query-image
validator uses.

The package has no notion of attachment role, owner, or hash. Those are
application pre-filters.

## `document/voyage`

### Policy

`NewPolicy(PolicyConfig)` validates and returns an opaque `Policy`. Pinned
values: provider `voyage`, endpoint `https://api.voyageai.com/v1`, model
`voyage-multimodal-3.5`, output dimension `1024`. `PolicyConfig` selects the
`media.Policy` caps, `MaxBatchItems` (≤ 64), `MaxRequestBytes` (≤ 64 MiB),
and `MaxResponseBytes` (≤ 8 MiB). `Values()` returns a read-only JSON-tagged
copy. `CanonicalJSON(manifest)` and `Fingerprint(manifest)` produce the
policy identity including the sorted list of probe-passed capability
authorities, mirroring `mistral.Policy`.

### Capabilities and manifest

Candidate capabilities, in fixed order:

| ID | What the probe demonstrates |
| --- | --- |
| `image_jpeg` | a JPEG document embeds |
| `image_png` | a PNG document embeds |
| `image_webp` | a WebP document embeds |
| `image_gif_still` | a single-frame GIF embeds |
| `image_gif_animated` | a multi-frame GIF embeds and its vector differs from the still first frame (motion is observed) |
| `video_mp4` | an MP4 document embeds |
| `query_text` | a text query embeds into the same space and ranks a matching document first |
| `query_image` | an image query embeds and ranks the matching document first |
| `interleaved_text_media` | a `[text, media]` document embeds and input order is respected |
| `batch_limits` | a batch at `MaxBatchItems` embeds and index order round-trips |

`CapabilityManifest{SchemaVersion, ProbeFixtureContract, ObservedOn,
Endpoint, Model, Dimension, MaxBatchItems, Results[]}` with
`CapabilityResult{CapabilityID, Status, ReasonCode, FixtureDigest,
RequestFingerprint, TotalTokens?}`. `ValidateComplete` pins schema and
fixture contract, endpoint/model/dimension, requires one result per
candidate in order, and requires non-passing results to be scrubbed of
observations. `Encode`/`DecodeCapabilityManifest` bound size, reject unknown
and duplicate keys. Manifests contain no media bytes, vectors, or secrets.

`Policy.Authorize(manifest, capabilityID) (Authorization, error)` fails
closed: the manifest must validate, target the policy, and record `passed`
for that capability with a request fingerprint that matches this policy.
`Authorization` exposes `Capability()` and `PolicyFingerprint()`; it does not
attest human consent.

### Probe fixtures

`WriteProbeFixtures(ctx, dir, FixtureOptions)` writes deterministic JPEG,
PNG, still GIF, and animated GIF fixtures with the Go standard library. WebP
and MP4 cannot be encoded by the standard library, so they are
operator-supplied synthetic seeds named `webp` and `mp4` in
`FixtureOptions.SeedDirectory`, following the Mistral legacy-seed mechanism.
`ValidateProbeFixtures` verifies the complete set locally, including that the
seeds are detected as the expected format and pass the policy caps, without
credentials or HTTP.

`RunCapabilityProbe(ctx, *Client, ProbeConfig)` runs the fixture matrix
serially and returns sanitized observations only.

### Client

```go
type Input struct{ Parts []Part }          // ordered; at most one media part
type Part struct{ Text string; Media *Media }
type Media struct{ Metadata media.Metadata; Bytes []byte }
type Result struct{ Vectors [][]float32; Usage Usage; Metrics RequestMetrics }

func NewClient(policy Policy, config ClientConfig) (*Client, error)
func (c *Client) EmbedDocuments(ctx, inputs []Input, authorizations []Authorization) (Result, error)
func (c *Client) EmbedQuery(ctx, input Input, authorization Authorization) ([]float32, Usage, error)
```

`ClientConfig` carries `APIKey`, `Timeout` (default 45 s, max 5 min),
`MaxRetries` (default 3, max 10), `RetryBaseDelay`, and an optional
`HTTPClient`. Every media part in a document batch must be covered by an
authorization whose capability matches the media's format and animation
state; a query image needs `query_image`; a text query needs `query_text`;
a document with a text part and a media part needs
`interleaved_text_media`. Batches over `MaxBatchItems` or whose estimated
encoded size exceeds `MaxRequestBytes` fail before allocation with
`ErrBatchTooLarge`.

Transport: `POST {endpoint}/multimodalembeddings`, bearer auth, request
`{inputs, model, input_type, truncation:false, output_dimension}`, media as
`data:<type>;base64,` URLs. Retries: exponential base delay doubling to a cap,
overridden by `Retry-After` (integer seconds or HTTP-date, clamped to one
hour) and only for `ErrTransientResponse`. Classification: 429 and 5xx and
transport failures → `ErrTransientResponse`; 401/403 →
`ErrUnauthorized`; 400 with a size-rejection body → `ErrBatchTooLarge`;
other 4xx → `ErrPermanentResponse`; unparseable, wrong count, wrong
dimension, duplicate or missing index, NaN or Inf → `ErrMalformedResponse`
(also retryable, once). Response bodies are bounded by `MaxResponseBytes`;
error bodies are read to 4 KiB and never included in errors.
`IsRetryable(err)`, `RetryAfter(err) (time.Duration, bool)`, and
`MetricsFromError(err)` mirror the Mistral package.

### `voyagetest`

`SyntheticManifest(policy, passed ...string)` and small deterministic fixture
bytes for application tests. Package documentation states that synthetic
manifests are not observations and must not be production upload authority.

## Package boundary

```text
application storage and workers
  -> document/voyage (optional provider transport)
  -> document/media (provider-neutral detection and eligibility)
```

`document/voyage` imports `document/media`; neither imports anything else in
Docbank. Applications may use `document/media` alone, for example to record
image dimensions at ingest without any provider.

## Testing

- Unit tests port the application's existing coverage: sniffing of every
  format including hand-built WebP and MP4 containers, animated GIF
  detection, malformed and unsupported inputs, every policy cap; Voyage
  request shape and index restoration through `httptest`, query variants,
  response validation, request and response limits, HTTP classification and
  retry with `Retry-After`, timeout and cancellation; manifest validation and
  scrubbing; `Authorize` fail-closed paths; probe fixture generation and
  validation including missing seeds.
- A `voyage_probe` build-tagged test runs the real probe when
  `VOYAGE_API_KEY` is set and both seeds are present, and fails closed
  otherwise. It stores no media, vectors, or responses.
- Both SQLite modes are unaffected; the packages have no storage.

## Documentation

`docs/document-understanding.md` gains a "Visual attachments" section with
the operator flow (`WriteProbeFixtures → ValidateProbeFixtures →
RunCapabilityProbe → retain manifest`) and the per-item flow
(`Authorize → application consent check → Inspect → EmbedDocuments`).
`docs/capabilities.md` boundary text is updated so it no longer implies
Docbank has no provider-facing media path while still stating that Docbank
does not compute or serve embeddings for its own vault.
