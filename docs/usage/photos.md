---
last_edited: 2026-09-27
title: Photo Assets
description: Group ordinary file nodes into revisioned photo assets.
---

# Photo Assets

Docbank groups ordinary file nodes into photo assets. The node and its
immutable content versions remain the byte authority. An asset stores only
membership, roles, display selection, exclusion, and bounded decision
receipts.

Image files and files with a concrete `video/*` MIME type are enrolled when
they are created. Audio, generic video, and generic RAW files stay ordinary
files until an operator promotes or creates an asset explicitly. Enrollment
is forward-only; adding this feature does not scan older files.

Replacing or reverting a file's content keeps the file in its asset with the
same role, even when the new media type would not qualify. A file whose new
content qualifies is not enrolled either. Membership changes only through
explicit asset operations or permanent node deletion.

An asset can contain `raw`, `image`, `video`, and `sidecar` members. The
default display order is RAW, image, then video. A vault preference can select
image before RAW, and an asset override wins over the vault preference.
Sidecars never display and must point at a RAW or image member in the same
asset.
Removing the selected member chooses another displayable member atomically,
or stores a null display when none remains. Assets are limited to 256 files.

## Previews

The daemon produces a grid preview with a 512-pixel maximum edge for each
included photo's selected display file. It discovers new imports continuously
and resumes missing work after restart. Completed results stay retained.

Embedded applications can request fit previews at 2560 pixels or large
previews at 4096 pixels. All sizes preserve aspect ratio and never upscale.
JPEG, PNG, GIF, still WebP, and supported embedded JPEGs in ARW, DNG, CR2,
NEF, and RAF files have decoder paths. Format support does not guarantee that
every individual file decodes. Unsupported and deterministic decode failures
are retained terminal results; temporary storage or read failures retry.

## CLI

Inspect the asset created for a node or use a stable asset UUID:

```text
docbank photos assets inspect <asset-id|node-selector>
docbank photos assets create <node-selector> [--kind photo|video] [--role raw|image|video]
docbank photos assets attach <asset-id> <node-selector> [--revision REV] [--role ROLE] [--sidecar-of-file-id ID]
docbank photos assets detach <asset-id> <file-id> [--revision REV]
docbank photos assets exclude <asset-id> [--revision REV] [--excluded=true]
docbank photos assets promote <node-selector> [--revision REV] [--kind KIND] [--role ROLE]
docbank photos assets display <asset-id> [file-id] [--revision REV]
```

`inspect`, `create`, and `promote` accept absolute virtual paths or `id:N`
node selectors, so `inspect id:42` finds the asset that owns file 42, even
after file 42 is trashed.
Existing-asset operations read the asset's current revision, send it, and
retry once if another write changes the asset first. Pass `--revision` with
the revision from your last inspection when a script needs the write to fail
instead. Omitting the file ID from `display` clears the asset override.

The vault preference is revisioned separately:

```text
docbank photos settings show
docbank photos settings set raw [--revision REV]
docbank photos settings set image [--revision REV]
docbank photos settings reset [--revision REV]
```

All commands emit bounded JSON. Exit code 4 means the revision is stale: an
explicit `--revision` no longer matched, or the one automatic retry lost to
another write. Read the asset or settings again before retrying. The daemon performs role,
ownership, sidecar, display, and audit checks.

## Import a camera folder

`photos import` reads a folder on the daemon host and imports its photos into a
vault folder:

```text
docbank photos import <source-root> [destination] [--json]
docbank jobs show <operation-id> --json
docbank jobs cancel <operation-id>
```

`photos import` queues the import as a background job and prints its operation
ID. Follow and stop it like any other durable job: `jobs show` reports progress
and the import receipt, and `jobs cancel` stops it.

The import reads RAW files (`.ARW`, `.CR2`, `.CR3`, `.DNG`, `.NEF`, `.ORF`,
`.RAF`, `.RW2`), images (`.JPG`, `.JPEG`, `.PNG`, `.GIF`, `.WEBP`, `.HEIC`),
videos (`.MP4`, `.MOV`, `.M4V`, `.AVI`, `.MPG`), and `.XMP` sidecars. It leaves
every other file out and counts it as `unsupported` in the receipt.
The source folder must exist when you start the import; otherwise the command
fails with exit code 2 and no import starts.

Files with the same folder and name, such as `IMG_0001.ARW`, `IMG_0001.JPG`,
and `IMG_0001.XMP`, become one photo. The XMP sidecar attaches to the RAW, or
to the JPEG when there is no RAW. An XMP with no same-name RAW or JPEG imports
as a plain file. A later full scan pairs companions when both source files
are still present. Matching uses files in the scan and their current duplicate
owners. A video always becomes its own photo. Each photo commits in one
transaction, so a failure leaves no half photo behind, and running the same
import again skips content already in the vault. A file edited since it was
imported, such as an XMP saved again by a photo editor, becomes a new version
of the file already in the photo, not a second file.

