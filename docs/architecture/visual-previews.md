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

Docbank does not yet produce previews automatically. The catalog and read
boundary are in place so format-specific producers can be added without
changing preview identity, retention, backup, or application integration.
