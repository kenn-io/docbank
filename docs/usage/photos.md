---
last_edited: 2026-10-14
title: Photo assets
description: Group camera files, browse photos, and organize albums in the web app, CLI, or HTTP API.
---

# Photo assets

Docbank groups ordinary file nodes into photo assets. Each file node and its
content versions still hold the bytes. An asset stores only membership, roles,
display selection, visibility, exclusion, and bounded decision receipts.

Image files and files with a concrete `video/*` MIME type are enrolled when
they are created. Audio, generic video, and generic RAW files stay ordinary
files until an operator promotes or creates an asset explicitly. Enrollment
applies only to new files. Adding this feature does not scan older files.

Replacing or reverting a file's content keeps the file in its asset with the
same role, even when the new media type would not qualify. A file whose new
content qualifies is not enrolled either. Membership changes only through
explicit asset operations or permanent node deletion.

An asset can contain `raw`, `image`, `video`, and `sidecar` members. The
default display order is RAW, image, then video. A vault preference can select
image before RAW, and an asset override wins over the vault preference.
Sidecars never display and must point at a RAW or image member in the same
asset. Removing the selected member chooses another displayable member
atomically, or stores a null display when none remains. Assets are limited to
256 files.

## Add photos in the web app

In **Photos > Library**, choose **Add photos** and set the vault destination, which defaults to `/`. Use **Choose folder** or **Choose files**, then start the import. An invalid destination fails before uploading. Changing the destination replans the selected files when you click **Add N groups**.

RAW, JPEG, and XMP files in the same folder with the same name form one photo. Videos stay separate. The drawer lists skipped files with their reasons, including unsupported types, invalid names or sizes, and duplicate relative paths. Each selection accepts up to 32,768 files, and its path-and-size declaration must fit the 8 MiB message limit.

The drawer reports additions, files already in the vault, ambiguous groups, and failures. Photos, album counts, and the plain Documents directory refresh after the import. Retained searches, tag filters, and frozen snapshots stay unchanged.

Imported groups share one collection named `browser: Card` when you pick a folder named `Card`, or `browser: Choose files` for individual files. Reusing a filename with changed bytes creates a separate file node, including when a browser client reuses a saved source identity. The existing file keeps its content and current version. Existing pairing rules decide whether the new file joins the same photo or a separate photo; identical RAW, image, and video content still returns already in vault. A lone XMP stays an ordinary file and is added again when picked in a new selection.

A lost receipt leaves the group unconfirmed. Reload the page, reopen **Add photos**, choose **Resume import**, and reselect the same files or folder. Docbank checks every file path and content hash before uploading, then retains the original source identity so the folder's bursts stay together. Resume survives a local daemon restart in the browser profile that saved the import. A persistent cookie locates the selection digest saved privately in that vault; clearing cookies removes access to it. The daemon uses a random host beneath `.docbank.localhost` to scope that cookie. Its earlier host was already random for each daemon run.

**Choose folder** and **Choose files** start a new selection. When a directory contains a photo with the same bytes as an earlier browser import, its new siblings join that folder's burst groups and share stack decisions with those earlier photos, even if the matching file has a different name. Directories without a matching photo keep their own source. When a directory matches several earlier folders, its new photos keep the fresh selection identity automatically. Exact duplicates retain their earliest folder provenance. Each selected directory resolves independently.

If an unfinished import is saved, Docbank asks before replacing it. Cancel keeps it available through Resume import. Saved imports from an unsupported format stay intact and require a compatible Docbank version to resume.

Identical RAW, image, and video content returns as already in vault. Other files, including lone XMP sidecars, may be added again. Failed groups remain available through **Add N groups**.

If cookies or checkpoint storage are unavailable, a notice lets you import for this session. Keep the tab open: this import cannot resume after closing it. Authentication failures, progress changed in another tab, and unsupported saved formats still block import and preserve the saved selection.

The browser checks the 32,768-file and 8 MiB JSON declaration limits before asking to replace saved progress or reading photo content. Changing the destination checks the declaration again.

Web imports run in the open tab. Control them in the drawer. **Stop after this group** finishes the current group before stopping the import.

Keep the tab open until the import finishes. This also works when you sign in to hosted Docbank with an API key. For large camera cards already on the daemon's computer, use `docbank photos import`.

## Record faces and people

Use a photo file's `current_version_id` from `docbank stat <path-or-id> --json` to record an observation:

```sh
docbank photos faces create <content-version-id> --box 0.1,0.2,0.3,0.4 --person <person-id>
docbank photos faces create <content-version-id> --person <person-id>
docbank photos faces list <content-version-id>
```

A box is `x,y,width,height`, each measured as a fraction of the EXIF-oriented preview before authored rotation. Omit `--person` for an unnamed face, or omit `--box` to mark a person in the whole photo. Naming a face records the same `depicted` person link as a whole-photo mark. Multiple faces of one person share that link. List returns up to 1,000 face boxes and 1,000 depicted-person records, including whole-photo marks and suppressed records referenced by a face. A photo holds at most 1,000 faces and 1,000 depicted people; a create past either limit fails.

A boxed create generates a face UUID. Retain the returned `face_id`, or use `--face-id <uuid>` to supply one. Retry with that same ID, box and version to recover the saved result. A supplied person must match the saved canonical person. A retry still succeeds after that person retires. A boxed command includes the retry ID after a connection failure, a server failure, or an unreadable success response. Conflicting reuse fails.

Name an existing box with `faces name`, using its face revision.

Read face revisions with `faces list` before correcting a name:

```text
docbank photos faces name <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces retract <content-version-id> --face <face-id> --revision REV
docbank photos faces reject <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces withdraw-rejection <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces dismiss <content-version-id> --face <face-id> --revision REV
docbank photos faces key <content-version-id> --face <face-id> --person <person-id> --revision REV --person-revision REV
docbank photos faces undo <receipt-id>
```

Each correction returns `receipt_id`. The CLI prints an operation UUID before sending a decision or undo; retry with the same inputs and `--operation-id` to recover its receipt after a lost response. HTTP decisions accept `operation_id` in the body; undo accepts it as a query parameter. Undo restores changed records while their saved revisions still match; edits to unrelated faces stay intact. A later edit returns `412 stale_revision`; read again before deciding. Undo also returns a receipt, so it can be undone. A rejected assigned person loses that face assignment atomically. Naming clears that face's rejection and dismissal while preserving decisions for other faces in a group. List returns `rejected_person_ids` and `dismissed`. Each face allows up to 1,000 rejected people; another rejection returns `409 photo_face_limit`.

Retracting the last face assignment leaves a neutral `withdrawn` assertion unless an independent whole-photo mark remains. Assertions expose `photo_mark` for that mark; withdrawing it through the Go store method `Store.AssertDocumentPerson` clears it. HTTP and CLI currently cannot withdraw a standalone whole-photo mark. Withdrawn assertions leave room for new depicted people. Explicit suppressions remain separate. Withdraw a rejection without changing the face's assignment with `faces withdraw-rejection`. Clear a key face with `faces key` and omit `--face`, using only `--person-revision`. Read the key face with `people show`; choosing a visible replacement works while the old key's photo is locked.

MCP `get_photo_faces` reads up to 100 face boxes and depicted-person records per page, reducing the page size to fit its response byte limit. Each named face includes its `person_id` and `action`. Pass `next_offset` as `offset` for the next page. MCP can't unlock Hidden photos. Manual faces and whole-photo marks use HTTP or the CLI.

New faces require the addressed current, live, included image or RAW member. Retained image and RAW versions remain readable and correctable until purge. Sidecars and replaced non-image versions cannot receive boxes or whole-photo marks. Face reads and manual writes require an unlock while the photo is Hidden. Audited vault writes return `audit_mutation_unsupported`. These commands record observations manually. Face detection is unavailable. Face boxes stay fixed; person decisions can be corrected and undone.

## Open and inspect a photo

Double-click a photo to select it and open it above Library. Enter
opens the photo while preserving any separate selection.
On touch, tap a photo to open it when nothing is selected. Use the checkbox to
start a selection. Taps then toggle selection. Double-tap opens a photo while a
selection is active, and also selects it if it wasn't selected. The Open control
stays reachable for screen readers and keyboard users.