The import leaves a group unpaired and lists it under `ambiguities` in the
receipt from `jobs show <operation-id> --json` when:

- two RAW files share a name, such as `IMG_0001.ARW` and `IMG_0001.DNG`: each
  RAW and JPEG becomes its own photo and the sidecar stays a plain file;
- same-name files already sit in separate photos: nothing is merged.

Pair the RAW and JPEG files yourself: `photos assets inspect <asset-id>` shows
file IDs, `photos assets detach` frees a file, and `photos assets attach` adds
it to the other photo. A later scan containing the sidecar and its image
attaches it to the settled photo. The final receipt lists every group that
remained ambiguous during that run, with each file's source path, node ID,
role, and photo asset ID.

The import hashes the discovered files, waits one second, then compares each
durable copy with that observation. It hashes every group member again before
committing. A changed or deleted source leaves the entire group out and counts
as changed, even if its size and modification time stayed the same. Import
again to pick it up. The web Jobs drawer shows progress and has a Cancel
button while the import runs; it leaves out source paths and the unpaired-group
list. Cancel takes effect before the next group. A daemon restart resumes an unfinished import by
scanning the folder again.

## HTTP and JSONL

The daemon exposes asset inspection by asset ID or node ID, plus create,
attach, detach, exclude, promote, display, and settings operations under
`/api/v1/photos`. Existing-asset and settings mutations require `If-Match`.
Responses carry the new revision in both the body and the `ETag` header.

Photo assets, file memberships, the singleton settings row, and change
receipts are part of the deterministic metadata JSONL stream. Restore checks
node ownership, local pointers, sidecar targets, display selection, enum
values, revisions, receipt JSON, and the complete graph before commit. Older
supported metadata streams restore an empty photo authority.

Email children are identified by `email_document_relations.child_version_id`.
An image produced by processing remains eligible when it is not an email
child. Existing graphs survive ordinary trash and restore; permanent node
deletion removes memberships and repairs the affected asset while preserving
an empty asset identity.

Automatic enrollment and explicit graph writes are skipped or refused when
audit authority is active, according to the existing audit boundary. The
preexisting graph is preserved and becomes read-only when audit is enabled.
## Browse photo assets over HTTP

`POST /api/v1/photos/assets/query` accepts a `query` object using [QueryV1](../architecture/http-api.md#saved-query-and-highlight-definitions), optional `coverage`, `page_size` from 1 through 250 and `cursor`. It returns `items`, the complete matching asset `total` and an optional `next_cursor`. The default page size is 50. Send the same intent and page options with each continuation. An edited saved query invalidates its earlier cursor; cursors expire after 15 minutes. Ordinary file changes follow live ordering and can move across the previous page boundary.

Use `kind:photo`, `camera:"Synthetic Camera"`, `lens:"Synthetic Lens"`, `iso:400`, `iso_min:100`, `iso_max:800`, `capture_after:2024-01-01`, `capture_before:2025-01-01`, `gps:"-10,170,10,-170"` or `asset:` followed by a canonical UUIDv4. Existing `collection:`, tags and text predicates combine with these fields. Camera and lens match either their exact make or exact model. Typed filter arrays OR their values; separate filters AND together. Dates use YYYY-MM-DD with an inclusive lower bound and exclusive upper bound. GPS uses inclusive decimal-string latitude/longitude bounds and permits boxes crossing the antimeridian.

One member must satisfy the whole query. If a RAW has camera A and its paired JPEG has lens B, `camera:A AND lens:B` requires one member with both facts. `camera:A OR lens:B` matches the pair once. `NOT camera:A` can match through the JPEG without A. Saved expressions follow the same rule. Sidecars can match ordinary predicates, while the display always comes from the chosen image, RAW or video file. Excluded, trashed and displayless assets stay out.

Sort by `capture_time`, `import_time`, `name`, `modified_at`, `size` or `media_type`, with `asc` or `desc`. Capture sorting retains explicit offsets and omitted-zone evidence; omitted zones use civil calendar coordinates. Missing or unreadable capture times sort last in both directions and do not match capture-date filters. Asset UUID orders equal keys. `path` and `relevance` are unsupported by this route. Document snapshots reject `capture_time` and `import_time` with an error naming Photos as the supported view.

Each row has grid, fit and large preview slots. `missing` means no retained result exists for that recipe. Stored `ready`, `unsupported` and `failed` outcomes retain their generation identity. Only ready slots include a generation URL and JPEG output metadata. Read that URL with the browser session header; credentials stay out of URLs. The read verifies complete bytes and current display eligibility, and returns `private, no-store`. A later display replacement or exclusion can make an earlier URL unavailable. Listing never creates previews.
