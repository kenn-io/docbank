---
title: Visual Previews
description: How Docbank identifies and retains canonical visual derivatives.
---

# Visual previews

A visual preview lets an application display a document without decoding the
original each time. Docbank retains the preview as one image tied to an
immutable content version. The original bytes and document identity remain
unchanged.

The preview catalog can describe image, camera RAW, and video sources. The
[built-in producer](#backup-and-embedded-reads) supports the still-image formats
listed below. The built-in recipes provide grid, fit, and large results with
maximum edges of 512, 2560, and 4096 pixels.

Docbank keeps previews separate from **document renditions**, which retain
normalized evidence, text, and provider artifacts.

## Recipe identity

A **recipe** records every choice that can affect the preview's bytes. Each
preview stores:

- the maximum output edge
- the output image format
- the orientation, color, and frame-selection policy
- a processor fingerprint covering the decoder, scaler, color conversion, and
  encoder implementations

The canonical recipe bytes produce a stable fingerprint. Changing any of these
choices creates a new immutable generation instead of rewriting an earlier
result. The processor fingerprint is a maintained descriptor independent of
the Go runtime. Changing a byte-producing implementation or policy requires a
descriptor revision.

A Go upgrade alone does not invalidate existing previews. If a standard-library
decoder or encoder change affects preview output, maintainers must revise the
descriptor to trigger regeneration. Without that revision, matching previews
are reused. Publishing a different result under the same recipe is rejected.

## Durable outcomes

A generation has one of three terminal states:

- `ready` records the output SHA-256, byte size, media type, and dimensions.
- `unsupported` records that the recipe does not support the source format.
- `failed` records a deterministic source or decoding failure.

Temporary read, storage, and cancellation failures are not durable outcomes.
They remain retryable and must not be mistaken for evidence about the source.

The active head belongs to one content version. Deleting that version
removes its preview generations. Content-addressed storage still deduplicates
identical preview bytes across versions, and garbage collection retains an
output while any generation references it.

Grid and fit generations stay outside the active head. Recording a new large
recipe advances the head. Receipt-backed publication of a recorded generation
preserves a different active generation and populates a missing head. Cached
ensure returns the exact generation without publishing it. Exact-size reads
select the immutable generation by its recipe fingerprint.

## Backup and embedded reads

Preview generations, active heads, and ready output blobs are included in
normal backups. Restore validates the canonical generation identity before it
recreates the catalog.

Embedded applications use `Vault.VisualPreview` to inspect the active result
and `Vault.OpenVisualPreview` to stream a ready output through the verified
reader contract. Unsupported and failed results remain queryable but do not
open as content.

`Vault.EnsureVisualPreview` synchronously produces a preview with the current
built-in recipe when no matching generation exists, then returns the active
result. If that recipe already has a recorded outcome and a head exists, ensure
skips production and returns the head even if another recipe published it.
This keeps ensure consistent with `Vault.VisualPreview` and
`Vault.OpenVisualPreview`. Production verifies the complete source before
decoding and holds the vault mutation boundary through publication, so callers
either observe a complete generation or a retryable error.

The built-in producer accepts JPEG, PNG, GIF, still WebP, and camera RAW
originals that contain a supported embedded JPEG preview. Camera RAW support
covers Fujifilm RAF and TIFF-family Sony ARW, Adobe DNG, Canon CR2, and Nikon
NEF files. It reads only bounded container metadata and the embedded preview.
The complete original is still verified before decoding. A well-formed RAW file
without a supported embedded preview records an `unsupported` result.

JPEG inputs may be grayscale or three-component images. CMYK, YCCK, and
embedded ICC profiles remain unsupported rather than receiving an unmanaged
color conversion. PNG inputs apply bounded EXIF orientation, reject embedded ICC
profiles, and composite transparency onto white because the canonical output
is JPEG. GIF inputs use their primary frame, including for animated sources.
WebP inputs apply bounded EXIF orientation and reject embedded ICC profiles.
Animated WebP remains unsupported by the built-in decoder.

Accepted images scale without upscaling to the selected recipe's maximum edge
and encode as a quality-90 JPEG. The decoded source image must have positive
dimensions and no more than 100,000,000 pixels. A larger image records
`failed` with `source_dimensions_exceed_limit`. For camera RAW files, this
limit applies to the embedded JPEG being decoded.

Malformed source bytes become a durable `failed` result. Unsupported media
types, decoder features, and color profiles become a durable `unsupported`
result. Read, verification, storage, and cancellation failures are retryable.

Every uncached attempt reads and verifies the full source before checking
decoder support, including unsupported formats such as HEIC. A failed
publication can leave an unrecorded preview file on disk in both daemon and
embedded use. Pack reclaims these files during reconciliation. Immediate
deletion could remove bytes needed by a concurrent upload.

The daemon continuously produces grid previews for the current display file
of each included photo. Missing catalog generations are its work queue. A
restart rediscovers unfinished work. All terminal outcomes prevent repeat work
for that exact recipe. Fit and large run on request and remain retained.
Opening an embedded vault starts no worker. Embedded applications call the
synchronous producer themselves.

Reading source metadata from ORF, RW2, CR3, or MP4 does not mean the built-in
producer can preview those formats. Other still-image formats, RAW containers
without a supported embedded JPEG, video frames, and managed color conversion
require additional producers, but they use the same generation, retention,
backup, and read contracts.
