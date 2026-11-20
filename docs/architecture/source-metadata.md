---
title: Source metadata
description: How Docbank records a limited set of facts found inside original files.
---

# Source metadata

Docbank reads facts found inside original files, such as image dimensions,
camera settings, and PDF properties. Before extracting this **source metadata**,
Docbank verifies the original bytes and stores each result as immutable local
evidence.

That evidence belongs to the content SHA-256. When you read a content version,
Docbank returns the source metadata alongside the version's node and attachment
facts, and moving or renaming the file does not change the extracted evidence.

Source metadata does not change document identity, and it is not user-authored
annotation. Embedded values can be incomplete, incorrect, or sensitive. Treat
them as useful evidence rather than authority over the original bytes.

## Typed fields

Every extracted field records a canonical key, a source namespace, a source
label, a typed value, and a sensitive flag. A **namespace** names the format or
metadata family where the extractor found the field.

The `kind` field selects one payload type: `string`, `string_list`, `integer`,
`number`, `boolean`, or `timestamp`. Timestamps keep the precision and timezone
stated by the source. While the canonical key describes the fact itself,
`source_field` keeps the format-specific label from the original file. For
example, PDF `CreationDate`, EXIF `DateTimeOriginal`, and MP4
`mvhd.CreationTime` all map to the `created` key inside their own namespaces.

When the source file contains them, the current extractor publishes these
fields:

| Namespace | Sources | Canonical keys |
|-----------|---------|------|
| `media.container` | JPEG, PNG, WebP, GIF, MP4, TIFF-family images, RAF, CR3 | `media.container.format`, `.kind`, `.width_px`, `.height_px`, `.frame_count`, `.animated`, `.duration_ms`; `created` for MP4 |
| `image.exif` | JPEG APP1, TIFF-family images and RAW, RAF, CR3 | `image.exif.camera_make`, `.camera_model`, `.body_serial_number`, `.lens_make`, `.lens_model`, `.orientation`, `.iso`, `.exposure_time_seconds`, `.f_number`, `.exposure_bias_ev`, `.focal_length_mm`, `.pixel_width`, `.pixel_height`, `.gps_latitude`, `.gps_longitude`, `.gps_timestamp`; `created`, `modified`, `creators`, `description` |
| `xmp` | XMP packets in PDF | `title`, `creators`, `subject`, `description`, `keywords`, `language`, `created`, `modified` |
| `pdf.info` | PDF Info, page tree, encryption, form fields, attachments, and actions | `title`, `creators`, `subject`, `keywords`, `created`, `modified`, `page_count`, `pdf.info.encrypted`, `.permissions`, `.signature_field_count`, `.last_signature_covers_file`, `.embedded_file_count`, `.has_javascript`, `.has_launch_actions` |
| `office.core` | OOXML core and application properties | `title`, `creators`, `subject`, `description`, `keywords`, `language`, `created`, `modified`, `page_count`, `office.core.word_count` |
| `office.custom` | OOXML custom properties | `office.custom.<normalized_name>` |
| `email` | RFC 5322 messages | `email.from`, `.to`, `.cc`, `.bcc`, `.subject`, `.sent`, `.received`; `attachment_count` |
| `calendar` | iCalendar | `title`, `description`, `creators`, `calendar.start`, `.end`, `.start.raw`, `.end.raw` |
| `media.id3` | ID3 tags | `title`, `creators`, `created`, `media.id3.album` |

In this table, a leading dot adds the namespace prefix. For example,
`.camera_model` means `image.exif.camera_model`.

Docbank publishes latitude and longitude together only when both coordinates
have valid hemisphere markers and valid degree, minute, and second ranges. If a
pair is incomplete, a coordinate is invalid, or the file has a zero-zero
placeholder, the extractor omits the coordinates and records a warning. Complete
EXIF GPS date and time fields combine into one UTC timestamp.

Docbank marks GPS coordinate fields, `image.exif.body_serial_number`, and
`email.bcc` as sensitive. The embedded API returns them because its caller is
trusted application code, while browser-session reads remove sensitive fields.
Other callers must enforce their own disclosure boundary.

## PDF evidence

The v22 extractor fingerprint queues retained originals of every format,
including photos and email, for re-extraction through the existing global
backfill.

