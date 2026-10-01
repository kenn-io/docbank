---
last_edited: 2026-09-29
title: People
description: List and edit canonical people through the daemon API and CLI, and read them through MCP.
---

# People

Docbank keeps one canonical person record for each person in the vault. The
daemon API and CLI can list, search, create, rename, retire, merge, and split
these records. MCP can find and read them. Every existing-person edit uses the
revision returned by the preceding read.

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
person. It does not list the person's custodian assignments.

Split needs the ID of each custodian assignment it moves. The only listing of
assignment IDs is per package: `GET /api/v1/packages/by-id/{package_id}/custodians`
or `list_package_custodians` in MCP. Split by assignment therefore works only
for package custodian claims.

## Edit people

Edits address the person's current `person_id`. A read through a merged
person's ID returns the survivor, but an edit through that ID returns 404; send
the edit to the `person_id` in the read response.

Rename and retire commands require `--revision`. HTTP clients send the
same value in `If-Match`. A stale value returns `412 stale_revision`.
Retirement keeps historical assertions but removes the person from active
list and search results.

Merge sends the survivor revision in `If-Match` and the absorbed revision
in the request body. Merge and split require an operation UUID. Retrying the
same request returns its saved receipt. A different request with the same UUID
returns `person_merge_conflict`.

A merge returns `422 person_merge_too_large`, and changes nothing, when:

- the merged person would exceed the per-person limit on identities or
  external UIDs, or
- the absorbed person has more linked records than one merge receipt holds.
  The receipt holds 256 KiB of moved IDs, about 6,900 identities, custodian
  assignments, document assertions, and open match candidates combined.

Split moves only the identity IDs, custodian assignment IDs, or external UIDs
listed in the request. The new display name is required. The store validates
that every selected member belongs to the source person. A split request
larger than 1 MiB returns `413` and changes nothing.

The split response includes `source_revision_after` and an ETag for that
revision. Use this fence for the next source edit. Replaying the same split
request returns the original fence, even after a later source edit.

After every successful person edit, the daemon rebuilds the links between
documents and people in the background. Until that finishes, document views can
show the earlier links. The person records themselves are already current.

MCP offers the person reads only; edit people through the CLI or HTTP API.
The CLI and MCP use the daemon, so they never open the vault directly.
