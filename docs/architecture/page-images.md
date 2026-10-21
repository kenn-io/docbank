---
last_edited: 2026-10-12
title: Verified page images
description: Page geometry and local page rendering for one document version, within fixed limits.
---

# Verified page images

The page API returns physical geometry and verified PNG images for the document
version that a request names. Importing a file does not automatically render
it. This API is separate from text extraction and visual previews.

PDF frames use the effective MediaBox, CropBox, and quarter-turn rotation. Boxes
are integer 1/10,000 points. Visible width and height are integer 1/10,000
inches. A rational transform maps source coordinates to a top-left origin, x
right and y down. Finite PDF coordinates are rounded half away from zero.
Invalid crops, non-default UserUnit, and unsupported precision fail. Page
dimensions are never guessed from a filename or a default paper size.

Still PNGs need one CRC-valid, metre-based pHYs chunk before image data, with
equal nonzero horizontal and vertical density. Animation, EXIF orientation,
missing density, and unequal density are unsupported. PNG rendering retains the
original bytes. Omit DPI or use zero for native density. A different requested
DPI is rejected. Actual numeric DPI is `pixels_per_metre * 127 / 5000`.
PDF requests default to 144 DPI and accept 1–1200 DPI within pixel limits.

## Configure the optional runtime

Rendering requires Linux amd64 and three pinned compiled executables: the
repository-owned inspector, Poppler's `pdftoppm`, and util-linux `prlimit`.
Other platforms, missing executables, and failed qualification leave rendering
unavailable without disabling other Docbank features. Retained frames and
images remain readable without a renderer.

Build the inspector from the same source revision as Docbank:

```sh
go build -tags fts5 -o bin/ ./document/pagerender/cmd/docbank-page-inspect
```

Install all three at immutable regular-file paths, resolve symlinks, calculate
their SHA-256 digests, and configure `config.toml`:

```toml
[page_runtime]
deployment_identity = "my-qualified-page-runtime-v1"

[page_runtime.inspector]
path = "/opt/docbank/page-runtime/docbank-page-inspect"
sha256 = "<64 lowercase hexadecimal characters>"

[page_runtime.renderer]
path = "/opt/docbank/page-runtime/pdftoppm"
sha256 = "<64 lowercase hexadecimal characters>"

[page_runtime.limiter]
path = "/opt/docbank/page-runtime/prlimit"
sha256 = "<64 lowercase hexadecimal characters>"
```

The digests are placeholders, not runnable defaults. Recipes record executable
digests and observed versions. The operator manages shared libraries, fonts,
and color resources. Requalify the deployment and change `deployment_identity`
whenever they change. The identity is an operator declaration. Docbank does not
independently verify the resource content, and the runtime is not a hermetic
bundle. Different output for a retained exact recipe is a conflict, never an
overwrite.

Both PDF inspection and rendering run under pre-exec address-space limits:
2 GiB for the Go inspector, including Go's virtual-memory reservation, and
512 MiB for Poppler. Each phase has 60 seconds. Each job has ten minutes.
These are process resource limits, not filesystem or network sandboxing.

Requests are bounded to 16 explicit pages, documents to 1,000 pages, and source
bytes to 64 MiB. Output is limited to 40 million decoded pixels, 16,384 pixels
per axis, 32 MiB per PNG, and 256 MiB per job. Geometry output has a 16 MiB
cap. Diagnostics have a 64 KiB cap. One worker executes jobs, with at most 64
queued or running. Full decoding, dimensions, size, and SHA-256 are verified.
PDF output may differ by one pixel per axis from the rounded frame at supported
DPI. The receipt records the actual dimensions. PNG dimensions must match exactly.
Successful process exit alone does not establish success.

## API and retained authority

Every request selects a node revision and exact source version, SHA-256, and
size. No operation falls back to the current head.

| Operation | Endpoint |
|-----------|----------|
| Read geometry and available image receipts | `POST /api/v1/pages/inventory` |
| Request explicit pages with an operation UUID | `POST /api/v1/pages/jobs` |
| Read an exact job | `POST /api/v1/pages/jobs/{id}` |
| Cancel an exact job | `POST /api/v1/pages/jobs/{id}/cancel` |
| Read PNG bytes bound to source, page, frame, recipe and image hashes | `GET /api/v1/pages/image` |

