---
last_edited: 2026-10-31
title: Verified export bundles
description: Download document versions, verified email PDFs, and attachment sets as verified ZIP bundles.
---

# Verified export bundles

You can use the web app or the authenticated HTTP API to export document
versions, retained email PDFs, rendered photos, attachment originals, Markdown
text, and page images. From the CLI, use `docbank export` for originals or
rendered photos, and use the
[native MCP export tools](mcp.md#native-export-jobs) to select original files
by exact document-version identity.

Before writing an archive, Docbank freezes your selection and role receipts.
Later edits to a saved query, tag, document head, or processing result do not
alter an admitted plan.

In the web app, choose **Export selection** for selected documents. To export a
whole frozen query, open the **Export** menu and choose **Download files…**.
After a mailbox import finishes and imports at least one message, you can also
choose **Export these messages**. Review the frozen counts, start the export,
and download the verified ZIP when it finishes. Closing the drawer does not
cancel an admitted job; reopen it in the same browser session to reconnect.
Original files keep their exact original bytes, and the bundle does not redact
or sanitize them.

## Export original files from the CLI

Create a JSON request with two UUIDv4 operation IDs of your choice and 1 to
1,000 document versions, pulling each document's node ID, version ID, SHA-256,
and size from its receipts. Here is a synthetic example that selects one empty
original:

```json
{
  "source_operation_id": "11111111-1111-4111-8111-111111111111",
  "plan_operation_id": "22222222-2222-4222-8222-222222222222",
  "members": [{
    "node_id": 12,
    "version_id": "33333333-3333-4333-8333-333333333333",
    "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "size": 0
  }]
}
```

Replace those synthetic values with your own receipts, save the file as
`selection.json`, and run:

```sh
docbank export preview --request selection.json --json
docbank export start <plan-id> --fingerprint <fingerprint> --operation-id <job-id> --json
docbank export status <job-id> --json
docbank export download <job-id> ./originals.zip
docbank export release <job-id>
```

Review the preview output before starting the job. Preview freezes your
selection, reports the planned original and metadata byte totals, and returns a
plan ID and fingerprint. Pass both values to `start` along with a new job
UUIDv4. The `start` command returns right away, and you can check progress with
`status`. You can only run `download` once the job reaches `completed`. Run
`cancel <job-id>` to request cancellation of active work; exiting the CLI does
not cancel a running job.

Every export command accepts `--json`. Preview returns the plan, `start` and
`status` return the job, and `download` returns the verified archive receipt
after saving the file. Cancel returns `{"job_id":"…","accepted":true}`, and
release returns `{"job_id":"…","released":true}`. Checking the status of a
failed job still succeeds, and you can inspect its `state` and `failure` fields
to see what went wrong.

The request file can be up to 1 MiB. The CLI checks for unknown fields and
invalid values as usage errors before contacting the daemon. Operation and
version IDs must be canonical UUIDv4 strings, hashes must be lowercase
hexadecimal, node IDs must be positive, and sizes must be nonnegative. You can
include an optional nonnegative `revision` to check the node revision; omitting
it or setting it to `0` skips the precondition. You can include multiple
distinct retained versions of the same node, though duplicate node and version
pairs are rejected.

The daemon checks the complete identity and size of every member and never
substitutes the current version. Names and paths freeze at preview time. Only
the originals you select are exported, without adding descendants, attachments,
or renditions.

You can select a published attachment explicitly as an ordinary original;
leaving it out omits its separate file and manifest row. An exported original
email still contains every MIME part in its raw bytes, and selecting only the
parent message does not redact its attachments.

Save your request and operation IDs in case you need to retry. If a response is
lost, repeat the exact same request; sending changed input under an existing ID
conflicts. After a long interruption, check the known job ID or repeat `start`
instead of replaying `preview`. The source's ten-minute replay window can
expire while the job itself stays available.

When you run `download`, the CLI streams the archive to a private staging file
next to your destination, verifies the ZIP against the job fingerprint and
receipts, and then publishes the complete file. The parent directory must
already exist and sit outside Docbank's data directory. An existing file at the
destination stays untouched unless you pass `--overwrite`, and replacement
happens only after verification passes. Failures before publication leave an
existing file intact, while an error after publication tells you the verified
file is already saved. Downloading does not automatically release the retained
job.

## Export photos

In **Photos**, open the **⋯** menu and choose **Export all N photos** to export
the current scope, or choose **Export selection** for selected photos. Select
**Download ZIP** to freeze the plan, build the verified archive, and download
it in your browser.

Preparing a new photo export captures the current membership, and retries or
changes to output settings reuse that frozen selection. If an export job has
already started, closing the drawer leaves it running.

### Output settings

- Choose JPEG or PNG, set JPEG quality from 1 to 100, and optionally enter a
  long edge in pixels. Leaving the long edge blank keeps the original size, and
  exports never enlarge an image. JPEG fills transparent areas with a white
  background. PNG exports use 8-bit RGB, or 8-bit grayscale when a Gray ICC
  profile is retained.
- Source orientation and your rotation are applied once, setting the exported
  orientation to 1. XMP orientation and dimensions become plain values matching
  the rendered pixels, and their original qualifiers are omitted.
- **Include image metadata** keeps source EXIF and unrelated XMP, then applies
  confirmed ratings, flags, labels, captions, creators, copyright, and assigned
  keywords. Supported EXIF and XMP credits stay intact, and confirmed empty
  values clear them.
- If the display file has at least one catalog tag, those current catalog tags
  replace embedded keywords, while tags on companion files are excluded.
  Otherwise, Docbank combines embedded XMP subjects and IPTC keywords into XMP
  without duplicates. Qualified or structured XMP subjects that cannot be
  combined safely stop preparation; to export that photo, add catalog tags or
  turn image metadata off.
- **Remove GPS** is checked by default. It strips GPS coordinates and related
  GPS fields while keeping descriptive location names intact. Turning image
  metadata off strips EXIF and XMP from every delivered file, making the GPS
  setting irrelevant.
- Color profiles stay attached to preserve appearance without conversion.

The metadata options control embedded metadata in rendered photos. Bundle
manifests still keep vault paths, original filenames, and source identities,
including SHA-256 hashes. Inside the ZIP, rendered files live at
`documents/<node>/<version>/photo.jpg` or `photo.png`. In `bundle.json`, each
document's `name` and `path` point to the original, while `roles[].path` points
to the rendered file. Older versions of Docbank reject photo bundles, and you
need to upgrade Docbank to read them.

### Supported sources and metadata

- Supported raster formats are JPEG, PNG, GIF, and static WebP. GIF exports
  render the first frame on its full canvas and read its ICC profile and XMP
  application extensions like other formats.
- ARW, DNG, CR2, NEF, and RAF display members export from their embedded JPEG
  previews. The drawer shows how many previews are used, and the manifest
  labels each member. RAW exports try previews from largest to smallest and
  fall back to the next usable one. A RAW file without a supported embedded
  preview stops preparation.
- HEIC, CR3, ORF, RW2, video, APNG, and animated WebP are not supported.
- When image metadata is enabled, EXIF and XMP packets are limited to 1 MiB
  each, and JPEG APP1 payloads are limited to 65,533 bytes. A retained RAW EXIF
  value over 1 MiB stops preparation, while omitted values of any size do not.
  If retained metadata exceeds the JPEG segment limit, preparation names the
  photo and stops; switch to PNG or turn image metadata off to export it.
- Docbank omits JPEG APP13, TIFF IPTC-NAA/IPTC-IIM and Photoshop resources,
  EXIF MakerNote, source pixel layout, DNG/RAW calibration, color matrices,
  camera profiles, raw digests and embedded original raw data, and embedded
  thumbnails. DNG camera model, serial number, lens, and original file name
  stay intact, and IPTC keywords copy into XMP before those containers are
  dropped. XMP thumbnails, Camera Raw crop settings, and container or gain-map
  references are also omitted.
- IPTC keywords support ASCII without a charset declaration, declared UTF-8,
  and declared Latin-1. Malformed records, unsupported encodings, and
  characters that XML cannot represent stop preparation only when embedded
  keywords are retained; turn image metadata off to export that photo.
- Extended XMP stops preparation when metadata is enabled. Turn image metadata
  off to export that photo.
- Authored values are written to the photo's XMP description, which is the
  first top-level `rdf:Description`, while descriptions of other subjects keep
  their values. An `rdf:ID` subject stops preparation when metadata is enabled.
  Changing only the rating keeps an embedded reject flag intact, even when the
  source rating has XMP qualifiers and GPS is retained.
- A valid non-identity XMP orientation that disagrees with the source
  orientation stops preparation when metadata is enabled, including when the
  orientation has XMP qualifiers. Turn image metadata off to export the viewed
  pixels.
- ICC profiles must use RGB and fit within 1 MiB. A Gray profile is accepted on
  grayscale pixels without transparency, and those photos export as grayscale
  JPEG or PNG. RAW exports keep the chosen embedded JPEG's profile; if that
  JPEG has none, an unassociated TIFF profile stops preparation. PNG cICP color
  declarations are not supported, even when image metadata is off.

### Preparation limits and recovery

- Each preparation allows up to 16 photos, 512 MiB of source bytes, and 1 GiB
  of rendered copies within a five-minute deadline. Sources over 512 MiB and
  images over 100 million pixels stop preparation. JPEG output allows at most
  65,535 pixels per side; for wider images, set a smaller long edge or choose
  PNG.
- Exporting full-size 100-million-pixel images can use several GiB of memory
  for source bytes, decoded pixels, transforms, and encoding. The 1 GiB cap
  applies to encoded output.
- Preparation sets aside one minute of its five-minute deadline for
  publication. Cancellation or hitting the rendering deadline interrupts pixel
  transforms and encoding, even after decoding finishes.
- A changed or unavailable photo, an unsupported color profile, or malformed
  metadata stops the entire preparation and names the offending photo in the
  error. A source that fails integrity verification also stops preparation; run
  `docbank vault verify` to check your vault. A source read failure such as a
  permissions error returns a server error instead. To work around a problem
  photo, switch to **Export selection** and select photos without it, or remove
  it from your existing selection and retry. For an unsupported color profile,
  export the untouched original using the document export flow.

When needed, the CLI prompts for your Hidden photos passcode and keeps the
unlock cookie only for that command. For release behavior and export limits,
see [Free a finished job slot](#free-a-finished-job-slot).

To export photos from the CLI, add `photo_render` to an exact-member request,
or replace `members` with a `photos` selection. JPEG requires a `quality` from
1 to 100. PNG accepts an omitted or zero `quality`, and any supplied value from
0 to 100 becomes 0 before plan identity and rendering. With `include_metadata: false`,
`remove_gps` becomes `false` before plan identity and rendering.
The example below exports the entire current scope, and you can add `asset_ids`
to restrict it to selected display members:

```json
{
  "source_operation_id": "11111111-1111-4111-8111-111111111111",
  "plan_operation_id": "22222222-2222-4222-8222-222222222222",
  "photos": {
    "query": {"v": 1, "syntax": "advanced", "mode": "lexical", "text": "", "filters": {}, "sort": {"field": "name", "direction": "asc"}},
    "hidden": false
  },
  "photo_render": {"format": "png", "long_edge": 2048, "include_metadata": true, "remove_gps": true}
}
```

Run the same `preview`, `start`, `status`, `download`, and `release` commands
shown earlier. Over HTTP, the source request uses `kind: "photos"` with the
same `photos` object, and the plan request uses
`roles: [{"role": "photo_rendered"}]` alongside `photo_render`. Preparation
freezes the display version, decisions, keywords, and revisions, and any change
during rendering requires a fresh preview. Generated copies stay retained for
the export.

## Inspect a retained plan

Keep the plan ID returned by `preview`. You can read its saved header and any
unavailable outputs without creating another preview:

```sh
docbank export show-plan <plan-id> --json
docbank export problems <plan-id> --json
docbank export problems <plan-id> --after 50 --json
```

`show-plan --json` returns the complete saved plan. In human-readable output,
`expires_at` appears as the **admission deadline**. A retained job can keep the
plan header readable after that deadline passes, though starting a new job from
it is refused. Reading a plan does not extend its admission deadline or
retention window.

CLI, MCP, and other API-key clients share one owner and cannot inspect plans
created by a browser session. Asking for an unknown or removed plan returns not
found, and an expired retained record returns `export_expired`. To run a new
export, create a fresh preview with new operation IDs, which captures a new
selection rather than the old snapshot. To check an existing job, use `status`.

`problems --json` returns `plan_id`, `fingerprint`, `after`, `next`, `total`,
and `items`. Each item lists the node, version, role, reason, and an optional
attachment `part_path`. Each call returns one page. Pass the returned nonzero `next` value
to `--after` to fetch the following page; `0` means you have reached the last
page. Human-readable output prints the next command for you. Offsets range from
0 to 2,600,000. An offset equal to `total` returns an empty final page, and an
offset past `total` causes a conflict.

These details reflect the frozen plan even if the source documents change
afterward. Totals count individual problems rather than documents, with a
separate entry for each requested attachment role whose inventory is
unavailable. Originals-only CLI and MCP previews normally have no unavailable
outputs, though richer plans created under the same API-key owner can have
them.

Each page holds at most 50 items and must fit within 64 KiB of encoded JSON. If
a page exceeds that size, the request fails with `export_limit` instead of
returning partial data. The HTTP problems route enforces the same limit and has
no option for a smaller page, so switching clients or releasing jobs cannot
make that page readable.

## Free a finished job slot

All CLI calls and other API-key clients share two retained job slots. Once two
completed exports occupy those slots, starting another returns `export_limit`
until you release a job or wait for it to expire and be cleaned up. Without an
explicit release, completed jobs stay retained for 24 hours.

After you save an archive or decide you no longer need it, run
`docbank export release <job-id>`, or call `DELETE /api/v1/exports/jobs/{id}`
through the API. Release works on completed, failed, or canceled jobs and
returns HTTP 204. It deletes that job's retained archive and status, frees its
slot, and leaves your local downloads and source documents untouched. If a job
is still active, cancel it first.

An active download or an unused download ticket blocks release with
`export_retained`. Unused tickets expire after two minutes, and release never
interrupts a download in progress. Even running `download && release` can
briefly return `export_retained` while the server finishes closing the download
lease; wait a moment and retry release. If release is interrupted after
removing the archive, `download` returns `410 export_expired` with a hint to
retry release and free the slot. If a release call fails, retry it. If the
response is lost, a not-found status confirms the job is gone, and repeating
release returns not found. Do not reuse a released job's operation ID. Its
replay protection has been removed.

Releasing a job does not remove its source and plan records before their
original admission deadlines or while another job retains them. The global cap
of 32 combined source and plan records still applies, and 16 fresh previews
within ten minutes can fill it even if you release every job. Check the limits
later on this page when scheduling batches.

## Email PDFs and attachments

To include email PDFs, choose **Include retained body PDFs**, find the retained
recipes, and pick your paper size and runtime recipe. Generate any missing PDFs
through the [email PDF workflow](email-pdf.md) before you export. Batch export
uses retained outputs and neither starts rendering nor silently substitutes
another recipe.

You can pick from three email attachment options:

| Choice | Downloaded outputs |
| --- | --- |
| Body PDF only | One separate PDF for each available selected message. |
| Include original attachments | Body PDFs and unchanged original attachment files. |
| Originals + separate qualified PDFs | Body PDFs, attachment originals, and separately retained PDFs for supported children. |

Only nested EML children with a qualified retained email-PDF receipt currently
support the separate-PDF option. Other attachment formats stay as originals
with declared unavailable PDF entries. Docbank never relabels an attached
original PDF as a rendered derivative, and it never appends attachment contents
to a body PDF.

By default, planning stops if any requested output or attachment inventory is
unavailable. Turn on **Allow declared unavailable outputs** to permit a partial
export, and review its unavailable-item pages before starting. A complete empty
inventory is valid, while a missing or partial inventory is not treated as
empty. Known occurrences stay in the manifest even when their requested output
is unavailable. The summary counts break out messages, attachment occurrences,
actual PDF files and pages, shared outputs, and unavailable outputs or
inventories.

By default, every selected occurrence gets its own output file. Turn on
**Share exact duplicate outputs** to keep every source and attachment receipt
while pointing identical outputs at an earlier file. To be shared, two outputs
must have equal original message bytes, the same attachment position, and equal
output bytes and recipes. Matching subjects or Message-ID values alone do not
establish duplicates, and ID-based output paths keep repeated subjects
separate.

## Create and download a bundle

1. Call `POST /api/v1/exports/sources` with an `operation_id` UUID and exactly one
   source kind: `explicit`, `nodes`, `query`, `saved_query`, `snapshot`,
   `mailbox_collection`, `upload`, or `photos`. The `photos` kind takes the
   selection shown earlier and allows at most 16 photos. Explicit members
   include `node_id`, `version_id`, `sha256`, and `size`, with an optional
   `revision` precondition. You can include distinct historical versions of the
   same document.
2. Call `POST /api/v1/exports/plans` with a new `operation_id`, the returned
   `source_id` and `member_hash`, and `roles`. Each role policy picks
   `original`, `text`, `pages`, `email_pdf`, `attachment_original`,
   `attachment_pdf`, or `photo_rendered`. The `photo_rendered` role requires
   `photo_render`, cannot be combined with other roles, and stops the entire
   preparation if any photo is unavailable. Missing outputs for other requested
   roles fail planning unless that policy sets `allow_unavailable: true`.
3. Call `POST /api/v1/exports/jobs` with a new `operation_id`, `plan_id`, and
   the plan's `fingerprint` to queue a durable job.
4. Poll `GET /api/v1/exports/jobs/{id}` or subscribe to its `/events` NDJSON
   stream. Each stream record delivers the current state with a monotonic
   sequence rather than replaying missed events, and a stream stays open for at
   most 30 seconds. Reconnect with `?after=<sequence>`. Only a `completed` job
   carries an archive receipt.
5. Call `POST /api/v1/exports/jobs/{id}/download` with `{}` or a safe
   `basename`. Docbank rechecks the actual archive and returns a two-minute,
   one-use download URL and receipt. Verify the complete downloaded ZIP against the
   receipt's SHA-256 and size.

If a response is lost, repeat the exact same request with the same operation
UUID. Sending different input under an existing UUID conflicts. An interrupted
source resolution cannot silently rerun a changed query; start a new operation
instead. To cancel active work, send `{}` to
`POST /api/v1/exports/jobs/{id}/cancel`. None of these endpoints accept a
server destination path.

Source-specific parameters:

- **Saved query.** Send `saved_query_id` and `saved_query_revision`.
- **Snapshot.** Send `snapshot_id` and `member_hash` before the snapshot cache
  expires. Revisions observed in a snapshot do not act as implicit revision
  preconditions.
- **Mailbox collection.** For `mailbox_collection`, send `collection_id`.
  Docbank accepts only a completed import, and membership comes from its
  imported receipt targets rather than the live collection or later document
  heads.

Both `query` and `saved_query` sources use the same frozen population service
as workspace queries.

Role-specific behavior:

- **Text and pages.** Text roles export retained, verified sanitized Markdown
  and record the actual processing profile, attachment, and build. Set
  `profile_fingerprint` to pick a specific profile. Page roles select one
  complete retained page-image recipe, and the export includes its physical
  frames and the recipe itself. Set `recipe_sha256` to pin the selected recipe.
  Export never starts rendering. If you omit a selector, Docbank selects and
  freezes the lowest available profile or complete recipe fingerprint.
  Unavailable roles have no file and no placeholder.
- **Email PDFs.** Email PDF roles require either `profile_fingerprint` or
  `recipe_sha256`, not both. A profile is specific to one source, while a
  recipe can cover multiple messages. After sealing the source, call
  `GET /api/v1/exports/sources/{id}/email-pdf-recipes` to discover body
  recipes; this advisory response caps at 50 recipes and fails if exceeded.
  When you include attachments, the body PDF must match that attachment set's
  decoded generation. A missing match counts as an unavailable output, so your
  chosen partial-export policy applies. Without attachments, multiple decoded
  generations for the selected recipe conflict instead of choosing one
  arbitrarily.
- **Attachments.** Attachment roles pin the publication and each child
  occurrence. If a parent has more than one publication, your plan must include
  a `publications` entry with `version_id` and `operation_id`. The API accepts
  up to 1,000 such selections and never picks the latest publication
  automatically. In the web app, **Find attachment sets** lists these choices,
  including empty sets. Over HTTP, call
  `GET /api/v1/exports/sources/{id}/attachment-publications?after=0` after
  sealing the source to page through up to 50 choices at a time with a `next`
  cursor. Use `GET /api/v1/exports/plans/{id}/problems?after=0` in the same way
  to read up to 50 unavailable details per page.

To share duplicate outputs, set `duplicate_policy: "collapse_exact_content"`.
Omitting it writes separate outputs. In `metadata.csv`, a shared output's
`role_path` points to the file it reuses, and with volume packaging that path
points inside the corresponding numbered ZIP.

## Upload more than 1,000 members

To export more than 1,000 members, create an `upload` source with `total` and
`member_hash`, where `member_hash` is the SHA-256 over UTF-8
`node_id:version_id\n` lines sorted by numeric node ID and then by version ID.
Upload zero-based chunks to `PUT /api/v1/exports/sources/{id}/chunks/{index}`
with `{"members":[...]}`. Every chunk must hold 1,000 members except the final
remainder. Retrying an identical chunk succeeds, while changing its content
conflicts. When every chunk is uploaded, send `{}` to
`POST /api/v1/exports/sources/{id}/seal`. Missing chunks, duplicate exact
members, or a count or hash mismatch prevent sealing.

## Retention and deletion

Sealed sources and plans each have a ten-minute admission window, and an
accepted job can run for up to two hours. A completed archive stays in
owner-private storage for 24 hours. New download tickets can retrieve it during
that window, and using a ticket does not delete the retained archive. Cleanup
removes expired records and files after any active download leases finish.

An active or completed job also keeps its plan readable at
`GET /api/v1/exports/plans/{id}`, returning the plan's original admission
deadline and fingerprint. That retention does not let you start new jobs after
the admission deadline passes.

While an export retains its data, destructive version pruning, permanent
deletion, and purging selected retained derivatives fail with `export_retained`
and return an expiry. Selected attachment publications stay protected until
export cleanup releases their plan authority, while ordinary document
replacement continues to work. Failed and canceled jobs release their
protection during cleanup once their remaining admission and short
terminal-record retention windows pass. Protection ends when cleanup removes
the retained records, not at the expiry timestamp alone.

If the daemon restarts, it fences old worker claims and rebuilds an interrupted
ZIP from the same stored plan. A portable backup includes sealed source and plan
authority along with its retained blob closure. Restoring from that backup
preserves those receipts and expiries, though it does not restore runnable
export handles, browser credentials, download tickets, private paths, or
completed archive files.

## Archive contract and limits

The `docbank-bundle-v1` format uses deterministic, uncompressed ZIP or ZIP64
archives. Inside the archive, generated ASCII paths identify each document
version and role, while original names and paths stay in the metadata.
`bundle.json` holds the plan, document receipts, and exact metadata values, and
`metadata.csv` provides a spreadsheet-friendly view with formula cells escaped.

Attachment occurrences follow their parent as bounded continuation rows. Each
row records the original message, selected publication, part position, child
identity, output status, and any retained PDF receipt. Available PDF receipts
bind the source, decoded generation, renderer recipe, body, output hash and
size, and page count. A `collapsed` role keeps its receipt and points to the
earlier physical output with `reuse_of`. Downstream tools can rely on these
ordered receipts. Ordinary export does not number, stamp, or concatenate PDFs.

`SHA256SUMS` covers every role file along with `metadata.csv` and
`bundle.json`, excluding itself. The separate receipt hashes the complete ZIP
to avoid a checksum cycle. Before publishing an archive, verification checks
local and central ZIP records, exact file membership, paths, sizes, CRCs,
SHA-256 digests, metadata, and the plan fingerprint. Docbank rejects compressed
entries, symlinks, duplicate or unsafe paths, prefixes, trailing data, and
incomplete streams.

Email exports default to bounded volumes. A single browser download contains
numbered `volumes/000001.zip` files alongside the complete parent metadata and
reconciliation manifest. Each child ZIP carries its own checksums and a
`volume.json` file binding the parent fingerprint, volume number, first output,
output count, and payload bytes. Parent verification checks every declared
output against its child volume, so swapping valid volumes fails
reconciliation.

To turn on volume packaging in the API, pass `volume_limits`, for example
`{"roles":1000,"role_bytes":536870912}`. You can lower either limit, though
neither can exceed 1,000 outputs or 512 MiB of payload per volume. Fixed
metadata and ZIP overhead add at most 1 MiB per volume. Outputs are never split
across volumes, and a single output larger than the chosen payload limit fails
planning. Omitting `volume_limits` produces a flat archive. Plan and progress
counts report physical output files, while a completed receipt counts outer
archive entries.

Single-export limits:

- Up to 100,000 document-version members, 300,000 role files, and 50 GiB of
  role bytes
- Up to 64 KiB per parent or attachment continuation row, and at most 400,000
  rows in total
- Up to 512 MiB of total archive metadata, with `bundle.json` capped at 256
  MiB, `metadata.csv` capped at 128 MiB, and checksums using the rest
- Up to 64 MiB for the ZIP directory and 52 GiB for the complete archive

Page export also limits stored page geometry to 32 KiB and budgets 32 KiB for
page receipts, estimated at 1 KiB plus recipe JSON per page. Those bounds can
exclude documents with a few dozen pages. Exceeding either bound fails planning
unless your `pages` policy sets `allow_unavailable: true`, which marks the
entire page role as unavailable and omits its images and frames. Other resource
limits still reject the plan.

Retention and concurrency limits:

- At most 32 retained sources and plans combined, eight retained jobs globally,
  and two retained jobs per owner. Completed, failed, and canceled jobs count
  against these limits until release or cleanup removes them. Every client
  using the daemon API key shares the `master` owner and its two-job allowance.
  Over-limit requests fail rather than truncating your selection.
- Photo preparation renders one request at a time per vault. Locking Hidden
  blocks new photo preparations, while already issued exports continue under
  the normal retention, release, and download expiry rules.
