---
last_edited: 2026-10-05
title: People
description: List and edit canonical people through the daemon API and CLI, and read them through MCP.
---

# People

Docbank keeps one canonical person record for each person in the vault. The
daemon API and CLI can list, search, create, rename, retire, merge, and split
these records. MCP can find and read them. Every edit to an existing person
uses the revision returned by the preceding read.

## List and search

`GET /api/v1/people?query=ada&limit=100` lists active people in folded
display-name order. The CLI equivalent is:

`docbank people list ada`

MCP clients use `find_people`. Cursors are bound to the query and the
requested person page.

`GET /api/v1/people` and `find_people` include `exact_match: true` when the
returned page contains a name equal to the query after Unicode folding. For
example, searching for `ada` matches `Ada`. The field is omitted when the page
has no exact match.

## Create and inspect

Create an operator-owned person with `docbank people create "Ada Lovelace"`.
Read it with `docbank people show <person-id>` or
`GET /api/v1/people/by-id/{person_id}`. The detail response includes identity
IDs, external UIDs, the current revision, `key_face_id`, and up to 1,000 assertion selectors. Pass `assertions_next_cursor` as `people show --assertion-cursor` to continue, and use `--assertion-limit` to choose a page size. Selectors include document assertions and retained photo versions; visibility filtering happens before the page limit. Hidden photo selectors stay locked until an unlock. The response also identifies the ID used to reach a merged person. It does not list the person's custodian assignments.

Split needs the ID of each custodian assignment it moves. The only listing of
assignment IDs is per package: `GET /api/v1/packages/by-id/{package_id}/custodians`
or `list_package_custodians` in MCP. Split by assignment therefore works only
for package custodian claims.

## Edit people

Edits address the person's current `person_id`. A read through a merged
person's ID returns the survivor, but an edit through that ID returns 404. Send
the edit to the `person_id` in the read response.

Rename and retire commands require `--revision`. HTTP clients send the
same value in `If-Match`. A stale value returns `412 stale_revision`.
Retirement keeps historical assertions but removes the person from active
list and search results.

Merge sends the survivor revision in `If-Match` and the absorbed revision
in the request body. Merge and split require an operation UUID. Retrying the
same request returns its saved receipt. A different request with the same UUID
returns `person_merge_conflict`.

A merge that would assign a rejected person to a face returns `409 person_merge_conflict`. Review the returned visible face IDs before retrying. Locked Hidden, excluded, and trashed conflicts return notices without those face IDs. Reinclude or restore the photo to correct excluded or trashed evidence. Rejected matches follow the surviving person.

A merge returns `422 person_merge_too_large`, and changes nothing, when:

- the merged person would exceed the per-person limit on identities or
  external UIDs, or
- the absorbed person has more linked records than one merge receipt holds.
  The receipt holds 256 KiB of moved IDs, about 6,900 identities, custodian
  assignments, document assertions, and open match candidates combined.

Split moves only the identity IDs, assertion IDs, custodian assignment IDs, or external UIDs listed in the request. Use `people split --assertion <assertion-id>` for a document or photo assertion. Its faces and selected key face move to the new person in the same transaction. Add `--unlock-hidden` to `people show` or `people split` to enter the passcode for Hidden evidence. The new display name is required. The store validates
that every selected member belongs to the source person. A split request
larger than 1 MiB returns `413` and changes nothing.

The split response includes `source_revision_after` and an ETag for that
revision. Send that revision with the next edit to the source person.
Replaying the same split request returns the original revision, even after a
later source edit.

After every successful person edit, the daemon rebuilds the links between
documents and people in the background. Until that finishes, document views can
show the earlier links. The person records themselves are already current.

MCP can read people but cannot edit them. Edit people through the CLI or HTTP
API. The CLI and MCP go through the daemon and do not open the vault directly.
