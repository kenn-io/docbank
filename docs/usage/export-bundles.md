---
last_edited: 2026-09-12
title: Verified export bundles
description: Download exact document versions, verified email PDFs and attachment sets in reconciled ZIP bundles.
---

# Verified export bundles

Use the web app or authenticated HTTP API to export exact document versions,
retained email PDFs, attachment originals, Markdown text, and page images.
Docbank freezes the selection and role receipts
before writing the archive. A later edit to a saved query, tag, document head,
or processing result does not change an admitted plan.

In the web app, choose **Export** for selected documents or a whole frozen
query. A completed mailbox import also offers **Export completed collection**.
Review the frozen counts, start the export, then download its verified ZIP.
Closing the drawer does not cancel an admitted job; reopen it in the same
browser session to reconnect. Original files remain original bytes; the bundle
does not redact or sanitize them. There is no CLI bundle-export command.

## Email PDFs and attachments

Choose **Include retained body PDFs**, find the retained recipes, and select
the paper size and exact runtime recipe. Generate missing PDFs through the
[email PDF workflow](email-pdf.md) before exporting. Batch export uses retained
outputs; it does not start rendering or silently substitute another recipe.

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
unavailable. **Allow declared unavailable outputs** permits an explicitly
partial export. Review its unavailable-item pages before starting. A complete
empty inventory is valid; a missing or partial inventory is not an empty one.
Known occurrences remain in the manifest even when their requested output is
unavailable. Counts distinguish messages, attachment occurrences, actual PDF
files and pages, shared outputs, and unavailable outputs or inventories.

Every selected occurrence gets its own output by default. **Share exact
duplicate outputs** retains every source and attachment receipt while pointing
identical outputs at an earlier file. Sharing requires equal original message
bytes, the same attachment position, and equal output bytes and recipes. Equal
subjects or Message-ID values do not establish duplicates. ID-based output
paths keep repeated subjects separate.

## Create and download a bundle

1. `POST /api/v1/exports/sources` with an `operation_id` UUID and exactly one
   source kind: `explicit`, `nodes`, `query`, `saved_query`, `snapshot`,
   `mailbox_collection`, or `upload`. Explicit members contain `node_id`, `version_id`, `sha256`, and
   `size`. An optional `revision` is a precondition. Distinct historical
   versions of the same document are allowed.
2. `POST /api/v1/exports/plans` with a new `operation_id`, the returned
   `source_id` and `member_hash`, and `roles`. Each role policy selects
   `original`, `text`, `pages`, `email_pdf`, `attachment_original`, or
   `attachment_pdf`. Missing requested roles fail planning unless
   that policy explicitly sets `allow_unavailable: true`.
3. `POST /api/v1/exports/jobs` with a new `operation_id`, `plan_id`, and the
   plan's `fingerprint`. The daemon queues a durable job.
4. Read `GET /api/v1/exports/jobs/{id}` or its `/events` NDJSON stream. Every
   stream record is a current-state delivery with a monotonic sequence, not
   a replay of missing events. A stream lasts at most 30 seconds; reconnect
   with `?after=<sequence>`. Only `completed` carries an archive receipt.
5. `POST /api/v1/exports/jobs/{id}/download` with `{}` or a safe `basename`.
   Docbank rechecks the
   actual archive and returns a two-minute, one-use download URL and receipt.
   Verify the complete downloaded ZIP against the receipt's SHA-256 and size.

Repeat the same operation UUID and request after a lost response. Changed
input under an existing UUID conflicts. An interrupted source resolution
cannot silently rerun a changed query; start a new operation explicitly.
Use `POST /api/v1/exports/jobs/{id}/cancel` with `{}` to cancel active work.
No request accepts a server destination path.

For a saved query, send `saved_query_id` and `saved_query_revision`. For a
snapshot, send `snapshot_id` and `member_hash` before its cache expires.
Queries use the same frozen population service as workspace queries.
Snapshot-observed revisions are not implicit revision preconditions.
For `mailbox_collection`, send `collection_id`. Only a completed import is
accepted. Membership comes from its exact imported receipt targets, not the
live collection or later document heads.

Text roles use retained, verified sanitized Markdown and record the actual
processing profile, attachment, and build. Set `profile_fingerprint` to select
one profile explicitly. Page roles select a complete retained page-image
recipe and include its physical frames and recipe. Set `recipe_sha256` to pin
that selection. Export does not start rendering. Without an explicit selector,
the lowest available profile or complete recipe fingerprint is selected and
frozen. An unavailable role has no file or invented placeholder.

Email PDF roles require exactly one `profile_fingerprint` or `recipe_sha256`.
A profile is source-specific; a recipe can cover multiple messages. Discover
body recipes with `GET /api/v1/exports/sources/{id}/email-pdf-recipes` after
sealing the source. This advisory response is bounded to 50 recipes and fails
explicitly if exceeded. Multiple decoded generations for the selected recipe
conflict instead of choosing one arbitrarily.