The OpenAPI document describes request fields. Inventory returns complete
frame/count authority separately from available images and their actual recipes.
A completed job covers its requested pages, not necessarily the whole document.
Requests for identical work already queued, running, or completed return that
job. Reusing the retained job's operation UUID returns its original result.
Submit a new UUID to retry failed or canceled work. Inventory rejects more than
16,000 retained image receipts in one response.

Frames, recipes, and receipts are immutable. Cancellation, source revision
changes, and obsolete claims block later publication. Cancellation lets the
current render finish but blocks its publication. Restart requeues unfinished
jobs with fresh claims. Partial images remain usable and participate in backup,
restore verification, and garbage-collection reachability.

Page authority remains with its exact content version and is removed when that
version is removed. Attachment/build derivative purge does not purge page
images. There is no separate page-purge API. The PyMuPDF text bridge is
unchanged.

## Native-PDF preparation

`POST /api/v1/redaction/prepare` inspects one exact retained PDF selected by
node ID, node revision and source version/hash/size. The API key is required;
browser sessions are not admitted. The CLI entry point is
[`redaction prepare`](../usage/document-processing.md#prepare-a-native-pdf).
This read creates no jobs, attachments, stored maps or output PDFs.

The processing service reads the live file and its node-owned version in one
snapshot. Historical versions are allowed. It checks the node revision before
comparing the immutable hash and size, then copies and verifies the entire
original under the shared physical-authority gate. Logical replacement or trash
after admission does not revoke that snapshot. The gate is released before
inspection, so maintenance does not wait for rendering.

The configured page runtime inspects the private bytes once. Native PDFium
inspection consumes that complete result, rechecking its source identity,
frames, parser identity and font domains. It does not launch a second
classifier. The original bytes and temporary engine resources are released
before the service gives up its preparation slot.

The response contract is `native-pdf-preparation/v1`: `selection`, ordered
`frames`, `evidence`, `evidence_sha256` and `map`. The evidence contract is
`native-pdf-evidence/v1`, containing the source, page-runtime fingerprint,
qualified renderer digest and native page observations. Observation bounds are
PDF point/10000; map boxes use top-left inch/10000 and name their frame digest.
Text spans count UTF-8 bytes. Text is neither normalized nor trimmed, and page
boundaries add no separators. Semantic `units` is empty.

The map's evidence digest binds this native evidence record. Existing `Align`
callers continue to bind normalized evidence instead. Any future mask admission
must accept the complete preparation envelope and check its evidence contract;
a bare `aligned-text/v1` map cannot identify the meaning of that digest. The
service and client check source, frame, evidence and map bindings. Client checks
establish response consistency, not independent verification of source pixels.

One preparation may run per daemon, with no queue. Shutdown cancels admitted
work and waits for cleanup. Limits are 8 KiB requests, 64 MiB originals, the
entire 1–16-page PDF, 8 MiB retained evidence, a 15 MiB result and a 16 MiB client
wire read. A refusal returns no partial result. The ordinary 60-second request
deadline and qualified renderer's 300 DPI, 512 MiB WASM and 1 GiB process-RSS
limits apply. Evidence size is not a peak-memory bound.

RSS refusals preserve a typed cause with observed and allowed byte counts in
daemon diagnostics. A live request receives 503 `redaction_preparation_memory`;
an internal deadline receives 503 `redaction_preparation_worker_timeout`.
Only the original request context selects 408 cancellation or 504 timeout.
A stopped service or missing runtime returns 503 unavailable; a second request
returns 503 busy. Limit and unsupported-source refusals are 422, stale selection
is 409, and missing or trashed selection is 404. Corrupt originals or invalid
inspector output return 500 integrity; other worker failures return 500 failed.

`TestNativePDFPreparationCLI` imports a synthetic Helvetica PDF and obtains the
map through the real daemon and CLI. A test-only consumer resolves its rectangle,
writes a fresh PDF, then checks sanitized text, a black mask interior and
unchanged retained pixels. `TestPDFRedactionSourcePreparation` separately
qualifies the existing normalized-evidence path, including fractional-position
Courier text. Neither test exposes mask execution as a product operation.

Resolution requires complete gap coverage across the entire document. Every
ordered gap must be fully masked, even away from a selected rectangle. An
unordered gap requires masks covering its whole page. Any uncovered gap rejects
the resolve with `mapping_incomplete` before rendering.

### Recognizing blank standard-font spaces

With the configured Linux amd64 page runtime, native inspection can establish
that whitespace in a qualified Courier, Times or Helvetica program paints
nothing. This avoids false gaps from a neighboring letter's boundary pixels.
It primarily helps programmatic PDFs such as fpdf and ReportLab output;
Word, LaTeX and browser PDFs commonly use other programs or composite encodings.

The proof has two parts:

1. The resource-limited inspector checks every font in the page's effective
   inherited resources, including unused fonts and ExtGState font entries.
   All must be simple Type1, MMType1 or TrueType fonts. It reads these facts
   before pdfcpu validation can repair font dictionaries. Type3, composite/CID,
   unknown or malformed fonts cannot establish this fact.
2. Inside the qualified PDFium runtime, each source text object's actual font
   program must equal one of twelve qualified standard programs byte for byte.
   The inspector enumerates all 256 raw codes between checked anchors on a
   scratch text page, then renders every whitespace-mapped code alone onto a
   transparent bitmap. Both scratch pages have explicit full-page crop boxes
   and checked effective bounds, so inherited source crops cannot hide probe
   ink. Any nonzero alpha is ink. A single inked alias makes that object's
   whitespace unproven, even if the alias is unused on the page.

The child returns `docbank-page-inspect/v2`, `parser_identity: pdfcpu/v0.16.3`
and one required `simple_fonts_only` boolean per page. Native inspection accepts
these facts only for the exact staged source identity and ordered frames.
Rebuild an older inspector and update its configured executable SHA-256 pin;
there is no v1 compatibility path. The changed inspector identity also changes
page-image recipe fingerprints. Font facts are transient and do not change
stored frames, redaction formats or the PDFium asset and recipe.

pdfcpu supplies only the code-domain fact. PDFium supplies the actual program,
Unicode mappings and painted glyphs. The remaining assumption is that both
parsers agree which fonts the page can select and whether they are simple.
Program equality alone is insufficient: a CID font can use the same program
bytes as a standard font while accepting more than 256 codes.

Every glyph in the twelve accepted programs is qualified at the scratch
geometry: 72-point type at (72,144) on a 288-point square page, rendered at
300 DPI to a 1,200-square transparent bitmap. The complete inventory and
requalification command live in `internal/pdfproduction/testdata/README.md`.
The 2,792 nonempty outlines must paint ink; the 24 empty glyphs must not.
This finite-raster proof does not extend to arbitrary fonts.

Each page admits at most 256 batches, 8,192 observations per batch and 2,048
isolated renders. Missing anchors, uncertain attribution and exhausted work
limits leave whitespace to the existing pixel decision. There is no elapsed-time
proof budget that changes the map with machine load. Program acceptance is
limited to 64 KiB, after the bounded WASM instance retrieves the program. The
existing hard runtime deadline and memory limits still apply. Proofs are not
cached across source objects: equal program bytes and Unicode tables can hide
different glyph encodings. Scratch pages are deleted before the existing
source-page visibility checks; the source bytes are never rewritten.

### Remaining refusals

The existing library alignment path uses the pixel decision without a usable
configured runtime. Native preparation requires that runtime. With an
unclassified page or unsupported program, either path uses the pixel decision. Classifier
failure does not create a new blanket refusal. Caller cancellation and PDFium
errors still end inspection. Only complete proofs skip the whitespace pixel
check; all earlier hidden-text, ActualText, crop, generated-character, nested
object and annotation refusals remain.

The pixel path samples the outward-rounded whitespace box without an extra
border. It can still include neighboring ink in Courier, Times and Helvetica
text. Ordinary glyph checks and ambiguity accounting retain their one-pixel
border. Sampling never rounds inward to discard boundary pixels, which could
miss small visible glyphs falsely mapped to whitespace. A proof that observes
an inked whitespace alias adds an ordered `unproven_whitespace` gap, even if
that alias is unused on the source page. Every gap still needs complete masking.

Preparation is an inspection step. It does not apply masks, approve a review,
retain redaction outputs, or provide restart recovery, numbering or delivery.
The saved-PDF qualification uses an upstream PDFium reader only in tests.