You can see these declared facts in the inspector's PDF section, in
`docbank document stat`, and in MCP `get_document`. MCP returns them in
`pdf_facts` and leaves other source metadata outside that response. The
inspector reads the exact content version you selected, including saved results
whose live document has changed.

| Key under `pdf.info.` | Type | Meaning |
| --- | --- | --- |
| `encrypted` | boolean | The original has PDF encryption. |
| `permissions` | string list | Raw permission flags defined by the encryption revision: Print, Modify, Copy text and graphics, Annotate and fill forms, Fill forms, Extract for accessibility, Assemble, and High-quality print flag. None means no declared operations. Unencrypted PDFs omit this field. |
| `signature_field_count` | integer | Logical signature fields, including unsigned fields and inherited field types, counted once per field rather than per widget. |
| `last_signature_covers_file` | boolean | The latest declared signed revision's byte range ends at the original file length. |
| `embedded_file_count` | integer | Distinct embedded stream references under reachable file specifications; repeated references count once, while distinct streams with equal bytes count separately. |
| `has_javascript` | boolean | Reachable dictionaries declaring `S: JavaScript` or `S: Rendition` with a `JS` entry. |
| `has_launch_actions` | boolean | Reachable dictionaries declaring `S: Launch` with `F`, `Win`, `Mac`, or `Unix`. |

Docbank looks for embedded streams under `EF` path entries `F`, `UF`, `DOS`,
`Mac`, or `Unix`. The action and file predicates include reachable application
data. They describe what the file declares without claiming that a viewer
executes those actions. For encryption permissions, revision 2 uses only its
defined operation bits, while later grants require revision 3 or above. The
High-quality print flag does not grant permission to print on its own; Print
must also be set. Docbank reports each revision-defined bit independently.

To check whole-file signature coverage, Docbank looks for four nonnegative,
ordered byte-range integers starting at zero, with an endpoint representable as
an integer. Regardless of field order, the greatest endpoint selects the latest
signed revision. If that endpoint falls before or after the current file length,
coverage is false, including appended bytes and smaller rewrites. A valid range
reaching the original file length establishes whole-file coverage even when an
earlier range is malformed. Otherwise, malformed signed values leave coverage
unknown and record a warning, while unsigned fields provide no coverage value.
Docbank does not validate certificates or cryptographic signatures.

Readable encrypted PDFs and PDFs with external references also supply Info and
XMP evidence, and a PDF that opens with the empty password can supply all of
these observations. When a PDF requires a password, Docbank records
`encrypted: true` with a `Password` warning and leaves the other facts unknown.
When encryption is unsupported, Docbank records `encrypted: true` with an
`Encryption` warning and leaves the other facts unknown. If an optional
inspection step fails, Docbank omits the affected facts with warnings while
keeping readable Info, XMP, and page evidence. A value of zero or false means
inspection completed. The existing 64 MiB source-metadata parser limit still
applies. Docbank inspects actions as data and does not execute them, and strict
processing inspection keeps its encryption and external-reference refusals.

When the extractor version changes, the daemon queues existing originals through
its usual metadata backfill. Re-extraction keeps prior evidence generations and
leaves the original bytes unchanged.

