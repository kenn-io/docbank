---
title: Source metadata
description: How Docbank records a limited set of facts found inside original files.
---

# Source metadata

Docbank records facts found inside original files, such as image dimensions,
camera settings, and PDF properties. It verifies the original bytes before
extracting this **source metadata** and retains each result as immutable
local evidence.

The evidence belongs to the content SHA-256. When a caller reads a content
version, Docbank returns that evidence with the version's node and attachment
facts. Moving or renaming the file does not change the extracted evidence.

Source metadata does not change document identity and is not user-authored
annotation. Embedded values can be incomplete, incorrect, or sensitive. They
are useful evidence, not authority over the original bytes.

## Typed fields

Each field records a canonical key, source namespace, source label, typed
value, and sensitive flag. A **namespace** identifies the format or metadata
family from which the extractor read the field.

The `kind` field selects one payload: `string`, `string_list`, `integer`,
`number`, `boolean`, or `timestamp`. Timestamps preserve the source's stated
precision and timezone. Keys describe the fact. `source_field` preserves its
format-specific label. For example, PDF `CreationDate`, EXIF `DateTimeOriginal`,
and MP4 `mvhd.CreationTime` each produce the key `created` in their own
namespace.

The current extractor publishes these fields when the source contains them:

| Namespace | Sources | Canonical keys |
|-----------|---------|------|
| `media.container` | JPEG, PNG, WebP, GIF, MP4, TIFF-family images, RAF, CR3 | `media.container.format`, `.kind`, `.width_px`, `.height_px`, `.frame_count`, `.animated`, `.duration_ms`; `created` for MP4 |
| `image.exif` | JPEG APP1, TIFF-family images and RAW, RAF, CR3 | `image.exif.camera_make`, `.camera_model`, `.lens_make`, `.lens_model`, `.orientation`, `.iso`, `.exposure_time_seconds`, `.f_number`, `.exposure_bias_ev`, `.focal_length_mm`, `.pixel_width`, `.pixel_height`, `.gps_latitude`, `.gps_longitude`, `.gps_timestamp`; `created`, `modified`, `creators`, `description` |
| `xmp` | XMP packets in PDF | `title`, `creators`, `subject`, `description`, `keywords`, `language`, `created`, `modified` |
| `pdf.info` | PDF Info dictionary and page tree | `title`, `creators`, `subject`, `keywords`, `created`, `modified`, `page_count` |
| `office.core` | OOXML core and application properties | `title`, `creators`, `subject`, `description`, `keywords`, `language`, `created`, `modified`, `page_count`, `office.core.word_count` |
| `office.custom` | OOXML custom properties | `office.custom.<normalized_name>` |
| `email` | RFC 5322 messages | `email.from`, `.to`, `.cc`, `.bcc`, `.subject`, `.sent`, `.received`; `attachment_count` |
| `calendar` | iCalendar | `title`, `description`, `creators`, `calendar.start`, `.end`, `.start.raw`, `.end.raw` |
| `media.id3` | ID3 tags | `title`, `creators`, `created`, `media.id3.album` |

In this table, a leading dot adds the namespace prefix. For example,
`.camera_model` means `image.exif.camera_model`.

Latitude and longitude are published together only when both coordinates have
valid hemisphere markers and degree, minute, and second ranges. An incomplete
pair, invalid coordinate, or zero-zero placeholder is omitted with a warning.
Complete EXIF GPS date and time fields become one UTC timestamp. GPS coordinate
fields and `email.bcc` are marked sensitive. The embedded API returns them
because its caller is trusted application code. Browser-session reads remove
sensitive fields. Other callers must enforce their own disclosure boundary.

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

The daemon processes source metadata in the background. Embedded callers choose
when to process a version with `EnsureSourceMetadata`. Both use the same parser.
Originals through 64 MiB fit the general in-memory read. JPEG, TIFF-family, RAF,
CR3, and MP4 originals beyond the 20 MiB general media-inspection limit use
bounded media parsing instead. JPEG and TIFF-based files use a 20 MiB leading
metadata window. The resulting generation includes a `metadata_window_limited`
warning because metadata after that window may be omitted.

For large media files, the worker verifies the complete content identity
before bounded parsing:

- **MP4:** scan top-level box headers, skip media payload boxes, and read bounded
  file-type and movie metadata. Include the movie-header creation time when
  present. Malformed or oversized metadata produces a durable warning.
- **RAF:** read the fixed header, a bounded embedded-JPEG metadata window, and a
  bounded raw-metadata directory. Malformed structure produces a durable warning.
