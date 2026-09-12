---
last_edited: 2026-09-12
title: Web application
description: Upload, browse, search, and organize the local vault in a responsive, authenticated web interface.
---

# Web application

Use the local web application to upload, browse, search, and tag documents.
You can also inspect versions, source records, permanent history, storage, and
backup snapshots. Start it with:

```bash
docbank web
```

Docbank starts or reconnects to the vault's daemon and opens the browser. The
daemon is the background process that owns the vault. The browser uses its
authenticated HTTP API; it does not open the database or stored files directly.
The daemon accepts connections only from the local machine.

Choose a task:

- [Browse the vault](#browse-the-vault), inspect document details, or
  [select documents on this page](#select-documents-on-this-page).
- [Upload files](#upload-verified-documents) or [download content](#download-verified-content).
- [Manage tags](#manage-tag-definitions) and [search text](#browse-tags-and-search-text).
- [Run complete queries](#work-with-a-frozen-query) with exact paging and facets.
- [Export a verified ZIP](#export-a-verified-zip) from selected documents or a frozen query.
- [Move documents to trash](#move-a-node-to-recoverable-trash) or [restore them](#restore-from-recoverable-trash).
- [Inspect versions](#inspect-immutable-versions) and [source records](#understand-where-a-document-came-from).
- [Read permanent history](#read-permanent-audited-history) or [verify its evidence](#verify-permanent-audit-evidence).
- Inspect [background jobs](#inspect-background-work), [storage](#inspect-physical-storage), or [backup snapshots](#inspect-backup-snapshots).

The browser has limited permissions. See [Browser authentication](#browser-authentication)
for their exact scope and [Current limits](#current-limits) for workflows that
require the CLI or another API client.

![The Docbank web application showing a synthetic vault tree and the selected document's authority.](https://docbank.ai/assets/generated/web-vault-browser.png)

*Select a document to see its stable identity and content hash beside the table.*

## Browse the vault

- Click a row once to inspect it. The authority card updates without opening
  or downloading the file.
- Double-click a folder, select it and press Enter, or use **Open folder** in
  the authority card to navigate into it.
- Use the back arrow to restore the previous folder or search view, including
  its inspected row and sort order.
- Click **Document**, **Size**, or **Modified** to sort. Click the active
  heading again to reverse its direction. Directories remain grouped ahead of
  files in folder views.
- Use refresh to reload the current stable directory ID. If another client
  renamed or moved that directory, the browser adopts its current canonical
  path.

The authority card is the details panel for the selected file or folder. A
node ID identifies that entry even after a move or rename. Its revision is a
change counter that Docbank uses to reject changes based on an outdated view.

The browser selects the first row after loading a folder. A selected directory
shows its path, revision, and modification time. A selected file additionally
shows its exact logical size, media type, immutable current-version UUID, and
SHA-256 content identity. The copy buttons copy the complete UUID or digest
even when the card wraps it across lines. Every selected node also shows its
assigned tag names. Hovering a tag shows its stable UUID and vault-wide
assignment count; the bounded tag stack expands in place when a node carries
more than six.

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

## Edit a complete query

Choose **Edit query** to write a complete expression, select simple or advanced
syntax, and edit its mode, sort, and structured facets. Facets stay separate
from field operands in the expression. Their summaries show the constraints
you entered; validation does not move operands into hidden filters.

The daemon validates each edited draft after a short pause. It resolves saved,
tag, and collection references and reports their observed revisions. Errors
remain visible; **Focus query error** selects the reported part of the
expression. A newer edit cancels and supersedes an older validation request.

**Save query draft** opens the saved-definition editor with the entire draft.
**Open query** beside a saved query opens it here. From an import collection,
**New query for this collection** starts a new draft scoped to that collection's
stable identity. Closing the query editor keeps the draft in the tab and URL;
**Discard query draft** removes it.

Validation does not execute a search. Once the draft is valid, **Run query**
creates a frozen result through the complete query endpoint. Ordinary
name/content search remains separate and does not inherit draft constraints.
See [field-aware query syntax](searching.md#preview-a-field-aware-query).

## Work with a frozen query

A completed run becomes the accepted frozen snapshot for the tab. Draft edits
remain separate until you choose **Run query** again. Choose 50, 100, or 250
documents per page before the run. The workspace shows exact document and byte
totals, previous and next page controls, and the node, content version, hash,
size, revision, and tags observed when the snapshot was created. Later vault
changes do not replace those rows.

Select a frozen row to inspect those original facts and the exact selected
version. The same card loads current path, revision, tags, provenance, and audit
status separately as live observations. A later rename, retag, or content
replacement therefore does not rewrite the snapshot facts or substitute the
new head for preview and download.

Use **Refine snapshot** to inspect collection, tag, media-family, extension,
modified-time, size, text-coverage, and duplicate facets. Choosing a supported
facet or changing the query creates a new snapshot; it never splices live
results into the accepted one. An unavailable facet says why instead of
presenting a zero count.

Snapshot handles last for one daemon lifetime, up to 15 minutes idle and 30
minutes total. Locking the browser session or stopping the daemon revokes them.
If paging reports that the snapshot is gone, run the complete query again and
use only the new snapshot and its cursors.

## Follow email attachments

Choose **Attachments** in the content inspector to see published attachment
occurrences and incoming parent messages for the exact selected version.
Repeated filenames or identical bytes remain separate occurrences, identified
by their MIME part and publication order. **Load more** requests another page;
the view stops at 1,000 occurrences per direction rather than expanding an
entire message family automatically.

Each occurrence separates its MIME decoding outcome from current processing
status. **Check inventory** reads that publication's receipt and reports
complete or partial inventory independently of indexing. An empty relation
list, an unavailable read, or an unchecked receipt does not prove that a
message has no attachments. Occurrences without a published child stay visible.
The tab does not publish documents, start processing, or grant provider consent.

**Open attachment** and **Open parent** enter a separate exact-version context.
Later content replacement does not substitute the live head or its media type
for that related version. Preview, text, and download retain the usual byte
verification and current-access checks. Missing or denied targets report an
error without removing the occurrence.

Follow nested messages with **Back to previous document**, or choose **Return
to frozen document** to restore focus to the original result. These steps do
not rerun the query, change its snapshot membership, or check attachment rows.
Ordinary live selections offer the same return path to the selected document.

## Export a verified ZIP

Choose **Export selection** in the selection dock for the checked documents.
The toolbar's **Export** opens the whole frozen query when one is active, or
the documents on the current page otherwise. Folders are not export members.
Exports support up to 100,000 exact documents and 50 GiB of role payloads.

Choose whether original files, retained text, and page images are required,
optional, or excluded. A missing required role stops planning. An optional
role records its unavailable status in the bundle. **Preview export** copies
and checks every frozen page, then stores the exact versions and available
role files. Later edits to the live query, tags, or current content do not
substitute new members or bytes.

Review the document count, member hash, role availability, plan fingerprint,
and expiry before choosing **Start reviewed export**. Role bytes estimate
payload only; the final ZIP also contains metadata and archive overhead.
Changing the source, role choices, or download filename requires another
preview. The filename applies only to the browser download; bundle paths stay
deterministic. Originals are not redacted or sanitized by annotation overlays.

The drawer offers **Download verified ZIP** only after it receives the exact
job's verified archive receipt. It displays the final size and SHA-256 so you
can check the downloaded file independently. The browser saves the archive
directly; Docbank does not load the entire ZIP into browser memory. Check your
browser's download list for local completion.

Closing the drawer stops its progress reader, not the server job. Reopen
**Export** in the same browser session to reconnect to that job, or choose
**Cancel export** to cancel it explicitly. A disconnected stream is not a
completed export. Expired authority requires a fresh preview; reloading or
ending the browser session does not preserve the drawer's job handle.

## Assign and remove tags

1. Select a file or folder.
2. Choose **Manage** beside its tags.
3. Select an existing tag and choose **Add tag**, or remove an assigned tag.

The dialog lists assigned tags separately from available tags. It loads the
first 1,000 definitions in name order, then displays related names in groups.
The picker uses the same color for a tag wherever it appears. Use the arrow
keys to navigate its options, Enter to select, and Escape to close it.

Every change is bound to the stable node ID and revision shown in the dialog.
After success, the browser updates the node revision and the tag's vault-wide
assignment count. It also refreshes the tag catalog and permanent-audit
status. If another person, agent, or CLI command changed the node first, the
dialog keeps the failed decision visible and asks you to refresh rather than
applying it to newer state.

### Tag selected documents

Use the document checkboxes to select files on the displayed page, then choose
**Add tags** in the selection dock. The picker shows whether all, some, or none
of the selected documents have the chosen tag. **Add to all** and **Remove from
all** apply one atomic, revision-fenced operation to at most 1,000 documents.
Folders and undisplayed query results are not included in page selection.

Keep the dialog open if the result is uncertain: **Retry same operation**
reuses its original identity and revisions. A stale selection requires an
explicit refresh rather than an automatic retry against newer documents.
After confirmation, the browser reloads current observations; the retained
receipt may describe an earlier successful operation. See
[selected-set tagging](organizing.md#tag-a-selected-set-atomically) for retention
and backup behavior.

### Tag a frozen query

In a frozen snapshot, checkboxes select only documents on the visible page.
**Tag whole query** instead captures every exact member across all pages, up to
250,000 documents, and prepares revision-fenced batches of at most 1,000. The
captured population does not expand when an untagged query starts receiving
the tag: completed changes appear as overlays while the frozen rows and facet
membership remain unchanged.

Before any prepared action can mutate the vault, save its recovery checkpoint,
select that saved file back, and confirm the displayed vault, action, tag,
operation, and exact target count. The browser also journals the action in
IndexedDB for that origin. Another tab on the same origin can resume it, but
only the original operation identities, requests, and expected revisions are
retried. If a response is lost after the daemon commits, **Retry same
operation** recovers the retained receipt without applying the change twice.

A daemon restart creates a fresh browser origin and session, so import the
saved checkpoint to continue. Import validates and stages the action; it does
not run it. The vault must match, and the new session still requires checkpoint
readback and explicit confirmation. A stale document revision fences the
action without silently refreshing its target or partially applying that
batch.

Recovery files contain private stable document identities, content-version
identities, hashes, sizes, and revisions. They omit paths, names, query text,
and browser credentials, but still need the same protection as vault metadata.
Abandoning removes the browser journal and does not roll back changes that
already committed.

## Manage tag definitions

Choose the tag-catalog button beside the toolbar selector to create, rename,
or delete the vault's shared tag definitions. Creating allocates a new stable
UUID. Renaming keeps that identity while advancing the definition revision and
every assigned node's metadata authority. A stale rename remains visible
instead of overwriting a definition changed by another person or agent.

Deleting requires a separate confirmation that names the exact stable ID,
revision, and current assignment count. It removes the definition and all of
its assignments, but never deletes a document or its stored content. Reusing
the same name later creates a different stable identity. After a rename or
deletion, the browser reloads the active folder, search, or tag view so node
revisions and assignment counts remain authoritative.

Tags use a color derived from their stable ID. A rename keeps that color.
Slash-separated names create display groups automatically: `matter/acme/reviewed`
appears as `reviewed` under `matter/acme`. The full name remains available in
the tooltip and to assistive technology. Groups do not create parent tags or
change assignment rules.

The catalog shows the first 1,000 name-sorted definitions and discloses the
complete count. Use `docbank tag`, the paginated HTTP API, or an embedded client
for exhaustive definition management and assignments outside the displayed selection.

## Saved queries and highlights

Open the bookmark button in the top bar to manage saved queries and highlight
sets. A new query starts with the current search text, selected tag and display
sort. The complete query editor preserves the expression, filters, mode and sort
together. Choose **Save as new** to name a definition, or **Edit** and **Save
changes** to update one. The catalog is paginated in groups of 100.

**Keep query draft** retains the complete query in the tab and URL fragment.
The fragment contains query text and filters, so treat copied URLs as private.
It does not contain browser-session credentials. Reloading restores the draft,
but does not renew the browser session; authentication still requires
`docbank web`. **Discard query draft** removes it from the URL.

Saved queries are definitions, not frozen result sets. Opening a definition in
the complete query editor and choosing **Run query** creates a new workspace
snapshot from that draft; keeping or saving the draft alone does not run it or
change the current live results. This browser run is not the durable saved-run
receipt exposed by the authenticated HTTP API. Unknown fields are rejected,
not silently dropped.

Highlight sets hold 1–64 unique literal terms, each up to 256 Unicode characters,
with lowercase `#rrggbb` colors. They cannot run as queries. Choose one in a
document's **Text** tab to apply its ordered colors without changing the saved
definition. Neither result rows nor document bodies are stored in browser
preferences.

Edits and deletions use the definition's inspected revision. A stale response
remains visible without automatically retrying against newer state. Reload the
definitions and reopen the item before deciding again. Deletion requires a
separate confirmation naming the definition, ID and revision; it never deletes
documents. An already loaded draft remains until explicitly discarded.

## Move a node to recoverable trash

Choose **Move to trash** on the selected live file or folder. Docbank opens a
confirmation that names the complete virtual path, stable node ID, and revision
being acted upon. A folder and its live descendants move to trash together.

The daemon applies the mutation only if that exact node revision is still
current. If another person, agent, or CLI command changed the node first, the
browser keeps the confirmation open and asks you to refresh instead of
silently acting on stale authority. A successful receipt removes the selection
and refreshes the current folder, search, or tag view.

This action is deliberately recoverable. It does not empty trash, garbage
collect content, reclaim packed space, or erase permanent audited history.

## Restore from recoverable trash

Choose the trash button in the top bar to inspect the newest 1,000 independently
restorable roots. Each entry shows its name, kind, stable node ID, revision,
trash time, and logical size for files. A folder entry represents the complete
subtree that left the live tree in that trash operation.

Choose **Restore** and confirm the inspected revision. Docbank returns the same
stable node and retained content to its original live parent when that
directory still exists. If the parent is unavailable, restore falls back to
the vault root; if the chosen name is already occupied, Docbank adds its normal
collision suffix. The completed receipt shows the actual canonical path rather
than predicting where the item should have landed.

If the revision changed, the confirmation stays open so you can refresh and
review the change. After a successful restore, the browser removes the item
from the trash drawer and reloads the tree from its root. This discards cached
paths that may no longer match the restored tree.

The browser cannot empty trash. Permanent tree-metadata deletion, subsequent
garbage collection, and packed-space reclamation remain explicit CLI or
master-authenticated API operations.

## Upload verified documents

1. Browse to the destination folder.
2. Choose the upload button.
3. Select one or more files from this device.
4. Check each file's result in the upload drawer.

Upload is available only in a live folder, not in search or tag results. The
drawer names the destination's stable directory ID and current path.

Docbank makes two bounded-memory passes over each selected file. The first pass
computes the browser's declared SHA-256 while showing hashing progress. The
second streams the bytes with visible progress over a dedicated upload channel.
Before the upload button becomes available, the daemon proves that channel
with a random secret issued through the ownership-pinned CLI handoff. File
bytes never use an ordinary reconnectable browser HTTP request.

The channel is bound to one browser session and one daemon lifetime. If it
breaks, the page permanently disables upload rather than reconnecting to
whatever process now owns the loopback port; run `docbank web` again to obtain
a newly proved channel. The daemon independently computes the hash and size
and grants node/blob authority only when both match. The browser compares that
receipt again before reporting **Added** or **Already present**, then refreshes
the destination by stable ID.

Files are independent queue entries: one rejection does not make another
success ambiguous, and failed entries can be retried. Cancellation can race
with a daemon commit whose receipt did not reach the browser, so the drawer
labels that item **Unconfirmed**, refreshes the destination, and directs the
operator to retry; the idempotent upload contract then converges on the stored
result. Name/content collisions retain the ordinary ingest suffix behavior
rather than overwriting a document. Selecting a local file never changes or
removes the source.

Browser upload accepts individual files. Folder recursion, server-filesystem
ingest, watched-inbox configuration, and replacing an existing document remain
CLI or authenticated API workflows.

## Download verified content

The selected document card previews PDF and PNG pages, eligible UTF-8 text,
and bounded static JPEG images. Text is rendered as inert text, never as
document HTML. Docbank
verifies the selected version UUID, size, media type, and SHA-256 after receiving
the complete body and before publishing text or an image URL. Unsupported files
and text larger than 16 MiB remain available through **Download verified
original** without being decoded in the page.

For PDF and PNG files, **Preview** reads retained page geometry and images.
Choose **Render page** when an image is missing. This requests only that page;
opening the inspector or moving between pages never starts bulk rendering.
Rendering needs the [optional local page runtime](../architecture/page-images.md).
PDF pages render at 144 DPI; PNGs retain their native physical density.
Unsupported geometry or PNG density, unavailable runtime, partial images,
render failure, and a changed source are shown separately. Retained images
remain readable when the runtime is unavailable.

**Previous page**, **Next page**, and the page selector navigate within the
document. **Fit width** and numeric zoom change only its display size. Each
page uses its own rotated crop dimensions. The browser checks the actual frame,
recipe, image digest, byte length, and decoded dimensions before displaying it.
Changing the source, page, session, or tab clears the old image and releases its
resources. **Cancel render** is available for the active request created here;
a matching request created elsewhere offers a refresh without taking ownership.

Choose **Text** to read the verified text rendition for the exact selected
version and processing profile. The browser binds the source, profile,
generation, attachment, build, artifact, length, and digest before decoding the
text. It reports failed, unprocessed, unconfigured, verified-empty, and
unavailable historical results separately. An eligible original UTF-8 text file is used only
as an exact-version fallback when no readable rendition is available.

Query text operands are highlighted from the resolved query, not by splitting
the expression in the browser. Negated and structured operands are excluded.
The browser refuses query highlights if a saved definition revision no longer
matches the accepted snapshot. **Find** and saved highlight sets share bounded,
non-overlapping marks and previous/next match controls. All content stays inert.

Previous and next document controls move through the accepted frozen snapshot,
including across its existing page cursors. They stop at the snapshot boundary
and do not replace an expired snapshot with live results. **Duplicates** lists
at most 16 live documents with the selected content hash. Opening one creates a
separately labeled live context; **Return** restores the exact frozen selection.

Choose **Download verified original** on a file to retrieve its selected
version. A live row selects its current head; a frozen query row keeps the
version captured by that snapshot even when the live head has since changed.
Docbank first
copies the object from loose or packed storage into owner-private daemon
staging while the browser shows verified byte progress. The selected node
revision, version UUID, SHA-256 identity, and exact size must still agree before
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

Choose **Version history** on any file to inspect every retained immutable
version without losing the current folder, search results, or selected
document. The newest version appears first. Each entry identifies whether the
content was created, replaced, or restored from a prior version, along with its
recorded time, node revision, logical size, and stable version UUID.

Selecting an entry exposes its complete authority: full version UUID, SHA-256
content identity, exact byte count, media type, canonical timestamp,
introducing operation UUID, and the source version for a revert. The current
head is marked explicitly; a revert remains a new immutable version rather
than erasing or relabeling the earlier one.

Choose **Download** in the complete-version panel to retrieve that exact
retained version through the same private staging, progress, terminal
verification, and one-use handoff as the current-content action. The stable
version UUID, owning node, SHA-256 identity, size, and current node revision
must all agree before preparation begins. Historical versions do not retain a
filename timeline, so the browser uses the live document's current name.
Version comparison remains a CLI or authenticated API workflow.

The drawer reads at most the newest 1,000 versions and says when older history
exists. Use `docbank versions list`, its pagination flags, or the authenticated
HTTP API for exhaustive automation. This view does not compare bytes, revert,
or prune history.

## Understand where a document came from

Choose **Provenance** on any file to inspect the origin facts Docbank retained
when it ingested that document. The newest ingest appears first. Each entry
shows its source kind and description, original reference, ingest time, and
whether it is the active origin or has been superseded by a corrected fact.

Selecting an entry exposes its complete stable provenance identity, ingest
UUID, node ID, canonical ingest timestamp, original modification time when one
was supplied, and supersession link. The drawer adopts the node state returned
with the provenance page, so a concurrent move displays the current canonical
path and a trashed document is labeled explicitly rather than retaining an
obsolete path from the table.

An original reference records the source at import time. It does not guarantee
that the source still exists. It may be a portable relative path from a watched inbox, a local path recorded by an
ordinary ingest, or an opaque reference supplied by an embedded application.
The browser does not open, validate, change, or retain that external source.

The drawer reads at most the newest 1,000 facts and says when older provenance
exists. Use `docbank provenance`, its pagination flags, or the authenticated
HTTP API for exhaustive automation. Provenance correction and external-content
pinning are not browser workflows.

## Browse tags and search text

Enter a word or phrase in the search box and press Enter. Results can match a
live document name or verified extracted text. The **Match** column identifies
which one. Name matches retain their API relevance ranking and appear before
content-only matches until you choose an explicit column sort.

Use the tag selector in the browser toolbar without a text query to browse
up to 1,000 live items carrying one exact assignment. The bounded result is
read in one metadata snapshot, so its node state and complete virtual paths
cannot mix concurrent moves, trash operations, or content updates across
pages. Trashed assignments are omitted from this live view and disclosed in
its count; `docbank tag nodes` remains the exhaustive paginated
live-and-trashed workflow.

With text in the search box, the same selector requires that tag in addition
to the name or content match. Tag names are displayed for people, while both
workflows are bound to the tag's stable UUID so a later rename does not
silently change which definition was selected. Changing the selector reruns
the current browse or search.

![The Docbank web application showing extracted-text search results in a synthetic vault.](https://docbank.ai/assets/generated/web-search-results.png)

*Search results display complete virtual paths and keep the same authority
inspection available from ordinary folder browsing.*

Clear the search box to return to the selected tag's assignment view, or to
the current directory when **All tags** is selected. The selector loads at most
the first 1,000 name-sorted tag definitions and discloses when more exist. Use
`docbank tag list` for the tag catalog. Use `docbank search` for directory,
media-type, or modification-time filters, structured JSON, or another result
limit. See [Searching](searching.md) for limits and truncation.

## Read permanent audited history

The authority card checks the selected node's stable audit membership. A green
**Protected** badge means ordinary deletion, version pruning, garbage
collection, and repacking cannot erase that node's retained history.
**Not audited** means audit authority exists in the vault but the selected node
is outside every permanent scope. **Dormant** means no scope has been enabled.

Choose **Audit history** on a protected node to open a wide timeline without
losing the current folder, search results, or selection. The timeline is newest
first and explains the primary change for each event: live and retained-trash
paths, content-version transitions, tag definitions and assignments, or
provenance. Select an event to inspect its complete immutable event, operation,
scope, and node identities; canonical timestamp and origin; before/after
revision, path, and version state; and typed tag or provenance payload.

The first page contains at most 50 events. **Load older events** follows the
API's append-stable cursor, so new activity cannot shift or duplicate the
history already being inspected. Protection status remains authoritative even
when a page contains no events. The web application does not infer protection
from an empty or non-empty timeline.

## Verify permanent audit evidence

Choose the shield-check button in the top bar to run the vault-wide permanent
audit verifier. This is more than reading stored status: Docbank independently
replays canonical audit history against the current node, version, membership,
topology, tag, and provenance projections, then reads and recomputes SHA-256 for
every unique blob retained by protected history.

A successful result reports the protected and verified blob totals, unique raw
bytes, vault and allocation-lineage identities, operation high-water mark,
allocation entry count and head, and each scope's terminal entry count and chain
head. Copy buttons preserve the complete identities even when they wrap. A
dormant vault is reported separately from a failed verification; metadata,
missing-content, corruption, and unreadable-content problems remain visible
with the affected hash.

This drawer runs only a fresh proof of current authority. It does not accept a
previous evidence bundle, enroll a scope, or change protected state. Record
`docbank audit verify --json` outside the vault and later use
`docbank audit verify --expected` when that external copy must act as a rollback
trust anchor. Closing the drawer cancels its active request; maintenance
contention or interruption remains visible and can be retried deliberately.

## Inspect background work

Choose the activity button in the top bar to inspect the jobs owned by the
current daemon. Each entry identifies the stable job name, whether it is
running, completed, failed, or cancelled, and its start and finish time.
Terminal failures include the daemon's bounded error text so an operator can
distinguish an idle system from a watcher, extractor, or automatic packer that
stopped.

Use refresh to request a new snapshot. The drawer does not start, stop, retry,
or reconfigure work; use the relevant configuration, CLI, or authenticated API
workflow after understanding the failure. Job records belong to one daemon
lifetime and disappear when that daemon restarts.

## Inspect physical storage

Choose the storage button in the top bar to see how the current vault occupies
managed blob storage. The summary separates four related quantities:

- **Loose content** is the physical inventory of individual raw or zstd
  files. It can include untracked files or redundant loose copies of packed
  objects, so it is not an authority count.
- **Live packed content** is authoritative content stored in immutable pack
  files. The view shows both its logical raw size and its stored size.
- **Pack files** is the complete stored payload of every pack, including live
  and logically dead entries.
- **Pending repack** is logically dead payload that still occupies those
  immutable pack files.

Below the aggregate cards, **Content stores** lists each fixed primary or
configured secondary with its filesystem/S3 kind, observed online,
unavailable, fenced, or unbound state, catalog-authorized objects, logical and
stored bytes, pack count, sole copies, and affected live documents. When an
unhealthy store is the only authority for an object, the drawer says how many
objects currently have no readable alternative. It never exposes binding
paths, endpoints, buckets, credential profiles, or ownership epochs.

Pending-repack bytes have not been reclaimed. This distinction matters when a
GC report has removed unreachable catalog mappings but the vault's disk usage
has not fallen by the same amount. The percentage beside the pending total
shows how much of the current pack payload is dead, not a compression ratio or
a promise that every pack is immediately eligible for compaction.

This drawer is read-only and refreshes from the daemon's current catalog
authority. It cannot pack, garbage-collect, or repack content. Use
`docbank storage status` for structured or scripted inspection, see
[Multi-store Storage](storage.md) for repair and placement, and run
`docbank storage repack` explicitly when you intend to rewrite eligible sparse
packs and retire their old files.

## Inspect backup snapshots

Choose the backup button in the top bar to inspect the immutable recovery
points in the repository selected by `[backup] repo` in `config.toml`. The
repository path and stable repository ID make the authority being inspected
explicit. Snapshots appear newest first with their tag, immutable ID, creation
time, full or incremental relationship, logical node/file/blob counts, logical
content bytes, and the pack bytes newly added by that capture.

An initialized repository with no snapshots is different from an unconfigured
repository, and the browser reports those states separately. The browser does
not accept an arbitrary server path, initialize a repository, create a
snapshot, verify content, restore a vault, or delete retention history.

Seeing a manifest in this list is not proof that its referenced bytes are
still readable. Run `docbank backup verify` to independently prove repository
integrity, and periodically restore into a separate vault to rehearse the
complete recovery path.

## Browse import collections

Choose **Import collections** in the top bar to browse the vault's import groups.
Each card shows its label or source description, ingest time, current live
file count, and logical bytes. Select a collection to browse its current
live members and inspect a document by its stable node identity. Counts are
current membership, not a historical import total. The browser refresh time
is separate from the ingest time.

Labels belong to the import group, not its documents. Rename or clear a label
under its inspected revision; if another client changes it first, the drawer
keeps your draft and reports the conflict. Reload the label before deciding
whether to save again. Label changes are unavailable once permanent audit
authority has been enabled.

Collection and member lists show at most 100 entries each. Empty groups,
failed reads, and truncated lists are reported separately; use the paginated
HTTP API to browse beyond that limit. This is direct member browsing, not
a collection-filtered text search.

Choose **Inspect collection quality** for document distributions, duplicate
content counts, zero-byte files, media/extension mismatches, and text coverage.
With multiple processing profiles, choose one before reading coverage. Without
a configured profile, coverage is unavailable, not zero failures or complete
processing. A configured policy does not mean an extraction adapter is running.

Coverage distinguishes complete, partial, failed, unprocessed, and activated
output with no text. Retained searchable output takes precedence over a failed
retry. The existing worker rejects blank provider output as a failed attempt.
Quality reads are bounded to 250,000 members, a 64 MiB census, and five seconds;
an exceeded limit returns an error, never a partial successful summary.

Select distribution values and choose **New query** to open a collection-scoped
draft. Multiple selected values use OR. This does not change live results or
execute a search. Concentrations describe common values, not document defects.

## Browser authentication

When Docbank opens the browser, it writes a small launch page beside the
owner-private daemon runtime record and passes only that credential-free local
file path to the operating system. Before doing so, the ownership-pinned CLI
asks the daemon to exchange its master API authority for a random,
daemon-lifetime browser session. The master key stays on that pinned connection
and never enters browser storage, a URL, or a child-process argument.

The daemon serves that session from a second listener with a cryptographically
random `.localhost` hostname and a newly selected loopback port. This browser
origin is independent of the configured API port and unique to one daemon
lifetime. A process that later captures either port therefore cannot leave a
service worker or cached script waiting for a future browser session.

The launch page carries the scoped session and its random upload-proof secret
in a URL fragment. Browsers do not include fragments in the initial HTTP
request; the application removes them from the address bar and holds them only
in page memory. Ordinary requests send `X-Docbank-Web-Session`. The daemon
accepts that credential for the following operations:

| Permission | Limit |
|------------|-------|
| Read the tree, nodes, search results, tags, versions, and provenance | Uses the same result limits and document IDs as the ordinary API. |
| Read audit status and history | Does not enroll or change a permanent scope. |
| Verify permanent audit history | Does not run general metadata/content verification or backup verification. |
| Read background jobs and physical storage status | Cannot start or change maintenance. |
| List backup snapshots | Uses only the repository already configured for this daemon. |
| List trash | Returns a bounded list of restorable roots. |
| Prepare, preview, or cancel an exact-version download | Writes only a private temporary file, enforces preview MIME and size limits, and issues one expiring ticket for that file. |
| Plan, preview, run, cancel, and download exports | Owns exact sources, frozen plans, jobs, and verified tickets within this browser session. Download naming cannot select a server destination. |
| Read verified text and exact-content duplicate context | Revalidates the selected source and rendition identities; duplicate context is bounded to 16 live-current references. |
| Read page geometry and images, request or cancel page rendering | Binds the exact source revision, version, page, frame, and renderer recipe; the inspector requests one page at a time. |
| Move to trash or restore | Requires the selected stable node ID and its current revision. |
| Add or remove a tag assignment | Requires the selected stable node ID and its current revision. |
| Create, rename, or delete a tag definition | Rename and delete require the inspected tag revision; deletion reports the removed assignment count. |
| Read and manage saved query or highlight definitions | Edit and delete require the saved definition's revision. Permanent audit history blocks these writes. |
| Read collections and their members | Returns bounded lists of live import membership. |
| Set or clear a collection label | Requires the inspected collection-label revision. |
| Create and page a frozen query snapshot | Uses complete validated query intent; handles remain bound to this session and daemon lifetime. |
| Apply a tag to an exact frozen population | Requires captured node revisions, checkpoint readback, and explicit confirmation; retries preserve the original operation identities. |

The browser manages saved definitions, validates complete query drafts, and
runs accepted drafts as frozen snapshots through its scoped session. See
[Saved queries and highlight sets](searching.md#save-complete-query-intent-over-http).

Upload uses a separate WebSocket: a connection that never reconnects during the
session. The browser must prove it holds the upload secret before sending any
file bytes. Upload can create only file nodes beneath the stable live
directory selected in the browser. The daemon independently checks the
caller-declared hash and size, as it does for other remote writers.

These permissions do not allow emptying trash, document mutations other than
the revision-fenced tag workflows above, audit enrollment, backup creation,
restore, maintenance, configuration changes, or access to general API
endpoints. A browser session never receives the master API key. See the
[HTTP API](../architecture/http-api.md) for the route and credential contracts.

The lock button revokes the session in daemon memory and clears the page.
Every remaining browser session and its dedicated browser origin disappear
when that daemon stops. Run `docbank web` again to create a fresh origin and
session against the ownership-proven daemon.

Closing the browser tab does not stop the daemon or revoke other sessions.
Use the lock button when the current tab should lose access immediately, and
use `docbank daemon stop` when every session and the daemon itself should end.

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
and trash. Search has no continuation cursor; narrow incomplete results as
described in [Searching](searching.md#how-do-i-handle-incomplete-results).
It uses the same name and verified-text matching rules as `docbank search`.
Results initially preserve the API's relevance ranking. Choosing Document,
Size, or Modified changes to that explicit column order; Document compares the
complete paths shown in search results rather than only their basenames.
Refreshing a folder resolves its stable node ID, current canonical path, and
children in one metadata snapshot, so a concurrent CLI or agent move cannot
leave the browser constructing child paths beneath an obsolete name.

The current web application does not compare versions, recursively import
folders, edit, revert, prune, move live nodes between folders, empty trash,
enroll audit scopes, or run maintenance, backup creation, backup verification,
general metadata/content verification, or restore operations. Frozen-query tag
actions are capped at 250,000 exact documents; use an authenticated API client
for larger or different bulk workflows.
Original-file previews support UTF-8 or ASCII text up to 16 MiB and bounded
static JPEG images up to 32 MiB. PDF and PNG page rendering accepts sources up
to 64 MiB within the [page runtime limits](../architecture/page-images.md).
Other document, image, audio, video, and archive formats use verified download.

If a page reports that its browser session or upload channel expired, ended,
or was rejected, run `docbank web` again. Neither credential nor the upload
channel survives daemon restart, and the previous random `.localhost` origin
is not reused.