Use the arrow keys, Previous and Next buttons, or the thumbnail rail to move
through the current result in query order, across calendar-month groups. Navigation loads the next page at the loaded edge
and stops at the query boundary. If loading fails, Retry navigation resumes it
inside the viewer, including after a cursor expires. Press I for available camera and exposure
details. Wheel, pinch, or double-click to zoom. Use Reset zoom to reset. Escape
returns to the scroll position and focus left on entering the viewer, including
when Forward reopens it.

The viewer shows the cached grid preview first, then a fit preview. Zoom loads
a large preview while preserving the zoom. It prepares only two neighboring
fit previews in each direction. A failed upgrade keeps the useful image. Retry
preview retries a failed request. Retained failed or unsupported previews have
no Retry; download the photo from Documents.

Use Reload photo after a temporary opening failure or when the photo version, display, or Library eligibility changes. Reloading unchanged content restarts preview loading, clears the failure, and keeps the zoom.

View original reads verified JPEG, PNG, or WebP bytes through the browser's
existing download path. Originals are limited to 32 MiB, 16,384 pixels per side,
and 40 million pixels. Use verified download in Documents for larger images.
Animated WebP originals may animate in the browser. RAW photos always use
server-produced JPEG previews.

A `/photos#photo=<asset-id>` URL opens that photo directly; an album URL such
as `/photos/albums/<album-id>#photo=<asset-id>` works the same way. Direct views
have no query navigation or thumbnail rail. Close returns to Library or to the
album in the URL. Back and
Forward also update the viewer. Browser sessions stay in memory. After a reload,
run `docbank web` for a fresh session, then append `&photo=<asset-id>` to its
fragment to reopen the direct photo.

## Edit metadata

Metadata editing currently works in the web app; CLI and MCP commands aren't available. Press I or choose Metadata to open the viewer's metadata panel. It edits rating, flag, color label, caption, creator, copyright, rotation, and keywords. Whole pair edits every image or RAW member together. Choose a member to edit it alone. Mixed means members have different values. Stage fields and keyword changes, then choose Apply. Reloading or closing the page asks before discarding staged edits. Clear caption, creator, or copyright sets mixed text to empty.

Select photos in the grid and choose Metadata in the bottom action bar to edit the selection. Select all results to edit the current search, including photos beyond the loaded page. Each edit allows at most 1,000 files, including both members of a pair. The editing session keeps the resolved photos through result refreshes until you change the selection, search, or inspected photo. Counts, checkboxes, album actions, drag, shortcuts, and trash use those same photos. After partial trash, failed photos stay selected with their captured revisions, including unloaded photos; retry them or choose Clear selection. A failed edit changes nothing. After a conflict, Reload metadata preserves your draft for review and an explicit retry. Keywords use the existing tag picker.

In Photos, 0 through 5 set the rating, P picks, X rejects, and U clears the flag. 6 sets red, 7 yellow, 8 green, and 9 blue. R rotates each selected photo 90° clockwise, and V sets purple. Use the inspector for other rotation values or No label. A shortcut applies any staged fields and keywords together with its decision; the Undo feedback names those edits. Apply or Clear edits before switching between pair members. Typing in a text field keeps its normal shortcuts. Edits update open photo lists in place. A list with search text or a rating, flag, label, keyword, or modified-date filter reloads after each edit, so an edited photo can leave it. Map also reloads after a rotation or its Undo. Undo or Ctrl/Cmd+Z restores the last edit, including keyword changes. Undo uses conservative revision checks. It reports a conflict if an edited file has changed; keyword Undo also conflicts after any revision change to an affected document. Reload metadata before making another edit.

The viewer refuses edits until its photo has loaded, and refuses them if the photo was replaced after the viewer loaded it. After a replacement, the viewer shows the new photo and drops the refused shortcut. Edits staged in the Metadata panel stay until you choose Reload metadata.

Rotation applies to the displayed photo in the grid, viewer, and album cover without rebuilding previews. Hidden photos require an active unlock for editing and Undo. Undo after hiding a photo asks you to unlock Hidden and keeps Library signed in.

## Browse in the web app

Open `docbank web` and choose **Photos** in the sidebar. Library opens at
`/photos`, newest captures first. A photo without a readable capture date sits
in the month its file was last modified, when Docbank recorded that time; the
viewer then shows File modified and leaves Captured blank. The rest appear under
Undated. The year buttons jump to the years loaded so far; scrolling loads
more photos and reveals older years. Choose **Load more** to continue from
the same position when further results remain.

Choose **Timeline** to see counts for every recorded capture day in the current
scope, including days beyond the loaded grid. The year ribbon shows each year's
total and relative density. Choose a year, month, or day to open its photos.
The grid pages within that range. Once its first page arrives, the grid clears
selection and starts at the top. A failed date change keeps the previous photos,
date, selection, and scroll position. **Retry** requests the chosen range again.
**Clear date** restores the full scope. Counts stay tied to the full
scope until you refresh.

Dates use the selected display file's recorded calendar day, so a capture near
midnight keeps its date regardless of the browser's timezone. A RAW/JPEG pair
counts once. A photo without a capture date counts on the UTC day its file was
last modified. When the same bytes were seen several times, the first
sighting that recorded a modified time decides, so re-importing copies never
moves a photo. Reverting a photo
restores the date of the original bytes; a photo replaced by content recorded
without a modified time is undated until reverted. The Undated total includes
photos with neither.
Timeline counts every included photo asset, even in a vault with many ordinary
documents. It shows a limit message when the scope spans more than 250,000
distinct capture days or the count runs past its time budget, and offers Retry.
A busy message asks you to retry after other timeline requests finish. Loading
the timeline does not block Documents queries.

Search filenames, camera or lens names, and location labels in the Photos
search box. Search opens in relevance order, with one tile per asset even
when several paired files match. Choose Capture date to
return to date browsing. Relevance shows the best 250 matches and the full total. Refine the search or sort by capture date to see all matches. Equal-score ties use capture placement descending, then asset ID.

Camera, Lens, Year, Location, and Albums narrow the grid. Counts cover the
whole matching library, including photos beyond the loaded page. Tiles appear before the separate count request finishes. Counts have a 30-second budget. Unavailable counts show their reason. Choose Retry counts after a timeout. Member or byte limits require a narrower search or filters. Each facet
omits its own selected filter while retaining search and the other filters.
Camera, lens, year, and location describe the selected display file; year
uses its recorded local capture date, or the UTC day of its modified time when it has none. Text may match any live member of a
pair, so a text match can appear under a different display-file camera or lens. Camera and lens count both make and model; their counts can sum to more than the asset total. Equivalent Unicode-folded values share one bucket and count each asset once. Buttons submit an original valid spelling; oversized metadata stays visible as informational counts. Year counts omit a selected whole-year range and retain other date ranges. Albums show live memberships. Choose Clear filters to reset the
search and facets. Clear filters appears when a search or filter is applied. A facet group disappears when none of the photos have a value for it and no value in it is selected. Changing a facet restores the applied search text. Changing scope clears selection and starts a fresh grid.

Choose Months or Capture sessions to group the grid. A session joins captures
with gaps of four hours or less. Compact, Comfortable, and Large change the
grid density. The browser remembers the density across reloads and fresh
`docbank web` links until the daemon restarts on a new address.

Click a photo to select it. Shift-click adds the range from the previous
selection, including loaded photos outside the screen. Ctrl-click or
Command-click toggles a photo. Each photo also has a checkbox for touch.
The selection dock can select loaded photos, select the complete live query, or clear the selection.

Photos, selection, and scroll position stay in place across Documents/Photos switches until the session locks or ends. Leaving an album for Library, Albums, or another album resets that album's sort, selection, and scroll position. Library keeps its own selection and position. Switching workspaces stops unfinished photo reads while retaining loaded photos. Previews already seen stay in a private browser cache while their Photos view is active. Only mounted photos keep image URLs. Docbank deletes the cache when leaving the view, when the session locks or ends, or when the page closes. If a page closes without that cleanup, for example after a browser crash, the next signed-in session at the same `docbank web` address deletes the leftover cache. Caches from earlier addresses stay in browser storage until site data is cleared. Previews still display when browser storage is unavailable, but revisiting them may download them again. Preview downloads bypass the browser HTTP cache, so only that private cache retains bytes.
Pending, unsupported, and failed previews have separate placeholders. Choose
Refresh previews to reload the listing after background preview work finishes.
If a page fails to load, the earlier photos remain visible. Retry requests the
failed page again. Refresh and expired-cursor recovery keep the current grid visible until the refreshed range succeeds. Refresh keeps selections and the range anchor only within the refreshed pages, plus failed trash targets. Whole-query selection keeps its query scope. Failed attempts keep the earlier view available for Retry. Each refresh page has a one-minute deadline.

