---
last_edited: 2026-09-12
title: Verified export bundles
description: Export exact document versions and retained text or page images through stored plans and independently verified ZIP archives.
---

# Verified export bundles

Use the authenticated HTTP API to export exact document versions, retained
Markdown text, and page images. Docbank freezes the selection and role receipts
before writing the archive. A later edit to a saved query, tag, document head,
or processing result does not change an admitted plan.

This is an API workflow. The web application and CLI do not have an export
wizard or export command. Original files remain original bytes; the bundle
does not redact or sanitize them.

## Create and download a bundle

1. `POST /api/v1/exports/sources` with an `operation_id` UUID and exactly one
   source kind: `explicit`, `nodes`, `query`, `saved_query`, `snapshot`, or
   `upload`. Explicit members contain `node_id`, `version_id`, `sha256`, and
   `size`. An optional `revision` is a precondition. Distinct historical
   versions of the same document are allowed.
2. `POST /api/v1/exports/plans` with a new `operation_id`, the returned
   `source_id` and `member_hash`, and `roles`. Each role policy selects
   `original`, `text`, or `pages`. Missing requested roles fail planning unless
   that policy explicitly sets `allow_unavailable: true`.
3. `POST /api/v1/exports/jobs` with a new `operation_id`, `plan_id`, and the
   plan's `fingerprint`. The daemon queues a durable job.
4. Read `GET /api/v1/exports/jobs/{id}` or its `/events` NDJSON stream. Every
   stream record is a current-state delivery with a monotonic sequence, not
   a replay of missing events. A stream lasts at most 30 seconds; reconnect
   with `?after=<sequence>`. Only `completed` carries an archive receipt.
5. `POST /api/v1/exports/jobs/{id}/download` with `{}`. Docbank rechecks the
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

Text roles use retained, verified sanitized Markdown and record the actual
processing profile, attachment, and build. Set `profile_fingerprint` to select
one profile explicitly. Page roles select a complete retained page-image
recipe and include its physical frames and recipe. Set `recipe_sha256` to pin
that selection. Export does not start rendering. Without an explicit selector,
the lowest available profile or complete recipe fingerprint is selected and
frozen. An unavailable role has no file or invented placeholder.

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

`SHA256SUMS` covers every role file, `metadata.csv`, and `bundle.json`. It
excludes itself. The separate receipt hashes the complete ZIP, avoiding a
checksum cycle. Verification checks local and central ZIP records, exact file
membership, paths, sizes, CRCs, SHA-256 values, metadata, and the plan fingerprint
before publication. Compressed entries, symlinks, duplicate or unsafe paths,
prefixes, trailing data, and incomplete streams are rejected.

An export admits at most 100,000 document-version members, 300,000 role files,
and 50 GiB of role bytes. Limits also include 64 KiB of metadata per member,
512 MiB total archive metadata, a 64 MiB ZIP directory, and a 52 GiB archive.
Within the metadata allowance, `bundle.json` is capped at 256 MiB and
`metadata.csv` at 128 MiB; checksums use the remaining allowance.
There are at most 32 retained sources and plans combined, eight retained jobs
globally, and two per owner. Over-limit work fails explicitly without truncating
the selection.
