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
docbank photos imports show <import-id> [--json]
docbank photos imports cancel <import-id> [--json]
```

Files with the same folder and name, such as `IMG_0001.ARW`, `IMG_0001.JPG`,
and `IMG_0001.XMP`, become one photo. The XMP sidecar attaches to the RAW, or
to the JPEG when there is no RAW. An XMP with no same-name RAW or JPEG imports
as a plain file, and joins the photo once an import finds its image. A file
that arrives in a later import joins the photo already made from its same-name
files. A video always becomes its own photo. Each photo commits in one
transaction, so a failure leaves no half photo behind, and running the same
import again skips content already in the vault.

The import leaves a group unpaired and lists it in `imports show` when:

- two RAW files share a name, such as `IMG_0001.ARW` and `IMG_0001.DNG`: each
  RAW and JPEG becomes its own photo and the sidecar stays a plain file;
- same-name files already sit in separate photos: nothing is merged.

Pair the RAW and JPEG files yourself: `photos assets inspect <asset-id>` shows
file IDs, `photos assets detach` frees a file, and `photos assets attach` adds
it to the other photo. The sidecar follows on the next import, and the paired
group drops off the list.
`imports show` lists the first 100 groups and says how many more there are.

The import waits one second after listing the folder. A group whose files
change after that is left out and counted as changed; import again to pick it
up. Progress and cancel appear in the web Jobs drawer. Cancel takes effect
before the next group. A daemon restart resumes an unfinished import by
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
Browsing and query predicates, technical photo metadata, owners, and browser
UI belong to later slices.