## Save and freeze a photo search

Choose Text for ordinary words, or Filters for expressions such as `rating:5 pick label:red`. In Filters, `pick` means `flag:pick`; quote `"pick"` to search for that word. Authored-text search matches caption, creator, and copyright on any live RAW, image, or video member. Photo search also includes camera and location metadata, filenames, and extracted text. Rating, flag, and color label facets use each asset's display file. Select None to find an empty flag or label.

Choose Save search to save the applied search. Albums lists saved queries whose kind filter contains only photos or videos as Smart sets. A `kind:photo` expression alone does not put a Documents query in this list; saving from Photos adds the kind filter. Ordering stays in the current view; reopening uses capture date, or relevance for a text query. The saved definition also runs in Documents: bare `pick` searches for the word, while `rating:` and `flag:` test each document file's authored decisions rather than the asset's display file. Hidden photo files remain accessible there. Use its saved-query editor to change or delete the definition.

Choose Freeze results after saving a smart set. A freeze uses the smart set's saved sort, with asset ID breaking ties, not the current browsing order. Smart sets saved from Photos store name ascending, so their freezes order photos by name. A smart set saved in Documents keeps its own sort; a `path` sort freezes by name ascending. Frozen runs contain Library photos only. The frozen result includes all matches, including those beyond the first 250 relevance results. Its count and expiry appear beside Save as album and remain available when you return from Albums. Saving the album preserves the frozen asset membership and result order and clears the freeze, even if the live query's matches have changed. Frozen results expire with the daemon's snapshot session; the album persists.

A frozen result can contain at most 250,000 assets. Larger freeze requests fail before saving a run receipt. A freeze also fails without a receipt if any matching photo leaves Library before the run is saved. Expired snapshots and assets that are missing, hidden, excluded, or trashed also fail before creating an album. Authored-field, tag, content, and display-file edits keep the frozen asset membership. If results expire while naming an album, choose Freeze again; the album name stays in the form.

## Hidden photos

Choose Hidden in the Photos sidebar and set a passcode. Open a photo's actions menu or right-click it and choose Hide. If the photo belongs to the selection, the action applies to each selected photo. A stale photo reports an error while successful changes remain saved.

Hidden photos disappear from Library, photo queries, and album contents. Albums retain their membership; their included count covers only visible photos. Background imports keep updating hidden files, including late sidecars. Import receipts retain ordinary photo IDs and file details. Documents, document tools, original bytes, and exports still expose the underlying files. Hidden is a Photos privacy control.

Enter the passcode in Hidden to unlock for five minutes. The countdown uses the server's remaining time, so a wrong browser clock does not change it. Lock closes every active Hidden session, and so does locking or ending any browser session. Already admitted exports retain their rendered copies and remain downloadable until export retention expires. Locking and expiry clear the Hidden grid, selection, and browser preview cache. Photos check for committed changes every two seconds, with a two-second request timeout. Once a window has opened Photos, hiding its tab queues a keepalive lock after pending Hidden writes, including an in-flight unlock. Leaving blanks every Photos view, closes open photos, removes their URL links, and clears cached previews. Library and the open album's photo list reload when you return; a failed load keeps those lists blank with an error and Retry. The Albums index keeps its album list. Page close sends an immediate keepalive lock and, when Hidden writes are pending, queues another after them; closing during an unlock remains best effort. A failed lock shows an error and Retry lock in any workspace. Hidden stays concealed until a lock succeeds. An open Library photo stays open when the refresh still lists it; otherwise its viewer reports that the photo left the result. Background views stay concealed until status validation succeeds. Hidden conceals after any failed check. Library, Map, Albums, and each open album share the Library access checks. Library conceals after a denied check or three failed checks in a row; a concealed view stays concealed until Retry or a later check succeeds. Expired browser sessions use the shared session-expired screen. Reloads retain grouping, density, loaded range, scroll position and the selection of unaffected photos; failed batch targets stay available for retry. Hidden previews carry `Cache-Control: no-store`. Preview generation continues while a photo is hidden.

Hidden supports direct links at `/photos/hidden#photo=<asset-id>` after unlock. Viewer navigation, larger previews and supported originals use the active Library or Hidden scope. Locking or concealing the view closes its viewer and removes displayed bytes.

Five incorrect passcodes within sixty seconds lock access for five minutes. Failed attempts and lockout survive restart. Backups preserve hidden flags, credentials, failed attempts, active lockouts and decision receipts. Restored vaults require the backed-up passcode and start without unlock sessions. Unlock sessions expire on restart. Changing the passcode revokes sessions. Disable Hidden verifies the passcode and returns every hidden photo to Library in one transaction. Operator reset removes credentials and sessions while preserving hidden flags, so a new setup can recover access.

```text
docbank photos hidden setup
docbank photos assets hide <asset-id> [--revision REV]
docbank photos assets unhide <asset-id|path-or-id> [--revision REV]
docbank photos hidden change
docbank photos hidden disable
docbank photos hidden lock
docbank photos hidden state
docbank photos hidden reset
```

Passcodes must be 1 to 1,024 bytes without CR or LF line breaks. Canonically equivalent Unicode spellings use the same passcode. CLI input uses protected terminal input or one line from stdin. Terminal setup and change ask for the new passcode twice. Change reads the current and new passcode on separate lines. CLI commands prompt for the passcode when reading or editing a hidden asset and forward the unlock cookie only within that invocation, without saving a credential file. For hidden photo details, run `docbank photos assets inspect /path/to/photo.jpg` and enter the passcode. This returns the details without unhiding the asset; MCP photo tools remain locked. Hide requires an asset ID. Unhide also accepts an asset ID or `id:<node-id>`; ordinary Documents and MCP document tools retain file access. Hidden state `change_id` is an opaque equality token: compare it for changes without interpreting or ordering its value. HTTP operations live under `/api/v1/photos/hidden`; hide and unhide use `/api/v1/photos/assets/{asset_id}/hide` and `/unhide` with `If-Match`. Browser sessions can use the interactive operations. Reset requires the daemon API key.

A hide retry that returns `hidden_locked` already took effect.

## Ratings and other decisions

Each RAW, image, and video member stores its own rating, flag, label, caption,
creator, copyright, and rotation. A RAW rated 5 and a JPEG rated 3 keep those
values. Inspection returns every file's revision and an `agreement` map.
`false` for a field means the displayable members have mixed values.

| Decision | Allowed values |
| --- | --- |
| Rating | 0 through 5 |
| Flag | empty, `pick`, or `reject` |
| Label | empty, `red`, `yellow`, `green`, `blue`, or `purple` |
| Rotation | 0, 90, 180, or 270 degrees clockwise on EXIF-normalized pixels |
| Caption, creator, copyright | Up to 16 KiB of UTF-8 text each |

A new member starts at revision 1 with no decisions. Detach and reattach keep
each file's identity, decisions, and revision. Hidden is a Photos privacy
control: Documents queries and audit history still show a hidden photo's
decisions.

`confirmed_fields` records supplied fields with bits rating=1, flag=2, label=4,
caption=8, creator=16, copyright=32, and rotation=64. Zero means untouched.
Docbank edits with empty text or numeric zero confirm a clear. Unsupplied
fields keep their earlier values and confirmation. Receipts, audit history,
undo, and JSONL backup preserve the mask. Unknown bits and non-default values
without confirmation are refused.

### Decisions from XMP sidecars

The daemon fills in an original file's decisions from its imported XMP sidecar
while that file is still at revision 1. Once a sidecar sets a decision, later
packets don't change it. Only non-default values confirm a decision. Rating 0,
empty text, no label or pick, and rotation 0 confirm nothing, because editors
write these defaults on untouched photos.

The daemon picks the original for each sidecar this way:

- A qualified name such as `capture.JPG.xmp` selects that original.
- A basename-only packet such as `capture.xmp` can select an extension with
  `photoshop:SidecarForExtension`. Without that claim, the packet applies to
  its linked file, which import prefers to be the RAW.
