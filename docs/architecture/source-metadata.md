---
title: Source Metadata
description: How Docbank records bounded facts found inside original files.
---

# Source metadata

Docbank extracts metadata from verified original bytes and stores it as local,
immutable evidence. The evidence belongs to the content SHA-256, not to a
filename or current path. A content version joins that evidence with its node
and attachment facts when it is read.

Source metadata does not change document identity and is not user-authored
annotation. Embedded values can be incomplete, incorrect, or sensitive. They
are useful evidence, not authority over the original bytes.

## Typed fields

Every field has a canonical key, a source namespace and label, a typed value,
and a sensitive flag. The current contract supports strings, string lists,
integers, numbers, booleans, and timestamps that preserve the precision and
timezone stated by the source.

Photo and video facts use two namespaces:

- `media.container.*` contains format, media kind, pixel dimensions, image
  frame count, animation state, and known video duration.
- `image.exif.*` contains camera and lens names, orientation, ISO, exposure
  time, aperture, exposure bias, focal length, EXIF pixel dimensions, and GPS.

GPS fields are marked sensitive. The embedded API returns them because its
caller is trusted application code. Browser-session reads remove sensitive
fields; other callers must enforce their own disclosure boundary.

## Current format boundary

Container facts are available for JPEG, PNG, WebP, GIF, and MP4 files through
20 MiB. JPEG APP1 EXIF and TIFF-based images provide EXIF facts. The TIFF path
also covers camera RAW formats that retain a standard TIFF header and EXIF
directories; formats with proprietary container headers need their own bounded
parser.

The source-metadata worker currently loads originals through 64 MiB for local
parsing. Larger originals are fully verified but receive an `input_too_large`
warning instead of extracted fields. Supporting large RAW and video files
requires a bounded seekable parser rather than a larger memory allowance.

## Generations and reads

An extractor fingerprint identifies the complete local parser bundle. A parser
change creates a new immutable generation and moves the active head for that
content SHA-256; old generations remain evidence. Retrying the same generation
is idempotent.

The HTTP node and content-version detail surfaces return the active generation.
Embedded applications call `Vault.SourceMetadata` with an immutable content
version ID. Both surfaces bind fields to the requested version and keep
filename, path, ingest time, and filesystem time in separate attachment facts.
