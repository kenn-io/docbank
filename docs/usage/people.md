---
last_edited: 2026-09-29
title: People
description: List and edit canonical people through the daemon API, CLI, and MCP.
---

# People

Docbank keeps one canonical person record for each person in the vault. The
daemon API, CLI, and MCP can list, search, create, rename, retire, merge, and
split these records. Every existing-person edit uses the revision returned by
the preceding read.

## List and search

`GET /api/v1/people?query=ada&limit=100` lists active people in folded
display-name order. The CLI equivalent is:

`docbank people list ada`

MCP clients use `find_people`. Cursors are bound to the query and the
requested person page.

## Create and inspect

Create an operator-owned person with `docbank people create "Ada Lovelace"`.
Read it with `docbank people show <person-id>` or
`GET /api/v1/people/by-id/{person_id}`. The detail response includes identity
IDs, external UIDs, the current revision, and the ID used to reach a merged
person. Use `docbank people custodians <person-id>` to page active custodian
assignments. Each assignment includes the coordinates for its scope: an
ingest ID for a collection, package IDs for a package, or a node ID and
content version ID for a document.

## Edit people

Rename and retire commands require `--revision`. HTTP clients send the
same value in `If-Match`. A stale value returns `412 stale_revision`.
Retirement keeps historical assertions but removes the person from active
list and search results.

Merge sends the survivor revision in `If-Match` and the absorbed revision
in the request body. Merge and split require an operation UUID. Retrying the
same request returns its saved receipt. A different request with the same UUID
returns `person_merge_conflict`.

Split moves only the identity IDs, custodian assignment IDs, or external UIDs
listed in the request. The new display name is required. The store validates
that every selected member belongs to the source person. HTTP applies Huma's
inherited 1 MiB request body limit before decoding; larger bodies return 413.
Valid finite selections within that limit remain supported.

The split response includes `source_revision_after` and an ETag for that
revision. Use this fence for the next source edit. Replaying the same split
request returns the original fence, even after a later source edit. Stored
split receipts keep their existing JSON fields.

Every successful person edit advances the document-person binding epoch.
Document links go stale until the daemon backfill republishes them. The
authority rows remain available during that rebuild.

MCP offers the person reads only; edit people through the CLI or HTTP API.
The CLI and MCP use the daemon, so they never open the vault directly.