PDF search predicates read the active generation for the current content hash.
Both publication and metadata import validate the full typed canonical schema
before storing an immutable generation. At query time, SQL predicates check the
stored contract, byte limit, JSON syntax, and checksum without decoding and
re-encoding every field for each predicate. Unsupported or damaged generations
contribute no PDF fields, and metadata reads still run full canonical
validation. Predicates match the field key, the `pdf.info` namespace, the value
kind, and the typed value. Missing observations match neither false nor zero,
and permissions use exact list membership. CLI `--pdf-filter` and web Query
syntax share these predicates without a projection table or schema change. See
[PDF evidence filters](../usage/searching.md#find-pdfs-by-recorded-evidence).

## Current format boundary

Within the 20 MiB general inspection limit, Docbank extracts container facts
from JPEG, PNG, WebP, GIF, and MP4 files. You get EXIF facts from JPEG APP1
EXIF and TIFF-based images. The TIFF path also handles camera RAW formats that
keep the standard TIFF header, along with the TIFF-derived Olympus ORF and
Panasonic RW2 headers. Fujifilm RAF files supply EXIF facts from their embedded
JPEG and original image dimensions from their RAF raw-metadata directory. Canon
CR3 files supply camera, lens, exposure, orientation, capture-time, GPS, and
image-dimension facts from their Canon TIFF/EXIF metadata boxes. Other
proprietary container headers need their own bounded parser.

The daemon processes source metadata in the background, while embedded callers
choose when to process a version with `EnsureSourceMetadata`. Both paths use the
same parser. During a background scan, the daemon selects each retained original
once with an indexed existence check instead of joining every content version
that shares the same bytes. Historical and trashed versions still retain their
originals, and the scan excludes blobs without a retained version. Work pages
follow SHA-256 order and skip originals that already have evidence for the
requested contract and extractor fingerprint.

Originals up to and including 64 MiB fit the general in-memory read. When a
JPEG, TIFF-family, RAF, CR3, or MP4 file exceeds the 20 MiB general
media-inspection limit, Docbank uses bounded media parsing instead. JPEG and
TIFF-based files read a 20 MiB leading metadata window, and the resulting
generation includes a `metadata_window_limited` warning because metadata after
that window may be omitted.

Before running bounded parsing on a large media file, the worker verifies the
complete content identity and handles each format within fixed bounds:

- **MP4.** The parser scans top-level box headers, skips media payload boxes,
  and reads bounded file-type and movie metadata, including the movie-header
  creation time when present. Malformed or oversized metadata produces a durable
  warning.
- **RAF.** The parser reads the fixed header, a bounded embedded-JPEG metadata
  window, and a bounded raw-metadata directory. Malformed structure produces a
  durable warning.
- **CR3.** The parser walks ISO base media file headers and bounded Canon
  metadata boxes without buffering or decoding the RAW image payload. Malformed
  structure produces a durable warning.

Storage and read failures remain retryable. Originals in other formats larger
than 64 MiB receive `input_too_large` until they have a bounded parser.

## Generations and reads

An extractor fingerprint identifies the complete local parser bundle. When a
parser changes, Docbank creates a new immutable generation and moves the active
head for that content SHA-256, while old generations remain evidence. Retrying
the same generation is idempotent. Because the fingerprint covers every parser,
any change to it makes the daemon re-read and re-extract every retained original
once in every vault, and moving those heads also rebuilds the affected
document-event generations. Tests pin the parser descriptor and check that the
shared email recipe, including the actual Go version, contributes to the
fingerprint. Format
[qualification](format-coverage.md#keep-the-record-current) uses a separate
implementation identity that excludes only the Go version.

The active head follows the last successful publication, even when that
publication is for an already-recorded generation. Fingerprints identify parser
bundles rather than ordering extractors by age. If you switch binaries, an
explicit `EnsureSourceMetadata` call reactivates the running binary's evidence
even when another extractor published more recently. That first call re-reads
and re-extracts the original, and later calls reuse the active generation. By
contrast, the daemon backfill only processes originals missing a generation for
its fingerprint, so it does not reactivate already-recorded evidence on its own.

When a generation has at least one camera, lens, exposure, dimension,
capture-time, orientation, or GPS claim, Docbank also writes one indexed photo
technical projection row that maps those claims into nullable typed columns.
Generations without any of those claims, such as PDFs and email, get no row.
The store records the `photo-technical/v3` recipe that built its rows in a
one-row state table, separate from extractor fingerprints, because the mapping
and embedded map can change without changing the source evidence. When the
recipe changes, the next store open re-projects every generation from its
retained canonical JSON without reading original files. Reads bind the
projection to the content version you request through its blob hash and active
source head, then validate the canonical source checksum. That keeps historical
version reads exact and lets duplicate versions share one generation row.

The projection also stores capture sort keys, local calendar dates, and
case-folded camera and lens labels, so browsing does not re-parse source
evidence. Photos uses a derived FTS5 index to search complete camera and lens
makes and models along with location labels. Only active source heads serve
matches, while retained generations stay available for rebuilds. These
projection rows, columns, and index entries stay out of metadata JSONL and
backups. Restore rebuilds them from the restored source generations' canonical
evidence without reading original blobs.

Capture timestamps keep their normalized text, raw text, precision, timezone
kind, and offset, and a date-only value preserves its omitted timezone. The GPS
adapter accepts only a finite, bounded latitude and longitude pair, and it keeps
those coordinates when the offline Natural Earth resolver returns no label.
Location labels hold coarse country, region, and nearby-city text:

- A region names the label only when a point inside the region lies in the
  photo's country, so overlapping simplified borders cannot pair one country
  with a neighbor's region.
- A city within 25 km names the label only when the map's country and region
  borders place it in the same country and region as the photo, so a city in a
  gap between simplified borders is never used.
- Ocean coordinates and border gaps stay unlabeled.

Both the HTTP node and content-version detail endpoints return the active
generation. In embedded Go applications, call `Vault.EnsureSourceMetadata` with
an immutable content version ID to run the current local extractor synchronously
for those exact bytes, or call `Vault.SourceMetadata` for a read-only lookup.
Calling `Vault.EnsureSourceMetadata` on a generation that is already current is
idempotent and does not start the daemon worker or scan unrelated content.
Because `Vault.EnsureSourceMetadata` shares the vault mutation gate with content
writes and physical maintenance, its exact-version lookup, verified read,
publication, and final result cannot straddle an authority change. If physical
source bytes are missing or corrupt, the call retains the embedded API's
`ErrContentUnavailable` identity. Every read endpoint and API binds fields to
the requested version and keeps filename, path, ingest time, and filesystem time
in separate attachment facts.

## Standalone photo sidecars

The source extractor reads standalone XMP packets rooted at `x:xmpmeta` with the
strict photo packet reader, while other XML such as bare `rdf:RDF`, RSS, and SVG
falls through to the existing format checks. The v21 extractor also reads EXIF
`BodySerialNumber` as `image.exif.body_serial_number` when the source supplies
it. Its fingerprint causes one re-extraction of every retained original and
rebuilds affected document-event generations.

When a packet is valid, the reader publishes `image.xmp.packet_valid` along with
supported rating, flag, label, caption, creator, copyright, rotation, and
`photoshop:SidecarForExtension` claims. It ignores unsupported values while
keeping the other decisions: custom color labels, unknown picks, unsupported
rotations, ratings that are not whole numbers from -1 through 5, and text over
16 KiB. XMP types `xmp:Rating` as a real number, so `3.0` is a three-star
rating. Caption, creator, and copyright text keep their whitespace, and the
reader stops buffering a text property once the text passes 16 KiB. Packets
share the 64 MiB limit on extracted originals, so large editing data does not
hide a rating.

Malformed XML publishes warnings without a valid-packet fact. Packets refused
only by sync, such as files with oversized editing data, still publish their
supported fields under the existing extraction contract. Extraction keeps its
case-sensitive `x-default` selection, while sync matches that language without
regard to letter case and preserves whitespace-only text. The tolerant embedded
XMP reader keeps its existing behavior. The first top-level RDF description
selects the subject; matching descriptions combine its properties, while other
subjects and nested resources do not contribute decisions.

These extracted source facts describe each sidecar version for search and detail
views, and sync-only RDF refusals in keywords or TIFF orientation preserve
unrelated extraction facts. XMP sync does not read those extracted facts.
Instead, the sidecar worker reads the packet bytes with the same strict reader
and also needs the original's extraction head for its orientation. XMP sync
receipts retain settled and outside packet histories with their own source
orientation. Work discovery, filters, and Inspector compare the same live input
token with the latest receipt, and publication rechecks it transactionally. See
[Sync XMP sidecars](../usage/photos.md#sync-xmp-sidecars) for merge rules and
the
[storage design](https://github.com/kenn-io/docbank/blob/main/docs/internal/storage-design.md#photo-asset-authority)
for receipt authority.

Provenance bindings identify each outside source path and its last observed
digest in retained source versions. Stored paths preserve Unicode code-point
spelling and case, use uppercase Windows drive letters and portable drive and
UNC separators, and keep literal backslashes in POSIX paths. Restoring those
bindings and versions on another operating system preserves the source keys and
sidecar pairing.

Checkout sidecars check extension claims against their frozen original entry's
filename, even after you rename the vault file or retire the checkout. Pruning
retains the latest provenance-bound version for each outside source path, so
rescans can compare its digest independently of sync receipts. The settled
version remains until sync can compare both sides and settle any conflict. Every
retained sync history keeps its required versions across sidecar detachment and
reattachment, including moves between originals.
