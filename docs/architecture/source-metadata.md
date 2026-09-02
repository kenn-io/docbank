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

Values are one of `string`, `string_list`, `integer`, `number`, `boolean`,
or `timestamp`; the `kind` field names which payload is present. The current
extractor publishes these namespaces:

| Namespace | Sources | Keys |
|-----------|---------|------|
| `media.container` | JPEG, PNG, WebP, GIF, MP4 | `format`, `kind`, `width_px`, `height_px`, `frame_count`, `animated`, `duration_ms` |
| `image.exif` | JPEG APP1, TIFF-based RAW, RAF, CR3 | `camera_make`, `camera_model`, `lens_make`, `lens_model`, `orientation`, `iso`, `exposure_time_seconds`, `f_number`, `exposure_bias_ev`, `focal_length_mm`, `pixel_width`, `pixel_height`, `gps_latitude`, `gps_longitude`, `gps_timestamp` |
| `xmp` | XMP packets in PDF and images | Dublin Core and XMP basic properties by their XMP name |
| `pdf.info` | PDF Info dictionary | Info entries by their PDF key, plus the page count |
| `office.core`, `office.custom` | OOXML core and custom properties | core properties by name, `word_count`, and each custom property under `office.custom.<name>` |
| `email` | RFC 5322 messages | `from`, `to`, `cc`, `bcc`, `subject`, `sent`, `received` |
| `calendar` | iCalendar | `start`, `end`, and their `.raw` source text |
| `media.id3` | ID3 tags | tag frames such as `album` by their common name |

GPS coordinates are the fields currently marked sensitive.

Latitude and longitude are published together only when both coordinates have
valid hemisphere markers and degree, minute, and second ranges. An incomplete
pair, invalid coordinate, or zero-zero placeholder is omitted with a warning.
Complete EXIF GPS date and time fields become one UTC timestamp. GPS coordinate
fields are marked sensitive. The embedded API returns them because its caller
is trusted application code. Browser-session reads remove sensitive fields;
other callers must enforce their own disclosure boundary.

## Current format boundary

Container facts are available for JPEG, PNG, WebP, GIF, and MP4 files within
the 20 MiB general inspection limit. JPEG APP1 EXIF and TIFF-based images
provide EXIF facts. The TIFF path also covers camera RAW formats that retain
the standard TIFF header, plus the TIFF-derived Olympus ORF and Panasonic RW2
headers. Fujifilm RAF files provide EXIF facts through their embedded JPEG and
the original image dimensions through their RAF raw-metadata directory. Canon
CR3 files provide camera, lens, exposure, orientation, capture-time, GPS, and
image-dimension facts through their Canon TIFF/EXIF metadata boxes. Other
proprietary container headers need their own bounded parser.

The source-metadata worker keeps the general in-memory parser for originals
through 64 MiB. JPEG, TIFF-based, and MP4 originals beyond the general
inspection limit use bounded media parsing instead. JPEG and TIFF-based files
use a 20 MiB leading metadata window. The resulting generation includes a
`metadata_window_limited` warning because metadata after that window may be
omitted.

For larger MP4 files, the worker verifies the complete content identity, scans
the top-level box headers, skips media payload boxes, and reads only bounded
file-type and movie metadata, including the movie-header creation time when
present. Malformed or oversized MP4 metadata produces a
durable warning. Storage and read failures remain retryable errors. Other
large RAF files similarly read only the fixed header, a bounded embedded-JPEG
metadata window, and a bounded raw-metadata directory after full verification.
Malformed RAF structure produces a durable warning; storage and read failures
remain retryable. Large CR3 files are also verified in full, but the extractor
walks only the ISO base media file headers and bounded Canon metadata boxes; it
does not buffer or decode the RAW image payload. Malformed CR3 structure
produces a durable warning, while storage and read failures remain retryable.
Other formats larger than 64 MiB still receive `input_too_large` until they
have a bounded parser.

## Generations and reads

An extractor fingerprint identifies the complete local parser bundle. A parser
change creates a new immutable generation and moves the active head for that
content SHA-256; old generations remain evidence. Retrying the same generation
is idempotent. Because the fingerprint covers every parser, any change to it
makes the daemon re-read and re-extract every retained original in every
vault; a test pins the fingerprint so that cost is taken deliberately.

The HTTP node and content-version detail surfaces return the active generation.
Embedded applications call `Vault.EnsureSourceMetadata` with an immutable
content version ID to run the current local extractor synchronously for those
exact bytes, or `Vault.SourceMetadata` for a read-only lookup. Ensuring an
already-current generation is idempotent and does not start the daemon worker
or scan unrelated content. The ensure operation shares the vault mutation gate
with content writes and physical maintenance, so its exact-version lookup,
verified read, publication, and final result cannot straddle an authority
change. Missing or corrupt physical source bytes retain the embedded API's
`ErrContentUnavailable` identity. All read surfaces bind fields to the
requested version and keep filename, path, ingest time, and filesystem time in
separate attachment facts.
