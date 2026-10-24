---
last_edited: 2026-10-22
title: People
description: Browse people and their photos, rename or merge people, and manage canonical records through the API and CLI.
---

# People

Docbank keeps one canonical person record for each person in the vault. The
daemon API and CLI can list, search, create, rename, retire, merge, and split
these records. MCP can find and read them. Every edit to an existing person
uses the revision returned by the preceding read.

## People in Photos

Open **Photos → People** to browse the people in your visible photos, 24 cards at a time. Each card shows an assigned face and the number of Library photos depicting that person. Several faces or paired RAW and JPEG files count as one photo. A card shows a person icon when no readable assigned face is available. People with no visible photo, such as email contacts, get no card.

Select a card to open that person's Library photos, or their Hidden photos when they have none in Library. While Hidden is unlocked, a separate Hidden count opens that person's Hidden photos. Hidden-only people appear while Hidden is unlocked and clear when it locks or expires. A readable key face supplies the portrait, including Hidden while unlocked; otherwise Docbank prefers the newest assigned Library face.

Use **Rename** to change the name. The list reloads its first page and applies the current search again. A renamed person disappears from the results if their new name no longer matches.

To merge two people, check **Select** on both cards, then choose **Merge**. Your selection stays while you search, so you can select people from different searches. Choose **Keep this one** on the person to keep, then confirm. The survivor keeps their name and receives the other person's records. The list reloads its first page with current counts. When both people have the same name and the same detail, the dialog also shows each person ID. Person filters chosen from a card or the face list follow renames and merges. A `person:` name typed in search keeps the old name, so update the search.

If someone else changed or removed either person first, the list reloads its first page; review the current cards and select again. A rejected name keeps your selection, and a failed merge keeps both people selected. An uncertain merge result also reloads the list so you can see whether it completed.

A merge can stop when a face assigned to one person was rejected for the other. The error lists each visible conflicting face with its content version. Use the CLI or API key that made the rejection to review it:

1. Read the face with `docbank photos faces list <content-version-id>`.
2. Withdraw the rejection with `docbank photos faces withdraw-rejection <content-version-id> --face <face-id> --person <rejected-person-id> --revision <face-revision>`.
3. Select both people again and retry the merge.

A browser merge is refused while Hidden is locked if either person has Hidden photos. Unlock Hidden and retry the merge.

Locked Hidden, excluded, or trashed conflicts return notices without face IDs. Unlock, reinclude, or restore the photo before reviewing its rejection. Locking Hidden clears the selection and dialog.

Name pickers in the inspector list people by their newest active, visible photo assertion, then by name. Namesakes stay together and share their group's newest assertion time. Naming a person on a new photo moves the group up. Adding another face or a whole-photo mark for someone already named on that photo keeps the assertion's time. A merge preserves retained and moved assertion times.

## List and search

`GET /api/v1/people?query=ada&limit=100` lists active people in folded
display-name order. Search matches the beginning of any word, so `Smith` finds `John Smith` and `John—Smith`. The CLI equivalent is:

`docbank people list ada`

MCP clients use `find_people`. Cursors are bound to the query, requested projection and ordering, and Hidden state they were read under. An invalid cursor or one from another query or Hidden state returns `people_cursor_expired`.

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
