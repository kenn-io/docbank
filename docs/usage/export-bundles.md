---
last_edited: 2026-10-05
title: Verified export bundles
description: Download document versions, verified email PDFs, and attachment sets as verified ZIP bundles.
---

# Verified export bundles

Use the web app or authenticated HTTP API to export document versions,
retained email PDFs, rendered photos, attachment originals, Markdown text, and page images.
Use `docbank export` or the [native MCP export tools](mcp.md#native-export-jobs)
for original files selected by exact document-version identity.

Docbank freezes the selection and role receipts before writing the archive. A
later edit to a saved query, tag, document head, or processing result does not
change an admitted plan.

In the web app, choose **Export** for selected documents or a whole frozen
query. A completed mailbox import also offers **Export completed collection**.
Review the frozen counts, start the export, then download its verified ZIP.
Closing the drawer does not cancel an admitted job. Reopen it in the same
browser session to reconnect. Original files keep their original bytes. The
bundle does not redact or sanitize them.

## Export original files from the CLI

Create a JSON request with two caller-chosen UUIDv4 operation IDs and 1–1,000
document versions. Obtain the node ID, version ID, SHA-256, and size from
the document's receipts. This synthetic request selects one empty original:

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

Replace the synthetic identities with your own receipts. Save the request as
`selection.json`, then run:

```sh
docbank export preview --request selection.json --json
docbank export start <plan-id> --fingerprint <fingerprint> --operation-id <job-id> --json
docbank export status <job-id> --json
docbank export download <job-id> ./originals.zip
docbank export release <job-id>
```

Review the preview before starting. It freezes the selection, reports planned
original and metadata bytes, and returns a plan ID and fingerprint. Supply those
values to `start` with a new job UUIDv4. Start returns immediately. Use `status`
to observe progress. Download requires `completed`. Use `cancel <job-id>` to
request cancellation of active work. Exiting the CLI does not cancel it.

All export commands accept `--json`. Preview returns the plan, start/status return
the job, and download returns the verified archive receipt after saving the file.
Cancel returns `{"job_id":"…","accepted":true}`, and release returns
`{"job_id":"…","released":true}`. Reading the status of a failed job still
succeeds. Inspect its `state` and `failure`.

The request file is limited to 1 MiB. Unknown fields and invalid values are
usage errors checked before daemon access. IDs must be canonical UUIDv4 strings,
hashes lowercase hexadecimal, node IDs positive, and sizes nonnegative. An
optional nonnegative `revision` checks the node revision. Zero or omission adds
no precondition. Distinct retained versions of one node are allowed, but
duplicate node/version pairs are not.

The daemon checks the complete identity and size. It never substitutes the
current version. Names and paths are frozen at preview time. Only the selected
originals are exported: no descendants, attachments, or renditions are added.

You can explicitly select a published attachment as an ordinary original.
Omitting it excludes its separate file and manifest row. An exported original
email still contains every MIME part in its original bytes; selecting only the
parent does not redact its attachments.

Keep the request and operation IDs for retries. Repeat exactly the same request
after a lost response. Changed input under an existing ID conflicts. After a
long interruption, inspect the known job ID or repeat `start`, rather than
replaying preview: the source's ten-minute replay window can expire while its
job remains available.

Download streams to a private stage beside the destination, verifies the ZIP
against the job fingerprint and receipts, then publishes the complete file.
The parent directory must exist and be outside Docbank's data directory. An
existing destination is preserved unless you pass `--overwrite`. Replacement
happens only after verification. Pre-publication failures leave it untouched.
An error after publication reports that the verified file is already saved.
Download does not release the retained job automatically.

## Export photos

In Photos, choose **Export photos** for the current scope or **Export selection** for selected photos. Choose JPEG or PNG, JPEG quality from 1 to 100, and an optional long edge in pixels. Blank keeps the original size; exports never enlarge an image. **Include metadata** carries source EXIF and unrelated XMP, then applies confirmed ratings, flags, labels, captions, creators, copyright and assigned keywords. Untouched embedded credits stay intact; confirmed empty values clear them. **Remove GPS** starts checked. Turning metadata off removes EXIF and XMP from every delivered file. Color profiles remain attached to preserve appearance without conversion. Source orientation and your rotation apply once; exported orientation is 1.

Choose **Prepare**, review the frozen plan, then **Start reviewed export** and **Download verified ZIP**. RAW display members use their embedded JPEG previews; the drawer and manifest label that origin. A RAW file without a supported embedded preview stops preparation. Unsupported color profiles, malformed metadata, sources over 512 MiB, and images over 100 million pixels also stop preparation. Preparation has a five-minute deadline and can be canceled.

Each preparation allows at most 16 photos, 512 MiB of source bytes, 512 million decoded pixels and 1 GiB of rendered copies, within five minutes. One unavailable photo stops the complete preparation; the error names the photo so you can exclude it and retry.

For the CLI, add `photo_render` to an exact-member request, or replace `members` with a `photos` selection. This selection exports the complete current scope; `asset_ids` restricts it to selected display members:

```json
{
  "source_operation_id": "11111111-1111-4111-8111-111111111111",
  "plan_operation_id": "22222222-2222-4222-8222-222222222222",
  "photos": {
    "query": {"v": 1, "syntax": "advanced", "mode": "lexical", "text": "", "sort": {"field": "name", "direction": "asc"}},
    "hidden": false
  },
  "photo_render": {"format": "jpeg", "quality": 90, "long_edge": 2048, "include_metadata": true, "remove_gps": true}
}
```

Use the same preview, start, status, download, and release commands shown above. The HTTP source request uses `kind: "photos"` with the same `photos` selection. Its plan request uses `roles: [{"role": "photo_rendered"}]` and `photo_render`. Preparation freezes the display version, decisions, keywords, and revisions; changes during rendering require a fresh preview. Generated copies remain retained for the export and stay outside authority backups.

## Inspect a retained plan

Keep the plan ID returned by preview. Read its saved header and any unavailable
outputs without creating another preview:

```sh
docbank export show-plan <plan-id> --json
docbank export problems <plan-id> --json
docbank export problems <plan-id> --after 50 --json
```

`show-plan --json` returns the complete saved plan. Human output labels
`expires_at` as the **admission deadline**: a retained job can keep the header
readable after that deadline, but a new start is refused. Reads extend neither
admission nor retention.

CLI, MCP, and other API-key clients share the same owner. They cannot inspect a
plan owned by a browser session. An unknown or removed plan returns not found.
An expired retained record returns `export_expired`. For a new export, create a
fresh preview with new operation IDs. It captures a new selection, not the old
snapshot. Use status to inspect an existing job.

`problems --json` returns `plan_id`, `fingerprint`, `after`, `next`, `total`, and
`items`. Each item identifies the node, version, role, reason, and optional
attachment `part_path`. Pass the returned nonzero `next` as `--after`. Zero
means the last page. Each call returns one page. Human output includes the next
command. Offsets range from zero to 2,600,000. An offset equal to the total
returns an empty final page. An offset beyond the total conflicts.

The details describe the frozen plan, even after source documents change.
Totals count problems, not documents. They include a separate problem for each
requested attachment role whose inventory is unavailable.
Originals-only CLI and MCP previews normally have no unavailable outputs, while
richer plans created through the same API-key owner can have them.

Pages contain at most 50 items and must fit within 64 KiB of encoded JSON.
A larger page fails with `export_limit` instead of returning partial data.
The HTTP problems route has the same cap and no option for a smaller page,
so switching clients or releasing jobs cannot make that page readable.

## Free a finished job slot

All CLI calls and other API-key clients share two retained job slots. After two
completed exports, another start returns `export_limit` until a job is released
or expires and is cleaned up. Without release, completed jobs remain for 24 hours.

Run `docbank export release <job-id>` after saving the archive or deciding it is
no longer needed. The API equivalent is `DELETE /api/v1/exports/jobs/{id}`.
Release accepts completed, failed, or canceled jobs and returns HTTP 204. It
removes that job's retained archive and status, frees its slot, and leaves local
downloads and source documents unchanged. Active jobs must be canceled first.

An active download or unused ticket blocks release with `export_retained`.
Unused tickets expire after two minutes. Release does not interrupt a download.
Even `download && release` can briefly return `export_retained` while the server
finishes releasing the download lease. Retry release after a short delay.
If release is interrupted after removing the archive, download returns
`410 export_expired` with a hint to retry release and free the retained slot.
If release fails, retry it. If its response was lost, a not-found status confirms
the job is gone. Repeated release returns not found. Do not reuse a released
job's operation ID: its replay protection has been removed.

Release does not remove source and plan records before their original admission
deadlines or another job's retention. The global cap of 32 source/plan records
still applies: 16 fresh previews can fill it within ten minutes even if every
job is released. Review the limits below when scheduling batches.

## Email PDFs and attachments

Choose **Include retained body PDFs**, find the retained recipes, and select
the paper size and runtime recipe. Generate missing PDFs through the
[email PDF workflow](email-pdf.md) before exporting. Batch export uses retained
outputs. It does not start rendering or silently substitute another recipe.

The email attachment choices are:

| Choice | Downloaded outputs |
| --- | --- |
| Body PDF only | One separate PDF for each available selected message. |
| Include original attachments | Body PDFs and unchanged original attachment files. |
| Originals + separate qualified PDFs | Body PDFs, attachment originals, and separately retained PDFs for supported children. |

Only nested EML children with a qualified retained email-PDF receipt currently
support the separate-PDF option. Other formats remain originals with declared
unavailable PDF entries. An attached original PDF is not relabeled as a rendered
derivative. Attachment contents are never appended to a body PDF.

The default stops planning if a requested output or attachment inventory is
unavailable. **Allow declared unavailable outputs** permits a partial export.
Review its unavailable-item pages before starting. A complete empty inventory
is valid. A missing or partial inventory is not an empty one.
Known occurrences remain in the manifest even when their requested output is
unavailable. Counts distinguish messages, attachment occurrences, actual PDF
files and pages, shared outputs, and unavailable outputs or inventories.

Every selected occurrence gets its own output by default. **Share exact
duplicate outputs** keeps every source and attachment receipt while pointing
identical outputs at an earlier file. Sharing requires equal original message
bytes, the same attachment position, and equal output bytes and recipes. Equal
subjects or Message-ID values do not establish duplicates. ID-based output
paths keep repeated subjects separate.

## Create and download a bundle

1. `POST /api/v1/exports/sources` with an `operation_id` UUID and exactly one
   source kind: `explicit`, `nodes`, `query`, `saved_query`, `snapshot`,
   `mailbox_collection`, or `upload`. Explicit members contain `node_id`,
   `version_id`, `sha256`, and `size`. An optional `revision` is a
   precondition. Distinct historical versions of the same document are allowed.
2. `POST /api/v1/exports/plans` with a new `operation_id`, the returned
   `source_id` and `member_hash`, and `roles`. Each role policy selects
   `original`, `text`, `pages`, `email_pdf`, `attachment_original`, or
   `attachment_pdf`. Missing requested roles fail planning unless that policy
   sets `allow_unavailable: true`.
3. `POST /api/v1/exports/jobs` with a new `operation_id`, `plan_id`, and the
   plan's `fingerprint`. The daemon queues a durable job.
4. Read `GET /api/v1/exports/jobs/{id}` or its `/events` NDJSON stream. Every
   stream record is a current-state delivery with a monotonic sequence, not
   a replay of missing events. A stream lasts at most 30 seconds. Reconnect
   with `?after=<sequence>`. Only `completed` carries an archive receipt.
5. `POST /api/v1/exports/jobs/{id}/download` with `{}` or a safe `basename`.
   Docbank rechecks the actual archive and returns a two-minute, one-use
   download URL and receipt.
   Verify the complete downloaded ZIP against the receipt's SHA-256 and size.

Repeat the same operation UUID and request after a lost response. Changed
input under an existing UUID conflicts. An interrupted source resolution
cannot silently rerun a changed query. Start a new operation instead.
Use `POST /api/v1/exports/jobs/{id}/cancel` with `{}` to cancel active work.
No request accepts a server destination path.

For a saved query, send `saved_query_id` and `saved_query_revision`. For a
snapshot, send `snapshot_id` and `member_hash` before its cache expires.
Queries use the same frozen population service as workspace queries.
Snapshot-observed revisions are not implicit revision preconditions.
For `mailbox_collection`, send `collection_id`. Only a completed import is
accepted. Membership comes from its imported receipt targets, not the live
collection or later document heads.

Text roles use retained, verified sanitized Markdown and record the actual
processing profile, attachment, and build. Set `profile_fingerprint` to select
one profile. Page roles select a complete retained page-image recipe and
include its physical frames and recipe. Set `recipe_sha256` to pin that
selection. Export does not start rendering. Without a selector, the lowest
available profile or complete recipe fingerprint is selected and frozen. An
unavailable role has no file and no placeholder.

Email PDF roles require exactly one `profile_fingerprint` or `recipe_sha256`.
A profile is source-specific, while a recipe can cover multiple messages.
Discover body recipes with `GET /api/v1/exports/sources/{id}/email-pdf-recipes`
after sealing the source. This advisory response is bounded to 50 recipes and
fails if exceeded. When attachments are included, the body PDF must match
that attachment set's decoded generation. A missing match is an unavailable
output, so the selected partial-export policy applies. Without attachments,
multiple decoded generations for the selected recipe conflict instead of
choosing one arbitrarily.

Attachment roles pin the publication and each child occurrence. If more than
one publication exists for a parent, the plan must supply a `publications`
entry containing `version_id` and `operation_id`. The API accepts at most 1,000
such selections. It never chooses the latest publication.

The drawer's **Find attachment sets** button lists these choices, including
empty sets. API callers use
`GET /api/v1/exports/sources/{id}/attachment-publications?after=0` after sealing
the source. Each page contains up to 50 choices and a `next` cursor.
`GET /api/v1/exports/plans/{id}/problems?after=0` returns up to 50 unavailable
details, with a `next` cursor until every detail is accounted for.

Set `duplicate_policy: "collapse_exact_content"` only to share duplicate
outputs. Omitting it preserves separate outputs. In `metadata.csv`, a shared
output's `role_path` points to the file it reuses. With volume packaging, that
path is inside the corresponding numbered ZIP.

## Upload more than 1,000 members

Create an `upload` source with `total` and `member_hash`. The member hash is
SHA-256 over UTF-8 `node_id:version_id\n` records sorted by numeric node ID,
then version ID. Upload zero-based chunks to
`PUT /api/v1/exports/sources/{id}/chunks/{index}` with `{"members":[...]}`.
Each chunk has 1,000 members except the final remainder. An identical chunk
retry succeeds, and changed content conflicts. Seal with
`POST /api/v1/exports/sources/{id}/seal` and `{}`. Missing chunks, duplicate
exact members, and count or hash mismatches prevent sealing.

## Retention and deletion

Sealed sources and plans each have a ten-minute admission window. An accepted
job can run for up to two hours. A completed archive stays in owner-private
storage for 24 hours. New tickets can retrieve it during that period. Using a
ticket does not delete the retained archive. Cleanup removes expired records
and files after active download leases finish.

An active or completed job also keeps its plan readable through
`GET /api/v1/exports/plans/{id}`. The returned plan keeps its original admission
deadline and fingerprint. Retention does not allow new jobs after that deadline.

During retention, destructive version pruning, permanent deletion, and purge
of selected retained derivatives return `export_retained` with an expiry.
Selected attachment publications are also retained until export cleanup
releases their plan authority. Ordinary document replacement remains available.
Failed and canceled work releases its protection through cleanup after the
remaining admission and short terminal-record retention windows. Protection
ends when cleanup removes the retained records, not at the expiry timestamp
alone.

A daemon restart fences old worker claims and rebuilds an interrupted ZIP
from the same stored plan. Portable backup includes sealed source and plan
authority plus its retained blob closure. Restoring a backup preserves those
receipts and expiries, but does not restore runnable export handles, browser
credentials, tickets, private paths, or completed archive files.

## Archive contract and limits

`docbank-bundle-v1` uses deterministic, uncompressed ZIP/ZIP64. Generated ASCII
paths identify each document version and role. Original names and paths stay
in metadata. `bundle.json` contains the plan and document receipts, and
`metadata.csv` provides a spreadsheet projection with formula cells escaped.
The JSON keeps exact metadata values.

Attachment occurrences follow their parent as bounded continuation rows.
Each row records the original message, selected publication, part position,
child identity, output status, and any retained PDF receipt. Available PDF
receipts bind source, decoded generation, renderer recipe, body, output
hash/size, and page count. A `collapsed` role keeps that receipt and references
the earlier physical output with `reuse_of`. Downstream consumers can use these
ordered receipts. The ordinary export performs no numbering, stamping, or PDF
concatenation.

`SHA256SUMS` covers every role file, `metadata.csv`, and `bundle.json`. It
excludes itself. The separate receipt hashes the complete ZIP, avoiding a
checksum cycle. Verification checks local and central ZIP records, exact file
membership, paths, sizes, CRCs, SHA-256 values, metadata, and the plan fingerprint
before publication. Compressed entries, symlinks, duplicate or unsafe paths,
prefixes, trailing data, and incomplete streams are rejected.

Email exports default to bounded volumes. A single browser download contains
numbered `volumes/000001.zip` files plus the complete parent metadata and
reconciliation manifest. Each child ZIP has its own checksums and `volume.json`
binding the parent fingerprint, volume number, first output, output count, and
payload bytes. Parent verification checks every declared output against its
child volume, so swapping valid volumes fails reconciliation.

The API enables this packaging with `volume_limits`, for example
`{"roles":1000,"role_bytes":536870912}`. Both limits may be lowered. Neither
may exceed 1,000 outputs or 512 MiB of payload per volume. Fixed metadata and
ZIP overhead add at most 1 MiB per volume. No output is split across volumes.
An individual output above the selected payload bound fails planning. Omitting
`volume_limits` produces the flat archive. Plan and progress counts report
physical output files, while a completed receipt counts outer archive entries.

An export admits at most 100,000 document-version members, 300,000 role files,
and 50 GiB of role bytes. Limits also include 64 KiB per parent or attachment
continuation row, at most 400,000 such rows, 512 MiB total archive metadata, a
64 MiB ZIP directory, and a 52 GiB archive. Within the metadata allowance,
`bundle.json` is capped at 256 MiB and `metadata.csv` at 128 MiB. Checksums use
the remaining allowance.

Page export also limits stored page geometry to 32 KiB and budgets 32 KiB for
page receipts, estimated as 1 KiB plus recipe JSON per page. These bounds can
exclude documents with a few dozen pages. Exceeding either bound fails planning
unless the `pages` policy sets `allow_unavailable: true`. That records the whole
page role as unavailable and omits its images and frames. Other resource limits
still reject the plan.

There are at most 32 retained sources and plans combined, eight retained jobs
globally, and two per owner. Completed, failed, and canceled jobs count until
release or cleanup removes them. All clients using the daemon API key share the
`master` owner and its two-job allowance. Over-limit work fails without
truncating the selection.
