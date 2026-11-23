---
last_edited: 2026-10-22
title: People
description: Browse people and their photos, rename or merge people, and manage canonical records through the API and CLI.
---

# People

Docbank keeps one canonical person record for each person in your vault. You can
list, search, create, rename, retire, merge, and split these records with the
daemon API or CLI, and MCP can find and read them. Whenever you edit an existing
person, you pass the revision returned by the preceding read.

## People in Photos

To browse the people in your visible photos, open **Photos → People**. The grid
loads 24 cards at a time, and each card shows an assigned face along with how
many Library photos depict that person. Multiple faces of the same person on one
image, or paired RAW and JPEG files, count as a single photo. When no readable
assigned face is available, the card displays a person icon instead. People with
no visible photos, such as email contacts, do not appear here.

Select a card to open that person's Library photos, or their Hidden photos if
they have none in Library. While Hidden is unlocked, a separate Hidden count
lets you open that person's Hidden photos directly, and Hidden-only people
appear in the grid until Hidden locks or expires. For the card portrait, Docbank
uses a readable key face, including a Hidden one while Hidden is unlocked.
Otherwise, Docbank prefers assigned Library faces over Hidden faces, then a
ready preview, and then the largest face box.

If another tab or the CLI changes people or photos while you have the People
page open, the page updates in place and keeps your search, loaded cards, and
selection. See [Live refresh](photos.md#live-refresh).

To change a person's name, choose **Rename**. The list reloads from its first
page and reapplies your current search. If the new name no longer matches, that
person disappears from the results.

To merge two people:

1. Check **Select** on both cards. Your selection stays active while you search,
   so you can pick people from different searches.
2. Choose **Merge**.
3. Under **Person to keep**, choose the **Keep \<name\>** option for the person
   you want to keep.
4. Check **Confirm merge**, then choose **Merge**.

The surviving person keeps their name and receives the other person's records,
and the list reloads its first page with current counts. When both people have
the same name and the same detail, the dialog also shows each person ID. Person
filters chosen from a card or the face list follow renames and merges, while a
`person:` name typed in search keeps the old name, so update the search.

If someone else changes or removes either person first, the list reloads its
first page; review the current cards and select again. A rejected name keeps
your selection, and a failed merge keeps both people selected. If a merge result
is uncertain, the list also reloads so you can see whether it completed.

A merge can stop when a face assigned to one person was rejected for the other.
The error lists each visible conflicting face with its content version. Review
it with the CLI or an API key; the browser cannot withdraw a rejection:

1. Read the face with `docbank photos faces list <content-version-id>`.
2. Withdraw the rejection with `docbank photos faces withdraw-rejection <content-version-id> --face <face-id> --person <rejected-person-id> --revision <face-revision>`.
3. Select both people again and retry the merge.

If either person has Hidden photos while Hidden is locked, the browser refuses
the merge. Unlock Hidden and retry. Browser sessions receive merge receipts with
zero moved-evidence counts and an empty absorbed name.

Conflicts involving locked Hidden, excluded, or trashed photos return notices
without face IDs. To review one of those rejections, unlock, reinclude, or
restore the photo first. Locking Hidden clears your selection and closes the
dialog.

In the inspector, name pickers order people by their newest active, visible
photo assertion, then by name. Namesakes stay grouped together and share their
group's newest assertion time. Naming a person on a new photo moves their group
up, while adding another face or a whole-photo mark for someone already named on
that photo keeps the assertion's time. Merges preserve retained and moved
assertion times.

## Face suggestions

To turn on local face suggestions, run `docbank processing faces`. The command
shows the missing download size before installing the face model. Registration
and ordinary daemon startup only reuse installed files and do not approve
downloads.

The background job embeds detected faces and boxes you draw on current image and
RAW files, including Hidden photos. When JPEG is the display image, a paired RAW
still keeps its own face crops, and each face's box, measurements, and name stay
intact. Faces that the model cannot align remain available for manual naming and
do not appear in suggestions.

In **Photos → People**, **Suggestions** shows likely matches and groups of
similar unnamed faces. Open a card to review its crops:

- For a named group, choose **Not X** on the sheet to reject the group and close
  the sheet. Rejecting a named group keeps that exact group rejected.
- For an unnamed group, choose an existing person or type a new name. Choose
  **Skip** to leave an unnamed face out of this review, or **Dismiss** to keep
  that exact group rejected while its faces remain available for later matches.
- On an individual face, choose **Not X** to keep that face from being suggested
  for the same person again. Rejecting that face closes the sheet. Reopen the
  named group to confirm its remaining faces.

Your decisions survive backup and restore.

When the selected face has a vector, the inspector's name picker puts similar
people first while keeping namesakes together. Faces without a vector keep the
usual ordering. Suggestions, crops, and similarity ranking follow your current
Hidden access scope. When the vault contains Hidden photos, locking Hidden
withholds a named group unless its remaining faces match the named reference
faces currently visible in Library. To review groups supported only by Hidden
references, unlock Hidden.

## List and search

`GET /api/v1/people?query=ada&limit=100` lists active people in folded
display-name order. Search matches the beginning of any word, so `Smith` finds
`John Smith` and `John—Smith`. The CLI equivalent is:

`docbank people list ada`

MCP clients use `find_people`. Cursors bind the query, projection, and ordering.
Invalid or mismatched cursors return `cursor_expired`; restart from the first
page.

## Browser names and visibility

Namesakes share one chooser, including case variants such as `Alex` and `ALEX`.
The chooser displays every candidate with a visible email, phone, or scoped
handle, or their creation date when no identity is available. Each row also
includes the full person ID. Select an existing
person or **New person named …**. **Mark person in photo** also accepts a new
name through **Create person** and saves the person and whole-photo mark
together. If you undo a face's new-name decision through the API or CLI, Docbank
retires the new person unless it was edited or has other evidence. Clearing a
name or a whole-photo mark leaves the person record intact.

To view linked photos for a candidate, choose **Photos**. Docbank loads up to
three linked-photo previews in the current Hidden scope, in timeline order,
which may include the current photo. Opening the chooser loads no previews on
its own, and only one candidate expands at a time. When grid previews are
missing, Docbank prepares them from up to three linked-photo candidates for the
expanded person. Switching candidates cancels the previous requests and clears
their previews.

Previews crop the assigned face from its exact current source version, including
a paired RAW with different framing. Replacing a source member removes its old
face evidence from the chooser and from the person Library filter. When identity
details distinguish the choices, **Whole photo** labels evidence without a face
box. When details tie, only matching face crops appear, and **Preview
unavailable** means no assigned crop could be prepared.

- Browser name search omits people whose only evidence is sensitive document
  actors or blind-copy assertions, even with Hidden unlocked.
- Browser typeahead hides people whose only evidence is in locked Hidden photos
  until you unlock Hidden.
- Browser results require at least one of:
    - A retained depicted-photo assertion on a visible photo.
    - An active non-blind-copy operator assertion or suppression on a visible
      document. These names are searchable before document publication.
    - A published non-sensitive document link.
    - A manually created unlinked person, including imported people.
- Clearing a face name retains its photo evidence. Browser search hides that
  person while its only depicted evidence is in locked Hidden photos and
  restores the same person after unlock.
- Automatically provisioned exact-identifier people stay hidden while processing
  is pending or failed, until a visible assertion or non-sensitive published
  document link supports their name.
- Unlinked people with supplied identities remain searchable with blank
  identity labels.
- When Hidden locks or expires, open name results, identity labels, and namesake
  previews clear.
- API-key, CLI, and MCP name searches include all active people, including people
  whose only photo evidence is Hidden.
- The picker explains that omitted people may share the typed name. You can
  create the person directly and merge a duplicate after unlock.

## Create and inspect

To create an operator-owned person, run `docbank people create "Ada Lovelace"`.
Read it with `docbank people show <person-id>` or
`GET /api/v1/people/by-id/{person_id}`.

The detail response includes identity IDs, external UIDs, the current revision,
`key_face_id`, and up to 1,000 assertion selectors. To continue paging, pass
`assertions_next_cursor` as `people show --assertion-cursor`, and use
`--assertion-limit` to choose a page size. Selectors include document assertions
and retained photo versions, with visibility filtering applied before the page
limit. Hidden photo selectors stay locked until an unlock. The response also
identifies the ID used to reach a merged person, though it does not list the
person's custodian assignments.

Split needs the ID of each custodian assignment it moves. Because assignment IDs
are listed only per package through
`GET /api/v1/packages/by-id/{package_id}/custodians` or
`list_package_custodians` in MCP, split by assignment works only for package
custodian claims.

## Edit people

Always address edits to the person's current `person_id`. Reading through a
merged person's ID returns the survivor, but editing through that ID returns
404. Send the edit to the `person_id` in the read response.

Rename and retire commands require `--revision`, and HTTP clients send the same
value in `If-Match`. A stale value returns `412 stale_revision`. Retirement
removes the person from active list and search results while keeping historical
assertions.

For a merge, send the survivor revision in `If-Match` and the absorbed revision
in the request body. Both merge and split require an operation UUID. Retrying
the same request returns its saved receipt, while a different request with the
same UUID returns `person_merge_conflict`.

If a merge would assign a rejected person to a face, it returns
`409 person_merge_conflict`. Review the returned visible face IDs before
retrying. Locked Hidden, excluded, and trashed conflicts return notices without
those face IDs; to fix excluded or trashed evidence, reinclude or restore the
photo first. Rejected matches follow the surviving person.

A merge returns `422 person_merge_too_large` and changes nothing when:

- The merged person would exceed the per-person limit on identities or external
  UIDs.
- The absorbed person has more linked records than one merge receipt holds. One
  receipt holds 256 KiB of moved IDs, about 6,900 identities, custodian
  assignments, document assertions, and open match candidates combined.

Split moves only the identity IDs, assertion IDs, custodian assignment IDs, or
external UIDs listed in the request, and requires a new display name. To move a
document or photo assertion, use `people split --assertion <assertion-id>`. Its
faces and selected key face move to the new person in the same transaction. To
enter the passcode for Hidden evidence, add `--unlock-hidden` to `people show`
or `people split`. The store validates that every selected member belongs to the
source person. A split request larger than 1 MiB returns `413` and changes
nothing.

The split response includes `source_revision_after` and an ETag for that
revision. Send that revision with the next edit to the source person. Replaying
the same split request returns the original revision, even after a later source
edit.

After each successful person edit, the daemon rebuilds the links between
documents and people in the background. Person records are already current,
while document views can show the earlier links until that rebuild finishes.

MCP can read people but cannot edit them. Make edits through the CLI or HTTP
API. The CLI and MCP go through the daemon and do not open the vault directly.