- A missing, ambiguous, or conflicting binding makes no decision. If you attach
  `capture.JPG.xmp` to the RAW, the name and the attachment conflict. The packet
  applies after you attach it to the JPEG.
- The daemon handles one photo's sidecars one at a time, lowest node ID first.
  When two select the same original, the first that makes a decision wins. An
  empty or rejected packet makes no decision, so the next sidecar can apply.
- An audited vault refuses changes outside its audit scopes, so sidecars make no
  decision there. Enrolling a scope that covers the photo makes its sidecars
  eligible again.
- A trashed original gets no decision. Restoring it makes the sidecar eligible
  again.

The daemon reads these XMP properties:

| Decision | XMP property |
| --- | --- |
| Rating | `xmp:Rating`. A rating of -1 means reject with rating 0, even when the packet also has a pick. It confirms only the flag. |
| Label | `xmp:Label`, case-insensitive |
| Flag | `Pick` in the `https://teststrip.app/xmp/1.0/` namespace |
| Rotation | `Rotation` in the `https://teststrip.app/xmp/1.0/` namespace |
| Caption | `dc:description`, preferring the `x-default` language |
| Creator | The first `dc:creator` entry |
| Copyright | `dc:rights`, preferring the `x-default` language |

An unsupported value is ignored and stays unconfirmed; the other decisions still apply. That
covers custom color labels, fractional ratings, unknown picks, other rotations,
and text over 16 KiB. Whitespace-only text counts as absent; other text keeps
its spaces and newlines. Malformed XML, a repeated property, multiple values in one property, text beside
a property's value, or nesting deeper than 64 elements rejects the whole packet. Empty and rejected packets leave the file, its
revision, and audit history unchanged. Sidecars don't import `dc:subject` as
tags or `tiff:Orientation` as rotation.

Each applied packet records the sidecar file and content version that supplied
it. Sidecar source-metadata detail shows a valid-packet fact or a warning.
Replacing a sidecar's bytes can still fill in a file that has no decision.
The daemon reconsiders a sidecar when the photo's members change, including
renames and restoration. Cancellation, stale inputs, and read failures retry.

Typed queries accept `rating_min`, `rating_max`, `flags`, and `labels`. In Photos,
each decision filter reads the asset's display file. With the default RAW display
rated 5/pick and a paired JPEG rated 1/red, `rating:5`, `flag:pick`,
`NOT rating:1`, and `NOT label:red` match, including with a linked XMP sidecar.
Both typed bounds from 3 through 3 and `rating_min:3 AND rating_max:3` exclude
the photo. Selecting the JPEG reverses those rating, flag, and label matches.
Changing the display file changes these decision matches.
Documents queries read each file's own decisions, so `NOT flag:pick` there
also matches files that aren't photos. Decisions and their receipts round-trip
through JSONL backup and restore.
Advanced expressions accept `rating:5`, `rating_min:4`, `rating_max:3`,
`flag:pick`, and `label:red`.

## Previews

The daemon produces a grid preview with a 512-pixel maximum edge for each
included photo's selected display file. It discovers new imports continuously
and resumes missing work after restart. It keeps completed results.

The web viewer and embedded applications can request fit previews at 2560 pixels or large
previews at 4096 pixels. All sizes preserve aspect ratio and never upscale.
JPEG, PNG, GIF, still WebP, and supported embedded JPEGs in ARW, DNG, CR2,
NEF, and RAF files have decoder paths. Format support does not guarantee that
every individual file decodes. Unsupported and deterministic decode failures
are stored as terminal results. Temporary storage or read failures retry.
JPEGs over 100 MP stop at one-eighth of the source edge instead of the
2560 and 4096 recipe edges.

## Quality signals

The daemon measures each ready grid preview locally. The photo query API
returns `quality.state` as `ready` with numeric scores after measurement,
or `pending` with `signals: null` while work can finish. A measured zero stays zero.
Replacing content or changing the display file selects that version's signals.
Videos have no quality object. `unavailable` with `signals: null` means work
has finished without scores: the grid preview is unsupported or failed, or its
verified bytes do not decode. Temporary read failures stay `pending` and retry.

All scores use the 0..1 scale. Focus measures adjacent luminance differences
on a 16-by-16 sample, stretched by a 0.15 calibration ceiling. At that size it
tracks coarse, large-scale contrast, not sharpness: a blurred copy of
a photo scores close to the original, and a sharp photo of a plain surface
scores low. Blur is `1 - focus`. Brightness uses
RGB luminance weights, with black at 0 and white at 1. `color_red`,
`color_green`, and `color_blue` are mean channel values. Framing scores the
contrast-weighted center against the rule-of-thirds intersections. Flat
images receive framing 0.5. Aesthetics combines focus, balanced exposure,
channel spread, and framing. These pixel heuristics do not judge subject
intent, distinguish motion blur from soft focus, or detect faces.

Scores can differ slightly from other tools because of image sampling differences.

Each scalar supports `_min` and `_max` query filters. JSON bounds are decimal
strings, such as `"brightness_min":"0.4"`, to preserve QueryV1's integer-only
JSON number contract. Advanced search accepts `brightness_min:0.4 color_blue_min:0.5`.
Quality filters follow the same version as other photo metadata filters. Photos
uses the asset's selected display; Documents uses each document's own version.
`unevaluated:true` selects versions eligible for measurement with pending or
unavailable measurements. Expressions reject `unevaluated:false`; use
`focus_min:0` for measured photos. Structured `"unevaluated":false` acts as omission.
Missing measurements fail numeric comparisons. Saved queries keep these filters.
Signals rebuild after restore and stay out of backups.

## CLI

Inspect the asset created for a node or use a stable asset UUID:

```text
docbank photos assets inspect <asset-id|path-or-id>
docbank photos assets create <path-or-id> [--kind photo|video] [--role raw|image|video]
docbank photos assets attach <asset-id> <path-or-id> [--revision REV] [--role ROLE] [--sidecar-of-file-id ID]
docbank photos assets detach <asset-id> <file-id> [--revision REV]
docbank photos assets exclude <asset-id> [--revision REV] [--excluded=true]
docbank photos assets promote <path-or-id> [--revision REV] [--kind KIND] [--role ROLE]
docbank photos assets display <asset-id> [file-id] [--revision REV]
```

`inspect`, `create`, and `promote` accept absolute virtual paths or `id:N`
node selectors, so `inspect id:42` finds the asset that owns file 42, even
after file 42 is trashed.

Operations on an existing asset read its current revision, send it, and retry
once if another write changes the asset first. Pass `--revision` with the
revision from your last inspection when a script needs the write to fail
instead. Omitting the file ID from `display` clears the asset override.

The vault preference is revisioned separately:

```text
docbank photos settings show
docbank photos settings set raw [--revision REV]
docbank photos settings set image [--revision REV]
docbank photos settings reset [--revision REV]
docbank photos settings set --burst-gap-seconds 5 [--revision REV]
```

Asset and settings commands emit bounded JSON. Exit code 4 means the revision is stale: the
`--revision` you passed no longer matched, or the one automatic retry lost to
another write. Read the asset or settings again before retrying. The daemon
performs role, ownership, sidecar, display, and audit checks.

## Albums

Albums group photo assets without moving their files. Manage albums in the
web app, CLI, or HTTP API.

In the web app, open Photos, then Albums. Cards and sidebar entries show starred albums first, with included photo counts. Cards also show covers. Open an album to rename, star, duplicate, or delete it, or sort its photos by Added, Captured, or Imported. Timeline, date choices, and the photo viewer work inside an album and stay within its photos. Select one photo and choose Use as cover. Removing photos from an album or deleting the album keeps the photos in Library.

To use the newest ready photo as the cover again, run `docbank photos albums cover <album-id>` or use the HTTP API.

Select photos in Library or an album, then choose Add to album in the selection dock. Type to find an album or create one. B adds to the last album chosen until you reload or the session locks; the dock and sidebar show that target. Without a target, B opens the picker. You can also drag a photo or selection onto a sidebar album. After every album change, Docbank reloads the album list. Membership changes also refresh the affected album's photo view. Trash and restore reload albums and every started photo view. A failed create or duplicate also reloads Albums, where the album may already appear.

