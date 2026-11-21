---
last_edited: 2026-10-31
title: Web application
description: Find, read, organize, and export documents in the local, authenticated web app.
---

# Web application

Use the local web application to find documents, read any saved version, and
export the records you select. You can also import files and email, organize
them with tags, and review configured processing before it runs. To start it,
run:

```bash
docbank web
```

Docbank starts or reconnects to the vault's daemon and opens your browser. The
daemon is the background process that owns the vault. Your browser talks to it
through an authenticated HTTP API instead of opening the database or stored
files directly, and the daemon accepts connections only from your local machine.

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
for what it can do and [Current limits](#current-limits) for workflows that
require the CLI or another API client.

![The Docbank web application showing a synthetic vault tree and the selected document's authority card.](https://docbank.ai/assets/generated/web-vault-browser.png)

*Select a document to see its stable identity and content hash beside the table.*

## Browse the vault

Switch between workspaces using **Documents** and **Photos** at the top of the
sidebar. In Photos, the sidebar shows **Library**, **Map**, **Albums**,
**People**, and your album list. Switching between Documents and Photos keeps
your place until the session locks, while leaving an album resets its view. You
can open Library at `/photos`, the album index at `/photos/albums`, People at
`/photos/people`, or an album at `/photos/albums/{id}`. Map requires
[tile configuration](../configuration.md#photo-map-tiles) and a restart. See
[Browse photos](photos.md#browse-in-the-web-app) for more on the photo workspace.

In the Documents sidebar, choose **All files**, **Searches**, **Productions**
(when sets exist), **Imports**, or **Trash**. Searches, Productions, Imports,
and Trash open full pages at `/searches`, `/productions`, `/imports`, and
`/trash`. Browser Back and Forward keep you on the right page. Reloading also
keeps the page when you sign in with an API key, while `docbank web` starts at
the root. Under **Export**, you can choose **Download files…** or
**Bates-numbered PDFs…**. **System** groups **Disk use**, **Running now**,
**Backups**, **Protected folders**, and **Search by meaning**. On a narrow
screen, choose **Open navigation** to reveal the same menu.

Folder breadcrumbs let you jump straight to any parent folder. Each file shows
an icon and a plain type name, so you do not need to recognize its media type.

- Click a row once to inspect it. The authority card updates without opening
  or downloading the file.
- To open a folder, double-click it, select it and press **Enter**, or choose
  **Open** in the authority card.
- Use the back arrow to return to the previous folder or search view, with its
  inspected row and sort order intact.
- Click **Document**, **Type**, **Size**, or **Modified** to sort, and click
  the active heading again to reverse the order. Directories stay grouped ahead
  of files in folder views. Narrow windows hide **Type** first, then
  **Modified**. Deep folder paths show the first folder and the last two, with
  the folders in between tucked into the **…** menu.
- Use refresh to reload the current directory by its ID. If another client
  renamed or moved that directory, the browser adopts its current canonical
  path.

The authority card is the details panel for the selected file or folder. Its
node ID identifies that entry even after a move or rename, and its revision is
a change counter that Docbank uses to reject updates based on an outdated view.

After loading a folder, the browser selects the first row. A selected directory
shows its path and modification time, and a selected file also shows its exact
logical size. Open **Details** to see the node ID, media type, current-version
UUID, and SHA-256 content identity. The copy buttons copy the full UUID or
digest even when the card wraps it across lines. Every selected node also lists
its assigned tag names. Hovering an assigned tag in the details panel shows its
full tag name, and the tag stack expands in place when a node carries more than
six tags.

![The slide-out navigation menu on a narrow screen](https://docbank.ai/assets/generated/web-navigation-mobile.png)

## Use keyboard shortcuts

To see every shortcut, press <kbd>?</kbd> or choose the keyboard button in the
top bar. Press <kbd>/</kbd> to focus search, <kbd>j</kbd> or <kbd>k</kbd> to
inspect the next or previous loaded row, <kbd>Space</kbd> to check or uncheck
the inspected file, <kbd>Enter</kbd> to open the inspected folder, and
<kbd>Escape</kbd> to clear your page selection. Row navigation stops at the
loaded page boundary and never fetches another page.

In Photos, press <kbd>B</kbd> to add selected photos to the last chosen album,
or to open the album picker when no target is set.

You can also assign digits <kbd>1</kbd> through <kbd>9</kbd> to tag definitions
in the same help dialog. Bindings are separate for each vault and stay in your
browser for the current daemon run. Reopening the page keeps them, while
restarting the daemon or clearing browser data requires setting them again.

Pressing a configured digit toggles that tag on the inspected file only,
regardless of which documents are checked. Docbank waits for the file's
complete assigned-tag list and sends its displayed revision with the change. If
the revision is stale, the error stays visible and Docbank never retries
against newer state.

App shortcuts pause whenever a text field, control, or dialog has keyboard focus.

![Keyboard shortcuts and a configured tag hotkey](https://docbank.ai/assets/generated/web-keyboard-shortcuts.png)

## Select documents on this page

Check a document's checkbox to mark it without changing the document shown in
the authority card. The bottom dock counts how many documents you have checked.

- To select or clear a range in the displayed sort order, hold **Shift** while
  clicking another checkbox. With the keyboard, focus a checkbox and press
  **Shift+Space**.
- To check every file currently shown, use the header checkbox or choose
  **Select visible documents**. Folders stay available for navigation and are
  not selected.
- To clear the checked documents, choose **Clear selection** or close the dock.

Your selection covers only the loaded page, even when more results exist.
Changing the folder, query, or tag clears it once the new rows arrive, though
an error that leaves the previous results on screen keeps their selection.
Refreshing the same view keeps only checked documents that are still visible.
Navigating back or ending the browser session also clears the selection.

![Two documents selected on the current page while the authority card shows one document's details.](https://docbank.ai/assets/generated/web-page-selection.png)

## Export selected rows as CSV

To export metadata for files on the current page, check their rows and choose
**Export page CSV**. The download keeps their displayed order and includes the
metadata already loaded, such as document IDs, versions, paths, sizes,
timestamps, and hashes, leaving unavailable fields blank. It does not download
document contents, fetch additional metadata, or include results beyond the
loaded page. Docbank escapes formula-leading text for spreadsheet imports and
preserves international filenames.

![Checked documents and the page CSV export action](https://docbank.ai/assets/generated/web-visible-page-csv.png)

## Edit a complete query

To build a complete query, enter text in the top bar and choose **Plain words**
or **Query syntax** from its menu. Choose **Advanced** to open **Filters and
sort**, where you can pick the sort field, direction, and documents per page.
**More** holds the processing profile selector (shown when more than one profile
is available) and **Filters JSON** for structured filters. Choose **Search** to
run the top-bar text with these settings.

Saved and linked queries also show their expression, labeled **Plain words** or
**Query syntax**, and **Syntax help** appears when you use query syntax. The
daemon validates each edited draft after a short pause and shows any errors
beside the expression or filters. Choose **Focus query error** to select the
reported part of an expression. A newer edit cancels any earlier validation
request.

Choose **Save…** to open a new definition in **Searches** with the entire draft,
or **Run** beside a saved search to open its results. From an import in
**Imports**, **Search this import** starts a draft scoped to that import. You
can close the modal by pressing **Escape**, clicking outside, or toggling
**Advanced**. Edited drafts stay in the tab with a dot on **Advanced**, and
reopening restores unfinished input too. Valid query edits also update the URL
fragment. Choose **Cancel** to discard the draft and clear its fragment; closing
an unchanged new draft leaves no dot.

Validation does not execute a search. Once your draft is valid, **Search**
creates a frozen result through the complete query endpoint, which uses word
search. Clear the search box to return to ordinary search. See
[field-aware query syntax](searching.md#preview-a-field-aware-query) for the
supported syntax.

![The complete query editor with a validated field and Boolean expression](https://docbank.ai/assets/generated/web-query-bar.png)

## Work with a frozen query

A completed run becomes the accepted frozen snapshot for the tab, while draft
edits stay separate until you choose **Search** again. Before you run the query,
choose 50, 100, or 250 documents per page. The workspace shows the result count,
**Previous page** and **Next page** controls when results span more than one
page, and the node, content version, hash, size, revision, and tags observed
when Docbank created the snapshot. Later vault changes do not replace those
rows. Choose **Clear search** to return to the folder, or **Tag all…** to tag
every result. Selecting rows opens the selection dock, which offers the same
actions as the folder view. Live-folder keyboard shortcuts stay inactive while
the snapshot is open.

Select a frozen row to inspect those original facts and the selected version.
The same card loads the current path, revision, tags, provenance, and audit
status separately as live observations. Because of that, a later rename, retag,
or content replacement does not rewrite the snapshot facts or substitute the new
head for preview and download.

Use **Filters** to narrow results by import, tag, media family, extension,
modified month, size, and duplicates. Imports appear under their names, and the
list leaves out values no result has along with groups that cannot narrow
anything. Choosing a facet or changing the query creates a new snapshot instead
of splicing live results into the accepted one.

In Photos, the date rail counts each included photo asset once by capture day.
It creates no snapshot and does not block Documents queries. Choose a year or
month to scroll there, even for dates beyond the loaded grid. See
[Browse photos](photos.md#browse-in-the-web-app) and
[Count photos by capture day](photos.md#count-photos-by-capture-day).

Snapshot handles last for one daemon lifetime, up to 15 minutes idle and 30
minutes total. Locking the browser session or stopping the daemon revokes them.
If paging reports that the snapshot is gone, run the complete query again and
use only the new snapshot and its cursors.

![A frozen query with fixed counts, facets, and document versions](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

## Read archived email

To read an EML document whose selected version has decoded metadata, choose
**Email** in the content inspector. You can read decoded headers, inspect the
original ordered headers, and switch between HTML and plain-text body
alternatives. Missing or invalid sender dates are labeled as such. Pending
decoding and incomplete MIME inventories do not appear as complete messages.

HTML opens in an isolated frame with sender scripts, forms, links, styles, and
remote resources disabled before display. Blocked or ambiguous images show
visible placeholders, and only verified PNG and JPEG MIME parts in the body's
nearest related group can display inline. Press **Escape** inside the frame to
return to its entry button. Long messages scroll, and **Show quoted text**
expands quoted content without changing the original or the full-body PDF
source.

The viewer accepts HTML bodies up to 16 MiB. It also enforces display limits of
100,000 markup candidates before parsing, 100,000 HTML nodes, 128 nesting
levels, and 32 MiB of serialized output. Literal `<` characters count toward the
conservative markup limit as well.

Inline images are limited to 128 occurrences, 5 MiB and 8 million pixels per
image, 8,192 pixels per side, and 16 MiB and 32 million pixels in aggregate.
PNG/JPEG limits and unsupported image formats produce visible placeholders.
Exceeding a body limit reports an unavailable operation instead of silently
truncating the body.

Choose **Inspect attachment documents** to open the attachment navigation
described below; inline display and attachment document publication stay
separate. **Download** downloads that EML version unchanged. The existing PDF
action downloads retained PDFs or uses the configured renderer, including the
complete chosen body and quoted text. New rendering requires the pinned local
Chromium/font configuration, and an unavailable renderer reports its reason.

![Archived email with decoded headers and its HTML body](https://docbank.ai/assets/generated/web-email-reader.png)

![Raw headers from the same retained message](https://docbank.ai/assets/generated/web-email-headers.png)

## Follow email attachments

Choose **Attachments** in the content inspector to see the documents attached to
the selected version under **Attachments**, and the messages it is attached to
under **Attached to**. A document with neither shows **No published
attachments**. Repeated filenames and identical bytes stay as separate rows.
Choose **Show more** to request another page, up to 1,000 rows per group. An
unavailable read reports an error, and an empty list in that case does not mean
the message has no attachments. Rows without a published document show their
outcome and cannot be opened.

Choose any row to open that related version in its own context. Later content
replacement does not substitute the live head or its media type for that related
version. Preview, text, and download keep the usual byte verification and
current-access checks, and missing or denied targets report an error without
removing the row.

To follow nested messages, choose **Back to previous document**, or choose
**Return to selected document** to restore focus to the original result. These
steps do not rerun the query, change its snapshot membership, or check
attachment rows. Ordinary live selections offer the same return path to the
selected document.

![Exact attachment documents linked to the selected email](https://docbank.ai/assets/generated/web-email-attachments.png)

## Export a verified ZIP

In Photos, choose **Export all N photos** in the **⋯** menu for the current
scope, or **Export selection** for checked photos. You can prepare JPEG or PNG
copies with size, image metadata, and GPS choices. Each preparation allows up to
16 photos, each at most 100 million pixels, and the verified ZIP keeps original
filenames, vault paths, and source hashes. See
[Export photos](export-bundles.md#export-photos).

For documents, choose **Export selection** in the selection dock to export your
checked rows, or **Export → Download files…** in the toolbar to export the whole
frozen query when one is active (or the documents on the current page
otherwise). Folders are not export members. Exports support up to 100,000
documents and 50 GiB of role payloads.

Choose **Download ZIP**. By default, the ZIP holds the original files. Open
**Options** to also include retained text, page images, or email body PDFs, to
share exact duplicate outputs, or to split the ZIP into volumes. You can set
each role to **Required**, **If available**, or **Skip**. A missing required
role stops the export before anything is built, while an **If available** role
records its unavailable status in the bundle.

When you choose **Download ZIP**, Docbank copies and checks every frozen page,
stores the selected versions and available role files, builds the archive, and
hands it to your browser. Later edits to the live query, tags, or current
content do not substitute new members or bytes. If some outputs are unavailable,
the drawer stops on the frozen plan and lists them; choose **Download anyway**
to continue. The ZIP filename applies only to the browser download, and bundle
paths stay deterministic. Originals are not redacted or sanitized by annotation
overlays.

The drawer offers the download only after it receives that job's verified
archive receipt. It displays the final size and SHA-256 so you can check the
downloaded file independently, and it keeps a **Download ZIP** button in case
your browser blocked the first download. Your browser saves the archive directly
without loading the entire ZIP into browser memory. Check your browser's
download list to confirm local completion. **New export**
[releases the finished job](export-bundles.md#free-a-finished-job-slot) for both
photos and documents, and asks you to wait while a download is still running.

Closing the drawer stops its progress reader, not the server job. To reconnect
to that job, reopen **Export** in the same browser session, or choose
**Cancel export** to stop it. A disconnected stream is not a completed export.
If authority expires, choose **Download ZIP** again. Reloading or ending the
browser session does not keep the drawer's job handle.

![A verified bundle ready to download from the reviewed frozen selection](https://docbank.ai/assets/generated/web-export-ready.png)

## Review and redact a PDF

To redact a live PDF, open it and choose **Redact PDF** in the reader, or use
the same action in the Documents inspector. Mark rectangles or whole pages in
the same reader, inspect the verified output and sanitized text, and download
one ZIP. Closing the review returns you to the reader's page and zoom, and
unsaved masks ask for confirmation before Docbank discards them. The original
stays unchanged. Reviews are temporary and require the configured Linux amd64
page runtime. See the
[browser PDF review workflow](document-processing.md#review-a-pdf-in-the-browser)
for coverage gaps, private review files, expiry, and download limits.

## Stamp selected pages with Bates labels

Bates labels are sequential identifiers stamped on PDF pages, often used when
exchanging records for review. To stamp a sealed package, open **Export →
Bates-numbered PDFs…** and select the package:

1. Create or select a label namespace, including its prefix and number width.
2. Choose **Preview Bates labels** and inspect the proposed labels and pages.
3. Reserve the displayed range, then choose **Start Bates export**.
4. Wait for **Stamped PDF ready**, then choose **Download PDF**.

The output contains the package's selected pages, and the stored page map keeps
their original source-page numbers. A reserved number is never issued again,
even if the export was abandoned.

![A completed Bates export with its reserved labels and verified PDF](https://docbank.ai/assets/generated/web-bates-export.png)

## Find a retained PDF by number

To look up a retained PDF, open **Productions** in the Documents sidebar, enter
the exact **Bates number**, and choose **Find**. Matching is case-sensitive and
preserves spaces and leading zeroes. A warning flags leading or trailing spaces
without removing them. Because Bates numbers are unique across the vault, an
exact number identifies at most one retained PDF and matching page. A match
opens automatically and shows the page, publication time, PDF size, delivery
kind, Bates range, SHA-256, and allocation reference. Choose **Download PDF** to
recover it, or **Clear** to return to the full production list.

Recovery works after a restart without the review files or a configured page
renderer. Downloads check file integrity against the retained record and do not
repeat the mask and approved-text checks. See the
[numbered delivery verification scopes](document-processing.md#deliver-one-reviewed-pdf-with-bates-numbers)
for a full check with your review files. The browser returns the PDF, not the
render ZIP, text sidecar, or review inputs.

## Assign and remove tags

1. Select a file or folder.
2. Choose **Add tag**, or **Manage** if it already has tags.
3. Type a tag name, then select a match or **Create “name”** to create and assign it. To remove an assigned tag, choose its **×** button.

The dialog opens with your cursor in the tag field and lists assigned tags
separately from available ones. It loads the first 1,000 definitions in name
order and groups related names together. The picker uses the same color for a
tag wherever it appears. You can move through options with the arrow keys,
select with **Enter**, and close the dialog with **Escape**.

Every change binds to the node ID and revision shown in the dialog. When a
change succeeds, the browser updates the node revision and the tag's vault-wide
assignment count, and refreshes both the tag catalog and permanent-audit status.
If another person, agent, or CLI command changed the node first, the dialog
keeps the failed decision visible and asks you to refresh instead of applying
it to newer state.

### Tag selected documents

To tag multiple files on the current page, check their boxes and choose
**Tag selected…** in the selection dock. Pick a tag, choose **Add** or
**Remove**, and press **Apply** to run one atomic, revision-checked operation
on at most 1,000 documents. You can also type a new name and choose
**Create “name”** to create the tag and apply it to every selected document.
Page selection never includes folders or undisplayed query results.

If the result is uncertain, choose **Check tag results** to inspect each
selected document. Review current membership, then decide whether to retry
readable documents that still need the change. A retry uses their current
revisions. After a confirmed retry, check again to include documents that could
not be read earlier. When you close the dialog after an uncertain change,
Docbank refreshes the view. If the initial change fails because a document is stale,
close the dialog, refresh, and select the documents again. After confirmation,
the browser reloads current observations; the retained receipt may describe an
earlier successful operation. See
[selected-set tagging](organizing.md#tag-a-selected-set-atomically) for
retention and backup behavior.

![The tag dialog shows how many selected documents already have the chosen tag](https://docbank.ai/assets/generated/web-batch-tags.png)

### Tag a frozen query

In a frozen snapshot, the results header shows the result count and
**Tag all…**, while the selection dock shows **Tag selected…** for checked rows
on the visible page. Both open the same dialog: choose or create a tag, pick
**Add** or **Remove**, and check the summary line below it (such as
`Adds “Reviewed” to 128 documents.`) before you apply. Tag changes cannot be
undone automatically, and **Remove** affects the whole selection, including
documents that already had the tag before an addition.

**Tag selected…** applies one atomic, revision-checked change to at most 1,000
documents and reports how many changed. If the result is uncertain, choose
**Check tag results** to inspect each selected document, review current
membership, and decide whether to retry readable documents that still need the
change. A retry uses their current revisions. After a confirmed retry, check
again to include documents that could not be read earlier. Closing the dialog
after an uncertain change refreshes the view. If the initial change is refused because a
document is stale, close the dialog, refresh, and select the documents again.

Choosing **Tag all…** → **Continue** captures every result, up to 250,000
documents, and prepares revision-checked batches of at most 1,000. The captured
population does not expand when an untagged query starts receiving the tag.
Completed changes appear as overlays while the frozen rows and facet membership
stay unchanged. New actions use confirmed tag revisions only when the receipts
connect back to the frozen document revision; other document changes still make
a document stale. Once prepared, an action keeps its original requests and
expected revisions.

**Apply** is your explicit confirmation and runs the batches in the same dialog
with progress and a **Pause** control. Choose **Review documents** to inspect
the vault and action identities along with node IDs, content versions, hashes,
sizes, and expected revisions in pages of 50. Choose **Cancel** before anything
is sent to drop the prepared action.

The browser journals the action in IndexedDB for that origin. If a run stops
early, for example after **Finish later**, a lost response, or a closed tab,
the results header shows that it did not finish and offers **Resume** and
**Discard…**. Another tab can resume it, though only the original operation
identities, requests, and expected revisions are retried. If a response is lost
after the daemon commits, **Retry** recovers the retained receipt without
applying the change twice. Starting a new **Tag all…** replaces a finished run.
While an earlier run is unfinished, **Continue** closes the dialog and shows
that run's **Resume** and **Discard…** notice instead of capturing the results
again.

A daemon restart creates a fresh browser origin and session, so choose
**Tag all…** → **Resume from file…** and select the saved recovery file to
continue. Importing validates and stages the action without running it. The
vault must match, and the new session still requires **Apply**. A stale
document revision blocks the action; Docbank does not silently refresh its
target or partially apply that batch.

If you do not have a recovery file, run **Tag all…** again. Tags that already
landed stay unchanged.

Recovery files contain instructions supplied by their creator. Checksums detect
inconsistent files, though they do not authenticate the creator. Review the
documents before applying an imported action. Receipts inside a file do not
update the snapshot's observed revisions; only receipts returned by the daemon
during the current session do that.

Recovery files hold private document identities, content-version identities,
hashes, sizes, and revisions. They leave out paths, names, query text, and
browser credentials, and still need the same protection as vault metadata.

Discarding removes the browser journal and does not roll back changes that
already committed. If a tag was deleted, the dialog still lets you save the
recovery file or discard the run. **Discard…** in the results header also
clears an unreadable journal for the current vault, so corrupt browser data
cannot block later actions.

## Manage tag definitions

To create, rename, or delete the vault's shared tag definitions, open the tag
filter and choose **Manage tags**. Creating a tag allocates a new UUID.
Renaming keeps that UUID while advancing the definition revision and every
assigned node's metadata authority. A stale rename stays visible instead of
overwriting a definition changed by another person or agent.

Deleting an unused tag takes one click. If the tag is assigned to files,
Docbank asks first and tells you how many files lose it. Deletion removes the
definition and all of its assignments, never a document or its stored content.
Reusing the same name later creates a different identity. After a rename or
deletion, the browser reloads the active folder, search, or tag view so node
revisions and assignment counts stay current.

Each tag gets a color derived from its ID, and renaming keeps that color.
Slash-separated names create display groups automatically: `matter/acme/reviewed`
appears as `reviewed` under `matter/acme`, while the full name stays available
in the tooltip and to assistive technology. Groups do not create parent tags or
change assignment rules.

The catalog shows the first 1,000 name-sorted definitions and reports the
complete count. For exhaustive definition management and assignments outside
the displayed selection, use `docbank tag`, the paginated HTTP API, or an
embedded client.

## Export dated search counts

Open **Searches → Run report** to create a CSV of search counts for selected
sources and date ranges. Review any unresolved dates before downloading the CSV
or evidence ZIP. **Recent reports** keeps the latest 100 requests and outcomes,
and **Use as draft** runs a request against current data without its old date
choices. See [Search exports](search-exports.md) for the full workflow, count
definitions, and expiry limits.

![Search counts for one selected version with CSV and frozen evidence downloads](https://docbank.ai/assets/generated/selected-report-result.png)

## Saved queries and highlights

Open the **Searches** page at `/searches` to run, rename, or delete a saved
search. You can also open **Run report** and **Recent reports** there. Create
new searches from **Advanced → Save…**. Choose **Rename** to edit the name and
description fields, or **Edit as JSON** to view the complete definition, then
choose **Save changes** to update it or **Save as new** to keep a copy. The
list paginates in groups of 100. Choose **New highlight list** to create
reusable highlights.

**Keep query draft** keeps the complete query in the tab and URL fragment. The
fragment contains query text and filters, so treat copied URLs as private,
though it does not contain browser-session credentials. Saving a definition
does not replace the kept draft or change the URL. Choosing **Cancel** in
advanced search, locking the tab, or having a session expire or get rejected
clears the draft and its URL fragment. Drafts do not carry over when you open a
new session with `docbank web`. Save a named definition before reloading or
leaving the session if you need it later.

Saved searches store the search definition, and **Run** creates a new result
set using current documents. Keeping or saving the draft alone does not run it
or change the current live results. This browser run does not produce the
durable saved-run receipt that the authenticated HTTP API exposes. Unknown
fields are rejected instead of silently dropped.

Highlight sets hold 1 to 64 unique literal terms, each up to 256 Unicode
characters, with lowercase `#rrggbb` colors. They cannot run as queries. Choose
a highlight set in a document's **Text** tab to apply its ordered colors
without changing the saved definition. Neither result rows nor document bodies
are stored in browser preferences.

Edits and deletions use the definition's inspected revision. A stale response
stays visible without automatically retrying against newer state; reload the
definitions and reopen the item before deciding again. Deletion requires a
separate confirmation naming the definition, and it never deletes documents or
discards the kept query draft.

![A named query retaining its complete expression, filters, and sort](https://docbank.ai/assets/generated/web-saved-queries.png)

![A reusable highlight set with literal terms and colors](https://docbank.ai/assets/generated/web-highlight-sets.png)

## Move a node to recoverable trash

To trash a live file or folder, choose **Move to trash**: use the trash button
for a file, or the folder's actions menu. The item leaves the list right away,
and a notice names it along with the item count for a folder. A folder and its
live descendants move to trash together. Choose **Undo** in the notice to
restore it.

The daemon applies the change only if that node revision is still current. If
another person, agent, or CLI command changed the node first, the notice shows
the error and the item stays listed. A successful receipt removes the selection
and refreshes the current folder, search, or tag view in place.

This action is recoverable. It does not empty trash, garbage collect content,
reclaim packed space, or erase permanent audited history.

## Restore from recoverable trash

Open the **Trash** page at `/trash` in the Documents sidebar to see the newest
1,000 independently restorable roots. Each entry shows its name, kind, node ID,
revision, trash time, and logical size for files. A folder entry represents the
complete subtree that left the live tree in that trash operation.

Choose **Restore** to return the item right away. Docbank restores the same
node and its content to its original live parent when that directory still
exists, or falls back to the vault root if the parent is unavailable. If the
chosen name is already occupied, Docbank adds its normal collision suffix. A
notice shows the actual canonical folder, names a missing original folder when
this browser trashed the item, and counts the files of a photo group. Choose
**Show** to open the folder.

If the revision changed, the notice shows the error and the item stays on the
page. After a successful restore, the browser removes the item from the trash
page and reloads the current view.

The browser cannot empty trash. Permanent tree-metadata deletion, subsequent
garbage collection, and packed-space reclamation stay CLI or
master-authenticated API operations.

## Upload verified documents

Upload is available only in a live folder, not in search or tag results. The
drawer shows the destination path once in its heading.

1. Browse to the destination folder.
2. Choose **Add → Upload files** or **Add → Upload folder**.
3. Select files or a folder from this device, then choose **Upload** in the drawer.
4. Check each file's result in the upload drawer.

Docbank makes two bounded-memory passes over each selected file. The first pass
computes the browser's declared SHA-256 while showing hashing progress, and the
second streams the bytes with visible progress over a dedicated upload channel.
Before the upload button becomes available, the daemon proves that channel with
a random secret issued through the ownership-pinned CLI handoff. File bytes
never use an ordinary reconnectable browser HTTP request.

The channel is bound to one browser session and one daemon lifetime. If it
breaks, Docbank retries the connection and verifies a fresh proof before
enabling uploads. Choose **Reconnect** if automatic retries fail. An upload
whose result is unconfirmed is never resent automatically; when you retry one,
the daemon checks for the stored result before receiving file bytes. The daemon
independently computes the hash and size and grants node/blob authority only
when both match. The browser compares that receipt again before reporting
**Added** or **Already present**, then refreshes the destination by ID.

Each file is an independent queue entry: one rejection does not make another
success ambiguous, and you can retry failed entries. Cancellation can race with
a daemon commit whose receipt did not reach the browser, so the drawer labels
that item **Unconfirmed**, refreshes the destination, and asks you to retry.
Uploads are idempotent, so the retry converges on the stored result.
Name/content collisions keep the ordinary ingest suffix behavior instead of
overwriting a document. Selecting a local file never changes or removes the
source.

The folder picker keeps each file's relative path. The browser creates the
matching subfolders under the destination, reusing any that already exist,
before uploading the files inside them. Server-filesystem ingest, watched-inbox
configuration, and replacing an existing document remain CLI or authenticated
API workflows.

## Import a mailbox

1. Browse to the destination folder and choose **Add → Import mailbox**.
2. Choose an MBOX file or Google Takeout ZIP, then choose **Upload and preview**.
3. Review the detected mailbox entries and choose **Import messages**.
4. Follow the imported and rejected counts until the job finishes.

Each message occurrence becomes its own email document. Repeated messages stay
as separate occurrences, and attachments become child documents. An interrupted
import resumes after its last saved message. See
[mailbox import](../cli-reference.md#docbank-email-mailbox) for CLI status and
receipts.

![A completed Takeout import with three separate message occurrences](https://docbank.ai/assets/generated/web-mailbox-import.png)

## Import load files

1. Browse to the destination folder.
2. Choose **Add → Import load files**.
3. Choose a ZIP that holds the DAT or CSV load file, an optional OPT or LFP
   page map, and the referenced original documents, images, and supplied text.
4. Pick the metadata profile, page-map profile, and source encoding, then
   choose **Upload and preview**.
5. Resolve any blocking diagnostics, name the package and sending party, then
   choose **Import package**.

The ZIP travels over the authenticated upload channel and is stored as a sealed
container before preflight reads it. As the job runs, the drawer shows
imported, gap, and total counts along with the operation ID.

**Keep supported records and report gaps** lets an import keep records when
referenced files become unavailable after preflight, though blocking preflight
diagnostics must still be resolved. Without this option, the worker verifies
every declared file before creating document entries. **Index package-supplied
text** makes the sender's text files searchable.

Browser source metadata omits fields marked sensitive by the retained mapping,
and compact Map-projection rows include latitude and longitude when map tiles
are configured. Member lists and raw timeline inputs require the daemon API
key.

Closing the drawer stops its progress updates. **Lock web session** revokes the
session and cancels unfinished imports it owns. Cancellation keeps committed
records and removes unfinished document entries as described in the
[package command reference](../cli-reference.md#docbank-package).

![A load-file ZIP preview before importing its documents and supplied text](https://docbank.ai/assets/generated/web-load-file-import.png)

## Process a version and read its text

A PDF's text layer appears in the **Text** tab without setup. For other
formats, configure a [processing profile](../configuration.md) to extract and
store text. With a configured embedding service, processing also prepares
documents for search by meaning.

1. Select a document and choose **Extract text** beside its name. This action
   appears when a configured profile includes text extraction. The processing
   dialog checks which profiles accept the file. Profiles that only prepare
   search vectors show **Index for search** instead.
2. Check where the file goes. A local profile says it runs on this computer,
   while a network or hosted profile names the service and its address and says
   whether the file name goes too. **Details** lists the exact plan, service,
   deployment, and fingerprints.
3. The first time you use a profile, check **Allow "profile" to process
   documents in this vault**. That permission covers this operator and profile
   configuration for every document in the vault until it is revoked or
   expires. Later runs need only the button.
4. When the run finishes, choose **Read text** to read the extracted text
   Docbank stored for that version.

To withdraw permission, open **Details** and choose **Revoke all processing
permissions**. That applies to every profile.

![The processing plan discloses local extraction and a configured network embedding service](https://docbank.ai/assets/generated/web-document-processing-plan.png)

![A finished text extraction with its text ready to read](https://docbank.ai/assets/generated/web-document-processing-result.png)

![Verified retained Markdown for the selected source version](https://docbank.ai/assets/generated/web-document-rendition.png)

Opening the plan or reading coverage does not start processing, and a restored
vault needs fresh consent before network processing. The
[processing guide](document-processing.md) explains consent and provider
requirements. To inspect results without the browser, see
[CLI coverage and text windows](../cli-reference.md#docbank-processing).

When a processing profile stores search vectors, each file row shows a
**Find similar** action. Choose it to compare its stored embeddings with the
file versions loaded in the current view; this calls no provider. If embeddings
are missing, build them with a processing run, which has its own review and
consent. See [Find similar documents](document-processing.md#find-similar-documents).

![Related documents found from stored embeddings in the loaded view](https://docbank.ai/assets/generated/web-similar-1440.png)

## Download verified content

The inspector places the preview above details, tags, and **History**. To
resize it, drag its left edge or focus the edge and press the arrow keys;
double-click the edge to reset. Your browser remembers the width. Click a path,
version ID, SHA-256, or ID to copy its full value. **Download** saves the
selected version. **Extract text** appears beside it when a configured profile
includes text extraction; otherwise, configured profiles offer **Index for
search**. The processing dialog checks which profiles accept the file. Files
have a direct **Move to trash** button. Folders have an **Open** button, and
unprotected folders have **Move to trash** in **⋯**.

Protected items show a **Protected** chip that opens **Audit history**. For an
unprotected folder, choose **Protect permanently…** in **⋯** to copy its
command-line preview command, which previews retained history and vault-wide
metadata before you explicitly enable irreversible protection.

To read a PDF or Markdown file in a large viewer, double-click it, press
**Enter** on its row, or choose **Open** in the inspector. Press **Escape** or
choose **Close** to return to the selected row. This also works in search
results and preserves the version selected by a frozen snapshot.

PDFs open in EmbedPDF with search, zoom, page navigation, thumbnails, text
selection, and printing. **Download** saves the selected original, and
**Redact PDF** switches live PDFs to review in the same workspace while frozen
search results stay read-only. Its rendering engine and fonts ship with
Docbank; viewing a PDF needs no optional server runtime or external asset
download. PDF previews are limited to 64 MiB. Markdown displays headings,
lists, tables, and code blocks; Docbank sanitizes embedded HTML and removes
form controls before display, and links open in a separate tab. Other eligible
UTF-8 text appears as plain text, with text previews limited to 16 MiB.

The inspector also previews bounded PNG, JPEG, and WebP images, and animated
WebP images may animate in the browser. Docbank verifies the selected version
UUID, size, media type, and SHA-256 before passing content to a viewer.
Unsupported or oversized files stay available through **Download**. Reclicking
a selected item keeps its preview and metadata, and browsing keeps the preview
area a consistent height while the next file loads.

Selecting a PDF in the inspector reads its retained page images without loading
the whole PDF or starting a browser PDF engine. Open the reader to load the
verified original. PNGs with retained page geometry also use the retained-page
preview. Choose **Render page** when an image is missing; this needs the
[optional local page runtime](../architecture/page-images.md). Other PNGs use
the verified original-image preview, including when that runtime is
unavailable.

Choose **Text** to read the verified text rendition for the selected version
and processing profile. The browser binds the source, profile, generation,
attachment, build, artifact, length, and digest before decoding the text, and a
check icon shows verification. **Find** searches within the displayed text and
shows its matches in place of query and saved highlights; match counts and
arrows appear when there is a query and matches, respectively. The **Text** tab
stays available for plain-text files even when **Preview** shows the same text.
Processing failures, verified-empty text, and unavailable historical renditions
stay distinct. Eligible original UTF-8 text is a fallback for that same version
when no readable rendition is available. Without a processing profile,
**Set up** opens the processing guide; with a profile, **Process…** opens the
processing plan.

Docbank highlights query text operands from the resolved query instead of
splitting the expression in the browser, excluding negated and structured
operands. The browser refuses query highlights if a saved definition revision
no longer matches the accepted snapshot. **Find** and saved highlight sets
share bounded, non-overlapping marks and previous/next match controls. All
content stays inert.

Previous and next document controls move through the accepted frozen snapshot,
including across its existing page cursors. They stop at the snapshot boundary
and do not replace an expired snapshot with live results. **Duplicates** lists
at most 16 live documents with the selected content hash. Opening one creates a
separately labeled live context, and **Return to selected document** restores
the frozen selection.

Choose **Download** on a file to retrieve its selected version. A live row
selects its current head, while a frozen query row keeps the version captured
by that snapshot even when the live head has since changed.

Docbank first copies the object from loose or packed storage into owner-private
daemon staging while the browser shows verified byte progress. The selected
node revision, version UUID, SHA-256 identity, and size must all still agree before
that work starts. The current node revision authorizes access to a retained
historical version without changing which bytes were selected. Because of that,
a concurrent replacement, move, or trash operation asks you to refresh instead
of silently downloading a different document.

After the complete content passes verification, Docbank issues a one-use
download ticket, and the browser starts its save only after that check
succeeds. Cancellation or a verification failure removes a pending private
staging file and publishes nothing to the browser. Changing the selected source
or locking the session also cancels pending previews and revokes published
image URLs. A ticket identifies only its prepared file, expires after two
minutes, and cannot call any other Docbank route. It is not the vault API key
or the browser session.

Preparation streams on the daemon and does not buffer the object in browser
memory, though it temporarily needs local free space equal to the document's
logical size. Docbank reports that verified bytes were handed to the browser,
not that the browser or operating system completed its final save. Use
`docbank document get` when automation needs a durable receipt after private
staging, file sync, atomic publication, and parent-directory sync.

## Inspect immutable versions

To inspect retained versions without losing your current folder, search
results, or selected document, choose **All versions** in a file's **History**
section. The newest version appears first. Each entry says whether the content
was created, replaced, or restored from a prior version, along with its
recorded time and size, and includes a **Download** button. **Details** shows
the version ID, SHA-256, media type, and the version it was restored from.

You can open a link with `#node=<node ID>&version=<version UUID>` to inspect
that exact version, even if it is older than the newest 1,000 listed in the
drawer. For drawers opened from these links, selecting a version updates the
address, and browser Back and Forward restore the selected version. Missing,
purged, or mismatched links show an unavailable message.

Selecting an entry shows its complete record: full version UUID, SHA-256
content identity, exact byte count, media type, canonical timestamp,
introducing operation UUID, and the source version for a revert. The current
head is marked. A revert creates a new immutable version and does not erase or
relabel the earlier one.

Trashed files carry a **Trash** label. Restore the file before downloading a
version.

Choose **Download** in the complete-version panel to retrieve that version
through the same private staging, progress, terminal verification, and one-use
handoff as the current-content action. The version UUID, owning node, SHA-256
identity, size, and current node revision must all agree before preparation
begins. Because historical versions do not keep a filename timeline, the
browser uses the live document's current name. Version comparison stays a CLI
or authenticated API workflow.

The drawer reads at most the newest 1,000 versions and tells you when older
history exists. Use `docbank document versions list`, its pagination flags, or
the authenticated HTTP API for exhaustive automation. This view does not
compare bytes, revert, or prune history.

## Understand where a document came from

The **History** section shows the five most recent versions and imports
together, newest first. Choose **All imports** to see the document's complete
import history. Each entry shows how the document arrived, its copyable source
path or filename, and the import date. Corrected entries show **Superseded**.

Open **Import details** for the import ID, record hash, document ID, source
reference, original modification time when supplied, and any replaced record.
Click any value to copy it. Because the drawer uses the document state returned
with the history, a concurrent move displays its current path and a trashed
document is labeled as trashed.

An original reference records the source at import time and does not guarantee
that the source still exists. It may be a portable relative path from a watched
inbox, a local path recorded by an ordinary ingest, or an opaque reference
supplied by an embedded application. The browser does not open, validate,
change, or retain that external source.

The drawer reads at most the newest 1,000 facts and tells you when older
provenance exists. Use `docbank document provenance`, its pagination flags, or
the authenticated HTTP API for exhaustive automation. Provenance correction and
external-content pinning are not browser workflows.

## Browse tags and search text

To enable text search by meaning, open **System → Search by meaning**, review
the download size, and choose **Turn on** to approve setup. Then run
`docbank daemon restart` when prompted and reopen the web app. Indexing starts
after the restart and covers existing documents and new imports. The
search-results hint opens this tab when search by meaning is off. A successful
built-in Turn on records the choice for other tools on this computer that share
the built-in server. If another tool recorded it, Docbank installs any missing
shared files and starts indexing at its next launch. If setup fails, choose
**Turn on** to try again. Configured providers keep their settings.

To search, enter a word or phrase in the search box and press **Enter**.
Results can match a live document name or verified extracted text. The results
heading shows the count, each row shows the name with its folder beneath, and
the **Match** column identifies which one matched. Use **Previous** and
**Next** in the inspector to step through the results. Name matches keep their
API relevance ranking and appear before content-only matches until you choose a
column sort.

Use the tag selector in the browser toolbar without a text query to browse up
to 1,000 live items carrying that tag. Docbank reads the result in one metadata
snapshot, so its node state and complete virtual paths cannot mix concurrent
moves, trash operations, or content updates across pages. Trashed assignments
are omitted from this live view and disclosed in its count. Use
`docbank tag nodes` for the exhaustive paginated list of live and trashed
assignments.

With text in the search box, the same selector requires that tag in addition to
the name or content match. The selector displays tag names, and both workflows
bind to the tag's UUID, so a later rename does not change which definition was
selected. Changing the selector reruns the current browse or search.

The search box searches words in names and document text. When a processing
profile has a query embedding binding, it also searches by meaning
automatically. The menu selects **Plain words** or **Query syntax**, matching
Photos. Query syntax accepts expressions such as `name:invoice AND tag:urgent`
and runs through the complete query endpoint.

**Rerank results** is opt-in when a processing profile supports it. Base rows
appear before reranking settles. Degraded reranking keeps those rows and shows
its cause, and other processing failures fall back to names and text with a
visible explanation.

![The Docbank web application showing extracted-text search results in a synthetic vault.](https://docbank.ai/assets/generated/web-search-results.png)

*Search results show each document's folder under its name and offer the same
authority card as ordinary folder browsing.*

Clear the search box to return to the selected tag's assignment view, or to the
current directory when **All tags** is selected. The selector loads at most the
first 1,000 name-sorted tag definitions and tells you when more exist. Use
`docbank tag list` for the tag catalog, and use `docbank search` for directory,
media-type, or modification-time filters, structured JSON, or another result
limit. See [Searching](searching.md) for limits and truncation.

## Read permanent audited history

When a selected node belongs to a permanent audit scope, the details panel
shows a green **Protected** badge beside its name. Ordinary deletion, version
pruning, garbage collection, and repacking cannot erase that node's history.

Click **Protected** to open the audit timeline without losing your current
folder, search results, or selection. The timeline is newest first and explains
the primary change for each event: live and retained-trash paths,
content-version transitions, tag definitions and assignments, or provenance.
Select an event to inspect its complete event, operation, scope, and node
identities; canonical timestamp and origin; before/after revision, path, and
version state; and typed tag, provenance, or photo payload. Photo states show
authored values and confirmed field names before and after edits and undo,
including confirmation of an empty caption.

The first page contains at most 50 events. **Load older events** follows the
API's append-stable cursor, so new activity cannot shift or duplicate the
history you are already inspecting. Protection status stays authoritative even
when a page contains no events; the web application does not infer protection
from an empty or non-empty timeline.

## Verify permanent audit evidence

Choose **System → Protected folders** to check protection status. If nothing is
protected, the section shows the command to protect a folder and skips
verification. Otherwise, including when status cannot be read, Docbank runs the
vault-wide permanent audit verifier. It replays canonical audit history against
the current node, version, membership, topology, tag, and provenance
projections, then reads and recomputes SHA-256 for every unique blob retained
by protected history.

A successful result reports the protected and verified file totals and the
protected size. **Details** holds the vault and allocation-lineage identities,
the operation high-water mark, the allocation entry count and head, and each
scope's terminal entry count and chain head. Copy buttons preserve the complete
identities even when they wrap. Metadata, missing-content, corruption, and
unreadable-content problems stay visible with the affected hash.

Verification results give you a fresh proof of current authority. The drawer
does not accept a previous evidence bundle, enroll a scope, or change protected
state. When an external copy must act as a rollback trust anchor, record
`docbank audit verify --json` outside the vault and later run
`docbank audit verify --expected`. Closing the drawer cancels its active
request. Maintenance contention or interruption stays visible, and you can
retry.

## Inspect background work

Choose **System → Running now** to check work in progress, paused workers, and
failures. Active work appears first, with counts when known and pause, resume,
worker count, and cancellation controls beside it. When workers are waiting for
work, a quiet **Nothing running** line sits above the worker list. The list
below shows every worker and its status, and offers pause controls for idle
workers. Queued operations stay in that list until they start.

A worker waiting for work shows **Idle**, and the header counts only work in
progress. Watched inboxes show their
[configured tags](../configuration.md#watched-inboxes), and a missing tag
appears beside the configured tags separately from any worker error.

You can pause or resume photo imports, visual previews, and the storage lanes
`place`, `evacuate`, `repair`, and `salvage`. Pause takes effect at the next
safe item boundary, and visual preview concurrency accepts 1 through 4. A
paused lane keeps its **Resume** control even after its last operation
disappears. The drawer reads lane settings with the job list, uses `PUT` to
change them, and refreshes every two seconds while open. If another client
changes controls first, review the refreshed settings and try again. Controls
survive daemon restarts. The list includes the newest 1,000 storage operations,
and finished storage operations clear within an hour after their retention ends
unless they still own pending cleanup. Worker records disappear when the daemon
restarts. Browser errors omit backend paths; use the CLI to inspect details.

## Inspect physical storage

Choose **System → Disk use** to see how your current vault uses managed blob
storage. Tiles show the total on disk and the number of stored files. When
packs hold dead payload, **Reclaimable** shows how much and what share of the
pack payload it is. **Locations** has one row per storage location with its
kind (filesystem or S3), stored size, object count, how many objects have no
other copy, and its observed online, unavailable, fenced, or unbound state.
When an unhealthy location is the only authority for an object, the drawer says
how many objects currently have no readable alternative. It never exposes
binding paths, endpoints, buckets, credential profiles, or ownership epochs.

**Storage** separates four related quantities:

- **Loose files.** The physical inventory of individual raw or zstd files. It
  can include untracked files or redundant loose copies of packed objects, so
  it is not an authority count.
- **Packed.** Authoritative content stored in immutable pack files. The view
  shows both its logical raw size and its stored size.
- **Pack files.** The complete stored payload of every pack, including live and
  logically dead entries.
- **Waiting to compact.** Logically dead payload that still occupies those pack
  files.

Waiting-to-compact bytes have not been reclaimed. This distinction matters when
a GC report removes unreachable catalog mappings while the vault's disk usage
has not fallen by the same amount. The percentage beside **Reclaimable** shows
how much of the current pack payload is dead, not a compression ratio or a
promise that every pack is immediately eligible for compaction.

This drawer is read-only and refreshes from the daemon's current catalog. It
cannot pack, garbage-collect, or repack content. Use `docbank storage status`
for structured or scripted inspection, see [Multi-store storage](storage.md)
for repair and placement, and run `docbank storage repack` when you intend to
rewrite eligible sparse packs and retire their old files.

## Inspect backup snapshots

Choose **System → Backups** to inspect the recovery points in the repository
selected by `[backup] repo` in `config.toml`. The drawer shows the repository
path and repository ID so you can tell which repository you are inspecting.
Snapshots appear newest first with their tag, creation time, full or
incremental relationship, file count, logical content bytes, and the bytes
newly added by that capture. Use the copy button on each row to copy its
snapshot ID.

An initialized repository with no snapshots is different from an unconfigured
repository, and the browser reports those states separately along with the
command that fixes each one. The browser does not accept an arbitrary server
path, initialize a repository, create a snapshot, verify content, restore a
vault, or delete retention history.

Seeing a manifest in this list does not prove that its referenced bytes are
still readable. Run `docbank backup verify` to check repository integrity, and
periodically restore into a separate vault to rehearse the complete recovery
path.

## Browse import history

Choose **Imports** in the sidebar to open the page at `/imports`. Each row
shows its label or source folder name, import time, document count, and size.
Select an import to see its source paths (each with a copy button), browse its
current live members, and open a document by its node identity. Counts show
current membership rather than a historical import total.

Labels belong to the import group, not its documents. You can rename or clear a
label under its inspected revision. If another client changes it first, the
page keeps your draft and reports the conflict; reload the label before
deciding whether to save again. Label changes are unavailable once permanent
audit authority has been enabled.

Collection and member lists show at most 100 entries each, and empty groups,
failed reads, and truncated lists are reported separately. Use the paginated
HTTP API to browse beyond that limit. This is direct member browsing, not a
collection-filtered text search.

Choose **Quality** to inspect document distributions, duplicate content counts,
zero-byte files, media/extension mismatches, and text coverage. If you have
multiple processing profiles, choose one before reading coverage. Without a
configured profile, coverage is unavailable; that does not mean zero failures
or complete processing, and a configured policy does not mean an extraction
adapter is running.

Coverage distinguishes complete, partial, failed, unprocessed, and activated
output with no text. Retained searchable output takes precedence over a failed
retry, and the existing worker rejects blank provider output as a failed
attempt. Quality reads are bounded to 250,000 members, a 64 MiB census, and
five seconds; exceeding a limit returns an error instead of a partial summary.

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
subsequent browser requests. Each tab keeps its scoped credentials in memory,
and you sign in again after a reload or daemon restart. If a PDF or Markdown
reader fails to load, **Reload Docbank** asks before discarding unsaved work.
Recovery keeps a command-line session signed in, while API-key sign-in requires
signing in again. If a later request finds the session expired or revoked,
Docbank clears that tab's selection and open dialogs and returns it to sign-in.

![The Open your Docbank page asking for an API key](https://docbank.ai/assets/generated/web-signin.png)

1. Set a fixed API port and a strong API key in `$DOCBANK_HOME/config.toml`.
   With `public_origin`, the key must be at least 32 characters; generate one
   with `openssl rand -hex 32`. Protect this file as a credential and configure
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
   that preserves the public `Host` and forwards WebSocket upgrades to the API
   port. Docbank takes authority from configuration, never `Forwarded` or
   `X-Forwarded-*` headers. Every request, including API-key requests, health
   checks, and static pages, must use the public authority, a concrete loopback
   backend authority, or an exact entry in `[web] allowed_hosts`. Docbank also
   admits these authorities through the server Host allowlist. Entries are
   `host[:port]` values; schemes and wildcards are rejected.
3. Open the public origin and enter the API key. Use HTTPS for remote access.
   An HTTP origin is accepted on loopback, while non-loopback HTTP requires
   `trust_private_network = true`, which explicitly trusts the private network
   and its proxy with the key and browser credentials.

Without `public_origin`, configured-key login is available at the daemon's
fresh loopback web origin, and key login adds no Host restriction beyond the
server's [`allowed_hosts`](../configuration.md). The usual `docbank web`
fragment handoff also works with ephemeral API keys and keeps its existing
upload proof and permissions.

### Serve Docbank under a path

When other applications share the public origin, set `base_path`:

```toml
[web]
public_origin = "https://library.example.test"
base_path = "/archive"
```

Restart the daemon, then open `https://library.example.test/archive/`. Keep
`public_origin` free of paths. Your proxy must preserve `/archive` in upstream
requests, preserve the public `Host`, and forward WebSocket upgrades. For
example, inside an existing Caddy HTTPS site:

```caddyfile
@docbank path /archive /archive/*
handle @docbank {
    reverse_proxy 127.0.0.1:7777
}
```

Use `handle`, which preserves the path, instead of `handle_path`, which strips
the prefix. The daemon redirects `/archive` to `/archive/`; nested pages,
browser API requests, uploads, downloads, and assets stay beneath that prefix.
Existing CLI API endpoints stay available at the backend root, and `docbank web`
includes the path in its loopback launch URL. The proxy needs to expose only
the configured prefix.

Login cookies are scoped to the configured path and have distinct names for
each origin and path, which lets separate Docbank instances share an origin.
Photo-import resume cookies use the mounted checkpoint endpoint, and pending
Bates exports in the same browser tab stay separate for each mount. As with any
applications on the same origin, their scripts share browser origin authority;
a path is not a browser security boundary.

### Browser sessions

Key login sets a host-only, HttpOnly, SameSite=Strict instance cookie, and
HTTPS also sets Secure. Each tab gets its own token and upload proof. The
cookie alone cannot read the vault: requests need both the tab token and the
cookie, and mutations also need the exact public Origin. Cross-site requests
are rejected, and credential requests reject redirects.

`session_lifetime` accepts 1 minute through 90 days; omitting it or setting
`"0"` uses 24 hours. Expiry is absolute. Failed sign-ins are not locked out;
the key length requirement keeps guessing impractical. A daemon allows up to
512 active key-login sessions. Sessions live only in memory and are excluded
from vault backups.

Choose **Sign out** to revoke the current tab. Login and logout do not rotate
or delete the shared cookie or invalidate other tabs. An operator can list and
revoke individual key-login sessions without seeing their credentials:

```bash
docbank web sessions
docbank web sessions --json
docbank web sessions revoke SESSION_ID
```

Session IDs are non-secret digests. These CLI commands use the daemon's
ownership-proven master connection, and browser credentials cannot manage other
sessions. Expiry, revocation, and daemon shutdown cancel the session's uploads,
exports, and prepared reads through the existing browser resource lifecycle.

## Browser authentication

When Docbank opens your browser, it writes a small launch page beside the
owner-private daemon runtime record and passes only that credential-free local
file path to the operating system. Before doing so, the ownership-pinned CLI
asks the daemon to exchange its master API authority for a random,
daemon-lifetime browser session. The master key stays on that pinned connection
and never enters browser storage, a URL, or a child-process argument.

The daemon serves that session from a second listener with a cryptographically
random `docbank-<random>.docbank.localhost` hostname and a newly selected
loopback port. This browser origin is independent of the configured API port
and unique to one daemon lifetime. Because of that, a process that later
captures either port cannot leave a service worker or cached script waiting for
a future browser session.

Photo import recovery uses a persistent HttpOnly, SameSite=Strict cookie on
`docbank.localhost` to locate the saved selection in the same vault after a
restart. The cookie grants no API access; each checkpoint request still needs
the current browser session along with a matching Host and Origin. Other
browser data stays with its per-run origin.

The launch page carries the scoped session and its random upload-proof secret
in a URL fragment, which browsers do not include in the initial HTTP request.
The application removes them from the address bar and holds them only in page
memory. Ordinary requests send `X-Docbank-Web-Session`. The daemon accepts that
credential for these operations:

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
| Search, rename, or merge people; read and draw photo faces; name or clear a face | Rename and merge require the person revisions and apply only to people the browser can list. See [People in Photos](people.md#people-in-photos). Naming a face can create its person. Standalone person creation requires the API key. Face edits require the face revision. Drawing creates a box. The browser can mark an existing person or create a person by name in a whole photo and clear whole-photo marks. Person links open a filtered Library or, from unlocked Hidden, a filtered Hidden view. Hidden face reads and edits require an unlock. |
| Set or clear a collection label | Requires the inspected collection-label revision. |
| Create and page a frozen query snapshot | Uses complete validated query intent. Handles remain bound to this session and daemon lifetime. |
| Apply a tag to an exact frozen population | Requires captured node revisions and confirmation. Retries preserve the original operation identities. |

Use the bookmark button to [manage saved queries and highlights](#saved-queries-and-highlights)
or **Advanced** to [validate and run a complete query](#edit-a-complete-query).

Uploads use a separate WebSocket. The daemon proves its identity to the browser
before file bytes are sent, including after a connection is replaced. Uploads
can create only file nodes beneath the live directory selected in the browser.
Folder uploads also create the subfolders they need through
`POST /api/v1/nodes`, which makes directories only. The daemon independently
checks the caller-declared hash and size, as it does for other remote writers.

These permissions do not allow emptying trash, document mutations other than
the revision-checked tag workflows above, audit enrollment, backup creation,
restore, maintenance, configuration changes, or access to general API
endpoints. A browser session never receives the master API key. See the
[HTTP API](../architecture/http-api.md) for the route and credential contracts.

Choosing the lock button revokes the session in daemon memory and clears the
page. Every remaining fragment browser session and its dedicated browser origin
disappear when that daemon stops. Run `docbank web` again to create a fresh
origin and session against the ownership-proven daemon.

Closing the browser tab does not stop the daemon or revoke other sessions. If
you want the current tab to lose access right away, use the lock button. To
stop the daemon, run `docbank daemon stop`; all browser sessions end when the
daemon stops.

The launch file stays beneath `$DOCBANK_HOME/web-launch/` with the same
owner-only Unix permissions or Windows DACL as the runtime record. It is
runtime state, excluded from snapshots, replaced by the next launch, and
removed when the daemon stops.

Use `docbank web --no-browser` only when another local program must open the
URL. That output contains live browser credentials. Do not put it in shell
history, logs, screenshots, issue trackers, or chat.

## Current limits

Folder, tag, search, and recoverable-trash views show at most 1,000 rows and
tell you when more exist. Use CLI or HTTP pagination to list complete folders,
tags, and trash. Search has no continuation cursor; narrow incomplete results
as described in [Searching](searching.md#how-do-i-handle-incomplete-results).
It uses the same name and verified-text matching rules as `docbank search`.
Results start in the API's relevance ranking, and choosing **Document**,
**Size**, or **Modified** switches to that column order. **Document** compares
the complete paths shown in search results rather than only their basenames.

Refreshing a folder resolves its node ID, current canonical path, and children
in one metadata snapshot, so a concurrent CLI or agent move cannot leave the
browser constructing child paths beneath an obsolete name.

Choose **Add → Upload folder** to upload a folder and its subfolders while
preserving their structure.

The current web application does not compare versions, edit, revert, prune,
move live nodes between folders, empty trash, enroll audit scopes, or run
maintenance, backup creation, backup verification, general metadata/content
verification, or restore operations. Frozen-query tag actions are capped at
250,000 documents. Use an authenticated API client for larger or different bulk
workflows.

Original-file previews support UTF-8 or ASCII text up to 16 MiB and PNG, JPEG,
and WebP images up to 32 MiB, and animated WebP images may animate in the
browser. PDF and PNG page rendering accepts sources up to 64 MiB within the
[page runtime limits](../architecture/page-images.md). Empty or larger sources
offer verified download without a page preview. Other document, image, audio,
video, and archive formats use verified download.

If a page reports that its browser session expired or was rejected, run
`docbank web` again, or sign in again if you use a key. Neither credential nor
the upload channel survives daemon restart, and the previous random
`.localhost` origin is not reused.