- **CR3:** walk ISO base media file headers and bounded Canon metadata boxes.
  Do not buffer or decode the RAW image payload. Malformed structure produces a
  durable warning.

Storage and read failures remain retryable. Other formats larger than 64 MiB
receive `input_too_large` until they have a bounded parser.

## Generations and reads

An extractor fingerprint identifies the complete local parser bundle. A parser
change creates a new immutable generation and moves the active head for that
content SHA-256. Old generations remain evidence. Retrying the same generation
is idempotent. Because the fingerprint covers every parser, any change to it
makes the daemon re-read and re-extract every retained original once in every
vault. Moving those heads also rebuilds the affected document-event generations.
Tests pin the parser descriptor and check that the shared email recipe,
including the actual Go version, contributes to the fingerprint. Format
[qualification](format-coverage.md#keep-the-record-current) uses a separate
implementation identity that excludes only the Go version.

The active head follows the last successful publication, including publication
of an already-recorded generation. Fingerprints identify parser bundles. They
do not order extractors by age. After switching binaries, an explicit ensure
reactivates the running binary's evidence even if another extractor published
more recently. The first ensure re-reads and re-extracts the original. Subsequent
ensures reuse that active generation. The daemon backfill only processes
originals missing a generation for its fingerprint, so it does not reactivate
already-recorded evidence by itself.

A generation with at least one camera, lens, exposure, dimension,
capture-time, orientation, or GPS claim also gets one indexed photo technical
projection row that maps those claims into nullable typed columns. Generations
without any of them, such as PDFs and email, get no row. The store records the
`photo-technical/v2` recipe that built its rows in a one-row state table,
separately from extractor fingerprints, because the mapping and embedded map
can change without changing the source evidence. When the recipe changes, the
next store open re-projects every generation from its retained canonical JSON,
without reading originals. Reads bind the
projection to the requested content version through its blob hash and active
source head, then validate the canonical source checksum. This keeps historical
version reads exact and lets duplicate versions share one generation row.

The projection also stores capture sort keys, local calendar dates and
case-folded camera/lens labels, so browsing does not re-parse source evidence.
These derived columns stay out of metadata JSONL and are rebuilt on restore.

Capture timestamps retain normalized text, raw text, precision, timezone kind,
and offset. A date-only value keeps its omitted timezone. The GPS adapter
accepts only a finite, bounded latitude and longitude pair and keeps those
coordinates when the offline Natural Earth resolver returns no label. Labels
are coarse country, region, and nearby-city text. A region names the label
only when a point inside the region lies in the photo's country, so overlapping
simplified borders cannot pair one country with a neighbor's region. A city
within 25 km names the label only when the map's country and region borders
place it in the same country and region as the photo, so a city in a gap
between simplified borders is never used. Ocean coordinates and border gaps
remain unlabeled.

Projection rows stay out of metadata JSONL and backups. Restore rebuilds them
from the restored source generations' canonical evidence. No original blob
read is needed.

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

## Standalone photo sidecars

The source extractor reads standalone XMP packets rooted at `x:xmpmeta` through
the strict photo packet reader. Other XML, including bare `rdf:RDF`, RSS, and SVG,
falls through to the existing format checks. Valid packets publish `image.xmp.packet_valid` and supported
rating, flag, label, caption, creator, copyright, and rotation claims. Custom color
labels and invalid properties are omitted. Unsupported rotations are omitted. Present empty or whitespace-only caption, creator, and copyright properties emit empty claims and confirm a clear; absent properties stay unconfirmed. Meaningful text retains its whitespace. Malformed, oversized, or
invalid packets publish warnings without a valid-packet fact. The tolerant
embedded XMP reader retains its existing behavior.

Photo initialization consumes checksum-checked evidence from the running extractor
fingerprint. It rechecks the sidecar version, bytes identity, membership, role,
trash state, and target revision in its transaction. Packets with no supported decisions and rejected packets make no owner edit; replacement bytes can initialize a revision-1 target.
Schema 32 caches considered undecided or rejected packets in `photo_sidecar_considered`,
keyed by sidecar file, target file, content version, and extractor fingerprint.
Idle listings use its primary-key index and skip canonical JSON parsing.
Logical restore rebuilds this derived cache from retained source evidence.
Applied decisions write the ordinary authored receipt with stable sidecar file,
node, and content-version provenance. Later pairing corrections preserve that
historical identity. Asset reads and initialization listings use source evidence
rather than scanning owner receipts.
