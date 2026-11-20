---
last_edited: 2026-10-31
title: Photo assets
description: Group camera files, browse photos, and organize albums in the web app, CLI, or HTTP API.
---

# Photo assets

Docbank groups ordinary file nodes into photo assets. Each file node and its content versions still hold the actual bytes, while the photo asset stores only membership, roles, display selection, visibility, exclusion, and bounded decision receipts.

When you add new image files or files with a concrete `video/*` MIME type, Docbank enrolls them as photo assets automatically. Audio, generic video, and generic RAW files stay ordinary files unless you promote them or create an asset explicitly. Automatic enrollment applies only to new files and does not scan older files already in your vault.

If you replace or revert a file's content, the file stays in its asset with the same role, even if the new media type would not qualify on its own. Likewise, replacing an ordinary file with content that qualifies does not enroll it. Asset membership changes only through explicit asset operations or permanent node deletion.

Each asset can hold up to 256 files across four roles: `raw`, `image`, `video`, and `sidecar`. By default, Docbank displays RAW first, then image, then video. You can change the vault preference to prefer image over RAW, and you can override that preference on individual assets. Sidecars never display and must point to a RAW or image member in the same asset. If you remove the selected display member, Docbank atomically picks another displayable member, or stores a null display when none remain.

## Live refresh

Open photo views update automatically whenever another tab, the CLI, an import, or a background worker changes your library. Your selection, scroll position, and metadata draft stay in place. If a photo becomes unavailable, it drops its cached image and shows that it is no longer available. If a photo changes while you have a draft open for it, Docbank refuses **Apply** until you choose **Reload metadata**.

The daemon accepts up to 128 simultaneous photo event streams. When all slots are full, additional streams retry automatically and wait up to 30 seconds between attempts. In browsers that support Web Locks, tabs share a single stream per origin.

## Add photos in the web app

To import photos from your computer in the browser:

1. Open **Photos > Library** and choose **Add photos**.
2. Set the vault destination folder. It defaults to `/`, and an invalid destination fails before any upload starts.
3. Choose **Choose folder** or **Choose files** to pick your photos.
4. Click **Add N groups** to start the import. If you change the destination after picking files, clicking **Add N groups** replans the selected files for the new path.

RAW, JPEG, and XMP files that share a folder and filename become one photo, while videos stay separate. The import drawer lists skipped files along with why they were skipped, such as unsupported types, invalid names or sizes, or duplicate relative paths. Each selection accepts up to 32,768 files, and its path-and-size declaration must fit within the 8 MiB message limit. The browser checks both limits before asking to replace saved progress or reading photo content, and checks again if you change the destination.

As the import runs, the drawer reports additions, files already in the vault, ambiguous groups, and failures. Failed groups stay available through **Add N groups**. When the import finishes, photos, album counts, and the plain Documents directory refresh automatically. Retained searches, tag filters, and frozen snapshots stay unchanged.

Imported groups share one collection named `browser: Card` when you pick a folder named `Card`, or `browser: Choose files` for individual files. Reusing a filename with changed bytes in a new selection creates a separate file node. The existing file keeps its content and current version. Existing pairing rules decide whether the new file joins the same photo or a separate photo; identical RAW, image, and video content still returns already in vault. A lone XMP stays an ordinary file and is added again when picked in a new selection.

A lost receipt leaves the result unknown. Reopen **Add photos**. **Interrupted import found** names the recorded folder and shows how many groups it contains. Choose **Choose the same folder…**, then **Add N remaining groups**. A saved checkpoint selects its import. Without one, Docbank adopts a single earlier import only when at least one selected file matches its exact path and bytes and no selected path has different recorded bytes. Zero or multiple matches start a fresh import. A group is already in the vault only when every member matches that import's exact path and bytes. For a checkpointed import, a selected path with different recorded bytes reports an error; missing members remain to send. Recovery ignores folder continuation, so a renamed or continued folder may send landed groups again. Identical photos still resolve to the same photo, but an ambiguous fresh import may add a standalone XMP again. **Dismiss** stops offering the recorded import in this browser. Choose the destination before resuming. File selections can be resumed when a checkpoint survives.

**Choose folder** and **Choose files** always start a new selection, and each selected directory resolves independently. If a directory contains a photo with the same bytes as an earlier browser import, its new siblings join that folder's burst groups and share stack decisions with those earlier photos, even if the matching file has a different name. Directories with no matching photo keep their own source identity, and directories that match several earlier folders automatically keep the fresh selection identity for their new photos. Exact duplicates retain their earliest folder provenance.

If an unfinished import is saved, Docbank asks before replacing it. Cancel keeps it available through **Choose the same files…** or **Choose the same folder…**. Saved imports from an unsupported format stay intact and require a compatible Docbank version to resume.

Identical RAW, image, and video content returns as already in vault. A recovered import skips a standalone sidecar when the chosen import records its exact path and bytes. Failed groups remain available through **Add N groups**.

If cookies or checkpoint storage are unavailable, a notice lets you import for this session. The vault can still find recorded browser imports. Authentication failures, progress changed in another tab, and unsupported saved formats still block import and preserve the saved selection.

Web imports run inside the open tab, and you control them from the drawer. To finish the current group and then stop, choose **Stop after this group**. Keep the tab open until the import completes. This works both locally and when you sign in to hosted Docbank with an API key. For large camera cards that are already on the daemon's computer, use `docbank photos import` instead.

## Record faces and people