Select loaded photos selects only the pages already loaded. Select all N photos selects the complete live query, including unloaded pages. Add to album, B, and drag use that same scope. Before preparing metadata, Trash and Hide change loaded photos only and are unavailable while all N photos are selected. A prepared metadata selection also allows trash for its resolved photos. The server resolves the query when you add, so the refreshed album count can reflect imports made since browsing. Explicit selections allow up to 1,000 photos per album change. For larger selections, choose Select all N photos to send one atomic query write. A revision conflict shows the server error and reloads the album list. An open form shows the reloaded album and keeps what you typed. Save again to retry with its current revision.

Create an album, add selected asset UUIDs or a complete query result, then
browse its members:

```text
docbank photos albums create "Holiday"
docbank photos albums list
docbank photos albums show <album-id>
docbank photos albums add <album-id> <asset-id> ...
docbank photos albums add <album-id> --query "{\"filters\":{\"kinds\":[\"photo\"]}}"
docbank photos albums members <album-id> --sort added_time --direction desc
```

`members` accepts `added_time`, `import_time`, or `capture_time`, plus `--page-size` and `--cursor`. Photos with neither a capture date nor a file modified time come last. Ties use ascending asset UUID. The cursor binds the album ID as well as the query and page options.

```text
docbank photos albums rename <album-id> "Trip"
docbank photos albums star <album-id> [--starred=false]
docbank photos albums cover <album-id> [asset-id]
docbank photos albums duplicate <album-id> "Trip copy"
docbank photos albums remove <album-id> <asset-id> ...
docbank photos albums delete <album-id>
```

Existing-album writes accept `--revision` with the same automatic read and retry as asset writes. Repeating an unchanged decision preserves the revision. Deleting an album keeps every photo and file. Duplication preserves added dates and member order. Removing the chosen cover clears the override. A ready grid preview of the chosen member wins; otherwise the newest added included member with a ready grid preview supplies the cover.

`add` and `remove` accept up to 1,000 explicit IDs or `--query` with strict QueryV1 JSON. A query selects its complete current photo result inside the membership transaction, including display metadata and duplicate collapse. Query scopes have no total member cap. Their sort field, including `added_time`, does not change the selected IDs. A query selects only visible, unhidden photos, even while Hidden is unlocked, so `remove --query` keeps excluded, trashed, and empty members; remove those by asset ID or delete the album. Coverage-dependent queries accept `--coverage` and `--profile-fingerprint`. Each changed action advances the album revision once and records all changed IDs in bounded receipts. An invalid ID, query, coverage, or stale revision rolls back the complete action.

Exclusion, ordinary trash, detach, and permanent file deletion keep album membership and its added date. `member_count` counts members with at least one file. Empty members disappear from counts, browsing, and the effective cover until a file is attached again. These file changes preserve the album revision and chosen cover. Included counts and member browsing also omit excluded and trashed photos until they become visible again. Album names can repeat. Use `set:` followed by an album UUID, or typed `filters.set_ids`, to filter by membership. Values within `set_ids` combine with OR.

## Import a camera folder

`photos import` reads a folder on the daemon host and imports its photos into a
vault folder:

```text
docbank photos import <source-root> [destination] [--json]
docbank jobs show <operation-id> --json
docbank jobs cancel <operation-id>
```

`photos import` queues the import as a background job and prints its operation
ID. Follow and stop it like any other background job: `jobs show` reports
progress and the import receipt, and `jobs cancel` stops it.

Pause imports or previews before their next group or photo, then resume them:

```text
docbank jobs pause photo_import
docbank jobs resume photo_import
docbank jobs pause derive:visual-previews
docbank jobs concurrency derive:visual-previews 3
docbank jobs resume derive:visual-previews
```

Preview concurrency accepts 1 through 4. Pause and concurrency survive a daemon
restart. Restoring a backup starts with unpaused lanes and concurrency 1.
Storage lanes `place`, `evacuate`, `repair`, `salvage`, and `photo_checkout` also accept pause and
resume, keeping their existing concurrency. Cancellation uses the operation ID.
Document workers show read-only controls in `jobs`.
If lane settings cannot be read, the import waits and keeps its progress.
It resumes when settings become readable; cancellation still works.
Cancelling a waiting import keeps its saved count and receipt. After a daemon
restart, imports scan again and skip content already imported.

