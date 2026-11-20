---
title: Visual previews
description: How Docbank identifies and stores canonical visual derivatives.
---

# Visual previews

A visual preview lets your application display a document without decoding the
original every time. Docbank stores each preview as a single image tied to an
immutable content version, leaving the original bytes and document identity
unchanged.

The preview catalog can describe image, camera RAW, and video sources, while
the [built-in producer](#backup-and-embedded-reads) supports the still-image
formats listed below. Its built-in recipes generate grid, fit, and large
previews with maximum edges of 512, 2560, and 4096 pixels.

Previews stay separate from **document renditions**, which retain normalized
evidence, text, and provider artifacts.

## Recipe identity

A **recipe** records every choice that can affect a preview's bytes. Each
preview stores:

- the maximum output edge
- the output image format
- the orientation, color, and frame-selection policy
- a processor fingerprint covering the decoder, scaler, color conversion, and
  encoder implementations

Docbank derives a stable fingerprint from the canonical recipe bytes. If you
change any of these choices, Docbank creates a new immutable generation instead
of rewriting an earlier result. The processor fingerprint is a maintained
descriptor that does not depend on the Go runtime, and changing a
byte-producing implementation or policy requires a descriptor revision.

Upgrading Go alone does not invalidate existing previews. If a standard-library
decoder or encoder change alters preview output, maintainers must revise the
descriptor to trigger regeneration. Without that revision, Docbank reuses
matching previews. Publishing a different result under the same recipe is
always rejected.

## Durable outcomes

Every generation ends in one of three terminal states:

- `ready` records the output SHA-256, byte size, media type, and dimensions.
- `unsupported` records that the recipe does not support the source format.
- `failed` records a deterministic source or decoding failure.

Temporary read, storage, and cancellation failures do not become durable
outcomes. They stay retryable and are never treated as evidence about the
source.

Each active head belongs to one content version, and deleting that version
removes its preview generations. Content-addressed storage still deduplicates
identical preview bytes across versions, while garbage collection keeps an
output as long as any generation references it.

Grid and fit generations stay outside the active head; recording a new large
recipe advances the head. Publishing an already-recorded generation through a
receipt preserves a different active generation and populates a missing head. A
cached ensure call returns the exact generation without publishing it, and
exact-size reads select an immutable generation by its recipe fingerprint.

## Backup and embedded reads

Normal backups include preview generations, active heads, and ready output
blobs. During restore, Docbank validates each canonical generation identity
before recreating the catalog.

In an embedded application, call `Vault.VisualPreview` to inspect the active
result and `Vault.OpenVisualPreview` to stream a ready output through the
verified reader contract. You can still query unsupported and failed results,
though they do not open as content.

When no matching generation exists, `Vault.EnsureVisualPreview` synchronously
produces a preview with the current built-in recipe and returns the active
result. If that recipe already has a recorded outcome and a head exists,
`Vault.EnsureVisualPreview` skips production and returns the head even when
another recipe published it. This keeps it consistent with
`Vault.VisualPreview` and `Vault.OpenVisualPreview`. Production verifies the
complete source before decoding and holds the vault mutation boundary through
publication, so you observe either a complete generation or a retryable error.

The built-in producer handles JPEG, PNG, GIF, still WebP, and camera RAW
originals that include a supported embedded JPEG preview. Camera RAW support
covers Fujifilm RAF and TIFF-family Sony ARW, Adobe DNG, Canon CR2, and Nikon
NEF files. For RAW files, the producer reads only bounded container metadata
and the embedded preview, while still verifying the complete original before
decoding. If a well-formed RAW file has no supported embedded preview, Docbank
records an `unsupported` result.

JPEG inputs can be grayscale or three-component images; CMYK and YCCK remain
unsupported.

After resizing, the producer converts JPEG, PNG, and WebP previews to sRGB.
Embedded JPEG previews inside RAW files follow the same rules using the
embedded preview's own profile and EXIF data. Photo exports keep source pixels
and profiles; see [export bundles](../usage/export-bundles.md).

- An embedded RGB ICC v2 or v4 profile converts with its colorant tags (`rXYZ`,
  `gXYZ`, `bXYZ`) and tone-curve tags (`rTRC`, `gTRC`, `bTRC`). If a profile
  also carries `A2B` lookup tables, the producer uses the colorants and curves
  and does not evaluate the lookup tables.
- A profile within 0.001 of the sRGB matrix and 0.0003 of the sRGB curve leaves
  pixels unchanged.
- When no usable profile is present, EXIF ColorSpace Uncalibrated (`0xffff`)
  with InteropIndex `R03` converts from Adobe RGB (1998). Cameras write this
  DCF marker for Adobe RGB photos that carry no profile.
- Everything else uses the stored pixel values as sRGB: lookup-table-only,
  gray, CMYK, malformed, and other-version profiles; profile data over 1 MiB,
  including inflated PNG profiles; WebP `ICCP` chunks without the `VP8X` ICC
  flag; and other EXIF ColorSpace values. These previews record no failure, the
  same way browsers ignore a profile they cannot use.

PNG inputs apply bounded EXIF orientation, and because the canonical output is
JPEG, they composite transparency onto white. PNG orientation comes from the
first eXIf chunk, whether it appears before or after the image data; if that
first eXIf chunk has no valid orientation, the image stays upright, and a
trailing eXIf chunk supplies orientation only. GIF inputs use their primary
frame even for animated sources. WebP inputs apply bounded EXIF orientation,
and the built-in decoder does not support animated WebP.

Accepted images must have positive source dimensions. They scale down without
upscaling to the selected recipe's maximum edge and encode as quality-90 JPEGs.
PNG, GIF, and WebP sources above 100,000,000 pixels record `failed` with
`source_dimensions_exceed_limit`, and camera RAW files apply the JPEG limits to
their embedded previews.

JPEGs above 100,000,000 pixels decode at one-eighth scale per edge using block
averages, including progressive JPEGs, up to 1,000,000,000 source pixels. Above
that ceiling, the decoder records `failed` with
`source_dimensions_exceed_limit` before allocating coefficient planes. Because
reduced decoding stops at one-eighth of the source edge, fit and large previews
for these JPEGs can come out smaller than the 2560/4096 recipe edges.
Subsampled chroma block averages use bilinear interpolation, and the decoder
checks for cancellation periodically during entropy decoding and pixel
reconstruction.

For oversize progressive JPEGs, the decoder uses DC scans only. AC scans must
still be marker-framed and nonempty, and the decoder skips their entropy
contents. Damaged AC data after valid DC scans still yields a correct
one-eighth preview. Measured on a 31622 x 31622 progressive JPEG (slightly
under the ceiling), peak live Go heap is about 230 MB with roughly 270 MB
allocated in total. Baseline JPEGs must Huffman-decode all AC entropy data, so
they have a lower reduced-decode ceiling of 200,000,000 pixels and record
`failed` with `source_dimensions_exceed_limit` above it. On one fast
workstation, noisy 4:2:0 quality-90 baseline images decode in about 2.2 s at
110 MP and 5.1 s at 199 MP, roughly 26 s per gigapixel, which is faster than
the standard library's full-resolution decode of the same files. That ceiling
leaves about six times that workstation's decode time under the 30 s on-demand
preview timeout, though slower hosts have less margin.

Malformed source bytes produce a durable `failed` result, while unsupported
media types, decoder features, and color models produce a durable `unsupported`
result. Read, verification, storage, and cancellation failures remain retryable.

Every uncached attempt reads and verifies the full source before checking
decoder support, including for unsupported formats such as HEIC. In both daemon
and embedded use, a failed publication can leave an unrecorded preview file on
disk. Pack reclaims these files during reconciliation; immediate deletion could
remove bytes needed by a concurrent upload.

The daemon continuously produces grid previews for the current display file of
each included photo, using missing catalog generations as its work queue and
rediscovering unfinished work after a restart. Any terminal outcome prevents
repeat work for that exact recipe. Fit and large previews run on request and
stay retained. Opening an embedded vault starts no background worker; embedded
applications call the synchronous producer directly.

Reading source metadata from ORF, RW2, CR3, or MP4 does not mean the built-in
producer can preview those formats. Other still-image formats, RAW containers
without a supported embedded JPEG, video frames, and lookup-table color
conversion require additional producers, and they follow the same generation,
retention, backup, and read contracts.