In the inspector, name search includes active operator assertions on visible versions before document-people publication, except blind-copy assertions. People known only from automatic sensitive evidence or blind-copy assertions stay out of results. People backed only by locked Hidden photos appear once you unlock Hidden. Manually created unlinked people remain searchable with blank identity labels. Automatically provisioned exact-identifier people appear by name once they have a visible assertion or a non-sensitive published document link. See [browser names and visibility](people.md#browser-names-and-visibility) for details. The picker explains the duplicate risk. Create the person directly and merge any duplicate after unlocking.

If a name search fails, choose **Retry** to repeat that page. Choices from earlier completed pages stay available if a later page fails, and **Create person** appears once the search completes.

To record a face or whole-photo observation from the CLI, first get the photo file's `current_version_id` from `docbank document stat <path-or-id> --json`, then run:

```sh
docbank photos faces create <content-version-id> --box 0.1,0.2,0.3,0.4 --person <person-id>
docbank photos faces create <content-version-id> --person <person-id>
docbank photos faces list <content-version-id>
```

A box uses `x,y,width,height`, with each value measured as a fraction of the EXIF-oriented preview before authored rotation. Omit `--person` to record an unnamed face, or omit `--box` to mark a person in the whole photo. Naming a face records the same `depicted` person link as a whole-photo mark, and multiple faces of the same person share that link. `faces list` returns up to 1,000 face boxes and 1,000 depicted-person records, including whole-photo marks and suppressed records referenced by a face. Each photo holds at most 1,000 faces and 1,000 depicted people, and creating past either limit fails.

Creating a boxed face generates a face UUID. Save the returned `face_id`, or pass `--face-id <uuid>` to supply your own. If a connection error, server failure, or unreadable success response interrupts a boxed create, retry with the same ID, box, and version to recover the saved result; the boxed command includes that retry ID automatically. If you supply a person, it must match the saved canonical person, and retries still succeed even if that person has since been retired. Reusing an ID with conflicting inputs fails.

Name an existing box with `faces name` using its face revision. Before correcting a name, read its current revision with `faces list`:

```text
docbank photos faces name <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces retract <content-version-id> --face <face-id> --revision REV
docbank photos faces reject <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces withdraw-rejection <content-version-id> --face <face-id> --person <person-id> --revision REV
docbank photos faces dismiss <content-version-id> --face <face-id> --revision REV
docbank photos faces key <content-version-id> --face <face-id> --person <person-id> --revision REV --person-revision REV
docbank photos faces undo <receipt-id>
```

Each correction returns a `receipt_id`. Before sending a decision or undo, the CLI prints an operation UUID so you can retry with the same inputs and `--operation-id` to recover the receipt if a response is lost. HTTP decisions accept `operation_id` in the request body, and undo accepts it as a query parameter. Undo restores changed records as long as their saved revisions still match, leaving edits to unrelated faces intact. If a later edit changed the revision, the request returns `412 stale_revision` and you need to read the face again before deciding. Because undo also returns a receipt, you can undo an undo, unless it retired the person created by a new-name face decision, in which case undoing that undo returns not found.

Rejecting an assigned person removes that face assignment atomically. Naming a face clears its rejection and dismissal while keeping decisions for other faces in a group intact. `faces list` returns `rejected_person_ids` and `dismissed`. Each face allows up to 1,000 rejected people; rejecting another returns `409 photo_face_limit`.

Retracting the last face assignment leaves a neutral `withdrawn` assertion unless an independent whole-photo mark remains. Assertions expose `photo_mark` for that mark, and withdrawing it through the Go store method `Store.AssertDocumentPerson` clears it. To clear a standalone whole-photo mark, use the browser or send an HTTP `retract` request with its `assertion_id` and assertion revision. Withdrawn assertions free up room for new depicted people, while explicit suppressions remain separate. To withdraw a rejection without changing the face's assignment, run `faces withdraw-rejection`. To clear a person's key face, run `faces key` without `--face` and pass only `--person-revision`. You can inspect a person's key face with `people show`, and you can choose a visible replacement even while the old key's photo is locked.

In MCP, `get_photo_faces` reads up to 100 face boxes and depicted-person records per page, shrinking the page size when needed to stay within its response byte limit. Each named face includes its `person_id` and `action`. The page's `people` list supplies the canonical `display_name` and `state` for people referenced by its faces and assertions, including retired labels, and a smaller page includes only the people it references. Pass `next_offset` as `offset` to fetch the next page. MCP cannot unlock Hidden photos. To add manual faces or whole-photo marks, use HTTP or the CLI.

To add new faces, target the current, live, included image or RAW member. Retained image and RAW versions stay readable and correctable until purge. You cannot add boxes or whole-photo marks to sidecars or replaced non-image versions. Reading faces or making manual writes on a Hidden photo requires an active unlock, and audited vault writes return `audit_mutation_unsupported`.

When you have [automatic face detection configured](../configuration.md#automatic-face-detection) and consent granted, detected faces appear unnamed in the same inspector rows, loupe boxes, and cull crops as soon as scanning finishes. In a parsable reply, detection clips boxes to the frame, skips any faces that remain invalid, and keeps the valid ones. If a reply is malformed or contains a nonempty face list with no valid faces, Docbank keeps earlier faces and leaves the scan incomplete; an explicit empty list completes the scan with no faces. Detection leaves caption suggestions and decisions untouched, and skips Hidden photos.

Detected faces include normalized boxes measured from the top left of the oriented preview, clockwise roll in radians relative to the oriented preview, a facing score from zero to one, and eyes-open and smile measurements. Any unavailable measurement has no value. When a new detector fingerprint runs, it refreshes matching automatic faces and removes unmatched automatic faces that nobody has acted on. Manual faces and faces with names, rejections, dismissals, candidates, or history keep their saved boxes and measurements, which preserves undo. A valid empty scan removes only untouched automatic faces. Each completed version and detector fingerprint scans once, including empty scans.

## Open and inspect a photo

To open a photo above **Library**, double-click it to select and open it, or press **Enter** to open it without changing any separate selection. On a touch screen, tap a photo to open it when nothing is selected. To start a selection on touch, use the checkbox; after that, taps toggle selection, and double-tapping opens a photo while also selecting it if it is not already selected. The **Open** control remains available for screen readers and keyboard navigation.

Move through the current results in query order, across calendar-month groups, using the arrow keys, the **Previous** and **Next** buttons, or the thumbnail rail. Navigation loads the next page when you reach the loaded edge and stops at the query boundary. If loading fails, choose **Retry navigation** to resume inside the viewer, even after a cursor expires. Press **I** to view available camera and exposure details. Zoom with the scroll wheel, pinch gesture, or a double-click, and choose **Reset zoom** to return to the default view. Pressing **Escape** returns you to the scroll position and focus you left when entering the viewer, including when browser **Forward** reopens it.

![A photo open in the viewer with its metadata panel](https://docbank.ai/assets/generated/web-photo-viewer.png)

The viewer displays the cached grid preview first, then upgrades to a fit preview. Zooming loads a large preview while keeping your zoom level. The viewer prepares only two neighboring fit previews in each direction. If an upgrade fails, the current image stays on screen and you can choose **Retry preview** to try the request again. Previews that are retained as failed or unsupported do not offer **Retry**; download those photos from **Documents** instead.

Choose **Reload photo** after a temporary opening failure, or when the photo version, display file, or Library eligibility changes. Reloading unchanged content restarts preview loading, clears the failure, and preserves your zoom.

Choose **View original** to read verified JPEG, PNG, or WebP bytes through the browser's existing download path. Originals are limited to 32 MiB, 16,384 pixels per side, and 40 million pixels; for larger images, use verified download in **Documents**. Animated WebP originals may animate in the browser. RAW photos always display server-produced JPEG previews.

You can open a photo directly with a `/photos#photo=<asset-id>` URL, or inside an album with `/photos/albums/<album-id>#photo=<asset-id>`. Direct views do not include query navigation or the thumbnail rail. Choosing **Close** returns to **Library** or to the album in the URL, and browser **Back** and **Forward** update the viewer as well. Because browser sessions stay in memory, reloading drops the session. After a reload, run `docbank web` for a fresh session and append `&photo=<asset-id>` to its URL fragment to reopen the photo directly.

## Edit metadata

Metadata editing is available in the web app; CLI and MCP commands are not available. The metadata panel starts open in both the library and the viewer. Press **I** or click the **ⓘ** button to hide or show it. To resize the panel, drag its left edge or focus the edge and press the arrow keys; double-click the edge to reset its width. Your browser remembers both visibility and width.

In the viewer, file details appear above the edit controls. Click a path, hash, version, document ID, photo ID, or file ID to copy it, or click a **Protected** chip to open **Audit history**.

The panel lets you edit rating, flag, color label, caption, creator, copyright, rotation, and keywords. Choose **Whole pair** to edit every image or RAW member together, or choose an individual member to edit it alone. **Mixed** indicates that members have different values. Stage your field and keyword changes, then choose **Apply**. Reloading or closing the page prompts you before discarding staged edits. Choosing **Clear caption**, **Clear creator**, or **Clear copyright** sets mixed text to empty.

To edit multiple photos at once, select them in the grid, or choose **Select all N photos** to edit the current search, including photos beyond the loaded page. Each edit allows up to 1,000 files, counting both members of a pair. Your editing session keeps the resolved photos across result refreshes until you change the selection, search, or inspected photo, and counts, checkboxes, album actions, drag, shortcuts, and trash all act on those same photos. After a partial trash, failed photos stay selected with their captured revisions, including unloaded photos, so you can retry them or choose **Clear selection**. A failed edit changes nothing. After a conflict, choose **Reload metadata** to keep your draft for review and an explicit retry. Keywords use the existing tag picker.

In **Photos**, keyboard shortcuts let you triage quickly:

- **0** through **5** set the star rating.
- **P** picks, **X** rejects, and **U** clears the flag.
- **6** sets red, **7** yellow, **8** green, **9** blue, and **V** sets purple.
- **R** rotates each selected photo 90° clockwise.

Use the inspector for other rotation values or **No label**. A shortcut applies any staged fields and keywords together with its decision, and the Undo feedback names those edits. Apply or clear edits before switching between pair members. While you are typing in a text field, normal text shortcuts stay active.

Edits update open photo lists in place. A Metadata list with search text or a rating, flag, label, keyword, or modified-date filter reloads after each edit, so an edited photo can leave the list. Meaning and Hybrid results keep their captured membership until you choose **Refresh**. **Map** also reloads after a rotation or its Undo. Choose **Undo** or press **Ctrl+Z** or **Cmd+Z** to restore the last edit, including keyword changes. Undo uses conservative revision checks and reports a conflict if an edited file has changed; keyword Undo also conflicts after any revision change to an affected document. Reload metadata before making another edit.

The viewer refuses edits until its photo has loaded, and refuses them if the photo was replaced after loading. After a replacement, the viewer shows the new photo and drops the refused shortcut, while edits staged in the Metadata panel stay until you choose **Reload metadata**.

When you edit one photo, a pending rotation suggestion appears under **Rotation**. Face tilt supplies the angle when measured faces agree, and the photo description can suggest an angle when no active faces remain. Choose **Rotate** to save the suggested angle, or **Dismiss** to confirm the current angle. Both actions support Undo, which reveals the current suggestion again. Responding requires a current nonzero proposal. If the suggested angle changes while the inspector is open, **Rotate** still saves the angle you saw, and if the proposal disappears, reload before responding. Face suggestions reflect current face decisions immediately; conflicting or entirely unmeasured faces leave rotation undecided. For a RAW/JPEG pair, select the displayed member in **Edit members** to respond to its rotation suggestion. Suggestions leave the displayed image unchanged until you respond, and you must apply or clear staged edits first.

Rotation applies to the displayed photo in the grid, viewer, and album cover without rebuilding previews. Hidden photos require an active unlock for editing and Undo. If you undo after hiding a photo, Docbank asks you to unlock **Hidden** and keeps **Library** signed in.

## Browse in the web app

To view your photos in the browser, run `docbank web` and choose **Photos** in the sidebar. **Library** opens at `/photos` with the newest captures first. In the sidebar, **System** opens **Disk use**, **Running now**, **Backups**, **Protected folders**, and **Search by meaning**.

If a photo has no readable capture date, Docbank places it in the month its file was last modified, provided Docbank recorded that time; the viewer then shows **File modified** and leaves **Captured** blank. Photos with neither date appear under **Undated**. On the right, the date rail lists years and months, including dates beyond the loaded grid, with no day choices. Hover over a year or month to see its photo count. As you scroll, Docbank loads more photos and reveals older years. When undated photos exist, the rail ends with "N undated", where N is the undated count. Choose **Load more** to continue from the same position when more results remain.

The title row displays the photo count, **Sort**, **Group**, **Density**, **Cull**, **Resume cull** when saved, **Add photos** in Library, the **⋯** menu, and the **ⓘ** button for the metadata panel. On screens 640px wide and narrower, the title row wraps as needed and **Group**, **Density**, **Cull**, and **Resume cull** move into **⋯**. The **⋯** menu holds **Trash rejected photos**, **Export**, and **Refresh previews**, plus **Rename album**, **Duplicate album**, and **Delete album** when you are inside an album. A floating **Loading photos…** pill appears while a page loads.

Choose a year or month in the rail to jump there. Docbank loads pages until it reaches that date while keeping your current search, filters, and album intact. Choosing a year jumps to its newest month. If a collapsed stack holds that month's photos, the jump lands on the stack, and a month with no photos jumps to the next older month. Date navigation requires newest-first capture order: **Capture date** in Library and search, or **Captured** in an album. In other sorts, the rail still displays dates but disables clicking with a hint to change the sort. Scrolling the grid yourself or opening a photo cancels a pending jump. If a page fails to load, dates that are already loaded still jump, and choosing **Retry** or refreshing resumes a pending jump. The rail marks the first photo visible below the section heading in every sort order. A chosen year stays expanded so its months remain reachable even if the grid ends before that year reaches the top; scrolling the grid yourself returns the expanded year to the top photo's year.

On screens 640 pixels wide or narrower, a full-width **Jump to date** selector above the grid replaces the date rail. It lists each year and its months with photo counts, followed by a disabled "N undated" option when undated photos exist. Choosing a date jumps like the rail does, and other sorts disable the selector with the same sort hint. The selector displays **Loading dates…** until the first counts arrive, and **Jump to date** while no month sits at the top of the grid.

The rail and selector appear whenever counts include dated or undated photos, including scopes that contain only undated photos. **Hidden** and **Map** have no date rail; in **Hidden**, use the **Year** filter instead. If counts fail or are unavailable, the rail or selector shows the reason and a **Retry** button, even when the grid is empty or a search has no matches. A failed count refresh keeps the last available dates beside the error. While a hide or trash operation runs, the rail and selector are disabled.

Dates use the selected display file's recorded calendar day, so a capture near midnight keeps its date regardless of your browser's time zone. A RAW/JPEG pair counts once. A photo with no capture date counts on the UTC day its file was last modified. When Docbank has seen the same bytes more than once, the first sighting that recorded a modified time decides the date, so re-importing copies never moves a photo. Reverting a photo restores the date of the original bytes, while replacing a photo with content recorded without a modified time leaves it undated until reverted. The **Undated** total includes photos with neither date.

Timeline counts every included photo asset, even in a vault with many ordinary documents. If the scope spans more than 250,000 distinct capture days or the count exceeds its time budget, Docbank shows a limit message and offers **Retry**. A busy message asks you to retry after other timeline requests finish. Loading the timeline does not block Documents queries. During a refresh, the rail keeps the last successful counts until new counts arrive.

To search filenames, camera or lens names, and location labels, type in the **Photos** top-bar search box and press **Enter**. Using the clear button or pressing **Escape** clears the box and reruns an applied search. Search opens in relevance order with one tile per asset, even when several paired files match. Choose **Capture date** to return to date browsing. Relevance shows the best 250 matches along with the total; refine the search or sort by capture date to see all matches. Equal-score ties break by capture placement descending, then asset ID.

Choose **Metadata**, **Meaning**, or **Hybrid** beside the search box. **Meaning** searches what photos show using a configured image/text embedding provider, and **Hybrid** combines metadata relevance with visual meaning. Both always use relevance order, including during metadata fallback, and keep supported photo filters. Their membership and order stay fixed while you page through results, while flags, ratings, rotation, and previews show current values as each page loads. Ranked results keep [burst stacks](#burst-stacks). Filter counts and the date timeline stay unavailable. Choose **Refresh** to run the search again.

**Meaning** and **Hybrid** retain up to 1,000 matches, including during metadata fallback, and show that limit when more matches exist. Metadata fallback captures matching photos in relevance order. Each page starts with up to 250 matches and includes every live member of each intersecting stack. Select all, export, albums, and **Cull** all use the captured IDs, including during fallback, and **Cull** reads the captured photos' current flags. Photos imported after capture appear when you refresh. To see matches beyond the 1,000-photo limit, narrow your search.

**Meaning** returns only photos that meet the image binding's configured similarity threshold. When none qualify, it shows an empty result without falling back to metadata. **Hybrid** still shows metadata matches even when no photos meet the similarity threshold. A binding without a threshold falls back to metadata with an image-search-unavailable notice. See [image/text retrieval configuration](../configuration.md#imagetext-retrieval).

When the provider, consent, or image index is unavailable, a status line explains why metadata matches are shown. Advanced Boolean queries that cannot preserve their meaning also use metadata search, as do expired provider credentials. Docbank sign-in failures remain errors.

To narrow the grid by **Camera**, **Lens**, **Location**, and **Albums**, open **Filters** beside the title. **Hidden** also offers **Year**, and **Library** shows **Year** whenever a saved search has an active capture range so you can edit or clear it. **Filters** starts closed and loads counts when you open it for live metadata results; **Meaning** and **Hybrid** omit counts, including during metadata fallback. The filter badge counts active filters you can edit in the panel, counting both ends of a range once.

Filter counts cover the whole matching library, including photos beyond the loaded page, and tiles appear before the separate count request finishes. Counts have a 30-second budget. Unavailable counts show their reason; choose **Retry counts** after a timeout, or narrow your search and filters if you hit member or byte limits. Each facet omits its own selected filter while keeping the search text and other filters.

Camera, lens, and location describe the selected display file, while **Year** uses its recorded local capture date, or the UTC day of its modified time if it has none. Because text search can match any live member of a pair, a text match can appear under a different display-file camera or lens. Camera and lens count both make and model, so their counts can sum to more than the asset total. Equivalent Unicode-folded values share one bucket and count each asset once. Filter buttons submit an original valid spelling, while oversized metadata stays visible as informational counts. Year counts omit a selected whole-year range and retain other date ranges, and **Albums** shows live memberships. Choose **Clear filters** to reset the search and facets; this button appears whenever a search or filter is applied. A facet group hides when none of the photos have a value for it and no value in it is selected. Changing a facet keeps any unsent search text and mode, while changing scope clears the selection and starts a fresh grid.

To group the grid, choose **Months** or **Capture sessions**. A session joins captures with gaps of four hours or less. Choose **Compact**, **Comfortable**, or **Large** to adjust grid density. Your browser remembers the density across reloads and fresh `docbank web` links until the daemon restarts on a new address.

To select photos, click a photo, **Shift**-click to select a range from the previous selection including loaded photos off-screen, or **Ctrl**-click or **Command**-click to toggle a photo. Each photo also has a checkbox for touch devices. From the selection dock, you can select loaded photos, select the complete live query, or clear the selection.

Your photos, selection, and scroll position stay in place when you switch between **Documents** and **Photos** until the session locks or ends. Leaving an album for **Library**, **Albums**, or another album resets that album's sort, selection, and scroll position, while **Library** keeps its own selection and position. Switching workspaces stops unfinished photo reads while keeping loaded photos.

Previews you have already seen stay in a private browser cache while their **Photos** view is active, and only mounted photos keep image URLs. Docbank deletes the cache when you leave the view, when the session locks or ends, or when the page closes. If a page closes without that cleanup, for example after a browser crash, the next signed-in session at the same `docbank web` address deletes the leftover cache. Caches from earlier addresses stay in browser storage until site data is cleared. Previews still display if browser storage is unavailable, though revisiting them may download them again. Preview downloads bypass the browser HTTP cache, so only that private cache retains bytes.

Pending, unsupported, and failed previews each display a separate placeholder. Choose **Refresh previews** from the **⋯** menu to reload the listing after background preview work finishes. If a page fails to load, earlier photos stay visible and you can choose **Retry** to request the failed page again. Refresh and expired-cursor recovery keep the current grid visible until the refreshed range succeeds. Refresh keeps selections and the range anchor only within the refreshed pages, plus any failed trash targets, while whole-query selection keeps its query scope. Failed attempts keep the earlier view available for **Retry**. Each refresh page has a one-minute deadline.

## Save and freeze a photo search

Choose **Plain words** for ordinary word search, or **Query syntax** for expressions such as `rating:5 pick label:red`. Switching modes reruns any typed text. In **Query syntax**, `pick` means `flag:pick`; wrap `"pick"` in quotes to search for the word itself. Authored-text search matches caption, creator, and copyright on any live RAW, image, or video member, plus every tag name assigned to the photo and any pending [photo suggestion](#suggested-metadata). Photo search also includes camera and location metadata, filenames, and extracted text. Rating, flag, and color label facets use each asset's display file. Choose **None** to find photos with an empty flag or label.

When a search or filter is applied, **Save search** appears so you can save it. **Albums** lists saved queries whose kind filter contains only photos or videos as **Smart sets**. A `kind:photo` expression alone does not place a Documents query in this list; saving from **Photos** adds the required kind filter. Ordering stays in the current view, and reopening uses capture date, or relevance for a text query. Saved definitions also run in **Documents**, where bare `pick` searches for the word, `rating:` and `flag:` test each document file's authored decisions rather than the asset's display file, and hidden photo files remain accessible. Use the **Documents** saved-query editor to change or delete a definition.

After saving a smart set, choose **Freeze results** to capture its current matches. A metadata search freezes all matches in the smart set's saved sort, breaking ties by asset ID. Smart sets saved from **Photos** store name ascending, while smart sets saved in **Documents** keep their own sort, with a `path` sort freezing by name ascending. **Meaning** and **Hybrid** searches freeze up to 1,000 results in relevance order. Frozen runs contain **Library** photos only. Their count and expiry appear beside **Save as album** and stay available when you return from **Albums**. Saving the album preserves the frozen asset membership and result order and clears the freeze, even if the live query's matches have changed. Frozen results expire with the daemon's snapshot session, while the saved album persists.

![A saved photo search shown as a smart set, with Freeze results](https://docbank.ai/assets/generated/web-photo-smart-set.png)

A frozen result holds at most 250,000 assets; larger freeze requests fail before saving a run receipt. A freeze also fails without a receipt if any matching photo leaves **Library** before the run is saved. Expired snapshots and assets that are missing, hidden, excluded, or trashed also fail before creating an album. Authored-field, tag, content, and display-file edits keep the frozen asset membership intact. If results expire while you are naming an album, choose **Freeze** again; the album name stays in the form.

## Hidden photos

To hide a photo, open its actions menu or right-click it and choose **Hide**. If **Hidden** does not yet have a passcode, **Hide** asks you to enter a new one twice, hides the photo, and leaves **Hidden** locked. If the photo is part of a selection, **Hide** applies to every selected photo. A stale photo reports an error while successful changes remain saved.

Hidden photos disappear from **Library**, photo queries, and album contents. Albums keep their membership, while their included count covers only visible photos. Background imports continue updating hidden files, including late sidecars, and import receipts retain ordinary photo IDs and file details. **Documents**, document tools, original bytes, and exports still expose the underlying files, because **Hidden** is a Photos privacy control.

To view hidden photos, open **Hidden** and enter the passcode. Unlocking lasts for five minutes by default. Operators can change future unlock lifetimes from one second through 24 hours by running `docbank photos settings set hidden_unlock_lifetime 15m`, or restore the default with `docbank photos settings reset hidden_unlock_lifetime`. The countdown uses the server's remaining time, so a wrong browser clock does not affect it.

Choosing **Lock** closes every active Hidden session, as does locking or ending any browser session. Exports that were already admitted keep their rendered copies and stay downloadable until export retention expires. Locking and expiry clear the Hidden grid, selection, and browser preview cache.

**Photos** checks for committed changes every two seconds, with a two-second request timeout. Once a window has opened **Photos**, hiding its tab queues a keepalive lock after any pending Hidden writes, including an in-flight unlock. Leaving blanks every **Photos** view, closes open photos, removes their URL links, and clears cached previews. **Library** and the open album's photo list reload when you return; if reloading fails, those lists stay blank with an error and **Retry**, while the **Albums** index keeps its album list. Closing the page sends an immediate keepalive lock and, when Hidden writes are pending, queues another lock after them; closing during an unlock remains best effort. A failed lock shows an error and **Retry lock** in any workspace, and **Hidden** stays concealed until a lock succeeds. An open **Library** photo stays open if the refresh still lists it; otherwise its viewer reports that the photo left the result.

Background views stay concealed until status validation succeeds. **Hidden** conceals after any failed check. **Library**, **Map**, **People**, **Albums**, and each open album share the Library access checks. **Library** conceals after a denied check or three failed checks in a row; a concealed view stays concealed until **Retry** or a later check succeeds. Expired browser sessions show the shared session-expired screen. Reloads keep grouping, density, loaded range, scroll position, and the selection of unaffected photos, while failed batch targets remain available for retry. Hidden previews carry `Cache-Control: no-store`. Preview generation continues while a photo is hidden.

![Hidden, locked, asking for its passcode](https://docbank.ai/assets/generated/web-photo-hidden.png)

After unlocking, **Hidden** supports direct links at `/photos/hidden#photo=<asset-id>`. Viewer navigation, larger previews, and supported originals use the active **Library** or **Hidden** scope. Locking or concealing the view closes its viewer and removes displayed bytes.

Five incorrect passcodes within sixty seconds lock access for five minutes. Failed attempts and lockouts survive daemon restarts. Backups preserve hidden flags, credentials, failed attempts, active lockouts, and decision receipts. Restored vaults require the backed-up passcode and start without unlock sessions, and unlock sessions expire on restart. Changing the passcode revokes active sessions. **Disable Hidden** verifies the passcode and returns every hidden photo to **Library** in one transaction. An operator reset removes credentials and sessions while keeping hidden flags intact, so a new setup can recover access.

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

Passcodes must be 1 to 1,024 bytes with no CR or LF line breaks, and canonically equivalent Unicode spellings count as the same passcode. CLI input reads from protected terminal input or one line from stdin. Terminal `setup` and `change` ask for the new passcode twice, and `change` reads the current and new passcodes on separate lines. When you read or edit a hidden asset from the CLI, the command prompts for the passcode and forwards the unlock cookie only within that invocation without saving a credential file. For hidden photo details without unhiding the asset, run `docbank photos assets inspect /path/to/photo.jpg` and enter the passcode; MCP photo tools remain locked. `hide` requires an asset ID, while `unhide` also accepts an asset ID or `id:<node-id>`. Ordinary **Documents** and MCP document tools keep file access. The Hidden state `change_id` is an opaque equality token that you compare for changes without interpreting or ordering its value. HTTP operations live under `/api/v1/photos/hidden`, and hide and unhide use `/api/v1/photos/assets/{asset_id}/hide` and `/unhide` with `If-Match`. Browser sessions can use the interactive operations, while reset requires the daemon API key.

If a hide retry returns `hidden_locked`, the hide already took effect.

## Ratings and other decisions

Each RAW, image, and video member stores its own rating, flag, label, caption, creator, copyright, and rotation, so a RAW rated 5 and a JPEG rated 3 keep their separate values. Inspecting an asset returns every file's revision and an `agreement` map, where `false` for a field means the displayable members have mixed values.

| Decision | Allowed values |
| --- | --- |
| Rating | 0 through 5 |
| Flag | empty, `pick`, or `reject` |
| Label | empty, `red`, `yellow`, `green`, `blue`, or `purple` |
| Rotation | 0, 90, 180, or 270 degrees clockwise on EXIF-normalized pixels |
| Caption, creator, copyright | Up to 16 KiB of UTF-8 text each |

A new member starts at revision 1 with no decisions. Detaching and reattaching a file keeps its identity, decisions, and revision. Because **Hidden** is a Photos privacy control, **Documents** queries and audit history still show a hidden photo's decisions.

`confirmed_fields` records supplied fields using bit values: rating=1, flag=2, label=4, caption=8, creator=16, copyright=32, and rotation=64. Zero means untouched. Docbank edits with empty text or numeric zero confirm a clear, while unsupplied fields keep their earlier values and confirmation. Receipts, audit history, undo, and JSONL backup preserve this mask. Unknown bits and non-default values without confirmation are refused.

To add a keyword in the inspector, type a keyword name to filter existing tags. Choose **Create “name”** to create and assign a new tag to the selected photo originals.

### Sync XMP sidecars

Photo decisions and keywords sync with each original's linked XMP sidecar inside the vault. On first import, Docbank reads supported XMP decisions into an original that has no catalog decisions. Catalog edits create immutable sidecar versions, and rescanning a source folder imports outside edits with provenance and an undoable metadata receipt. Docbank writes only vault content and leaves source folders unchanged.

After an upgrade, the first background sync writes a vault sidecar for each photo that has a Docbank decision, a tag, or a linked sidecar. Other photos get a sidecar when you edit their decisions or keywords.

1. Edit decisions or keywords in **Metadata**. Pending changes write a new sidecar version, and a photo without a sidecar gets `<original-name>.xmp` beside its original.
2. Rescan after editing an outside XMP file. Properties changed on only one side merge automatically, and **Metadata** offers **Undo sidecar import** while its file and keyword revisions still match.
3. If both sides changed the same property differently, **Metadata** displays both **Catalog** and **Sidecar** values. Choose **Use Catalog** or **Use Sidecar** to settle the conflicting properties together; unrelated changes merge with either choice. Divergent keyword sets require a choice for the entire set, and the whole packet waits until every conflict is resolved.

Editing a conflicting catalog value keeps the choice open until the values agree or you choose one side. Equal values become the comparison point for later edits. You can resolve XMP conflicts in the browser only; the CLI and MCP have no route for them.

| Decision | XMP property |
| --- | --- |
| Rating | `xmp:Rating`, whole numbers 0 through 5. Rating -1 imports as reject without changing the catalog rating. Sync writes -1 for reject so other applications see the rejection. Clearing reject restores the independent catalog rating. |
| Flag | `Pick` in `https://teststrip.app/xmp/1.0/` |
| Label | `xmp:Label`, case-insensitive supported colors |
| Rotation | `tiff:Orientation`, composed with the original's EXIF orientation, and Teststrip's `Rotation`. Changes to either import after source extraction; a source without orientation uses 1. |
| Caption | `dc:description`, preferring case-insensitive `x-default` |
| Creator | The first `dc:creator` entry |
| Copyright | `dc:rights`, preferring case-insensitive `x-default` |
| Keywords | `dc:subject`, merged with catalog tags by name |

Docbank writes a reject as `xmp:Rating="-1"` alone, which keeps your independent catalog rating intact. Changing that rating in another app clears the reject, though an outside app can preserve the reject by writing `ts:Pick="reject"`. Clearing reject in Docbank writes the kept catalog rating back out.

Caption and copyright edits replace `x-default` while keeping translations in order, and clearing writes an empty default. Creator edits keep later entries, while clearing removes the entire creator property. Text preserves spaces exactly. If you save caption, creator, or copyright text that XML cannot store, the error names the field and character. Older stored values trigger the same error during sync and stay editable.

First sync treats absent and default packet values as untouched. Equal values converge, while divergent present values require a choice. After that, removing a previously present property clears it.

#### Rescans and pairing

Docbank compares each outside copy of a sidecar against the last version it read from that copy, so reverting a copy to older bytes still counts as an edit, and a new copy that disagrees with the catalog asks for a choice. Nothing reaches the catalog or the sidecar until every choice is made, and a single **Undo sidecar import** reverses the whole sync. Moving or renaming an original keeps its sidecar attached.

A qualified filename such as `capture.JPG.xmp` selects that specific original. Import attaches an unqualified `capture.xmp` to the RAW, or to the JPEG when no RAW exists. `photoshop:SidecarForExtension` must agree with the attached original. A conflicting claim or ambiguous match stops the first sync until you fix its attachment or filename. After that first sync, the saved attachment stands, and later renames or claims in the sidecar do not change it.

An original syncs with one linked sidecar at a time. If more than one sidecar is attached, **Metadata** shows **Invalid** with the reason, and nothing imports or writes until you detach or trash all but one; retained history and open choices return when sync resumes. Because other applications read only the first description in an XMP packet, a changed caption, creator, copyright, or keyword set moves into that first description. Moving a collection preserves its language and the targets of its relative links.

#### Status and recovery

Use `xmp:pending` to find photos waiting for work, including source extraction, and `xmp:conflict` to find unresolved choices, including choices retained during failed reads. Dirty drafts, active writes, stale choices, and locked hidden photos disable the applicable actions.

- **Live status.** **Metadata** checks sync status in every state while open, so a background import or conflict appears without reopening the panel.
- **Missing bytes and failed reads.** Missing source or sidecar bytes stay **Pending** and retry automatically after repair. Failed reads keep existing choices and disable their buttons.
- **Malformed packets.** Malformed or unmergeable properties mark the entire packet **Invalid**, while unsupported scalar values stay untouched until that catalog field changes. Catalog values and successful histories stay unchanged until the packet is corrected. Keyword changes remove Lightroom aliases whose leaf keyword was removed, while other writes preserve them.
- **Size limits.** Packets over 1 MiB and text properties over 16 KiB refuse publication. Invalid or oversized incoming keywords refuse the whole packet. A merged keyword set over 64 KiB refuses the whole publication; shorten or remove keywords to recover.
- **Sync history limit.** A sync history over its size limit marks the packet **Invalid**. To start a new history, detach the sidecar and attach a different XMP file; detaching and reattaching the same file keeps the existing history.
- **Blocked sidecar creation.** An occupied generated filename or the 256-member limit stops sidecar creation, and **Metadata** shows the reason.
- **Trashing or detaching the last sidecar.** Removing the last sidecar suppresses automatic recreation until you change the saved decisions or keywords. If the original's detached, Docbank-generated sidecar still occupies that filename, sync reattaches it and keeps its history. Renaming the original keeps recreation suppressed, and reattaching resumes sync. Missing conflicting sidecars retain their choices.
- **Audited vaults.** Audited vaults require coverage of both the original and sidecar and refuse new sidecar attachments. Enrolling the relevant scope reopens sync.
- **Hidden photos.** Hidden photos sync in the background; interactive reads and choices require an unlock.

Typed queries accept `rating_min`, `rating_max`, `flags`, and `labels`. In **Photos**, each decision filter reads the asset's display file. For example, if the default RAW display is rated 5/pick and a paired JPEG is rated 1/red, then `rating:5`, `flag:pick`, `NOT rating:1`, and `NOT label:red` match, including with a linked XMP sidecar. Both typed bounds from 3 through 3 and `rating_min:3 AND rating_max:3` exclude the photo. Switching the display to the JPEG reverses those rating, flag, and label matches, because changing the display file changes which member's decisions match. In **Documents**, queries read each file's own decisions, so `NOT flag:pick` also matches files that are not photos. Decisions and their receipts round-trip through JSONL backup and restore. Advanced expressions accept `rating:5`, `rating_min:4`, `rating_max:3`, `flag:pick`, `label:red`, `xmp:pending`, and `xmp:conflict`.

## Previews

For each included photo's selected display file, the daemon builds a grid preview with a 512-pixel maximum edge. It discovers new imports continuously, resumes missing work after a restart, and keeps completed previews.

The web viewer and embedded applications can also request fit previews at 2560 pixels or large previews at 4096 pixels. Every size preserves the aspect ratio and never upscales. Docbank has decoder paths for JPEG, PNG, GIF, still WebP, and supported embedded JPEGs in ARW, DNG, CR2, NEF, and RAF files, though format support does not guarantee that every individual file decodes. Unsupported files and deterministic decode failures are stored as terminal results, while temporary storage or read failures retry automatically. JPEGs larger than 100 MP stop at one-eighth of the source edge instead of the 2560-pixel and 4096-pixel recipe edges.

## Quality signals

The daemon measures each ready grid preview locally. In the photo query API, `quality.state` returns `ready` with numeric scores once measurement finishes, or `pending` with `signals: null` while work can still complete. A measured zero stays zero. Replacing a file's content or changing the display file selects that version's signals, and videos have no quality object. A state of `unavailable` with `signals: null` means measurement finished without scores because the grid preview is unsupported or failed, or because its verified bytes do not decode; detected faces still appear in `face_reports` without Sharpness or Light. Temporary read failures stay `pending` and retry.

All scores use a 0..1 scale:

- **Focus and blur.** Focus measures adjacent luminance differences on a 16-by-16 sample, stretched by a 0.15 calibration ceiling. At that sample size it tracks coarse, large-scale contrast rather than sharpness, so a blurred copy of a photo scores close to the original, and a sharp photo of a plain surface scores low. Blur is `1 - focus`.
- **Brightness and color.** Brightness uses RGB luminance weights, with black at 0 and white at 1. `color_red`, `color_green`, and `color_blue` are mean channel values.
- **Framing and aesthetics.** Framing scores the contrast-weighted center against the rule-of-thirds intersections, and flat images receive a framing score of 0.5. Aesthetics combines focus, balanced exposure, channel spread, and framing.

These pixel heuristics do not judge subject intent, distinguish motion blur from soft focus, or detect faces. Scores can also differ slightly from other tools because of image sampling differences.

Each scalar supports `_min` and `_max` query filters. To preserve QueryV1's integer-only JSON number contract, pass JSON bounds as decimal strings such as `"brightness_min":"0.4"`. In advanced search, you can write `brightness_min:0.4 color_blue_min:0.5`. Quality filters follow the same version rules as other photo metadata filters: **Photos** uses the asset's selected display file, while **Documents** uses each document's own version. Use `unevaluated:true` to select eligible versions with pending or unavailable measurements. Expressions reject `unevaluated:false`, so use `focus_min:0` for measured photos; structured `"unevaluated":false` acts as omission. Missing measurements fail numeric comparisons. Saved queries preserve these filters. Signals stay out of backups and rebuild after restore.

## CLI

Inspect the asset created for a node, or manage assets directly using a stable asset UUID:

```text
docbank photos assets inspect <asset-id|path-or-id>
docbank photos assets create <path-or-id> [--kind photo|video] [--role raw|image|video]
docbank photos assets attach <asset-id> <path-or-id> [--revision REV] [--role ROLE] [--sidecar-of-file-id ID]
docbank photos assets detach <asset-id> <file-id> [--revision REV]
docbank photos assets exclude <asset-id> [--revision REV] [--excluded=true]
docbank photos assets promote <path-or-id> [--revision REV] [--kind KIND] [--role ROLE]
docbank photos assets display <asset-id> [file-id] [--revision REV]
```

`inspect`, `create`, and `promote` accept absolute virtual paths or `id:N` node selectors. For example, `inspect id:42` finds the asset that owns file 42, even after file 42 is trashed.

When you modify an existing asset, the CLI reads its current revision, sends it, and retries once if another write updates the asset first. If a script needs the write to fail on a conflict instead, pass `--revision` with the revision from your last inspection. Omitting the file ID from `display` clears the asset's display override.

Manage the vault's photo preferences separately, using their own revision:

```text
docbank photos settings show
docbank photos settings set raw [--revision REV]
docbank photos settings set image [--revision REV]
docbank photos settings reset [--revision REV]
docbank photos settings set --burst-gap-seconds 5 [--revision REV]
```

Asset and settings commands output bounded JSON. Exit code 4 means the revision is stale, either because the `--revision` you passed no longer matches or because the automatic retry lost to another write; read the asset or settings again before retrying. The daemon enforces all role, ownership, sidecar, display, and audit checks.

## Albums

Albums let you group photo assets without moving their files. You can manage albums in the web app, CLI, or HTTP API.

In the web app, open **Photos** and choose **Albums**. Both album cards and sidebar entries list starred albums first along with their included photo counts, and cards also display covers. Open an album to star it, choose **Rename album**, **Duplicate album…**, or **Delete album…** from **⋯**, or sort its photos by **Added**, **Captured**, or **Imported**. When you sort by **Captured**, newest first, you can use the date rail to jump within the album. Opening a photo keeps viewer navigation inside that album. To set a custom cover, select a single photo and choose **Use as cover**. Removing photos from an album or deleting an album keeps the photos in **Library**.

![Albums with a saved smart set and an album card](https://docbank.ai/assets/generated/web-photo-albums.png)

To reset an album cover to the newest ready photo, run `docbank photos albums cover <album-id>` or use the HTTP API.

To add photos to an album from **Library** or another album, select them and choose **Add to album** in the selection dock, then type to find an existing album or create a new one. Press **B** to add to the last album you chose until you reload or the session locks; the dock and sidebar display that target, and pressing **B** without a target opens the picker. You can also drag a photo or selection onto an album in the sidebar. After every album change, Docbank reloads the album list, and membership changes also refresh the affected album's photo view. Trash and restore operations reload albums and every started photo view. A failed create or duplicate also reloads **Albums**, where the album may already appear.

Choosing **Select loaded photos** selects only the pages already loaded, while **Select all N photos** selects the complete live query, including unloaded pages. **Add to album**, **B**, and drag use that same scope. Before preparing metadata, **Trash** and **Hide** act on loaded photos only and are unavailable while all N photos are selected; a prepared metadata selection also allows trashing its resolved photos. Because the server resolves the query when you add, the refreshed album count can reflect imports made since you started browsing. Explicit selections allow up to 1,000 photos per album change. For larger selections, choose **Select all N photos** to send one atomic query write. If a revision conflict occurs, Docbank shows the server error and reloads the album list; an open form shows the reloaded album while keeping what you typed, so you can save again to retry with its current revision.

From the CLI, create an album, add selected asset UUIDs or a complete query result, and browse its members:

```text
docbank photos albums create "Holiday"
docbank photos albums list
docbank photos albums show <album-id>
docbank photos albums add <album-id> <asset-id> ...
docbank photos albums add <album-id> --query "{\"filters\":{\"kinds\":[\"photo\"]}}"
docbank photos albums members <album-id> --sort added_time --direction desc
```

`members` accepts `added_time`, `import_time`, or `capture_time`, along with `--page-size` and `--cursor`. Photos with neither a capture date nor a file modified time appear last, and ties break by ascending asset UUID. Each cursor binds the album ID along with the query and page options.

Manage existing albums with these commands:

```text
docbank photos albums rename <album-id> "Trip"
docbank photos albums star <album-id> [--starred=false]
docbank photos albums cover <album-id> [asset-id]
docbank photos albums duplicate <album-id> "Trip copy"
docbank photos albums remove <album-id> <asset-id> ...
docbank photos albums delete <album-id>
```

Writes to existing albums accept `--revision` with the same automatic read and retry behavior as asset writes, and repeating an unchanged decision leaves the revision untouched. Deleting an album keeps every photo and file. Duplicating an album preserves added dates and member order. Removing the chosen cover clears the cover override. A ready grid preview of the chosen member takes precedence; otherwise the newest added included member with a ready grid preview supplies the cover.

`add` and `remove` accept up to 1,000 explicit IDs, or `--query` with strict QueryV1 JSON. A query selects its complete current photo result inside the membership transaction, including display metadata and duplicate collapse, with no total member cap. Its sort field, including `added_time`, does not change which IDs are selected. Because a query selects only visible, unhidden photos even while **Hidden** is unlocked, `remove --query` leaves excluded, trashed, and empty members in the album; remove those by asset ID or delete the album instead. Coverage-dependent queries accept `--coverage` and `--profile-fingerprint`. Each action that changes membership advances the album revision once and records all changed IDs in bounded receipts. An invalid ID, query, coverage, or stale revision rolls back the entire action.

Exclusion, ordinary trash, detach, and permanent file deletion all keep an asset's album membership and added date. `member_count` counts members that have at least one file. Empty members disappear from counts, browsing, and the effective cover until a file is attached again. These file changes leave the album revision and chosen cover unchanged. Included counts and member browsing also omit excluded and trashed photos until they become visible again. Album names can repeat. To filter by album membership, use `set:` followed by an album UUID, or use typed `filters.set_ids`. Values within `set_ids` combine with OR.

## Import a camera folder

Use `photos import` to import a folder on the daemon host into a vault folder:

```text
docbank photos import <source-root> [destination] [--json]
docbank jobs show <operation-id> --json
docbank jobs cancel <operation-id>
```

`photos import` queues the import as a background job and prints its operation ID. You can track and stop it like any other background job: run `jobs show` to check progress and the import receipt, and `jobs cancel` to stop it.

You can pause imports or preview generation before their next group or photo, adjust preview concurrency, and resume them later:

```text
docbank jobs pause photo_import
docbank jobs resume photo_import
docbank jobs pause derive:visual-previews
docbank jobs concurrency derive:visual-previews 3
docbank jobs resume derive:visual-previews
```

Preview concurrency accepts values from 1 through 4. Pause state and concurrency settings survive a daemon restart, while restoring a backup starts with unpaused lanes and concurrency 1. Storage lanes `place`, `evacuate`, `repair`, `salvage`, and `photo_checkout` also support pause and resume while keeping their existing concurrency. Cancellation uses the operation ID, and document workers appear as read-only controls in `jobs`. If lane settings cannot be read, the import waits while keeping its progress and resumes once settings become readable, and cancellation still works while waiting. Cancelling a waiting import preserves its saved count and receipt. After a daemon restart, unfinished imports scan again and skip content that was already imported.

![Photo import progress and its cancellation control in System, Running now](https://docbank.ai/assets/generated/web-photo-import-dark.png)

The importer reads RAW files `.ARW`, `.CR2`, `.CR3`, `.DNG`, `.NEF`, `.ORF`, `.RAF`, and `.RW2`, images `.JPG`, `.JPEG`, `.PNG`, `.GIF`, `.WEBP`, and `.HEIC`, videos `.MP4`, `.MOV`, `.M4V`, `.AVI`, and `.MPG`, and `.XMP` sidecars. Every other file is skipped and counted as `unsupported` in the receipt. The source folder must exist when you start the import; otherwise the command fails with exit code 2 and no import starts.

Files in the same folder with the same base name, such as `IMG_0001.ARW`, `IMG_0001.JPG`, and `IMG_0001.XMP`, become one photo. A qualified XMP filename attaches to its named original, while an unqualified name attaches to the RAW, or to the JPEG when no RAW exists. If an XMP packet claims a conflicting extension, Docbank leaves the attachment unchanged and marks it **Invalid** until you fix its attachment or filename. An XMP with no same-name RAW or JPEG imports as a plain file, and a later full scan pairs companions when both source files are still present. Matching uses files in the scan and their current duplicate owners. A video always becomes its own photo. Each photo commits in one transaction, so a failure never leaves a half-built photo behind, and re-running the same import skips content already in the vault. In a filesystem photo import, a file edited since its earlier import, such as an XMP saved again by a photo editor, becomes a new version of the file already in the photo rather than a second file. See [Sync XMP sidecars](#sync-xmp-sidecars) for how changed XMP files merge.

The importer leaves a group unpaired and lists it under `ambiguities` in the receipt from `jobs show <operation-id> --json` in two cases:

- Two RAW files share a base name, such as `IMG_0001.ARW` and `IMG_0001.DNG`. Each RAW and JPEG becomes its own photo and the sidecar stays a plain file.
- Same-name files already belong to separate photos. Nothing is merged automatically.

To pair those RAW and JPEG files yourself, find their file IDs with `photos assets inspect <asset-id>`, free a file with `photos assets detach`, and add it to the other photo with `photos assets attach`. A later scan that includes both the sidecar and its image attaches the sidecar to the settled photo. The final receipt lists every group that stayed ambiguous during the run, including each file's source path, node ID, role, and photo asset ID.

Before committing, the importer hashes discovered files, waits one second, compares each durable copy with that observation, and hashes every group member again. If a source file changes or disappears during that check, the entire group is skipped and counted as changed, even when its size and modification time stayed the same. Run the import again to pick it up.

In the web app, the **Jobs** drawer shows import progress and a **Cancel** button while the import runs, omitting source paths and the unpaired-group list. Cancellation takes effect before the next group. A daemon restart resumes an unfinished import by scanning the folder again.

## Create writable copies

Use a checkout when you want to edit copies outside the vault while Docbank holds onto their exact source versions:

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

You can combine `--asset`, `--album`, and `--years` to copy their union, and each file appears once. Year ranges include both endpoints. Hidden and excluded photos stay out. Use `--all` on its own with a positive byte limit; other selections can also set a byte limit. Run `estimate` to see the asset count, file count, and total bytes without creating a checkout.

The destination must be an existing empty folder on the daemon's computer, separate from the vault, storage roots, watched folders, and other checkouts. Creating a checkout requires a local daemon connection.

Docbank preserves original filenames inside capture-date and asset-ID folders so RAW, JPEG, and XMP members stay paired. A sidecar requires a selected, eligible original, and recorded sidecar links remain valid even when filenames differ. Each file uses its saved source path, even after importing renamed duplicates, while files without a saved source identity use their current document name. A sidecar with no source filename of its own takes its name from the original it ships beside: the photo's only `capture` RAW, or its only `capture` original, gets `capture.xmp` for Lightroom, Camera Raw, and Teststrip, with that original's extension recorded in `photoshop:SidecarForExtension`. Other originals, such as a JPEG beside `capture.ARW`, get `capture.JPG.xmp`. When capture metadata is missing, Docbank uses the modification date, and `undated` when neither date exists. Every copy is verified before publication, and copies are private to the daemon user.

If a photo is hidden, excluded, detached, or trashed while copying is in progress, Docbank skips it, finishes copying the remaining eligible files, and marks the checkout failed with the first error.

Estimate and creation skip an entire photo if it has invalid filenames or duplicate names within the photo. Both report `skipped_photos`, capped at 20 entries with `asset_id`, `file_id`, `name`, and `reason`, plus `skipped_photo_count`, with creation returning these fields inside `checkout` and `status` and `list` continuing to display them. Asset, file, and byte totals count only kept photos, and a selection with no remaining files fails. Names longer than 255 bytes are invalid, or 255 UTF-16 units on Windows; on Windows, reserved names, forbidden punctuation, control characters, and trailing dots or spaces are also invalid. To work around skipped photos, retry with an explicit `--asset` selection that leaves them out. Renaming an imported document in Docbank does not change its copied filename, because that name comes from its recorded source path. If the destination filesystem treats two names as identical, publication fails while preserving any copies already published.

Creation returns the checkout and operation IDs as JSON. **Jobs** reports progress and lets you cancel, and you can pause or resume the `photo_checkout` lane to control copying. Failed and cancelled creations keep their ID, partial files, and error, while releasing the destination reservation and holds for unpublished files. Published copies keep their base-version holds. Before scanning or committing copies from a failed or cancelled creation, rebind it first.

After a daemon restart, any started creation, including one waiting on a paused lane, reports failure with its ID, verified partial results, and error. Recovery keeps base versions for copies that may have been edited after installation, and copying stays stopped. If a temporary file or folder lock blocks inspection, recovery stays queued with its error and source holds until the lock clears, though cancellation can still finish while the folder is unavailable. Published and uncertain copies keep their source holds until retirement.

Queued, running, and completed checkouts hold their frozen source versions against pruning and emptying trash. Completed checkouts and failed or cancelled commits keep their folder reservation until retirement. Clear any partial files before reusing a released destination.

Watches verify that they do not overlap checkout folders when they start, and filesystem stores check at daemon startup and at registration. At startup, an overlapping store stays unattached while the daemon checks in the background, and unrelated stores keep running. Registration rejects overlapping roots. Released checkout folders stay protected as long as they still contain copies or Docbank cannot inspect them; clear or move the copies from a released folder so the waiting watch or store can start. A missing folder, or a regular file replacing it or an ancestor, also releases protection. A waiting watch displays the checkout folder and required action in the ERROR column of `docbank watch list` and in the watch API's `error` field, clearing that reason once the wait ends.

`status` reports file and byte totals, published counts, and the creation outcome, along with the latest scan or commit failure. Run `docbank jobs show <operation-id> --json` to see the error, receipt counts, and paths.

Backups include checkout selections, historical version references, and whether retention has been released. Published, uncertain, and explicitly held bases keep their holds through restore until retirement, while backup releases unused selection holds and retired checkouts stay released. Restore leaves checkouts unbound and omits machine paths, job state, and copy observations. You cannot create new checkouts in audited vaults, though `commit` still works on existing checkouts after audit activation.

To verify a restored checkout folder on the daemon's computer, run `rebind`. It hashes and classifies every present tracked path and records absent paths as missing; at least one tracked path must be present as a regular file. Edited files bind too, so inspect the receipt before committing. `rebind` also repairs an idle bound checkout after a folder identity change and refreshes published counts from verified present files. To rebind to a different folder, the previous folder must be empty or missing, so clear or release it first. `rebind` marks present files published and re-holds their surviving base versions while keeping existing source holds, including held bases whose working files are missing. `commit` holds each new base it creates. Pruned unpublished bases stay as unheld history; an edited file whose base was pruned reports a conflict while independent files stay editable. Folder reservations stay local and independent of source holds.

If `scan` or `commit` reports that the destination identity changed after reconnecting or remounting a drive, run `docbank photos checkout rebind <checkout-id> <folder>` with the folder's current path on the daemon's computer. `rebind` can verify the original path again, or bind a new mount path if the previous folder is empty or missing.

`scan` and `rebind` return a queued operation ID. **Jobs** shows files inspected and bytes verified while hashing, and you can cancel through **Jobs** or with `docbank jobs cancel <operation-id>`. Run `docbank jobs show <operation-id> --json` for the final receipt, including counts and up to 100 non-clean and 100 untracked paths; the counts include paths omitted from those lists. To page through every entry's working state, run `docbank photos checkout status <checkout-id> --after <file-id> --limit 100`. `status` defaults to 100 entries and accepts up to 250; pass the returned `next_after` as `--after` for the next page. Completed scans persist settlement observations. A restart repeats an interrupted scan from the beginning against the current working bytes, and a cancelled rebind leaves a restored checkout unbound.

After editing a working file, run `commit`. Docbank scans the file, waits for two seconds of unchanged identity, bytes, size, and modification time, and then creates a new version on the same photo file. A commit hashes every tracked file once, then rehashes only pending files after one settlement interval, giving any file that changed during hashing one more interval. Every scan compares full bytes, including edits that keep the same size and modification time. Files that are still changing remain pending and fail the job with their paths, while independent stable edits commit and any pending retirement completes. The checkout base and commit receipt advance together, and sidecars keep their existing file nodes and pairing. **Jobs** labels these operations **Commit**, **Scan**, and **Rebind** inside the **Photo checkout** lane, and all checkout operations share its pause control. Restarts resume interrupted commits, and new versions enter the existing metadata and preview backfills. Independent edits commit even when another file conflicts, after which the operation reports failure with the conflicting paths; repeated scans keep reporting those conflicts as long as the vault head or photo binding differs.

`scan` previews `clean`, `pending`, `edited`, `missing`, and `conflict` entries and lists untracked paths separately. Editing a file whose Docbank base or binding changed fails the commit with the conflicting path while leaving the working file and recorded base intact. Untouched working files stay clean even if their Docbank base changes. Unstable files stay pending while other files continue scanning. Missing files do not change photo authority, and renamed or untracked files require a separate import decision. Unreadable tracked files, unreadable folders, or filenames with invalid UTF-8 stop the job until you fix the folder and retry. Untracked regular files are listed without opening them.

To recover from a conflict:

1. Copy the edited file outside the old checkout.
2. Create a new checkout from the current photo, copy your saved edit over its matching path, and commit that checkout. If the photo file no longer exists in Docbank, import the saved file separately instead.
3. Retire the old checkout once your edit is preserved.

Running `retire` releases source holds and the destination reservation while keeping copies, selection, receipts, and job history. Queued and running jobs record retirement as pending and release the checkout once they finish or are cancelled; if the lane is paused, resume it or cancel the job through **Jobs** to finish retirement. Because a pending request stays local with its job, after restoring a backup you can reissue `retire` without rebinding to release the restored holds. Watches and storage bindings still require the released folder to be empty or missing. Backups include retirement state and advanced bases, while working bytes stay outside backups.

## HTTP and JSONL

Under `/api/v1/photos`, the daemon exposes asset inspection by asset ID or node ID along with create, attach, detach, exclude, promote, display, and settings operations. Mutations to existing assets and settings require `If-Match`, and responses return the new revision in both the body and the `ETag` header.

Photo assets, file memberships, the singleton settings row, and change receipts are included in the deterministic metadata JSONL stream. Before committing a restore, Docbank checks node ownership, local pointers, sidecar targets, display selection, enum values, revisions, receipt JSON, and the complete graph. Older supported metadata streams restore an empty photo authority.

Email children are identified by `email_document_relations.child_version_id`. Any image produced by processing stays eligible as long as it is not an email child. Existing graphs survive ordinary trash and restore. Permanent node deletion removes file memberships and repairs the affected asset while keeping an empty asset identity.

When audit authority is active, automatic enrollment and explicit graph writes are skipped or refused according to the existing audit boundary, while the preexisting graph stays intact. Photo trash and restore remain available under audit and record both audited node changes and photo revision receipts. Hidden unlock and lock also remain available, whereas setup, change, disable, reset, hide, and unhide are unavailable while audit authority is active.

## Browse photo assets over HTTP

Send a `POST /api/v1/photos/assets/query` request with a `query` object using [QueryV1](../architecture/http-api.md#saved-query-and-highlight-definitions), along with optional `coverage`, `page_size` from 1 through 250, `facets`, `cursor`, `snapshot_id`, and `hidden`. With `projection: "map"`, `page_size` allows up to 1,000; larger pages return a validation error. Omit `hidden` or set it to `false` for visible photos. Setting `hidden: true` selects only hidden photos and requires an active unlock on every page.

You can request `camera`, `lens`, `year`, `location`, `set`, `rating`, `flag`, or `label` facets. The response returns `relevance_available`, `items`, the matching asset `total`, first-page `facets`, and an optional `next_cursor`. Facets count distinct eligible assets across the whole scope and omit their own root structured filter; keep them across continuations. The web app requests counts separately from tile pages using `page_size: 1` and a time sort. If a facet hits a time, member, or byte limit, the response returns an unavailable reason. Later pages keep the initial total, so start a new browse to refresh it.

The default page size is 50. Send the same query and page options with each continuation. Editing a saved query invalidates its earlier cursor, and cursors expire 15 minutes after issuance. In metadata mode, `lexical`, relevance returns a single page of up to `page_size` matches, the total, and no `next_cursor`, and sending a metadata cursor with relevance returns `invalid_photo_cursor`. Other metadata sorts read live results, so file changes can move assets across the previous page boundary.

Meaning, `semantic`, and Hybrid, `hybrid`, freeze their ranked matches and return `snapshot_id`, the captured membership `member_hash`, `requested_mode`, and `actual_mode`. Send the same `snapshot_id`, query, page size, and projection with each ranked cursor; omitting `snapshot_id` returns `invalid_photo_cursor`. With `stacks: true`, ranked pages include every live display row for each intersecting stack among the captured members, so responses can exceed `page_size`. Filter counts and the date timeline stay unavailable.

In the web app, the **Photos** filter panel keeps selected camera and lens filters editable while ranked results are active. When duplicate collapse is enabled, duplicate images collapse before the 1,000-result bound, leaving room for distinct matches.

`coverage_incomplete: true` indicates that some photos in the filtered scope lack current image vectors, while available image matches stay ranked. `truncated: true` means retrieval bounds left additional matches outside the captured set of up to 1,000 photos. Whole-result actions use the captured asset IDs. If image search falls back, `actual_mode` is `lexical` and `fallback_reason` explains why. Metadata fallback keeps up to 1,000 photos in a snapshot, sets `truncated: true` when more match, and uses the original requested query for continuation. Its pages use the same stack grouping and omit filter counts and the date timeline, and action requests use the captured asset IDs. Hidden view uses live metadata results without a snapshot.

In query expressions, you can use `kind:photo`, `camera:"Synthetic Camera"`, `lens:"Synthetic Lens"`, `location:"Paris, Île-de-France, France"`, `iso:400`, `iso_min:100`, `iso_max:800`, `capture_after:2024-01-01`, `capture_before:2025-01-01`, `gps:"-10,170,10,-170"`, `rating:5`, `flag:pick`, `label:red`, `xmp:pending`, `xmp:conflict`, or `asset:` or `set:` followed by a canonical asset or album UUIDv4. Existing `collection:`, tag, and text predicates combine with these fields. Camera and lens match the complete make or model, ignoring case with Unicode case folding. Values inside one typed filter array combine with OR, while separate filters combine with AND. `locations` and `location:` match the complete location label exactly.

To find unflagged or unlabeled photos, use typed `flags:[""]` or `labels:[""]`, or `flag:""` and `label:""` in expressions. `NOT flag:pick` selects unflagged and rejected photos, and `NOT label:red` selects unlabeled photos and other colors.

Date filters use each photo's recorded local calendar day with an inclusive lower bound and exclusive upper bound. A January 1 photo stays in January 1 date filters even if its recorded UTC offset puts it on January 2 in UTC. GPS filters use inclusive decimal-string latitude and longitude bounds and allow boxes that cross the antimeridian.

Camera, lens, location, ISO, capture-date, and GPS predicates all read the asset's selected display file rather than combining metadata from other members. If a selected RAW has camera A and its paired JPEG has lens B, `camera:A` matches and `lens:B` does not, and a sidecar without camera metadata cannot make `NOT camera:A` match. Changing the display file changes which metadata matches.

Photos text search also searches camera and lens makes and models and location labels from active metadata heads, while `name:` searches filenames only. Relevance ranks matches in order: filename matches first, then authored text and tag names, pending suggestions, technical metadata, and retained content-only matches. A photo that matches the full query only by using pending suggestion text ranks in the suggestions group, below fully authored matches. Each group uses its own BM25 scores, and prefix, phrase, and NEAR operands keep their matching meaning when scored.

After the complete expression narrows eligible members, the best-matching member supplies the asset's score. Authored caption, creator, and copyright match any live non-sidecar member of the asset, so a JPEG caption can match an asset selected by `extension:cr2`. Filename, technical metadata, retained content, name, extension, tag, and collection predicates still match individual members. One member must satisfy the complete expression, using the display file for its photo metadata predicates. For example, `extension:xmp AND camera:A` can match a sidecar paired with a display image from camera A. Saved expressions follow the same rule. Document metadata filters use each document's own file, while ordinary **Documents** text membership keeps its filename and retained-content sources. Excluded, trashed, and displayless assets stay out of results.

Sort by `relevance`, `capture_time`, `import_time`, `name`, `modified_at`, `size`, or `media_type`, with direction `asc` or `desc`. Set `filters.set_ids` to album UUIDs to browse their members; `added_time` requires exactly one album ID after normalization. Capture-time album queries can return burst stacks, while added-time queries keep individual rows. Capture sorting converts recorded offsets to UTC, and omitted zones use civil calendar coordinates. A photo without a readable capture time sorts by its file's modified time when Docbank recorded one; photos with neither sort last in both directions and do not match capture-date filters. Equal keys break ties by asset UUID. Names and media types compare only their first 1,024 characters, so longer values sharing that prefix also fall back to UUID order. Relevance orders by evidence tier and numeric score in the requested direction, then capture placement descending and asset UUID ascending, with only members retained after duplicate collapse contributing scores. On an unchanged library, other sorts page without duplicates or gaps. `path` is unsupported by this route, and document snapshots reject `capture_time`, `import_time`, and `added_time` with an error naming Photos as the supported view.

Each row includes grid, fit, and large preview slots. `missing` means no result is stored for that recipe, while stored `ready`, `unsupported`, and `failed` outcomes keep their generation identity. Only ready slots include a generation URL and JPEG output metadata. Fetch that URL with the browser session header, keeping credentials out of URLs.

Visible display-preview responses carry `Cache-Control: private, no-cache`, so the browser may cache bytes but must revalidate with the server before reusing them. Hidden previews require an active unlock and return `Cache-Control: no-store`, including on `304` responses. A matching `If-None-Match` returns `304` after the server verifies the session, current display, exclusion state, and any required Hidden unlock, avoiding a preview blob read. An excluded asset or replaced display returns `404`, even with a matching validator. Any response returning new bytes verifies the complete image. Listing never triggers preview generation.

The initial count evaluates the whole query, and later pages seek from the last sort key. Queries that collapse duplicate content still evaluate the complete matching population to pick representatives before returning a page.

To prepare a `grid`, `fit`, or `large` preview for a specific `content_version_id`, send `POST /api/v1/photos/assets/{asset_id}/preview`. Include `hidden: true` and an active unlock cookie for a hidden photo. The endpoint returns the same preview slot shape as browsing, reuses retained terminal outcomes, refuses excluded, trashed, or changed displays, and enforces a 30-second bound per request. For namesake crops, include `person_id` with `size: "grid"` to prepare an assigned source version; preparation and byte reads require its live photo membership, active assignment, and Hidden access, and source reads use `no-store`. Camera, lens, exposure, dimensions, and orientation come from the current display's technical metadata. When map tiles are configured, browser Map-projection rows include the selected display file's nullable GPS latitude and longitude.

## Count photos by capture day

Run `docbank photos timeline` to print JSON counts for every recorded capture day in a photo scope, newest day first. The web app's date rail uses these same counts.

```bash
docbank photos timeline
docbank photos timeline --query '{"filters":{"capture_after":"2024-01-01"}}'
```

`--query` takes strict QueryV1 JSON and defaults to `{}`, which covers every included photo. `--coverage` and `--profile-fingerprint` work the same way as in photo browsing. The output's single `capture_day` facet lists one entry per day. `total` counts every matching asset, and `missing` counts assets with neither a readable capture date nor a recorded file modified time. A photo without a capture date counts on the UTC day of its file's modified time. Days follow the same recorded calendar day rules as `capture_after` and `capture_before`.

Over HTTP, send the same `query` and optional `coverage` to `POST /api/v1/photos/assets/timeline`. See the [HTTP reference](../architecture/http-api.md) for limits and errors.

Apply or clear staged metadata edits before changing the selection, search, sort, or viewed photo, or before hiding or trashing photos. **Hide** uses the prepared selection, including unloaded photos, and keeps failed targets selected with their captured revisions.

## Suggested metadata

When you have a [configured vision profile](../configuration.md#photo-descriptions) and processing consent, the inspector displays a suggested caption, suggested tags, and read-only visible text for the displayed file. Choose **Accept selected** to save the checked caption or tags as authored metadata; accepted fields leave the suggestion, and **Dismiss** closes the rest without changing authored metadata. Choose **Undo** or press **Ctrl+Z** or **Cmd+Z** to restore the caption and tags from before the latest acceptance while their saved revisions still match. Undo leaves suggestion decisions unchanged, so accepted fields stay settled. Unchecked fields and visible text remain searchable until you dismiss them. Derivative purge and consent revocation leave pending suggestion text in place; dismissing the suggestion or purging the photo version removes it.

Photo search matches suggested text below authored caption, creator, and copyright and above technical metadata. **Matched AI suggestion** appears on any result that relies on suggested text to match. Accepted tags become ordinary tags, so every tag name assigned to a photo matches text search, even after a rename or content replacement.

Switching the profile keeps existing suggestions and dismissals intact. Photos described before orientation support need an explicit processing run under the updated vision prompt to get a provider rotation answer. After changing a prompt, recompute the [pinned vision fingerprints](../configuration.md#photo-descriptions), review the updated processing plan, and grant fresh consent. A consented explicit run under a different provider fingerprint can refresh rotation while keeping pending and accepted caption and tag decisions. After a dismissal, an explicit run with a different provider fingerprint can offer replacement text along with the new rotation answer, while automatic processing leaves settled descriptions alone. A rotation-only refresh keeps the text producer fingerprint, so another explicit run under a different fingerprint can ask for rotation again, including after an uncertain answer. Backups keep dismissals, accepted-field decisions, and a marker for pending rotation work, while leaving generated text and angles out. After a restore and fresh consent, Docbank describes only undecided fields again and skips descriptions with nothing left to decide. A dismissed caption stays dismissed when restore regenerates its pending rotation, and confirmed rotations do not regenerate. A profile you dismissed on a photo never regenerates another profile's undecided fields on that photo; instead, those go to another ready profile that you have not dismissed there, which automatic work tries for restored photos.

## Export photos

To export photos in the web app, choose **Export all N photos** from the **⋯** menu for the entire current scope, or choose **Export selection** for selected photos. In **Map**, **Export selection** includes selected photos outside the current map area. The [export drawer](export-bundles.md#export-photos) prepares JPEG or PNG copies using your size, metadata, and GPS choices and delivers a verified ZIP. RAW display members export their embedded previews and carry that label. Removing GPS leaves descriptive location names intact. When metadata is enabled, current catalog tags on the display file replace embedded keywords only if that file has at least one tag, and companion-file tags are excluded. Conflicting XMP orientation stops preparation when metadata is enabled; turn metadata off to export the viewed pixels. A changed or unavailable photo stops preparation with an error naming it. See the linked guide for preparation and metadata limits.

![The photo export drawer for three selected photos](https://docbank.ai/assets/generated/web-photo-export.png)

## Move photos to trash

To move selected photos to trash, select them in **Library** and choose **Move to trash**. Confirming moves every member of each selected photo, including RAW files, images, videos, and sidecars, to recoverable trash together. If a photo's revision has changed, Docbank refuses that photo and keeps failed photos selected for retry. Each photo moves atomically, while a multi-photo selection runs sequentially and can make partial progress. Changing the selection changes the targets for the next action. A failed photo outside the loaded grid keeps its captured revision; load and select it again before accepting a newer revision.

To trash rejected photos across the entire current scope, including photos beyond the loaded page, choose **Trash rejected photos** from the **⋯** menu in **Photos**. The preview counts eligible photos and files, lists up to 20 mixed pairs with their total count, and counts photos that stay in Docbank. Every original member, including individually trashed originals, must carry the stored `reject` flag, and sidecars follow their photo. The preview returns exact targets in asset ID order, capped at 1,000 live member files; any photo that would exceed the limit waits for the next batch. Confirming moves those photos atomically. A missing target, changed asset or original node revision, changed reject flag, or changed **Library** or **Hidden** scope refuses the whole batch and requires a new preview. Sidecar writes from XMP sync do not count as changes, whereas a sidecar version that sync has not read yet, such as an outside edit, does. Changes elsewhere in the scope, including album membership, leave the previewed targets valid. Larger scopes still show complete counts alongside how many photos move in the current batch; preview and confirm again for the rest. To narrow the scope, select up to 64 photos and choose **Selected photos** in the modal. Hidden scope requires an active unlock at both preview and confirmation. An audit refusal names the blocked photo and file and leaves the entire batch in Docbank; close the dialog, select up to 64 other photos, and choose **Selected photos** to move them.

![Move rejects preview counting the photos that will move to trash](https://docbank.ai/assets/generated/web-photo-rejects.png)

Moving rejects preserves the loaded range, browsing position, and surviving selections. If reloading fails, the grid stays available, and **Retry** reloads the range and reconciles selections.

HTTP clients preview rejects with `POST /api/v1/photos/rejects/preflight` sending `{query, hidden}`, then confirm with `POST /api/v1/photos/rejects/trash` sending `{hidden, targets}`. Each target includes `asset_id`, the asset `revision`, and `member_revision`, which is the sum of the original members' node revisions; sidecar syncing after the preview does not make it stale. Confirmation reads each member once in the writer transaction, verifies its revision and every original's reject flag, and moves the reviewed files.

From the CLI, run `docbank photos assets trash <asset-id>` for the same operation, using `--revision` to bind the action to an inspected asset revision. Ordinary **Documents** deletion and `docbank document remove` still remove the selected file or folder.

To restore a photo group, open **Trash** in either workspace. **Trash** lists files, including hidden photos' files, in the same way as **Documents**. One row represents its independently trashed members and displays how many trashed files restoring it recovers, including companions inside folders. Restoring any member recovers the complete group. The selected member's revision guards the request, and Docbank reads all affected roots in the same transaction. Restoring a member inside a trashed folder restores that folder's subtree along with the photo's companions elsewhere, returning selected destination folders before their files. Separately trashed unrelated items stay in trash.

Trash keeps file bytes, content versions, photo relationships, and album membership intact. Trash and restore each advance the affected asset revision once and record a change receipt. Permanent deletion waits until every member is trashed, old enough, and free of retention references. For a partially trashed photo, trash the remaining companions with the asset action or detach its live companions before emptying trash. See [Trash and garbage collection](trash-and-gc.md).

Metadata JSONL v1 includes `trash` and `restore` receipts recording the operation and asset revision change, while node trash state remains in the node records. Older Docbank clients refuse imports containing these operations, so use a version that supports photo trash to restore that metadata.

## Map

When you open **Map** from an album, it plots that album's GPS photos. Otherwise, it plots GPS photos matching your current **Library** search and filters. Hidden photos are never mapped. Metadata results sort by capture date, while Meaning and Hybrid results sort by relevance.

To enable **Map**, configure [photo map tiles](../configuration.md#photo-map-tiles) in `[web.map_tiles]` and restart Docbank; **Map** stays hidden from the sidebar until then. Choose **Map** beside **Library** to browse GPS-positioned photos from the current query. Nearby photos group into numbered clusters. Click a marker to open the photo viewer, or click a cluster to zoom in or spread overlapping markers so you can open each photo. Pressing **Escape** returns focus to the marker or its cluster and keeps the map position. The map also keeps its position when an access check conceals and refreshes it, while switching workspaces resets the map position.

![Map with photo clusters beside the photos in view](https://docbank.ai/assets/generated/photo-map-enabled-1440-dark.png)

Each visit to **Map** starts a fresh result and selection, adding GPS eligibility and keeping the search mode's order. Metadata **Map** fetches the newest 20,000 matching located photos by capture date in pages of 1,000, up to 20 requests. Meaning and Hybrid **Map** show up to 1,000 matches, including during metadata fallback. When relevance results are capped, **Map** displays the search status and how many of the best matches it is showing. Narrow your search or filters to see older photos. **Map** has no date filter, and choosing a date in **Library** does not change what **Map** fetches. Returning to **Map** rechecks access and reloads only after a hide or unhide. Like **Library**, **Map** picks up other clients' trash, imports, or edits on refresh. Markers and the grid appear as each page arrives, and earlier results stay visible during a refresh or if a page fails; choose **Retry** to resume a failed page. An expired cursor starts a fresh load. The viewer and arrow keys follow the **Map** query, while the viewport filters only the grid. Panning keeps your selection, and the trash confirmation notes any selections outside the visible grid.

## Cull and compare

Choose **Cull** in **Library** or an album to review the entire current query full-screen. Docbank captures the query's order and membership before starting. A cull accepts up to 250,000 photos; narrow larger queries first. Flag or rating changes keep photos in the current pass, and the next pass refreshes eligibility. Cull decisions set flags and update both files of a RAW and JPEG pair; if the pair's flags or ratings differ, Cull updates only the shown file and says so. Star ratings leave a photo unflagged. Press **S** or click the scope button in the header to cycle through unflagged, picks, rejects, and all photos, with confirmation before resetting an unfinished pass.

![Full-screen Cull with flag, skip, and filmstrip controls](https://docbank.ai/assets/generated/web-photo-cull.png)

### Burst stacks

**Library** groups adjacent capture times from the same import folder, camera make, model, and body serial number. Photos with no recorded serial group with other photos that have no serial, while known serials stay separate. A flat **Choose files** selection shares one folder identity. A collapsed stack shows its representative frame and a count. Selecting a collapsed stack selects every frame; expand it to select individual frames.

![Library showing collapsed burst stacks, each with a frame count](https://docbank.ai/assets/generated/web-photo-stacks.png)

Clearing the search box returns to Metadata browse, and Meaning or Hybrid requests with blank text also use Metadata browse.

Meaning and Hybrid keep their result membership, order, and search status while paging and during background updates, while flags, ratings, rotation, and previews show current values on each page and background refresh. Only an explicit refresh, trashing from the results, or a query change fetches new matches; **Select all** keeps the photos it captured. A ranked page that intersects a stack includes all its live display rows from the captured set. The stack sits at its highest-ranked member's position with the chosen representative first. Continuations may repeat stack rows, and the browser keeps one row per asset ID. Ranked results omit filter counts and the date timeline. Hidden view uses authorized metadata matches and explains the fallback.

To regroup existing photos, choose a **Burst gap** from 1 to 30 seconds in the **Cull** header. Capture-time pages keep each stack together. A photo with missing or outdated camera details groups as one with no serial, so it stacks only with other photos that have no serial. Per-photo flags and ratings remain available. Unreadable originals leave camera details unavailable until they can be read, and background extraction retries them. A notice explains that groups can change after details update, and **Cull** refreshes them when you choose **Retry grouping** or change the burst gap.

When an image index is active, adjacent frames in the same folder and camera stream link if they fall within the configured burst gap, or within 60 seconds with cosine similarity of at least 0.95 in any shared image space. A linked chain stacks together if its total span is at most 60 seconds and it meets the stack size limits; otherwise its frames group by burst gap alone. This applies across capture-date **Library**, metadata relevance pages, ranked snapshots, **Cull**, and stack decisions. Frames without a vector keep burst-gap grouping, and pending image work leaves decisions available. If several bindings supply indexed vectors for one asset in a space, grouping chooses the lowest row identity, ordered by vector-set ID, input ID, then input checksum. Live photo changes, including index publication, quietly regroup **Cull**'s frozen members while its controls remain available.

Hidden photos do not take part in image links. The **Hidden** view groups photos by burst gap only.

In **Cull**, press **H** or **L** to move between stacks, and **J** or **K** to move within a stack. Check frames in the filmstrip and choose **Keep selected** to retain those frames while leaving flags owned by another decision unchanged. Navigation preserves checked frames, while starting a new pass or regrouping clears them. Because a **Cull** stack holds only frames in the current pass, any frame excluded from the pass keeps its flag. Choose **Keep all** to pick every frame in the stack, or choose **Keep current** or press **Return** to pick the viewed frame and reject the rest. Later **Return** decisions replace earlier **Return** assignments only for frames still owned by **Return**. Stack decisions update every live member file together, including paired originals, with one receipt and one Undo.

![Cull on a burst stack with Keep current, Keep all, and Keep selected](https://docbank.ai/assets/generated/web-photo-cull-stack.png)

To change which frame represents a stack without changing any frame's flags, choose **Choose representative**. A rejected frame in a mixed stack cannot become its representative; choose a picked or unflagged frame, or change its flag first.

When auto-advance is on, **P**, **X**, and **U** move to the next photo, while **0** through **5** set the rating and stay on the current photo.

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
| I | Cycle photo details |
| ? | Show the key map |
| Esc | Leave cull mode |

Choose **Skip** to record a skipped photo, and use **Undo** in the decision toast to reverse a decision. **Ctrl**, **Cmd**, and **Alt** shortcuts pause during cull. Apply or clear pending metadata edits before entering, moving, or leaving cull mode.

Choose **Resume cull** to restore your saved pass, position, visited and skipped photos, scope, and auto-advance setting for this vault in this browser. Its captured membership survives snapshot expiry and browser restarts. Because each daemon restart gives local `docbank web` a new browser address, saved progress from an earlier address no longer appears; [signing in on a server or container](web.md#sign-in-on-a-server-or-container) keeps a single address. Earlier progress stays in browser storage until site data is cleared. Starting a new cull asks before replacing saved progress, and storage failures appear when you choose **Cull** or **Resume**. If another tab changes saved progress, close and resume the latest pass. At completion, Docbank checks the captured set once; flag changes or Undo check it again, and **Previous** cancels a check in progress. **Review unflagged remainder** appears whenever unflagged photos remain anywhere in the captured set, including skipped and never-viewed photos, while missing or newly hidden photos count as unavailable. Cull is not offered in **Hidden**.

To compare photos side by side, select 2 to 8 photos and choose **Compare** in the selection bar. Zoom, pan, and arrow-key navigation move all panes together, and **Z** toggles 1:1 for each displayed preview. Choose **Manual closeup**, then click a face or draw a region on a full frame to show matching crop tiles below the full frames. Choose **Clear closeup** to remove them. Manual closeup uses the area you chose rather than automatic face detection.

![Compare showing three photos side by side](https://docbank.ai/assets/generated/web-photo-compare.png)

### People in photos

Once a photo preview loads, choose **Add face** to draw a face box. Drag a box around the face, or focus the photo's face control, position it with the arrow keys, and press **Enter**. Press **Escape** to cancel placement. If a create request has an uncertain result, choose **Retry face** or press **Enter** to retry the same box; new placements wait until the pending request resolves.

In the inspector's **People** section, pick a name from the search results or choose **Create person**. Each face has its own name picker. Creating a person and naming the face save together, so a revision conflict creates neither. After an uncertain response, the picker reloads the face before letting you make another choice.

People you create stay in your vault even if you clear their photo assignment or remove their face, and removed faces still count toward the photo's 1,000-face limit. Run `docbank people retire` to retire an unwanted person.

Names can be up to 200 UTF-8 bytes. Duplicate names are allowed: choose **Create person** to add a different person with the same name, and merge people later if needed. See [browser names and visibility](people.md#browser-names-and-visibility) for search eligibility and duplicate-name recognition.

Name search ignores case and matches the start of any word. For example, **Smith** finds **John Smith** and **John—Smith**. Letters, numbers, and combining marks stay within a word, while punctuation and other separators start a new one. Search text is literal, including `%` and `_`. Choose **Load more** to load additional results, and finish loading the search before **Create person** becomes available.

To edit or remove face assignments:

- **Clear a name.** Open a named face's picker and choose **Clear name**.
- **Remove a face.** Choose **Remove face** to dismiss a mistaken box and its closeup. Removing an unnamed manual face takes effect immediately because you can redraw it, while removing an assigned face asks for confirmation. Removed faces cannot be restored in the browser, though the face-decision API retains undo receipts.
- **Mark a person in the whole photo.** Choose **Mark person in photo** to select an existing person or create one from a new name; the person and whole-photo mark save together. Choose **Clear whole-photo mark** and confirm to clear a photo-wide mark while keeping any named faces.

HTTP callers supply `person_name` with a caller-chosen UUIDv4 `person_id` and reuse both fields when retrying. A name without an ID, or an existing ID with a different name, returns `422 invalid_photo_face`, and a retired person returns `404 not_found`. Retrying with the same ID and name reuses the saved assertion.

Each face shares its number across the inspector, Loupe, and Cull closeup, and removing a face closes any gaps in those numbers. Unnamed faces appear as **Face N**. Cull restores face captions after a rating refresh, even if the photo leaves the **Library** filter. The [namesake chooser](people.md#browser-names-and-visibility) lists identity details and loads face previews when you expand a candidate.

Click an active person's name beside the picker to filter photos to that person. **Library** links open **Library**, and **Hidden** links stay in the unlocked **Hidden** view. Filtering clears the text search and shows every match in capture-time order. Naming or clearing a face refreshes loaded views that have person filters or search text in the same Hidden scope. Choose **Retry** after a **People** page error, including an empty first page. Retired names keep their labels and correction controls. Face labels follow the photo through rotation, zoom, and pan.

**Cull** displays whole-photo marks first, followed by one closeup per face in creation order, matching the **Face N** labels. Closeups reuse the fit preview already loaded for the photo and rotate with it. If that preview is unavailable, each tile displays **No preview** and you can choose **Retry preview** to try again.

## Best shots

Burst stacks display **KEEP** and **CUT** suggestions automatically when at least two frames have comparable measurements. The planner ranks shared focus, open eyes, aesthetics, and framing with weights of 100, 90, 50, and 45. The eyes score uses the least-open prominent face, and because blur duplicates inverse focus, it adds no second vote. Ties favor RAW originals, then stable photo IDs. Unmeasured frames stay undecided, frames with confirmed flags leave planning before ranking, and a stack with an owner-confirmed pick receives no further keeper proposals.

Face closeups display **Sharpness**, **Light**, **Facing**, **Prominence**, **Eyes**, and **Smile**, showing **Unavailable** for missing observations. A face's worst measured sharpness, light, facing, or eyes score sets its grade:

- **Red.** Below 0.33 for a subject occupying at least 2.1% of the frame; smaller subjects below 0.33 grade yellow instead.
- **Yellow.** From 0.33 to below 0.42.
- **Green.** 0.42 and above.

**Smile** describes the face without affecting its grade, and tiny crops leave sharpness and light unavailable.

Select a stack and choose **Accept all** or **Dismiss all**, or use **P** to pick and **X** to reject through the ordinary photo editor. Acceptance verifies the displayed **KEEP** and **CUT** choices, complete file revisions, and observed stack membership in one transaction; if a choice or stack has changed, Docbank asks you to reload and changes nothing. Each batch creates one receipt. Undo restores accepted or manually edited flags, or reverses the latest dismissal so the suggestions become available again. Dismissing confirms each offered frame's current flag as your choice, and **Keep current** preserves those flags on the other frames.

Backups keep confirmed flags, while pending suggestions and face report caches regenerate locally. Audited vaults support the same best-shot decisions and ordinary **P** and **X** edits.
