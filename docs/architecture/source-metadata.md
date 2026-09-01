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

Container facts are available for JPEG, PNG, WebP, GIF, and MP4 files within
the 20 MiB general inspection limit. JPEG APP1 EXIF and TIFF-based images
provide EXIF facts. The TIFF path also covers camera RAW formats that retain
the standard TIFF header, plus the TIFF-derived Olympus ORF and Panasonic RW2
headers. Fujifilm RAF files provide EXIF facts through their embedded JPEG and
the original image dimensions through their RAF raw-metadata directory. Other
proprietary container headers need their own bounded parser.

The source-metadata worker keeps the general in-memory parser for originals
through 64 MiB. JPEG, TIFF-based, and MP4 originals beyond the general
inspection limit use bounded media parsing instead. JPEG and TIFF-based files
use a 20 MiB leading metadata window. The resulting generation includes a
`metadata_window_limited` warning because metadata after that window may be
omitted.

For larger MP4 files, the worker verifies the complete content identity, scans
the top-level box headers, skips media payload boxes, and reads only bounded
file-type and movie metadata. Malformed or oversized MP4 metadata produces a
durable warning. Storage and read failures remain retryable errors. Other
large RAF files similarly read only the fixed header, a bounded embedded-JPEG
metadata window, and a bounded raw-metadata directory after full verification.
Malformed RAF structure produces a durable warning; storage and read failures
remain retryable. Other formats larger than 64 MiB still receive
`input_too_large` until they have a bounded parser.

## Generations and reads

An extractor fingerprint identifies the complete local parser bundle. A parser
change creates a new immutable generation and moves the active head for that
content SHA-256; old generations remain evidence. Retrying the same generation
is idempotent.

The HTTP node and content-version detail surfaces return the active generation.
Embedded applications call `Vault.SourceMetadata` with an immutable content
version ID. Both surfaces bind fields to the requested version and keep
filename, path, ingest time, and filesystem time in separate attachment facts.