Attachment roles pin the publication and each exact child occurrence. If more
than one publication exists for a parent, the plan must supply an explicit
`publications` entry containing `version_id` and `operation_id`; the API accepts
at most 1,000 such selections. It never chooses the latest publication.
`GET /api/v1/exports/plans/{id}/problems?after=0` returns up to 50 unavailable
details, with a `next` cursor until every detail is accounted for.
Set `duplicate_policy: "collapse_exact_content"` only for explicit sharing;
omitting it preserves separate outputs.

## Upload more than 1,000 members

Create an `upload` source with `total` and `member_hash`. The member hash is
SHA-256 over UTF-8 `node_id:version_id\n` records sorted by numeric node ID,
then version ID. Upload zero-based chunks to
`PUT /api/v1/exports/sources/{id}/chunks/{index}` with `{"members":[...]}`.
Each chunk has 1,000 members except the final remainder. An identical chunk
retry succeeds; changed content conflicts. Seal with
`POST /api/v1/exports/sources/{id}/seal` and `{}`. Missing chunks, duplicate
exact members, and count or hash mismatches prevent sealing.

## Retention and deletion

Sealed sources and plans each have a ten-minute admission window. An accepted
job can run for up to two hours. A completed archive stays in owner-private
storage for 24 hours. New tickets can retrieve it during that period; using a
ticket does not delete the retained archive. Bounded cleanup removes expired
records and files after active download leases finish.

During retention, destructive version pruning, permanent deletion, and purge
of selected retained derivatives return `export_retained` with an expiry.
Selected attachment publications are also retained until export cleanup
releases their plan authority.
Ordinary document replacement remains available. Failed and canceled work
releases its protection through cleanup after the remaining admission and
short terminal-record retention windows.

A daemon restart fences old worker claims and rebuilds an interrupted ZIP
from the same stored plan. Portable backup includes sealed source and plan
authority plus its retained blob closure. Restoring a backup preserves those
receipts and expiries, but does not restore runnable export handles, browser
credentials, tickets, private paths, or completed archive files.

## Archive contract and limits

`docbank-bundle-v1` uses deterministic, uncompressed ZIP/ZIP64. Generated ASCII
paths identify each document version and role; original names and paths stay
in metadata. `bundle.json` contains the plan and document receipts, and
`metadata.csv` provides a spreadsheet projection with formula cells escaped.
The JSON retains exact metadata values.

Attachment occurrences follow their parent as bounded continuation rows.
Each row records the original message, selected publication, part position,
exact child identity, output status, and any retained PDF receipt. Available
PDF receipts bind source, decoded generation, renderer recipe, body, output
hash/size and page count. A `collapsed` role keeps that receipt and references
the earlier physical output with `reuse_of`. These ordered receipts are usable
by downstream consumers; the ordinary export performs no numbering, stamping,
or PDF concatenation.

`SHA256SUMS` covers every role file, `metadata.csv`, and `bundle.json`. It
excludes itself. The separate receipt hashes the complete ZIP, avoiding a
checksum cycle. Verification checks local and central ZIP records, exact file
membership, paths, sizes, CRCs, SHA-256 values, metadata, and the plan fingerprint
before publication. Compressed entries, symlinks, duplicate or unsafe paths,
prefixes, trailing data, and incomplete streams are rejected.

Email exports default to bounded volumes. A single browser download contains
numbered `volumes/000001.zip` files plus the complete parent metadata and
reconciliation manifest. Each child ZIP has its own checksums and `volume.json`
binding the parent fingerprint, volume number, first output, output count and
payload bytes. Parent verification checks every declared output against its
child volume; swapping valid volumes is not valid reconciliation.

The API enables this packaging with `volume_limits`, for example
`{"roles":1000,"role_bytes":536870912}`. Both limits may be lowered; neither
may exceed 1,000 outputs or 512 MiB of payload per volume. Fixed metadata and
ZIP overhead add at most 1 MiB per volume. No output is split across volumes;
an individual output above the selected payload bound fails planning. Omitting
`volume_limits` produces the flat archive. Plan and progress counts report
physical output files; a completed receipt counts outer archive entries.

An export admits at most 100,000 document-version members, 300,000 role files,
and 50 GiB of role bytes. Limits also include 64 KiB per parent or attachment
continuation row, at most 400,000 such rows,
512 MiB total archive metadata, a 64 MiB ZIP directory, and a 52 GiB archive.
Within the metadata allowance, `bundle.json` is capped at 256 MiB and
`metadata.csv` at 128 MiB; checksums use the remaining allowance.
There are at most 32 retained sources and plans combined, eight retained jobs
globally, and two per owner. Over-limit work fails explicitly without truncating
the selection.
