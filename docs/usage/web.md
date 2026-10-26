---
last_edited: 2026-10-11
title: Web application
description: Find, read, organize, and export documents in the local, authenticated web app.
---

# Web application

Use the local web application to find documents, read any saved version, and
export the records you select. You can also import files and email, organize
them with tags, and review configured processing before it runs. Start it with:

```bash
docbank web
```

Docbank starts or reconnects to the vault's daemon and opens the browser. The
daemon is the background process that owns the vault. The browser uses its
authenticated HTTP API. It does not open the database or stored files directly.
The daemon accepts connections only from the local machine.

Choose a task:

- [Browse the vault](#browse-the-vault), [use keyboard shortcuts](#use-keyboard-shortcuts), inspect document details, or
  [select documents on this page](#select-documents-on-this-page).
- [Upload files](#upload-verified-documents), [import email](#import-a-mailbox), or [download content](#download-verified-content).
- [Process a version and read its text](#process-a-version-and-read-its-text).
- [Read archived email](#read-archived-email) and [follow attachments](#follow-email-attachments).
- [Manage tags](#manage-tag-definitions) and [search text](#browse-tags-and-search-text).
- [Run complete queries](#work-with-a-frozen-query) with exact paging and facets.
- [Export a verified ZIP](#export-a-verified-zip) from selected documents or a frozen query.
- [Move documents to trash](#move-a-node-to-recoverable-trash) or [restore them](#restore-from-recoverable-trash).
- [Inspect versions](#inspect-immutable-versions) and [source records](#understand-where-a-document-came-from).
- [Read permanent history](#read-permanent-audited-history) or [verify its evidence](#verify-permanent-audit-evidence).
- Inspect [background jobs](#inspect-background-work), [storage](#inspect-physical-storage), or [backup snapshots](#inspect-backup-snapshots).

The browser has limited permissions. See [Browser authentication](#browser-authentication)
for their scope and [Current limits](#current-limits) for workflows that
require the CLI or another API client.

![The Docbank web application showing a synthetic vault tree and the selected document's authority card.](https://docbank.ai/assets/generated/web-vault-browser.png)

*Select a document to see its stable identity and content hash beside the table.*

## Browse the vault

**Documents** and **Photos** at the top of the sidebar switch workspaces.
Photos shows **Library**, **Map**, **Albums**, and your album list in the sidebar.
Documents/Photos switches keep your place until the session locks. Leaving an album resets its view. Open Library at `/photos`, the
album index at `/photos/albums`, or an album at `/photos/albums/{id}`. Map requires [tile configuration](../configuration.md#photo-map-tiles) and a restart. See [Browse photos](photos.md#browse-in-the-web-app).

Use the sidebar for **All files**, **Saved searches**, **Import history**, and
**Trash**. **Export** offers **Download files…**, **Search term report…**, and
**Bates-numbered PDFs…**. **System** groups **Disk use**, **Running now**, **Backups**,
**Protected folders**, and **Usage reporting**. On a narrow screen, **Open navigation** reveals the
same menu.

Folder breadcrumbs let you jump to any parent. Each file has an icon and a
plain type name, so you do not need to recognize its media type.

- Click a row once to inspect it. The authority card updates without opening
  or downloading the file.
- Double-click a folder, select it and press Enter, or use **Open folder** in
  the authority card to navigate into it.
- Use the back arrow to restore the previous folder or search view, including
  its inspected row and sort order.
- Click **Document**, **Type**, **Size**, or **Modified** to sort. Click the
  active heading again to reverse its direction. Directories remain grouped
  ahead of files in folder views. Narrow windows hide **Modified**, then
  **Type**. Deep folder paths show the first folder and the last two, with the
  folders between them in the **…** menu.
- Use refresh to reload the current directory by its ID. If another client
  renamed or moved that directory, the browser adopts its current canonical
  path.

The authority card is the details panel for the selected file or folder. A
node ID identifies that entry even after a move or rename. Its revision is a
change counter that Docbank uses to reject changes based on an outdated view.

The browser selects the first row after loading a folder. A selected directory
shows its path and modification time. A selected file also shows its exact
logical size. Open **Details** for the node ID, media type, current-version
UUID, and SHA-256 content identity. The copy buttons copy the complete UUID or
digest even when the card wraps it across lines. Every selected node also shows its assigned tag names.
Hovering a tag shows its UUID and vault-wide assignment count. The tag stack
expands in place when a node carries more than six tags.

![The slide-out navigation menu on a narrow screen](https://docbank.ai/assets/generated/web-navigation-mobile.png)

## Use keyboard shortcuts

Press <kbd>?</kbd> or choose the keyboard button in the top bar to see every
shortcut. Press <kbd>/</kbd> to focus search, <kbd>j</kbd> or <kbd>k</kbd> to
inspect the next or previous loaded row, <kbd>Space</kbd> to check or uncheck
the inspected file, <kbd>Enter</kbd> to open the inspected folder, and
<kbd>Escape</kbd> to clear page selection. Row navigation stops at the loaded
page boundary and never fetches another page.

In Photos, <kbd>B</kbd> adds selected photos to the last chosen album, or opens the album picker when no target is set.

The same help dialog can assign digits <kbd>1</kbd> through <kbd>9</kbd> to tag
definitions. Bindings are separate for each vault and stay in this browser for
the current daemon run. Reopening the page keeps them. Restarting the daemon or
clearing browser data requires setting them again.

Pressing a configured digit toggles that tag on the inspected file only,
regardless of checked documents. Docbank waits for the file's complete
assigned-tag list and sends its displayed revision with the change. A stale
revision remains visible as an error and is never retried against newer state.

App shortcuts pause while a text field, control, or dialog owns the keyboard.

![Keyboard shortcuts and a configured tag hotkey](https://docbank.ai/assets/generated/web-keyboard-shortcuts.png)

## Select documents on this page

Check a document's checkbox to mark it without changing the document shown in
the authority card. The bottom dock shows how many documents are checked.

- Hold Shift while clicking another checkbox to select or clear a range in the
  displayed sort order. With the keyboard, focus a checkbox and press Shift+Space.
- Use the header checkbox or **Select visible documents** to check the files
  currently shown. Folders remain available for navigation and are not selected.
- Choose **Clear selection** or close the dock to clear the checked documents.

Selection covers only the loaded page, even when more results exist. Changing
the folder, query, or tag clears it when the new rows arrive. If an error leaves
the previous results on screen, their selection stays. Refreshing the same
view keeps only checked documents that are still visible. Back navigation and
ending the browser session also clear the selection.

![Two documents selected on the current page while the authority card shows one document's details.](https://docbank.ai/assets/generated/web-page-selection.png)

## Export selected rows as CSV

Check file rows and choose **Export page CSV**. The download preserves their
displayed order and includes the metadata already loaded, such as document IDs,
versions, paths, sizes, timestamps, and hashes. Unavailable fields stay blank.
It does not download document contents, fetch additional metadata, or include
results beyond the loaded page. Formula-leading text is escaped for spreadsheet
imports, and international filenames are preserved.

![Checked documents and the page CSV export action](https://docbank.ai/assets/generated/web-visible-page-csv.png)

## Edit a complete query

Enter text in the top bar and choose **Plain words** or **Query syntax** from
its menu. Choose **Advanced** to open **Filters and sort**. Select the sort
field, direction, and documents per page. **More** holds the processing profile
selector, shown when more than one profile is available, and **Filters JSON** for
structured filters. **Search** runs the top-bar text with these settings.

Saved and linked queries also show their expression, labeled **Plain words**
or **Query syntax**. **Syntax help** appears for query syntax. The daemon
validates each edited draft after a short pause. Errors appear beside the
expression or filters. **Focus query error** selects the reported part of an
expression. A newer edit cancels an older validation request.

**Save…** opens a new definition in **Saved searches** with the entire draft.
**Run** beside a saved search opens its results. From an import in **Import history**,
**Search this import** starts a draft scoped to that import.
Escape, clicking outside, or toggling **Advanced** closes the modal. Edited
drafts stay in the tab, with a dot on **Advanced**; reopening restores unfinished
input too. Valid query edits also update the URL fragment. **Cancel** discards
the draft and clears its fragment. Closing an unchanged new draft leaves no dot.

Validation does not execute a search. Once the draft is valid, **Search**
creates a frozen result through the complete query endpoint, which uses word
search. Clear the search box to return to ordinary search.
See [field-aware query syntax](searching.md#preview-a-field-aware-query).

![The complete query editor with a validated field and Boolean expression](https://docbank.ai/assets/generated/web-query-bar.png)

## Work with a frozen query

A completed run becomes the accepted frozen snapshot for the tab. Draft edits
remain separate until you choose **Search** again. Choose 50, 100, or 250
documents per page before the run. The workspace shows the result count, **Previous page**
and **Next page** controls when results span more than one page, and the node, content
version, hash, size, revision, and tags observed when the snapshot was created. Later
vault changes do not replace those rows. **Clear search** returns to the folder.
**Tag all…** tags every result. Select rows to open the selection dock, which offers the same
actions as the folder view. Live-folder keyboard shortcuts stay inactive
while the snapshot is open.

Select a frozen row to inspect those original facts and the selected version.
The same card loads current path, revision, tags, provenance, and audit status
separately as live observations. A later rename, retag, or content replacement
therefore does not rewrite the snapshot facts or substitute the new head for
preview and download.

Use **Filters** to narrow by import, tag, media family, extension, modified month,
size, and duplicates. Imports appear under their names. The list leaves out values no
result has and groups that cannot narrow anything. Choosing a facet or changing the
query creates a new snapshot. It never splices live results into the accepted one.

Photos' date rail counts each included photo asset once by capture day. It
creates no snapshot and does not block Documents queries. Choose a year or month
to scroll there, including dates beyond the loaded grid. See
[Browse photos](photos.md#browse-in-the-web-app) and
[Count photos by capture day](photos.md#count-photos-by-capture-day).

Snapshot handles last for one daemon lifetime, up to 15 minutes idle and 30
minutes total. Locking the browser session or stopping the daemon revokes them.
If paging reports that the snapshot is gone, run the complete query again and
use only the new snapshot and its cursors.

![A frozen query with fixed counts, facets, and document versions](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

## Read archived email

Choose **Email** in the content inspector for an EML document whose selected
version has decoded metadata. Read decoded headers, inspect the original
ordered headers, and switch between HTML and plain-text body alternatives.
Missing or invalid sender dates are labeled as such. Pending decoding and
incomplete MIME inventories do not appear as complete messages.

HTML opens in an isolated frame. Sender scripts, forms, links, styles, and
remote resources are disabled before display. Blocked or ambiguous images have
visible placeholders. Only verified PNG and JPEG MIME parts in the body's nearest
related group can display inline. Press **Escape** inside the frame to return
to its entry button. Long messages scroll, and **Show quoted text** expands
quoted content without changing the original or the full-body PDF source.

The viewer accepts HTML bodies up to 16 MiB. Its additional display limits are
100,000 markup candidates before parsing, 100,000 HTML nodes, 128 nesting
levels, and 32 MiB of serialized output. Literal `<` characters also count
toward the conservative markup limit.

Inline images are limited to 128 occurrences, 5 MiB and 8 million pixels per
image, 8,192 pixels per side, and 16 MiB and 32 million pixels in aggregate.
PNG/JPEG limits and unsupported image formats produce visible placeholders.
Body limits report an unavailable operation instead of silently truncating the
body.

**Inspect attachment documents** opens the attachment navigation described
below. Inline display and attachment document publication remain separate.
**Download** downloads that EML version unchanged. The
existing PDF action downloads retained PDFs or uses the configured renderer,
including the complete chosen body and quoted text. New rendering requires the
pinned local Chromium/font configuration. An unavailable renderer reports its
reason.

![Archived email with decoded headers and its HTML body](https://docbank.ai/assets/generated/web-email-reader.png)

![Raw headers from the same retained message](https://docbank.ai/assets/generated/web-email-headers.png)

## Follow email attachments

Choose **Attachments** in the content inspector to list the documents
attached to the selected version under **Attachments** and the messages it is
attached to under **Attached to**. A document with neither shows **No
attachments**. Repeated filenames and identical bytes stay separate rows.
**Show more** requests another page, up to 1,000 rows per group. An
unavailable read reports an error, and an empty list then does not mean the
message has no attachments. Rows without a published document show their
outcome and cannot be opened.

Choose a row to open that related version in its own context. Later content
replacement does not substitute the live head or its media type for that
related version. Preview, text, and download keep the usual byte verification
and current-access checks. Missing or denied targets report an error without
removing the row.

Follow nested messages with **Back to previous document**, or choose **Return
to frozen document** to restore focus to the original result. These steps do
not rerun the query, change its snapshot membership, or check attachment rows.
Ordinary live selections offer the same return path to the selected document.

![Exact attachment documents linked to the selected email](https://docbank.ai/assets/generated/web-email-attachments.png)

## Export a verified ZIP

In Photos, choose **Export all N photos** in the **⋯** menu for the current scope or **Export selection** for checked photos. Prepare JPEG or PNG copies with size, image metadata and GPS choices. Each preparation allows up to 16 photos, each at most 100 million pixels. The verified ZIP retains original filenames, vault paths and source hashes. See [Export photos](export-bundles.md#export-photos).

Choose **Export selection** in the selection dock for the checked documents.
The toolbar's **Export → Download files…** opens the whole frozen query when one is active, or
the documents on the current page otherwise. Folders are not export members.
Exports support up to 100,000 documents and 50 GiB of role payloads.

Choose **Download ZIP**. By default the ZIP holds the original files. Open
**Options** to also include retained text, page images, or email body PDFs, to
share exact duplicate outputs, or to split the ZIP into volumes. Each role is
**Required**, **If available**, or **Skip**. A missing required role stops the
export before anything is built. An **If available** role records its
unavailable status in the bundle.

**Download ZIP** copies and checks every frozen page, stores the selected
versions and available role files, builds the archive, and hands it to your
browser. Later edits to the live query, tags, or current content do not
substitute new members or bytes. When some outputs are unavailable, the drawer
stops on the frozen plan and lists them; choose **Download anyway** to continue.
The ZIP filename applies only to the browser download, and bundle paths stay
deterministic. Originals are not redacted or sanitized by annotation overlays.

The drawer offers the download only after it receives that job's verified
archive receipt. It displays the final size and SHA-256 so you can check the
downloaded file independently, and keeps a **Download ZIP** button in case the
browser blocked the first download. The browser saves the archive directly.
Docbank does not load the entire ZIP into browser memory. Check your browser's
download list for local completion.
**New export** [releases the finished job](export-bundles.md#free-a-finished-job-slot) for both photos and documents, and asks you to wait while a download is still running.

Closing the drawer stops its progress reader, not the server job. Reopen
**Export** in the same browser session to reconnect to that job, or choose
**Cancel export** to cancel it. A disconnected stream is not a completed
export. Expired authority requires choosing **Download ZIP** again. Reloading or ending the
browser session does not preserve the drawer's job handle.

![A verified bundle ready to download from the reviewed frozen selection](https://docbank.ai/assets/generated/web-export-ready.png)

## Stamp selected pages with Bates labels

Open **Export → Bates-numbered PDFs…** and select a sealed package. Bates labels
are sequential identifiers stamped on PDF pages, often used when exchanging
records for review.

1. Create or select a label namespace, including its prefix and number width.
2. Choose **Preview Bates labels** and inspect the proposed labels and pages.
3. Reserve the displayed range, then choose **Start Bates export**.
4. Wait for **Stamped PDF ready**, then choose **Download verified PDF**.

The output contains the package's selected pages. The stored page map keeps
their original source-page numbers. A number that was reserved is never issued
again, even if the export was abandoned.

![A completed Bates export with its reserved labels and verified PDF](https://docbank.ai/assets/generated/web-bates-export.png)

## Assign and remove tags

1. Select a file or folder.
2. Choose **Add tag**, or **Manage** when it already has tags.
3. Type a tag name, then select a match or **Create “name”** to create and assign it. Remove an assigned tag with its **×** button.

The dialog opens with the cursor in the tag field and lists assigned tags separately from available tags. It loads the
first 1,000 definitions in name order, then displays related names in groups.
The picker uses the same color for a tag wherever it appears. Use the arrow
keys to navigate its options, Enter to select, and Escape to close it.

Every change is bound to the node ID and revision shown in the dialog.
After success, the browser updates the node revision and the tag's vault-wide
assignment count. It also refreshes the tag catalog and permanent-audit
status. If another person, agent, or CLI command changed the node first, the
dialog keeps the failed decision visible and asks you to refresh rather than
applying it to newer state.

### Tag selected documents

Use the document checkboxes to select files on the displayed page, then choose
**Tag selected…** in the selection dock. The picker shows whether all, some, or none
of the selected documents have the chosen tag. **Add to all** and **Remove from
all** apply one atomic, revision-checked operation to at most 1,000 documents.
Type a new name and choose **Create “name”** to create the tag and add it to every selected document.
Folders and undisplayed query results are not included in page selection.

Keep the dialog open if the result is uncertain: **Retry same operation**
reuses its original identity and revisions. A stale selection requires you to
refresh. The browser does not retry automatically against newer documents.
After confirmation, the browser reloads current observations. The retained
receipt may describe an earlier successful operation. See
[selected-set tagging](organizing.md#tag-a-selected-set-atomically) for retention
and backup behavior.

![The tag dialog shows how many selected documents already have the chosen tag](https://docbank.ai/assets/generated/web-batch-tags.png)

### Tag a frozen query

In a frozen snapshot, the results header shows the result count and
**Tag all…**, and the selection dock shows **Tag selected…** for the checked
rows on the visible page. Both open the same dialog: choose or create a tag,
pick **Add** or **Remove**, and read the line under it, such as “Adds
“Reviewed” to 128 documents. Undo with Remove.”, before you apply. Removing a
tag can't be undone automatically.

**Tag selected…** applies one atomic, revision-checked change to at most 1,000
documents and reports how many changed. If the response is lost, **Retry**
resends the same operation without applying it twice. A stale document means
you close the dialog and select again.

**Tag all…** → **Continue** captures every result, up to 250,000 documents,
and prepares revision-checked batches of at most 1,000. The captured
population does not expand when an untagged query starts receiving the tag.
Completed changes appear as overlays while the frozen rows and facet
membership remain unchanged. New actions use confirmed tag revisions only when
the receipts connect back to the frozen document revision. Other document
changes still make it stale. Once prepared, an action keeps its original
requests and expected revisions.

**Apply** saves a recovery file, then runs the batches in the same dialog, with
progress and **Pause**. Pressing **Apply** is the explicit confirmation.
**Review documents** lists the vault and action identities and,
in pages of 50, node IDs, content versions, hashes, sizes, and expected
revisions. **Cancel** before anything was sent drops the prepared action.

The browser journals the action in IndexedDB for that origin. When a run
stops early, for example after **Finish later**, a lost response, or a closed
tab, the results header shows that it didn't finish, with **Resume** and
**Discard…**. Another tab can resume it, but only the original operation
identities, requests, and expected revisions are retried. If a response is
lost after the daemon commits, **Retry** recovers the retained receipt without
applying the change twice. Starting a new **Tag all…** replaces a finished
run.

A daemon restart creates a fresh browser origin and session, so choose
**Tag all…** → **Resume from file…** and select the saved recovery file to
continue. Import validates and stages the action but does not run it. The
vault must match, and the new session still requires reading the file back
and **Apply**. A stale document revision blocks the action. Docbank does not
silently refresh its target or partially apply that batch.

Recovery files contain instructions supplied by their creator. Checksums detect
inconsistent files, but they do not authenticate the creator. Review the
documents before applying an imported action. Receipts inside a file do not
update the snapshot's observed revisions. Only receipts returned by the daemon
during this session do that.

Recovery files contain private document identities, content-version
identities, hashes, sizes, and revisions. They omit paths, names, query text,
and browser credentials, but still need the same protection as vault metadata.

Discarding removes the browser journal and does not roll back changes that
already committed. If a tag was deleted, the dialog still lets you save the
recovery file or discard the run. **Discard…** in the results header also
clears an unreadable journal for the current vault, so corrupt browser data
cannot block later actions.

## Manage tag definitions

Open the tag filter and choose **Manage tags** to create, rename,
or delete the vault's shared tag definitions. Creating a tag allocates a new
UUID. Renaming keeps that UUID while advancing the definition revision and
every assigned node's metadata authority. A stale rename remains visible
instead of overwriting a definition changed by another person or agent.

Deleting an unused tag happens in one click. Deleting a tag that is on files
asks first and says how many files lose it. Deletion removes the definition and
all of its assignments, but never deletes a document or its stored content. Reusing the
same name later creates a different identity. After a rename or deletion, the
browser reloads the active folder, search, or tag view so node revisions and
assignment counts stay current.

Tags use a color derived from their ID. A rename keeps that color.
Slash-separated names create display groups automatically: `matter/acme/reviewed`
appears as `reviewed` under `matter/acme`. The full name remains available in
the tooltip and to assistive technology. Groups do not create parent tags or
change assignment rules.

The catalog shows the first 1,000 name-sorted definitions and discloses the
complete count. Use `docbank tag`, the paginated HTTP API, or an embedded
client for exhaustive definition management and assignments outside the
displayed selection.

## Export dated search counts

Open **Export → Search term report…** to create a CSV of search counts for
selected sources and date ranges. Review unresolved dates before downloading
the CSV or evidence ZIP. **Recent reports** keeps the latest 100 requests and
outcomes. **Use as draft** runs a request against current data without its old
date choices. See [Search exports](search-exports.md) for the full workflow,
count definitions, and expiry limits.

![Search counts for one selected version with CSV and frozen evidence downloads](https://docbank.ai/assets/generated/selected-report-result.png)

## Saved queries and highlights

Open **Saved searches** in the sidebar to run, rename, or delete a saved search.
Create searches from **Advanced → Save…**. **Rename** opens the name and
description fields; **Edit as JSON** reveals the complete definition. Choose
**Save changes** to update it or **Save as new** to keep a copy. The list is
paginated in groups of 100. **New highlight list** creates reusable highlights.

**Keep query draft** keeps the complete query in the tab and URL fragment.
The fragment contains query text and filters, so treat copied URLs as private.
It does not contain browser-session credentials. Saving a definition does not
replace the kept draft or change the URL. **Cancel** in advanced search, locking the
tab, or an expired or rejected session clears the draft and its URL fragment.
Drafts do not carry over when you open a new session with `docbank web`. Save a
named definition before reloading or leaving the session if you need it later.

Saved searches retain the search definition. **Run** creates a new result set
using current documents. Keeping or saving the draft alone does not run it or
change the current live results. This browser run does not produce the durable
saved-run receipt that the authenticated HTTP API exposes. Unknown fields are
rejected, not silently dropped.

Highlight sets hold 1–64 unique literal terms, each up to 256 Unicode characters,
with lowercase `#rrggbb` colors. They cannot run as queries. Choose one in a
document's **Text** tab to apply its ordered colors without changing the saved
definition. Neither result rows nor document bodies are stored in browser
preferences.

Edits and deletions use the definition's inspected revision. A stale response
remains visible without automatically retrying against newer state. Reload the
definitions and reopen the item before deciding again. Deletion requires a
separate confirmation naming the definition. It never deletes
documents or discards the kept query draft.

![A named query retaining its complete expression, filters, and sort](https://docbank.ai/assets/generated/web-saved-queries.png)

![A reusable highlight set with literal terms and colors](https://docbank.ai/assets/generated/web-highlight-sets.png)

## Move a node to recoverable trash

Choose **Move to trash** on the selected live file or folder: the trash
button for a file, or the folder's actions menu. The item leaves the list at
once and a notice names it, with the item count for a folder. A folder and its
live descendants move to trash together. Choose **Undo** in the notice to
restore it.

The daemon applies the mutation only if that node revision is still
current. If another person, agent, or CLI command changed the node first, the
notice shows the error and the item stays listed. A successful receipt removes
the selection and refreshes the current folder, search, or tag view in place.

This action is recoverable. It does not empty trash, garbage
collect content, reclaim packed space, or erase permanent audited history.

## Restore from recoverable trash

Choose **Trash** in the sidebar to inspect the newest 1,000 independently
restorable roots. Each entry shows its name, kind, node ID, revision,
trash time, and logical size for files. A folder entry represents the complete
subtree that left the live tree in that trash operation.

Choose **Restore** to return the item at once. Docbank returns the same
node and its content to its original live parent when that directory still
exists. If the parent is unavailable, restore falls back to the vault root. If
the chosen name is already occupied, Docbank adds its normal collision suffix.
A notice shows the actual canonical folder, names a missing original folder
when this browser trashed the item, and counts the files of a photo group. Choose **Show** to open the folder.

If the revision changed, the notice shows the error and the item stays in the
drawer. After a successful restore, the browser removes the item from the trash
drawer and reloads the current view.

The browser cannot empty trash. Permanent tree-metadata deletion, subsequent
garbage collection, and packed-space reclamation remain CLI or
master-authenticated API operations.

## Upload verified documents

1. Browse to the destination folder.
2. Choose **Add → Upload files** or **Add → Upload folder**.
3. Select files or a folder from this device, then choose **Upload** in the drawer.
4. Check each file's result in the upload drawer.

Upload is available only in a live folder, not in search or tag results. The
drawer shows the destination path once in its heading.

Docbank makes two bounded-memory passes over each selected file. The first pass
computes the browser's declared SHA-256 while showing hashing progress. The
second streams the bytes with visible progress over a dedicated upload channel.
Before the upload button becomes available, the daemon proves that channel
with a random secret issued through the ownership-pinned CLI handoff. File
bytes never use an ordinary reconnectable browser HTTP request.

The channel is bound to one browser session and one daemon lifetime. If it
breaks, the page permanently disables upload rather than reconnecting to
whatever process now owns the loopback port. Run `docbank web` again to obtain
a newly proved channel. The daemon independently computes the hash and size
and grants node/blob authority only when both match. The browser compares that
receipt again before reporting **Added** or **Already present**, then refreshes
the destination by ID.

Files are independent queue entries: one rejection does not make another
success ambiguous, and failed entries can be retried. Cancellation can race
with a daemon commit whose receipt did not reach the browser, so the drawer
labels that item **Unconfirmed**, refreshes the destination, and directs the
operator to retry. Uploads are idempotent, so the retry converges on the stored
result. Name/content collisions keep the ordinary ingest suffix behavior
rather than overwriting a document. Selecting a local file never changes or
removes the source.

The folder picker keeps each file's relative path. The browser creates the
matching subfolders under the destination (reusing any that already exist)
before it uploads the files inside them. Server-filesystem ingest,
watched-inbox configuration, and replacing an existing document remain CLI or
authenticated API workflows.

## Import a mailbox

1. Browse to the destination folder and choose **Add → Import mailbox**.
2. Choose an MBOX file or Google Takeout ZIP, then **Upload and preview**.
3. Review the detected mailbox entries and choose **Import messages**.
4. Follow the imported and rejected counts until the job finishes.

Each message occurrence becomes its own email document. Repeated messages
remain separate occurrences, and attachments become child documents. An
interrupted import resumes after its last saved message. See
[mailbox import](../cli-reference.md#docbank-mailbox) for CLI status and receipts.

![A completed Takeout import with three separate message occurrences](https://docbank.ai/assets/generated/web-mailbox-import.png)

## Import load files

1. Browse to the destination folder.
2. Choose **Add → Import load files**.
3. Choose a ZIP that holds the DAT or CSV load file, an optional OPT or LFP
   page map, and the referenced original documents, images, and supplied text.
4. Pick the metadata profile, page-map profile, and source encoding, then
   choose **Upload and preview**.
5. Resolve blocking diagnostics, name the package and sending party, then
   choose **Import package**.

The ZIP uses the authenticated upload channel and is stored as a sealed
container before preflight reads it. The drawer shows imported, gap, and total
counts, plus the operation ID, as the job runs.

**Keep supported records and report gaps** allows an import to keep records
when referenced files become unavailable after preflight. Blocking preflight
diagnostics must still be resolved. Without this option, the worker verifies
every declared file before creating document entries. **Index package-supplied
text** makes the sender's text files searchable.

Browser source metadata omits fields marked sensitive by the retained mapping; compact Map-projection rows include latitude and longitude when map tiles are configured.
Member lists and raw timeline inputs require the daemon API key.

Closing the drawer stops its progress updates. **Lock web session** revokes
the session and cancels unfinished imports it owns. Cancellation keeps
committed records and removes unfinished document entries as described in the
[package command reference](../cli-reference.md#docbank-package).

![A load-file ZIP preview before importing its documents and supplied text](https://docbank.ai/assets/generated/web-load-file-import.png)

## Process a version and read its text

Processing extracts a document's text and stores it. With a configured
embedding service, it also prepares the document for search by meaning.
Configure a [processing profile](../configuration.md) first. A new vault has
none.

1. Select a document and choose **Extract text** beside its name. This action appears when a configured profile can extract text from the file.
2. Read where the file goes. A local profile says it runs on this computer;
   a network or hosted profile names the service and its address, and says
   whether the file name goes too. **Details** lists the exact plan, service,
   deployment, and fingerprints.
3. The first time you use a profile, tick **Allow "profile" to process
   documents in this vault**. That permission covers this operator and profile
   configuration for every document, not just this file, until it is revoked
   or expires. Later runs need only the button.
4. When the run finishes, choose **Read text** to read the extracted text
   Docbank stored for that version.

To withdraw permission, open **Details** and choose **Revoke all processing
permissions**. It applies to every profile.

![The processing plan discloses local extraction and a configured network embedding service](https://docbank.ai/assets/generated/web-document-processing-plan.png)

![A finished text extraction with its text ready to read](https://docbank.ai/assets/generated/web-document-processing-result.png)

![Verified retained Markdown for the selected source version](https://docbank.ai/assets/generated/web-document-rendition.png)

Opening the plan or reading coverage does not start processing. A restored
vault needs fresh consent before network processing. The
[processing guide](document-processing.md) explains consent and provider
requirements. To inspect results without the browser, see
[CLI coverage and text windows](../cli-reference.md#docbank-processing).

When a processing profile stores search vectors, each file row shows a
**Find similar** action. Choose it to compare its stored embeddings
with the file versions loaded in the current view. This calls no provider. If
embeddings are missing, build them with a processing run, which has its own
review and consent. See [Find similar documents](document-processing.md#find-similar-documents).

![Related documents found from stored embeddings in the loaded view](https://docbank.ai/assets/generated/web-similar-1440.png)

## Download verified content

The inspector puts the preview above details, tags, and **History**. Drag the inspector's left edge, or focus it and press the arrow keys, to resize it. Double-click the edge to reset. Your browser remembers the width. Click a path, version ID, SHA-256, or ID to copy its full value. **Download** saves the selected version. **Extract text** appears beside it when a configured profile fits the file. The **⋯** menu holds **Move to trash**. Folders have an **Open** button.

Protected items show a **Protected** chip that opens **Audit history**. For an unprotected folder, choose **Protect permanently…** in **⋯** to copy its command-line preview command. The command previews retained history and vault-wide metadata before you explicitly enable irreversible protection.

The selected document card previews PDF and PNG pages, eligible UTF-8 text,
and bounded PNG, JPEG, and WebP images. Animated WebP images may animate in the browser. Text is rendered as inert text, never as
document HTML. Docbank verifies the selected version UUID, size, media type, and
SHA-256 after receiving the complete body and before publishing text or an
image URL. Unsupported files
and text larger than 16 MiB remain available through **Download** without being decoded in the page.

For PDF files and PNGs with retained page geometry, **Preview** reads retained
pages and images. Other PNGs use the verified original-image preview, including
when the optional page runtime is unavailable.

Choose **Render page** when an image is missing. This requests only that page.
Opening the inspector or moving between pages never starts bulk rendering.
Rendering needs the [optional local page runtime](../architecture/page-images.md).
PDF pages render at 144 DPI. The viewer prefers retained images at that density.
Otherwise it displays the highest retained DPI and offers **Render page at 144
DPI** when the runtime is available. PNGs keep their native physical density.
Unsupported geometry, unavailable runtime, partial images, render failure, and
a changed source are shown separately. Retained images remain readable when the
runtime is unavailable.

**Previous page**, **Next page**, and the page selector navigate within the
document. Navigation rechecks the inventory to include newly retained images.
**Fit width** and numeric zoom change only its display size. Each page uses its
own rotated crop dimensions. The browser checks the actual frame, recipe, image
digest, byte length, and decoded dimensions before displaying it. Changing the
source, page, session, or tab clears the old image and releases its resources.
**Cancel render** is available for the active request created here. A matching
request created elsewhere offers a refresh without taking ownership.

Choose **Text** to read the verified text rendition for the selected version
and processing profile. The browser binds the source, profile, generation,
attachment, build, artifact, length, and digest before decoding the text. A
check icon shows verification. **Find** searches within the displayed text
and shows its matches in place of query and saved highlights;
match counts and arrows appear when there is a query and matches, respectively.
The Text tab stays hidden when it would repeat a plain-text original in Preview.
Processing failures, verified-empty text, and unavailable historical renditions
remain distinct. Eligible original UTF-8 text is a fallback for that same version
when no readable rendition is available. Without a processing profile, **Set up**
opens the processing guide. With a profile, **Process…** opens the processing plan.

Query text operands are highlighted from the resolved query, not by splitting
the expression in the browser. Negated and structured operands are excluded.
The browser refuses query highlights if a saved definition revision no longer
matches the accepted snapshot. **Find** and saved highlight sets share bounded,
non-overlapping marks and previous/next match controls. All content stays inert.

Previous and next document controls move through the accepted frozen snapshot,
including across its existing page cursors. They stop at the snapshot boundary
and do not replace an expired snapshot with live results. **Duplicates** lists
at most 16 live documents with the selected content hash. Opening one creates a
separately labeled live context. **Return** restores the frozen selection.

Choose **Download** on a file to retrieve its selected
version. A live row selects its current head. A frozen query row keeps the
version captured by that snapshot even when the live head has since changed.

Docbank first copies the object from loose or packed storage into owner-private
daemon staging while the browser shows verified byte progress. The selected
node revision, version UUID, SHA-256 identity, and size must still agree before
that work starts. The current node revision authorizes access to a retained
historical version but never changes which bytes were selected. A concurrent
replacement, move, or trash operation therefore asks you to refresh instead of
silently downloading a different document.

After the complete content passes verification, Docbank issues a one-use
download ticket. The browser starts its save only after that check succeeds.
Cancellation or a verification failure removes a pending private staging file
and publishes nothing to the browser. Changing the selected source or locking
the session also cancels pending previews and revokes published image URLs. A
ticket identifies only its prepared file, expires after two minutes, and cannot
call any other Docbank route. It is not the vault API key or the browser
session.

Preparation streams on the daemon and does not buffer the object in browser
memory. It temporarily needs local free space equal to the document's logical
size. Docbank reports that verified bytes were handed to the browser, not that
the browser or operating system completed its final save. Use `docbank get`
when automation needs a durable receipt after private staging, file sync,
atomic publication, and parent-directory sync.

## Inspect immutable versions

Choose **All versions** in a file's **History** section to inspect retained versions
without losing the current folder, search results, or selected document. The
newest version appears first. Each entry says whether the content was
created, replaced, or restored from a prior version, with its recorded time and
size, and has a **Download** button. **Details** shows the version ID, SHA-256,
media type, and the version it was restored from.

Open a link with `#node=<node ID>&version=<version UUID>` to inspect that exact
version, including versions older than the newest 1,000 listed in the drawer.
For drawers opened from these links, selecting a version updates the address,
and browser Back and Forward restore the selected version. Missing,
purged, or mismatched links show an unavailable message.

Selecting an entry shows its complete record: full version UUID, SHA-256
content identity, exact byte count, media type, canonical timestamp,
introducing operation UUID, and the source version for a revert. The current
head is marked. A revert is a new immutable version and does not erase or
relabel the earlier one.

Trashed files carry a **Trash** label. Restore the file before downloading a
version.

Choose **Download** in the complete-version panel to retrieve that version
through the same private staging, progress, terminal verification, and one-use
handoff as the current-content action. The version UUID, owning node, SHA-256
identity, size, and current node revision must all agree before preparation
begins. Historical versions do not keep a filename timeline, so the browser
uses the live document's current name.
Version comparison remains a CLI or authenticated API workflow.

The drawer reads at most the newest 1,000 versions and says when older history
exists. Use `docbank versions list`, its pagination flags, or the authenticated
HTTP API for exhaustive automation. This view does not compare bytes, revert,
or prune history.

## Understand where a document came from

The **History** section shows the five most recent versions and imports together, newest first. Click an import entry, such as **Added from command line**, to see its complete import history.
Each entry shows how the document arrived, its copyable source path or
filename, and the import date. Corrected entries show **Superseded**.

Open **Import details** for the import ID, record hash, document ID, source
reference, original modification time when supplied, and any replaced record.
Click a value to copy it. The drawer uses the document state returned with the
history, so a concurrent move displays its current path and a trashed document
is labeled as trashed.

An original reference records the source at import time. It does not guarantee
that the source still exists. It may be a portable relative path from a watched
inbox, a local path recorded by an ordinary ingest, or an opaque reference
supplied by an embedded application.
The browser does not open, validate, change, or retain that external source.

The drawer reads at most the newest 1,000 facts and says when older provenance
exists. Use `docbank provenance`, its pagination flags, or the authenticated
HTTP API for exhaustive automation. Provenance correction and external-content
pinning are not browser workflows.

## Browse tags and search text

Enter a word or phrase in the search box and press Enter. Results can match a
live document name or verified extracted text. The results heading shows the
count, each row shows the name with its folder beneath, and the **Match** column
identifies which one. **Previous** and **Next** in the inspector step through the
results. Name matches keep their API relevance ranking and appear before
content-only matches until you choose a column sort.

Use the tag selector in the browser toolbar without a text query to browse
up to 1,000 live items carrying that tag. The result is read in one metadata
snapshot, so its node state and complete virtual paths cannot mix concurrent
moves, trash operations, or content updates across pages. Trashed assignments
are omitted from this live view and disclosed in its count. Use
`docbank tag nodes` for the exhaustive paginated list of live and trashed
assignments.

With text in the search box, the same selector requires that tag in addition
to the name or content match. The selector displays tag names, but both
workflows are bound to the tag's UUID, so a later rename does not change which
definition was selected. Changing the selector reruns the current browse or
search.

The search box searches words in names and document text. When a processing
profile has a query embedding binding, it also searches by meaning
automatically. The menu selects **Plain words** or **Query syntax**, matching
Photos. Query syntax accepts expressions such as `name:invoice AND tag:urgent`
and runs through the complete query endpoint.

**Rerank results** is opt-in when a processing profile supports it. Base rows
appear before reranking settles. Degraded reranking keeps those rows and shows
its cause. Other processing failures fall back to names and text with a visible
explanation.

![The Docbank web application showing extracted-text search results in a synthetic vault.](https://docbank.ai/assets/generated/web-search-results.png)

*Search results show each document's folder under its name and offer the same
authority card as ordinary folder browsing.*

Clear the search box to return to the selected tag's assignment view, or to
the current directory when **All tags** is selected. The selector loads at most
the first 1,000 name-sorted tag definitions and discloses when more exist. Use
`docbank tag list` for the tag catalog. Use `docbank search` for directory,
media-type, or modification-time filters, structured JSON, or another result
limit. See [Searching](searching.md) for limits and truncation.

## Read permanent audited history

The authority card checks the selected node's audit membership. A green
**Protected** badge means ordinary deletion, version pruning, garbage
collection, and repacking cannot erase that node's history.
**Not audited** means audit authority exists in the vault but the selected node
is outside every permanent scope. **Dormant** means no scope has been enabled.

Choose **Audit history** on a protected node to open a wide timeline without
losing the current folder, search results, or selection. The timeline is newest
first and explains the primary change for each event: live and retained-trash
paths, content-version transitions, tag definitions and assignments, or
provenance. Select an event to inspect its complete event, operation,
scope, and node identities; canonical timestamp and origin; before/after
revision, path, and version state; and typed tag, provenance, or photo payload. Photo states show authored values and confirmed field names before and after edits and undo, including confirmation of an empty caption.

The first page contains at most 50 events. **Load older events** follows the
API's append-stable cursor, so new activity cannot shift or duplicate the
history already being inspected. Protection status remains authoritative even
when a page contains no events. The web application does not infer protection
from an empty or non-empty timeline.

## Verify permanent audit evidence

Choose **System → Protected folders** to run the vault-wide permanent
audit verifier. The verifier does more than read stored status. Docbank
replays canonical audit history against the current node, version, membership,
topology, tag, and provenance projections, then reads and recomputes SHA-256 for
every unique blob retained by protected history.

A successful result reports the protected and verified file totals and the
protected size. **Details** holds the vault and allocation-lineage identities,
the operation high-water mark, the allocation entry count and head, and each
scope's terminal entry count and chain head. Copy buttons preserve the complete
identities even when they wrap. A vault with nothing protected shows the
`docbank audit enable` command instead of a failure. Metadata, missing-content,
corruption, and unreadable-content problems stay visible with the affected hash.

This drawer runs only a fresh proof of current authority. It does not accept a
previous evidence bundle, enroll a scope, or change protected state. Record
`docbank audit verify --json` outside the vault and later use
`docbank audit verify --expected` when that external copy must act as a rollback
trust anchor. Closing the drawer cancels its active request. Maintenance
contention or interruption remains visible, and you can retry.

## Inspect background work

Choose **System → Running now** to see work in progress, paused workers,
and failures. Active work appears first, with counts when known and its pause,
resume, worker count, and cancellation controls beside it. When workers are
waiting for work, a quiet **Nothing running** line sits above the worker list.
The list below shows every worker and its status, and offers pause controls for
idle workers. Queued operations stay in that list until they start.

Pause or resume photo imports, visual previews, and the storage lanes `place`,
`evacuate`, `repair`, and `salvage`. Pause takes effect at the next safe item
boundary. Visual preview concurrency accepts 1 through 4. A paused lane keeps
its Resume control after its last operation disappears.
The drawer reads lane settings with the job list and uses `PUT` to change them.
The drawer refreshes every two seconds while open. If another client changes
controls first, review the refreshed settings and try again. Controls survive
daemon restarts. The list includes the newest 1,000 storage operations.
Finished storage operations clear within an hour after their retention ends
unless they still own pending cleanup. Worker records disappear
when the daemon restarts. Browser errors omit backend paths; use the CLI to
inspect details.

## Inspect physical storage

Choose **System → Disk use** to see how the current vault occupies
managed blob storage. Tiles show the total on disk and the number of stored
files. When packs hold dead payload, **Reclaimable** shows how much and what
share of the pack payload it is. **Locations** has one row per storage location
with its kind (filesystem or S3), stored size, object count, how many objects
have no other copy, and its observed online, unavailable, fenced, or unbound
state. When an unhealthy location is the only authority for an object, the drawer
says how many objects currently have no readable alternative. It never exposes
binding paths, endpoints, buckets, credential profiles, or ownership epochs.

**Storage** separates four related quantities:

- **Loose files** is the physical inventory of individual raw or zstd files.
  It can include untracked files or redundant loose copies of packed objects,
  so it is not an authority count.
- **Packed** is authoritative content stored in immutable pack files.
  The view shows both its logical raw size and its stored size.
- **Pack files** is the complete stored payload of every pack, including live
  and logically dead entries.
- **Waiting to compact** is logically dead payload that still occupies those
  pack files.

Waiting-to-compact bytes have not been reclaimed. This distinction matters when a
GC report has removed unreachable catalog mappings but the vault's disk usage
has not fallen by the same amount. The percentage beside **Reclaimable** shows
how much of the current pack payload is dead, not a compression ratio or a
promise that every pack is immediately eligible for compaction.

This drawer is read-only and refreshes from the daemon's current catalog. It
cannot pack, garbage-collect, or repack content. Use `docbank storage status`
for structured or scripted inspection, see [Multi-store storage](storage.md)
for repair and placement, and run `docbank storage repack` when you intend to
rewrite eligible sparse packs and retire their old files.

## Inspect backup snapshots

Choose **System → Backups** to inspect the recovery points in
the repository selected by `[backup] repo` in `config.toml`. The drawer shows
the repository path and repository ID so you can tell which repository you are
inspecting. Snapshots appear newest first with their tag, creation time,
full or incremental relationship, file count, logical content bytes, and the
bytes newly added by that capture. A copy button on each row copies the
snapshot ID.

An initialized repository with no snapshots is different from an unconfigured
repository, and the browser reports those states separately, each with the
command that fixes it. The browser does
not accept an arbitrary server path, initialize a repository, create a
snapshot, verify content, restore a vault, or delete retention history.

Seeing a manifest in this list is not proof that its referenced bytes are
still readable. Run `docbank backup verify` to check repository integrity, and
periodically restore into a separate vault to rehearse the complete recovery
path.

## Browse import history

Choose **Import history** in the sidebar.
Each row shows its label or source folder name, import time, document count,
and size. Select an import to see its source paths, each with a copy button,
browse its current live members, and open a document by its node identity. Counts are
current membership, rather than a historical import total.

Labels belong to the import group, not its documents. Rename or clear a label
under its inspected revision. If another client changes it first, the drawer
keeps your draft and reports the conflict. Reload the label before deciding
whether to save again. Label changes are unavailable once permanent audit
authority has been enabled.

Collection and member lists show at most 100 entries each. Empty groups,
failed reads, and truncated lists are reported separately. Use the paginated
HTTP API to browse beyond that limit. This is direct member browsing, not
a collection-filtered text search.

Choose **Quality** for document distributions, duplicate
content counts, zero-byte files, media/extension mismatches, and text coverage.
With multiple processing profiles, choose one before reading coverage. Without
a configured profile, coverage is unavailable. That does not mean zero failures
or complete processing. A configured policy does not mean an extraction adapter
is running.

Coverage distinguishes complete, partial, failed, unprocessed, and activated
output with no text. Retained searchable output takes precedence over a failed
retry. The existing worker rejects blank provider output as a failed attempt.
Quality reads are bounded to 250,000 members, a 64 MiB census, and five seconds.
An exceeded limit returns an error instead of a partial summary.

Select extensions, media types, or media families and choose **New query** to
open a collection-scoped draft. Multiple selected values use OR. This does not
change live results or execute a search. Concentrations describe common values,
not document defects.

![An import collection with a saved label and its document members](https://docbank.ai/assets/generated/web-collections.png)

![An import collection showing file types and processing coverage](https://docbank.ai/assets/generated/web-collection-quality.png)

## Sign in on a server or container

When an operator configures `server.api_key`, the **Open your Docbank** page
accepts that key and exchanges it for the same limited browser credentials
issued by `docbank web`. The input clears after submission. Docbank never
stores the key in browser storage, returns it in a response, or uses it for
subsequent browser requests. Each tab keeps its scoped credentials in memory.
Sign in again after a reload or daemon restart. If a later request finds the
session expired or revoked, Docbank clears that tab's selection and open
dialogs and returns it to sign-in.

![The Open your Docbank page asking for an API key](https://docbank.ai/assets/generated/web-signin.png)

1. Set a fixed API port and a strong API key in `$DOCBANK_HOME/config.toml`.
   With `public_origin`, the key must be at least 32 characters; generate one
   with `openssl rand -hex 32`. Protect this file as a credential. Configure
   the exact public origin:

   ```toml
   [server]
   api_port = 7777
   api_key = "output-of-openssl-rand-hex-32"
   idle_timeout = "0"

   [web]
   public_origin = "https://archive.example.test"
   allowed_hosts = []
   session_lifetime = "24h"
   ```

2. Keep the daemon on loopback. Terminate HTTPS at a protected reverse proxy
   that preserves the public `Host` and forwards WebSocket upgrades to the
   API port. Docbank takes authority from configuration, never `Forwarded`
   or `X-Forwarded-*` headers. Every request, including API-key requests,
   health checks, and static pages, must use the public authority, a concrete
   loopback backend authority, or an exact entry in `[web] allowed_hosts`.
   Docbank also admits these authorities through the server Host allowlist.
   Entries are `host[:port]` values; schemes and wildcards are rejected.
3. Open the public origin and enter the API key. Use HTTPS for remote access.
   An HTTP origin is accepted on loopback; non-loopback HTTP requires
   `trust_private_network = true`, which explicitly trusts the private
   network and its proxy with the key and browser credentials.

Without `public_origin`, configured-key login is available at the daemon's
fresh loopback web origin, and key login adds no Host restriction beyond the
server's [`allowed_hosts`](../configuration.md). The usual `docbank web` fragment handoff also works
with ephemeral API keys and keeps its existing upload proof and permissions.

Key login sets a host-only, HttpOnly, SameSite=Strict instance cookie; HTTPS
also sets Secure. Each tab receives its own token and upload proof.
The cookie alone cannot read the vault. Requests need the tab token and cookie;
mutations also need the exact public Origin. Cross-site requests
are rejected. Credential requests reject redirects.

`session_lifetime` accepts 1 minute through 90 days. Omitting it or setting
`"0"` uses 24 hours. Expiry is absolute. Failed sign-ins are not locked out;
the key length requirement keeps guessing impractical. A daemon allows up to 512 active key-login sessions.
Sessions are held only in memory and are excluded from vault backups.

Choose **Sign out** to revoke the current tab. Login and logout do not rotate
or delete the shared cookie or invalidate other tabs. An operator can list
and revoke individual key-login sessions without seeing their credentials:

```bash
docbank web sessions
docbank web sessions --json
docbank web sessions revoke SESSION_ID
```

Session IDs are non-secret digests. These CLI commands use the daemon's
ownership-proven master connection; browser credentials cannot manage other
sessions. Expiry, revocation and daemon shutdown cancel the session's uploads,
exports and prepared reads through the existing browser resource lifecycle.

## Browser authentication

When Docbank opens the browser, it writes a small launch page beside the
owner-private daemon runtime record and passes only that credential-free local
file path to the operating system. Before doing so, the ownership-pinned CLI
asks the daemon to exchange its master API authority for a random,
daemon-lifetime browser session. The master key stays on that pinned connection
and never enters browser storage, a URL, or a child-process argument.

The daemon serves that session from a second listener with a cryptographically
random `docbank-<random>.docbank.localhost` hostname and a newly selected loopback port. This browser
origin is independent of the configured API port and unique to one daemon
lifetime. A process that later captures either port therefore cannot leave a
service worker or cached script waiting for a future browser session.

Photo import recovery uses a persistent HttpOnly, SameSite=Strict cookie on
`docbank.localhost` to locate the saved selection in the same vault after a
restart. The cookie grants no API access. Each checkpoint request still needs
the current browser session, matching Host and Origin. Other browser data stays
with its per-run origin.

The launch page carries the scoped session and its random upload-proof secret
in a URL fragment. Browsers do not include fragments in the initial HTTP
request. The application removes them from the address bar and holds them only
in page memory. Ordinary requests send `X-Docbank-Web-Session`. The daemon
accepts that credential for the following operations:

| Permission | Limit |
|------------|-------|
| Read the tree, nodes, search results, tags, versions, and provenance | Uses the same result limits and document IDs as the ordinary API. |
| Read audit status and history | Does not enroll or change a permanent scope. |
| Verify permanent audit history | Does not run general metadata/content verification or backup verification. |
| Read background jobs and physical storage status | Can pause or resume supported lanes, set visual preview concurrency, and cancel individual storage operations. |
| List backup snapshots | Uses only the repository already configured for this daemon. |
| List trash | Returns a bounded list of restorable roots. |
| Prepare, preview, or cancel an exact-version download | Writes only a private temporary file, enforces preview MIME and size limits, and issues one expiring ticket for that file. |
| Plan, preview, run, cancel, and download exports | Owns exact sources, frozen plans, jobs, and verified tickets within this browser session. Download naming cannot select a server destination. |
| Read verified text and exact-content duplicate context | Revalidates the selected source and rendition identities. Duplicate context is bounded to 16 live-current references. |
| Read retained email metadata and MIME parts | Source version, decoder generation, part, role, byte size, and SHA-256 are checked before display. Browser reads cannot start decoding. |
| Read page geometry and images, request or cancel page rendering | Binds the source revision, version, page, frame, and renderer recipe. The inspector requests one page at a time. |
| Move to trash or restore | Requires the selected node ID and its current revision. |
| Create a folder (`POST /api/v1/nodes` with no query string) | Makes directories only, for folder uploads. File creation stays on the upload channel. |
| Add or remove a tag assignment | Requires the selected node ID and its current revision. |
| Create, rename, or delete a tag definition | Rename and delete require the inspected tag revision. Deletion reports the removed assignment count. |
| Read and manage saved query or highlight definitions | Edit and delete require the saved definition's revision. Permanent audit history blocks these writes. |
| Read collections and their members | Returns bounded lists of live import membership. |
| List, create, rename, star, set covers, duplicate, or delete photo albums; add or remove members | Existing-album writes require the inspected revision. Membership requests accept up to 1,000 IDs per request or a complete live query without a total member cap. Removing members or deleting albums keeps the photos. See [Albums](photos.md#albums). |
| Prepare, edit, or undo photo metadata (`POST /api/v1/photos/authored/prepare`, `/api/v1/photos/authored/edit`, `/api/v1/photos/authored/{receipt_id}/undo`) | Edits bind up to 1,000 live files to their asset, file and node revisions. Each edit target may also supply `content_version_id`, which fences the asset's displayed version whichever member is edited. A replaced version or a different display file returns HTTP 412 `stale_revision`. Fields and keywords share one transaction and receipt. Undo requires matching revisions; hidden photos also require an unlocked Hidden view. |
| Search people; read and draw photo faces; name or clear a face | Naming a face can create its person. Standalone person creation requires the API key. Face edits require the face revision. Drawing creates a box. The browser can mark an existing person or create a person by name in a whole photo and clear whole-photo marks. Person links filter the current workspace; Hidden links stay in the unlocked Hidden view. Hidden face reads and edits require an unlock. |
| Set or clear a collection label | Requires the inspected collection-label revision. |
| Create and page a frozen query snapshot | Uses complete validated query intent. Handles remain bound to this session and daemon lifetime. |
| Apply a tag to an exact frozen population | Requires captured node revisions, a saved recovery file, and confirmation. Retries preserve the original operation identities. |

Use the bookmark button to [manage saved queries and highlights](#saved-queries-and-highlights)
or **Advanced** to [validate and run a complete query](#edit-a-complete-query).

Upload uses a separate WebSocket: a connection that never reconnects during the
session. The browser must prove it holds the upload secret before sending any
file bytes. Upload can create only file nodes beneath the live
directory selected in the browser. Folder uploads also create the subfolders they
need through `POST /api/v1/nodes`, which makes directories only. The daemon independently checks the
caller-declared hash and size, as it does for other remote writers.

These permissions do not allow emptying trash, document mutations other than
the revision-checked tag workflows above, audit enrollment, backup creation,
restore, maintenance, configuration changes, or access to general API
endpoints. A browser session never receives the master API key. See the
[HTTP API](../architecture/http-api.md) for the route and credential contracts.

The lock button revokes the session in daemon memory and clears the page.
Every remaining fragment browser session and its dedicated browser origin disappear
when that daemon stops. Run `docbank web` again to create a fresh origin and
session against the ownership-proven daemon.

Closing the browser tab does not stop the daemon or revoke other sessions.
Use the lock button when the current tab should lose access immediately, and
use `docbank daemon stop` to stop the daemon. All browser sessions end when
the daemon stops.

The launch file remains beneath `$DOCBANK_HOME/web-launch/` with the same
owner-only Unix permissions or Windows DACL as the runtime record. It is
runtime state, excluded from snapshots, replaced by the next launch, and
removed when the daemon stops.

Use `docbank web --no-browser` only when another local program must open the
URL. That output contains the live browser credentials. Do not put it in shell
history, logs, screenshots, issue trackers, or chat.

## Current limits

Folder, tag, search, and recoverable-trash views show at most 1,000 rows and
say when more exist. Use CLI or HTTP pagination to list complete folders, tags,
and trash. Search has no continuation cursor. Narrow incomplete results as
described in [Searching](searching.md#how-do-i-handle-incomplete-results).
It uses the same name and verified-text matching rules as `docbank search`.
Results initially preserve the API's relevance ranking. Choosing Document,
Size, or Modified changes to that column order. Document compares the complete
paths shown in search results rather than only their basenames.

Refreshing a folder resolves its node ID, current canonical path, and
children in one metadata snapshot, so a concurrent CLI or agent move cannot
leave the browser constructing child paths beneath an obsolete name.

The current web application does not compare versions, recursively import
folders, edit, revert, prune, move live nodes between folders, empty trash,
enroll audit scopes, or run maintenance, backup creation, backup verification,
general metadata/content verification, or restore operations. Frozen-query tag
actions are capped at 250,000 documents. Use an authenticated API client
for larger or different bulk workflows.

Original-file previews support UTF-8 or ASCII text up to 16 MiB and PNG, JPEG,
and WebP images up to 32 MiB. Animated WebP images may animate in the browser. PDF and PNG page rendering accepts sources up to
64 MiB within the [page runtime limits](../architecture/page-images.md). Empty
or larger sources offer verified download without a page preview. Other
document, image, audio, video, and archive formats use verified download.

If a page reports that its browser session or upload channel expired, ended,
or was rejected, run `docbank web` again. Neither credential nor the upload
channel survives daemon restart, and the previous random `.localhost` origin
is not reused.
