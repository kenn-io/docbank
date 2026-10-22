---
last_edited: 2026-10-21
title: People
description: Browse people and their photos, rename or merge people, and manage canonical records through the API and CLI.
---

# People

Docbank keeps one canonical person record for each person in the vault. The
daemon API and CLI can list, search, create, rename, retire, merge, and split
these records. MCP can find and read them. Every edit to an existing person
uses the revision returned by the preceding read.

## People in Photos

Open **Photos → People** to browse everyone visible to your browser session, 24 cards at a time. Each card shows an assigned face and the number of Library photos depicting that person. Several faces or paired RAW and JPEG files count as one photo. People with document evidence or no photo evidence still have a card, with a person icon when no readable assigned face is available.

Select a card to open that person's Library photos. While Hidden is unlocked, cards also show a separate **Hidden** photo count that opens the same person in Hidden. If Hidden locks before that view opens, it opens without the person filter. Hidden-only people and their counts clear when Hidden locks or expires. A readable key face supplies the portrait; otherwise Docbank uses the newest readable assigned face, from Library first and then from unlocked Hidden.

Use **Rename** to change the name. The card updates in place, so the people you have already loaded stay on screen.

To merge two people, check **Select** on both cards, then choose **Merge**. Your selection stays while you search, so you can select people from different searches. The dialog shows both cards side by side with their faces and counts. Choose **Keep this one** on the person to keep, then confirm. The survivor keeps their name and receives the other person's records; the other card disappears and the survivor's counts refresh. When both people have the same name and the same detail, the dialog also shows each person ID.

If someone else changed either person first, the dialog shows both current cards and asks you to confirm again. If either person is no longer available, the dialog closes and you select again. If a merge result can't be confirmed, **Retry merge** sends the same request to retrieve the saved result.

A merge can stop because a face is linked to one person and marked "not" the other. The dialog lists each such face you can open, including faces on older versions of a photo and faces whose name was cleared. Use **Withdraw "not …"** on a face and confirm to remove that rejection; the face keeps its name. Then confirm the merge again. Faces in locked Hidden photos or in excluded or trashed photos are reported but not listed. Locking Hidden clears the selection, Hidden counts, and any open dialog.

Name pickers in the inspector list people with the newest face or photo evidence first, then by name. Namesakes stay together. This order comes from the vault, so it survives sign-in and follows people through merges. It reflects when evidence was added: naming a face that was already linked doesn't move the person up.

## List and search

`GET /api/v1/people?query=ada&limit=100` lists active people in folded
display-name order. Search matches the beginning of any word, so `Smith` finds `John Smith` and `John—Smith`. The CLI equivalent is:

`docbank people list ada`

MCP clients use `find_people`. Cursors are bound to the query and the
requested person page.

## Browser names and visibility

Namesakes share one chooser, including case variants such as `Alex` and `ALEX`. The chooser lists every candidate with a visible email, phone, or scoped handle, or their creation date when no identity is available. Each row includes the full person ID. Select an existing person or **New person named …**. **Mark person in photo** also accepts a new name through **Create person** and saves the person and whole-photo mark together. Undoing a face's new-name decision through the API or CLI retires the new person unless it was edited or has other evidence. Clearing a name or a whole-photo mark leaves the person.

Choose **Photos** on a candidate to load up to three linked-photo previews in the current Hidden scope. Only one candidate expands at a time; opening the chooser loads no previews. Linked photos follow timeline order and may include the current photo. Missing grid previews are prepared from up to three linked-photo candidates for the expanded person. Switching candidates cancels the previous requests and clears their previews.

Previews crop the assigned face from its exact current source version, including a paired RAW with different framing. Replacing a source member removes its old face evidence from the chooser and person Library filter. When identity details distinguish the choices, **Whole photo** labels evidence without a face box. When details tie, only matching face crops appear; **Preview unavailable** means no assigned crop could be prepared.

- Browser name search omits people whose only evidence is sensitive document actors or blind-copy assertions, even with Hidden unlocked.
- Browser typeahead hides people whose only evidence is in locked Hidden photos until you unlock Hidden.
- Browser results require at least one of:
    - A retained depicted-photo assertion on a visible photo.
    - An active non-blind-copy operator assertion or suppression on a visible document. These names are searchable before document publication.
    - A published non-sensitive document link.
    - A manually created unlinked person, including imported people.
- Clearing a face name retains its photo evidence. Browser search hides that person while its only depicted evidence is in locked Hidden photos and restores the same person after unlock.
- Automatically provisioned exact-identifier people stay hidden while processing is pending or failed, until a visible assertion or non-sensitive published document link supports their name.
- Unlinked people with supplied identities remain searchable with blank identity labels.
- When Hidden locks or expires, open name results, identity labels, and namesake previews clear.
- API-key, CLI, and MCP name searches include all active people, including people whose only photo evidence is Hidden.
- The picker explains that omitted people may share the typed name. You can create the person directly and merge a duplicate after unlock.

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
