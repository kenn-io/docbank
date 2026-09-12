---
last_edited: 2026-09-11
title: Verified Page Images
description: Exact-version page geometry and bounded local page rendering.
---

# Verified page images

The page API returns physical geometry and verified PNG images for an explicitly
selected document version. Importing a file does not automatically render it.
This API is separate from text extraction and visual previews.

PDF frames use the effective MediaBox, CropBox and quarter-turn rotation. Boxes
are integer 1/10,000 points; visible width and height are integer 1/10,000 inches.
A rational transform maps source coordinates to a top-left origin, x right and
y down. Finite PDF coordinates are rounded half away from zero. Invalid crops,
non-default UserUnit and unsupported precision fail explicitly. Page dimensions
are never guessed from a filename or a default paper size.

Still PNGs need one CRC-valid, metre-based pHYs chunk before image data, with
equal nonzero horizontal and vertical density. Animation, EXIF orientation,
missing density and unequal density are unsupported. PNG rendering retains the
original bytes: omit DPI or use zero for native density; a different requested
DPI is rejected. Actual numeric DPI is `pixels_per_metre * 127 / 5000`.
PDF requests default to 144 DPI and accept 1–1200 DPI within pixel limits.

## Configure the optional runtime

Rendering requires Linux amd64 and three pinned compiled executables: the
repository-owned inspector, Poppler's `pdftoppm`, and util-linux `prlimit`.
Other platforms, missing executables and failed qualification leave rendering
unavailable without disabling other Docbank features. Retained frames and
images remain readable without a renderer.

Build the inspector from the same source revision as Docbank:

```sh
go build -tags fts5 -o docbank-page-inspect ./document/pagerender/cmd/docbank-page-inspect
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
digests and observed versions. Shared libraries, fonts and colour resources
remain operator-managed: requalify the deployment and change
`deployment_identity` whenever they change. This is an operator declaration,
not independently verified resource content or a hermetic runtime bundle.
Different output for a retained exact recipe is a conflict, never an overwrite.

Both PDF inspection and rendering run under pre-exec address-space limits:
2 GiB for the Go inspector, including Go's virtual-memory reservation, and
512 MiB for Poppler. Each phase has 60 seconds; each job has ten minutes.
These are process resource limits, not filesystem or network sandboxing.

Requests are bounded to 16 explicit pages, documents to 1,000 pages, and source
bytes to 64 MiB. Output is limited to 40 million decoded pixels, 16,384 pixels
per axis, 32 MiB per PNG and 256 MiB per job. Geometry output has a 16 MiB cap;
diagnostics have a 64 KiB cap. One worker executes jobs, with at most 64 queued
or running. Full decoding, expected dimensions, size and SHA-256 are verified;
successful process exit alone does not establish success.

## API and retained authority

Every request selects a node revision and exact source version, SHA-256 and
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
Identical retries return the original operation and progress. Inventory rejects
more than 16,000 retained image receipts in one response.

Frames, recipes and receipts are immutable. Cancellation, source revision changes
and obsolete claims fence later publication. Restart requeues unfinished jobs
with fresh claims. Partial images remain usable and participate in backup,
restore verification and garbage-collection reachability.

Page authority remains with its exact content version and is removed when that
version is removed. Attachment/build derivative purge does not purge page images;
there is no separate page-purge API. The PyMuPDF text bridge is unchanged.