![Photo import progress and its cancellation control in Background jobs](https://docbank.ai/assets/generated/web-photo-import-dark.png)

The import reads RAW files (`.ARW`, `.CR2`, `.CR3`, `.DNG`, `.NEF`, `.ORF`,
`.RAF`, `.RW2`), images (`.JPG`, `.JPEG`, `.PNG`, `.GIF`, `.WEBP`, `.HEIC`),
videos (`.MP4`, `.MOV`, `.M4V`, `.AVI`, `.MPG`), and `.XMP` sidecars. It leaves
every other file out and counts it as `unsupported` in the receipt. The source
folder must exist when you start the import. Otherwise the command fails with
exit code 2 and no import starts.

Files with the same folder and name, such as `IMG_0001.ARW`, `IMG_0001.JPG`,
and `IMG_0001.XMP`, become one photo. A qualified XMP filename attaches to its named original. Unqualified names attach to the RAW, or to the JPEG when there is no RAW; their packet extension claim selects the authored target after extraction. An XMP with no same-name RAW or JPEG imports
as a plain file. A later full scan pairs companions when both source files
are still present. Matching uses files in the scan and their current duplicate
owners. A video always becomes its own photo. Each photo commits in one
transaction, so a failure leaves no half photo behind, and running the same
import again skips content already in the vault. In a filesystem photo import, a file edited since it was
imported, such as an XMP saved again by a photo editor, becomes a new version
of the file already in the photo, not a second file.

The import leaves a group unpaired and lists it under `ambiguities` in the
receipt from `jobs show <operation-id> --json` in two cases:

- Two RAW files share a name, such as `IMG_0001.ARW` and `IMG_0001.DNG`. Each
  RAW and JPEG becomes its own photo and the sidecar stays a plain file.
- Same-name files already sit in separate photos. Nothing is merged.

Pair the RAW and JPEG files yourself: `photos assets inspect <asset-id>` shows
file IDs, `photos assets detach` frees a file, and `photos assets attach` adds
it to the other photo. A later scan containing the sidecar and its image
attaches it to the settled photo. The final receipt lists every group that
remained ambiguous during that run, with each file's source path, node ID,
role, and photo asset ID.

The import hashes the discovered files, waits one second, then compares each
durable copy with that observation. It hashes every group member again before
committing. A changed or deleted source leaves the entire group out and counts
as changed, even if its size and modification time stayed the same. Import
again to pick it up.

The web Jobs drawer shows progress and has a Cancel button while the import
runs. It leaves out source paths and the unpaired-group list. Cancel takes
effect before the next group. A daemon restart resumes an unfinished import by
scanning the folder again.

## Create writable copies

Use a checkout to edit copies outside the vault while Docbank retains their exact source versions:

```text
docbank photos checkout estimate --album <album-id>
docbank photos checkout create <empty-folder> --album <album-id>
docbank photos checkout create <empty-folder> --years 2023:2025
docbank photos checkout create <empty-folder> --all --byte-limit 100000000000
docbank photos checkout list
docbank photos checkout status <checkout-id>
docbank photos checkout scan <checkout-id>
docbank photos checkout commit <checkout-id>
docbank photos checkout rebind <checkout-id> <folder>
docbank photos checkout retire <checkout-id>
docbank jobs show <operation-id> --json
docbank jobs cancel <operation-id>
```

Combine `--asset`, `--album`, and `--years` to copy their union. Each file appears once. Year ranges include both endpoints. Hidden and excluded photos stay out. Use `--all` alone with a positive byte limit. Other selections can also set a byte limit. Estimate returns asset count, file count, and bytes without creating a checkout.

The destination must be an existing empty folder on the daemon's computer. Keep it separate from the vault, storage roots, watched folders, and other checkouts. Creation requires a local daemon connection.

Docbank preserves original filenames under capture-date and asset-ID folders so RAW, JPEG, and XMP members stay paired. A sidecar requires a selected, eligible original. Recorded sidecar links remain valid when filenames differ; each file uses its saved source path, even after importing renamed duplicates. Files without a saved source identity use their current document name. Docbank uses the modification date when capture metadata is absent, and `undated` when neither date exists. It verifies every copy before publication. Copies are private to the daemon user.

A photo hidden, excluded, detached, or trashed during copying is skipped. Docbank copies the remaining eligible files, then marks the checkout failed with the first error.

Estimate and creation skip the whole photo for invalid filenames or duplicate names within a photo. The estimate and the saved checkout report `skipped_photos`, capped at 20 entries with `asset_id`, `file_id`, `name`, and `reason`, plus `skipped_photo_count`. Creation returns these fields inside `checkout`. Status and list keep showing the skipped photos. Asset, file, and byte totals include only kept photos. A selection with no remaining files fails. Names longer than 255 bytes are invalid, or 255 UTF-16 units on Windows. On Windows, reserved names, forbidden punctuation, control characters, and trailing dots or spaces are also invalid. Retry with an explicit `--asset` selection that leaves out the listed photos. Renaming an imported document in Docbank does not change its copied filename, which comes from its recorded source path. If the destination filesystem treats two names as identical, publication fails and preserves copies already published.

Creation returns the checkout and operation IDs as JSON. Jobs reports progress and accepts cancellation. Pause or resume the `photo_checkout` lane to control copying. Failed and cancelled creations keep their ID, partial files, and error. They release the destination reservation and holds for files that were never published. Published copies keep their base-version holds. Rebind a failed or cancelled creation before scanning or committing its copies.

After a daemon restart, a started creation, including one waiting on a paused lane, reports failure with its ID, verified partial results, and error. Recovery preserves base versions for copies that may have been edited after installation. Copying stays stopped. If a temporary file or folder lock prevents inspection, recovery stays queued with its error and source holds until the lock clears. Cancellation can finish while the folder is unavailable. Published and uncertain copies keep their source holds until retirement.

Queued, running, and completed checkouts hold their frozen source versions against pruning and emptying trash. Completed checkouts and failed or cancelled commits keep their reservation until retirement. Clear any partial files before reusing a released destination.

Watches check separation from checkout folders when they start. Filesystem stores check at daemon start and at registration. At startup, an overlapping store stays unattached while the daemon checks in the background; unrelated stores keep running. Registration rejects overlapping roots. Released checkout folders stay protected while they contain copies or Docbank can't inspect them. Clear or move the copies from a released folder so the waiting watch or store can start. A missing folder or a regular file replacing it or an ancestor also releases protection. A waiting watch shows the checkout folder and the action needed in `docbank watch list`'s ERROR column and the watch API's `error` field. The reason clears when the wait ends.

Status reports file and byte totals, published counts, and the creation outcome. Checkout status shows the latest scan or commit failure. `docbank jobs show <operation-id> --json` shows the error and receipt counts and paths.

Backup includes checkout selections, historical version references, and whether retention has been released. Published, uncertain, and explicitly held bases retain their holds through restore until retirement. Backup releases unused selection holds. Retired checkouts stay released. Restore leaves checkouts unbound and carries no machine paths, job state, or copy observations. Checkout creation is unavailable in audited vaults. Commit works on existing checkouts after audit activation. Use `rebind` on the daemon's computer to verify a restored folder. Rebind hashes and classifies every present tracked path and records absent paths as missing. At least one tracked path must be present as a regular file. Edited files bind too; check the receipt before committing. Rebind also repairs an idle bound checkout after the folder identity changes, and refreshes published counts from verified present files. Rebinding to a different folder requires the previous folder to be empty or missing; clear or release it first. Rebind marks present files published and re-holds their surviving base versions. It preserves existing source holds, including held bases whose working files are missing. Commit holds each new base it creates. Pruned unpublished bases remain unheld history; an edited file whose base was pruned reports a conflict while independent files remain editable. Folder reservations are local and independent of source holds.

After reconnecting or remounting a drive, `scan` or `commit` can report that the destination identity changed. Run `docbank photos checkout rebind <checkout-id> <folder>` with the folder's current path on the daemon's computer. Rebind can verify the original path again or use a new mount path when the previous folder is empty or missing.

`scan` and `rebind` return a queued operation ID. Jobs shows files inspected and bytes verified while hashing; cancel through Jobs or `docbank jobs cancel <operation-id>`. Read `docbank jobs show <operation-id> --json` for the final receipt, including counts and at most 100 non-clean and 100 untracked paths. Counts include paths omitted from these lists. Use `docbank photos checkout status <checkout-id> --after <file-id> --limit 100` to page through every entry's working state. Status defaults to 100 entries and accepts up to 250; pass the returned `next_after` as `--after` for the next page. Completed scans persist settlement observations. Restart repeats an interrupted scan from the beginning against the current working bytes. A cancelled rebind leaves a restored checkout unbound.

After editing a working file, run `commit`. Docbank scans it, waits for two seconds of unchanged identity, bytes, size, and modification time, then creates a new version on the same photo file. A commit hashes every tracked file once, then rehashes only pending files after one settlement interval. A file that changed while it was being hashed gets one more interval. Every scan compares full bytes, including edits that preserve size and modification time. Files still changing remain pending and fail the job with their paths; independent stable edits commit, and pending retirement completes. Its checkout base and commit receipt advance together. Sidecars keep their existing file nodes and pairing. Jobs labels the operations Commit, Scan, and Rebind within the Photo checkout lane. All checkout operations share its pause control. Restart resumes interrupted commits. New versions enter the existing metadata and preview backfills. Independent edits commit even when another file conflicts; the operation then reports failure with the conflicting paths. Repeated scans keep those conflicts while the vault head or photo binding differs.

`scan` previews `clean`, `pending`, `edited`, `missing`, or `conflict` entries and lists untracked paths separately. Editing a file whose Docbank base or binding changed fails the commit with the conflicting path; the working file and recorded base stay intact. Untouched working files stay clean even when their Docbank base changes. Unstable files stay pending while other files continue scanning. Missing files change no photo authority. Renamed and untracked files require a separate import decision. Unreadable tracked files, unreadable folders, or filenames with invalid UTF-8 stop the job; correct the folder and retry. Untracked regular files are listed without opening them.

To recover a conflict, copy the edited file outside the old checkout. Create a checkout from the current photo, copy the saved edit over its matching path, and commit that checkout. If the photo file no longer exists in Docbank, import the saved file separately. Retire the old checkout after preserving the edit.

Retirement releases source holds and the destination reservation while preserving copies, selection, receipts, and job history. Queued and running jobs accept retirement as pending and release the checkout when they finish or are cancelled. If the lane is paused, resume it or cancel the job through Jobs to finish retirement. A pending request stays local with its job; after restoring a backup, reissue `retire` without rebinding to release the restored holds. Watches and storage bindings still require the released folder to be empty or missing. Retirement and advanced bases are included in backup; working bytes stay outside it.

## HTTP and JSONL

The daemon exposes asset inspection by asset ID or node ID, plus create,
attach, detach, exclude, promote, display, and settings operations under
`/api/v1/photos`. Existing-asset and settings mutations require `If-Match`.
Responses carry the new revision in both the body and the `ETag` header.

Photo assets, file memberships, the singleton settings row, and change
receipts are part of the deterministic metadata JSONL stream. Restore checks
node ownership, local pointers, sidecar targets, display selection, enum
values, revisions, receipt JSON, and the complete graph before commit. Older
supported metadata streams restore an empty photo authority.

Email children are identified by `email_document_relations.child_version_id`.
An image produced by processing remains eligible when it is not an email
child. Existing graphs survive ordinary trash and restore. Permanent node
deletion removes file memberships and repairs the affected asset while preserving
an empty asset identity.

Automatic enrollment and explicit graph writes are skipped or refused when
audit authority is active, according to the existing audit boundary. The
preexisting graph is preserved. Photo trash and restore remain available under audit and record audited node changes plus photo revision receipts.
Hidden unlock and lock remain available. Setup, change, disable, reset, hide, and unhide are unavailable while audit authority is active.

## Browse photo assets over HTTP

`POST /api/v1/photos/assets/query` accepts a `query` object using
[QueryV1](../architecture/http-api.md#saved-query-and-highlight-definitions),
optional `coverage`, `page_size` from 1 through 250, `facets`, `cursor`, and `hidden`. With `projection: "map"`, `page_size` allows up to 1,000. Larger pages return a validation error.
Omit `hidden` or set it to `false` for visible photos. `hidden: true` selects
only hidden photos and requires an active unlock on every page.
Request `camera`, `lens`, `year`, `location`, `set`, `rating`, `flag`, or
`label` facets. The response includes `relevance_available`, `items`, the
matching asset `total`, first-page `facets`, and an optional `next_cursor`.
Facets count distinct eligible assets across the whole scope and omit their
own root structured filter. Keep them across continuations.
The web app requests counts separately from tile pages, with `page_size: 1` and a time sort.
Facet time, member, or byte limits return an unavailable reason. Later pages keep that total. Start a new browse to refresh it.
The default page size is 50. Send the same query and page options with each
continuation. Editing a saved query invalidates its earlier cursor. Cursors
expire 15 minutes after issuance. Relevance returns one page of up to `page_size` matches, the full total, and no `next_cursor`. A cursor with relevance returns `invalid_photo_cursor`. Other sorts read live results, so file changes can move assets across the previous page boundary.

Use `kind:photo`, `camera:"Synthetic Camera"`, `lens:"Synthetic Lens"`,
`location:"Paris, Île-de-France, France"`, `iso:400`, `iso_min:100`, `iso_max:800`, `capture_after:2024-01-01`,
`capture_before:2025-01-01`, `gps:"-10,170,10,-170"`, `rating:5`, `flag:pick`,
`label:red`, or `asset:` or `set:`
followed by a canonical asset or album UUIDv4. Existing `collection:`, tag, and text predicates combine
with these fields. Camera and lens match the complete make or model, ignoring
case using Unicode case folding. Values in one typed filter array combine with
OR. Separate filters combine with AND. `locations` and `location:` match
the complete location label exactly.

Use typed `flags:[""]` or `labels:[""]` to select unflagged or unlabeled photos. Use `flag:""` or `label:""` for the same filters in expressions. `NOT flag:pick` selects unflagged and rejected photos; `NOT label:red` selects unlabeled photos and other colors.

Dates use the photo's recorded local calendar day, with an inclusive lower
bound and exclusive upper bound. A January 1 photo remains in January 1 date
filters even if its recorded UTC offset puts it on January 2 in UTC. GPS uses
inclusive decimal-string latitude/longitude bounds and permits boxes crossing
the antimeridian.

Camera, lens, location, ISO, capture-date, and GPS predicates use the asset's selected
display file. Metadata from other members is not combined with it. If the
selected RAW has camera A and its paired JPEG has lens B, `camera:A` matches
and `lens:B` does not. A sidecar without camera metadata cannot make
`NOT camera:A` match. Changing the display file changes these metadata matches.

Photos text also searches camera and lens makes/models and location labels from
active metadata heads. `name:` searches filenames only. Relevance orders filename matches, authored text, technical metadata, then retained content-only matches. Each group uses its own BM25 scores. Prefix, phrase and NEAR operands keep their matching meaning when scored.
The best matching member supplies an asset's score, after the complete
expression restricts eligible members. Authored caption, creator, and copyright match any live non-sidecar member of the asset, so a JPEG caption can match an asset selected by `extension:cr2`. Filename, technical metadata, retained content, name, extension, tag, and collection predicates still match
individual members. One member must satisfy the complete expression, using
the display file for its photo metadata predicates. For example,
`extension:xmp AND camera:A` can match a sidecar paired with a display image
from camera A. Saved expressions follow the same rule. Document metadata filters use each document's own file; ordinary Documents text membership keeps its filename and retained-content sources. Excluded, trashed, and displayless assets
stay out.

Sort by `relevance`, `capture_time`, `import_time`, `name`, `modified_at`, `size`, or
`media_type`, with `asc` or `desc`. Set `filters.set_ids` to album UUIDs to browse their members. `added_time` requires exactly one album ID after normalization. Capture-time album queries can return burst stacks; added-time queries keep individual rows. Capture sorting converts recorded offsets
to UTC. Omitted zones use civil calendar coordinates. A photo without a
readable capture time sorts by its file's modified time when Docbank recorded
one. Photos with neither sort last in both directions and do not match
capture-date filters. Asset UUID orders equal keys. Names and media types compare only their
first 1,024 characters, so longer values that share that prefix also fall back
to UUID order. Relevance orders evidence tier and numeric score in the requested direction, then capture placement descending and asset UUID ascending. Only members retained after duplicate collapse contribute scores. An unchanged library pages without duplicates or gaps with other sorts. `path` is
unsupported by this route. Document
snapshots reject `capture_time`, `import_time`, and `added_time` with an error naming Photos
as the supported view.

Each row has grid, fit, and large preview slots. `missing` means no result is
stored for that recipe. Stored `ready`, `unsupported`, and `failed` outcomes
keep their generation identity. Only ready slots include a generation URL and
JPEG output metadata. Read that URL with the browser session header.
Credentials stay out of URLs.

Visible preview responses use `Cache-Control: private, no-cache`: the browser may keep
bytes but must check with the server before reusing them. Hidden previews require
an active unlock and use `Cache-Control: no-store`, including `304` responses. A matching
`If-None-Match` returns `304` after the server checks the session, current
display, exclusion state, and any required Hidden unlock. This avoids reading the preview blob again.
An excluded asset or replaced display returns `404`, even with a matching
validator. A response with new bytes verifies the complete image. Listing
never creates previews.

The initial count evaluates the whole query. Later pages seek from the last
sort key. Queries that collapse duplicate content still evaluate the complete
matching population to choose representatives before returning a page.

`POST /api/v1/photos/assets/{asset_id}/preview` prepares a `fit` or `large`
preview for the supplied `content_version_id`. Include `hidden: true` and an active unlock cookie to prepare a hidden photo. It returns the same preview
slot shape as browsing, reuses retained terminal outcomes, and refuses an
excluded, trashed, or changed display. Each request has a 30-second bound.
Camera, lens, exposure, dimensions, and orientation come from the current
display's technical metadata. When map tiles are configured, browser Map-projection
rows include the selected display file's nullable GPS latitude and longitude.

## Count photos by capture day

`docbank photos timeline` prints JSON counts for every recorded capture day in
a photo scope, newest day first. The web app's Timeline uses the same counts.

```bash
docbank photos timeline
docbank photos timeline --query '{"filters":{"capture_after":"2024-01-01"}}'
```

`--query` takes strict QueryV1 JSON and defaults to `{}`, every included
photo. `--coverage` and `--profile-fingerprint` work as in photo browsing. The
output's single `capture_day` facet lists one value per day. `total` counts
every matching asset, and `missing` counts those with neither a readable
capture date nor a recorded file modified time. A photo without a capture date
counts on the UTC day of its file's modified time. Days follow the same recorded calendar day as `capture_after` and
`capture_before`, so a day's count matches browsing that day.

Over HTTP, send the same `query` and optional `coverage` to
`POST /api/v1/photos/assets/timeline`. The
[HTTP reference](../architecture/http-api.md) lists its limits and errors.

Apply or clear staged metadata edits before changing the selection, search, sort, date, or viewed photo, or hiding or trashing photos. Hide uses the prepared selection, including unloaded photos, and keeps failed targets selected with their captured revisions.

## Export photos

Choose **Export photos** for the whole current scope, or **Export selection** for selected photos. In Map, use **Export selection**; it includes selected photos outside the current map area. The [export drawer](export-bundles.md#export-photos) prepares JPEG or PNG copies with your size, metadata, and GPS choices, then delivers a verified ZIP. RAW display members export embedded previews and carry that label. GPS removal leaves descriptive location names intact. With metadata enabled, current catalog tags on the display file replace embedded keywords only when that file has at least one tag; companion-file tags are excluded. Conflicting XMP orientation stops preparation with metadata enabled; turn metadata off to export the viewed pixels. A changed or unavailable photo stops preparation with an error naming it; the linked guide lists preparation and metadata limits.

## Move photos to trash

Select photos in Library and choose **Move to trash**. Confirming moves every member of each selected photo, including RAW files, images, videos, and sidecars, to recoverable trash together. A changed photo revision refuses the action; failed photos remain selected for retry. Each photo is atomic; a selection runs sequentially and can make partial progress. Changing the selection changes the next action's targets. A failed photo outside the loaded grid keeps its captured revision; load and select it again before accepting a newer revision.

Choose **Move rejects** in Photos to preview the complete current scope, including photos beyond the loaded page. The preview counts eligible photos and files, lists up to 20 mixed pairs with their total count, and counts photos that stay in Docbank. Every original member, including individually trashed originals, must have the stored `reject` flag; sidecars follow their photo. The preview returns exact targets in asset ID order, limited to 1,000 live member files. A photo that would exceed the limit waits for the next batch. Confirmation moves those photos atomically. A missing target, changed asset or member node revision, changed reject flag, or changed Library/Hidden scope refuses the whole batch and requires another preview. Changes elsewhere in the scope, including album membership, leave the previewed targets valid. Larger scopes still show complete counts and how many photos will move now. Preview and confirm again for the rest. Select up to 64 photos and choose **Selected photos** in the modal to narrow the scope. Hidden scope requires an active unlock at preview and confirmation. An audit refusal names the blocked photo and file and leaves the entire batch in Docbank. Close the dialog, select up to 64 other photos, and choose **Selected photos** to move them.

Moving rejects preserves the loaded range, browsing position, and surviving selections. If reloading fails, the grid stays available. Retry reloads the range and reconciles selections.

HTTP clients preview with `POST /api/v1/photos/rejects/preflight` and `{query, hidden}`, then confirm with `POST /api/v1/photos/rejects/trash` and `{hidden, targets}`. Each target carries `asset_id`, the asset `revision`, and `member_revision`, the sum of all member node revisions. Confirmation reads each member once in the writer transaction, checks its revision and every original's reject flag, and moves the reviewed files.

Use `docbank photos assets trash <asset-id>` for the same operation from the CLI. `--revision` binds the action to an inspected asset revision. Ordinary Documents deletion and `docbank rm` still remove the selected file or folder.

Open **Trash** in either workspace to restore a photo group. Trash lists files, including hidden photos' files, just like Documents. One row represents its independently trashed members and shows how many trashed files restore will recover, including companions inside folders. Restoring any member recovers the complete group. The selected member's revision guards the request; Docbank reads all affected roots in the same transaction. A member inside a trashed folder restores that folder's subtree and the photo's companions elsewhere. Selected destination folders return before their files. Separately trashed unrelated items stay in trash.

Trash keeps file bytes, content versions, photo relationships, and album membership intact. Trash and restore each advance the affected asset revision once and record a change receipt. Permanent deletion waits until every member is trashed, old enough, and free of retention references. For a partially trashed photo, trash the remaining companions with the asset action or detach its live companions before emptying trash. See [Trash and garbage collection](trash-and-gc.md).

Metadata JSONL v1 includes `trash` and `restore` receipts recording the operation and asset revision change; node trash state remains in the node records. Older Docbank clients refuse imports containing these operations; use a version that supports photo trash to restore that metadata.

## Map

Map opened from an album plots that album's GPS photos. Otherwise, it plots GPS photos matching the current Library search and filters. Hidden is never mapped. Results sort by capture date.

Configure [photo map tiles](../configuration.md#photo-map-tiles) in `[web.map_tiles]` and restart Docbank. Choose Map beside Library to browse GPS-positioned photos from the current query. Markers group nearby photos into numbered clusters. Click a marker to open the photo viewer. Escape returns focus to the marker or its cluster and retains the map position. The map also keeps its position when an access check conceals and refreshes it. Switching workspaces resets the map position. A cluster zooms in or spreads overlapping markers so you can open each photo.

Map uses album membership when opened from an album. Otherwise it inherits Library's text, filters and date. It sorts by capture date, then adds GPS eligibility. Each visit starts a fresh result and selection. Map fetches the newest 20,000 matching located photos by capture date in pages of 1,000, at most 20 requests. When more match, Map says how many it's showing. Narrow the search, filters or date to see older photos. Returning to the Map rechecks access and reloads only after a hide or unhide. Like Library, the Map picks up other clients' trash, imports or edits on refresh. Markers and the grid show each page as it arrives. Earlier results stay visible during a refresh or if it fails. Retry resumes the failed page. An expired cursor starts a fresh load. The viewer and arrow keys follow the Map query, while the viewport filters only the grid. Panning retains selection, and the trash confirmation discloses selections outside the grid.

## Cull and compare

Choose **Cull** in Library or an album to review the whole current query full-screen. Docbank captures its order and membership before starting. A cull accepts up to 250,000 photos; narrow larger queries before starting. Flag or rating changes keep photos in the current pass; the next pass refreshes eligibility. Cull decisions are flags. A decision changes both files of a RAW and JPEG pair. When the pair's flags or ratings differ, it changes only the shown file and says so. Star ratings leave a photo unflagged. **S** cycles through unflagged, picks, rejects, and all, with confirmation before resetting an unfinished pass.

### Burst stacks

Library groups adjacent capture times from the same import folder, camera make, model, and body serial number. Photos with no recorded serial group with other photos with no serial; known serials stay separate. A flat **Choose files** selection shares one folder identity. A collapsed stack shows its representative and a count. Selecting it selects every frame. Expand it to select frames separately.

Set **Burst gap (seconds)** from 1 to 30 to regroup existing photos. Capture-time pages keep each stack together. Photos with missing or outdated camera details appear individually, while healthy stacks remain grouped and their stack actions remain available. Per-photo flags and ratings remain available. Unreadable originals leave camera details unavailable until they can be read; background extraction retries them. Cull refreshes incomplete groups when you choose **Retry grouping** or change the burst gap. Groups can change after details update.

In Cull, use H/L to move between stacks and J/K to move within a stack. Check frames in the filmstrip and choose **Keep selected** to retain those frames. Flags owned by another decision stay unchanged. Navigation preserves the checked frames; starting a new pass or regrouping clears them. A Cull stack holds only frames in the current pass, so a frame the pass excludes keeps its flag. **Keep all** picks every frame in the stack. **Keep current (Return)** picks the viewed frame and rejects the rest. Later Return decisions replace earlier Return assignments only for frames still owned by Return. Stack decisions update every live member file together, including paired originals, with one receipt and one Undo.

Choose **Choose representative** to change which frame represents the stack while preserving every frame's flags. A rejected frame in a mixed stack cannot become its representative. Choose a picked or unflagged frame, or change its flag first.

With auto-advance on, P, X, and U move to the next photo; 0-5 change the rating and stay on the current photo.

| Key | Action |
| --- | --- |
| Arrow keys | Previous or next photo |
| H / L | Previous or next stack |
| J / K | Next or previous frame within the stack |
| Return | Keep the viewed frame and reject the other frames, except those with a flag you set yourself |
| P / X / U | Pick, reject, or clear the flag |
| 0–5 | Clear or set the star rating |
| A | Toggle auto-advance, initially on |
| S | Start the next scope pass |
| Z | Toggle 1:1 preview pixels and fit |
| I | Cycle EXIF information |
| ? | Show the key map |
| Esc | Leave cull mode |

Use **Skip** to record a skipped photo. The decision toast offers **Undo**. Ctrl, Cmd, and Alt shortcuts pause during cull. Apply or clear pending metadata edits before entering, moving, or leaving.

**Resume cull** restores the saved pass, position, visited and skipped photos, scope, and auto-advance setting for this vault in this browser. Its captured membership survives snapshot expiry and browser restarts. Each daemon restart gives local `docbank web` a new browser address, so saved progress from the previous address no longer appears; [signing in on a server or container](web.md#sign-in-on-a-server-or-container) keeps one address. Earlier progress stays in browser storage until site data is cleared. Starting another cull asks before replacing saved progress. Storage failures appear when you choose Cull or Resume. If another tab changes the saved progress, close and resume the latest pass. At completion, Docbank checks the captured set once; flag changes or Undo check it again, and **Previous** cancels a check in progress. **Review unflagged remainder** appears when unflagged photos remain anywhere in the captured set, including skipped and never-viewed photos. Missing or newly hidden photos are counted as unavailable. Cull is not offered in Hidden.

Select 2–8 photos and choose **Compare**. Zoom, pan, and arrow-key navigation move the panes together. **Z** toggles 1:1 for each displayed preview. Choose **Manual closeup**, then click a face or draw a region on a full frame. Matching crop tiles appear below the full frames; **Clear closeup** removes them. The closeup uses your chosen location rather than automatic face detection.
