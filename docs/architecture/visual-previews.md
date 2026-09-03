---
title: Visual Previews
description: How Docbank identifies and retains canonical visual derivatives.
---

# Visual previews

Docbank has a separate catalog for visual previews of images, camera RAW files,
and video. A preview is a retained derivative of one immutable content version.
It does not replace the original and does not change document identity.

The catalog is deliberately separate from document renditions. Document
renditions describe normalized evidence, text, and provider artifacts. A visual
preview is a single display-oriented image that another application can resize
for grids, detail views, or search results without decoding the original again.

## Recipe identity

Every preview records the complete recipe that can affect its bytes:

- the maximum output edge;
- output image format;
- orientation, color, and frame-selection policy; and
- a processor fingerprint covering the decoder, scaler, color conversion, and
  encoder implementations.

The canonical recipe bytes produce a stable fingerprint. Changing any of these
choices creates a new immutable generation instead of rewriting an earlier
result.

## Durable outcomes

A generation has one of three terminal states:

- `ready` records the output SHA-256, byte size, media type, and dimensions;
- `unsupported` records that the recipe does not support the source format; or
- `failed` records a deterministic source or decoding failure.

Temporary read, storage, and cancellation failures are not durable outcomes.
They remain retryable and must not be mistaken for evidence about the source.

The active head belongs to an exact content version. Deleting that version
removes its preview generations. Content-addressed storage still deduplicates
identical preview bytes across versions, and garbage collection retains an
output while any generation references it.

## Backup and embedded reads

Preview generations, active heads, and ready output blobs are included in
normal backups. Restore validates the canonical generation identity before it
recreates the catalog.

Embedded applications use `Vault.VisualPreview` to inspect the active result
and `Vault.OpenVisualPreview` to stream a ready output through the verified
reader contract. Unsupported and failed results remain queryable but do not
open as content.

`Vault.EnsureVisualPreview` synchronously produces the current preview for one
exact version when no matching generation exists. It verifies the complete
source before decoding and holds the vault mutation boundary through
publication, so callers either observe a complete generation or a retryable
error.

The built-in producer accepts JPEG, PNG, GIF, still WebP, and camera RAW
originals that contain a supported embedded JPEG preview. Camera RAW support
covers Fujifilm RAF and TIFF-family Sony ARW, Adobe DNG, Canon CR2, and Nikon
NEF files. It reads only bounded container metadata and the embedded preview;
the complete original is still verified before decoding. A well-formed RAW file
without a supported embedded preview records an `unsupported` result.

JPEG inputs may be grayscale or three-component images; CMYK, YCCK, and
embedded ICC profiles remain unsupported rather than receiving an unmanaged
color conversion. PNG inputs apply bounded EXIF orientation, reject embedded ICC
profiles, and composite transparency onto white because the canonical output
is JPEG. GIF inputs use their primary frame, including for animated sources.
WebP inputs apply bounded EXIF orientation and reject embedded ICC profiles;
animated WebP remains unsupported by the built-in decoder.
Accepted images scale without upscaling to a 4096-pixel maximum edge
and encode as a quality-90 JPEG. Malformed source bytes become a durable
`failed` result; unsupported media types, decoder features, and color profiles
become a durable `unsupported` result. Read, verification, storage, and
cancellation failures are retryable.

Preview production is application-driven: opening a vault does not start a
worker. Other still-image formats, RAW containers without a supported embedded
JPEG, video frames, and managed color conversion require additional producers,
but they use the same generation, retention, backup, and read contracts.
