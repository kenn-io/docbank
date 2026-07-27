---
title: Web application
description: Browse and search the local vault in a responsive, authenticated, read-only web interface.
---

# Web application

Run:

```bash
docbank web
```

Docbank starts or reconnects to the selected vault's compatible daemon and
opens its local web application. It is a read-only document browser: navigate
the virtual tree, sort a folder by document name, size, or modification time,
search names and extracted text, and inspect the selected document's stable
node ID, revision, current version ID, SHA-256 identity, exact size, and media
type. The authority card also shows every tag assigned to the selected file or
folder, while search can require one exact tag. Protected documents expose
their newest-first permanent audit timeline and complete event authority. The
storage button separates loose content, live packed content, pack-file payload,
and logically dead packed bytes awaiting repack. The activity button reports
the daemon's supervised extraction, watched-inbox, and automatic-packing jobs.
Every file also exposes its retained immutable content versions and the stable
authority behind each one, plus the immutable provenance Docbank recorded when
the document entered the vault.

The browser is another client of the authenticated HTTP API. It does not open
SQLite or the blob store, and it has no private route that the CLI or an agent
cannot use. The daemon remains loopback-only.

![The Docbank web application showing a synthetic vault tree and the selected document's authority.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/v0.11.0/web-vault-browser.png)

*The root browser keeps the document table primary while the authority card
shows the selected file's stable identity and verified content hash.*

## Browse the vault

- Click a row once to inspect it. The authority card updates without opening
  or downloading the file.
- Double-click a folder, select it and press Enter, or use **Open folder** in
  the authority card to navigate into it.
- Use the back arrow to restore the previous folder or search view, including
  its selection and sort order.
- Click **Document**, **Size**, or **Modified** to sort. Click the active
  heading again to reverse its direction. Directories remain grouped ahead of
  files in folder views.
- Use refresh to reload the current stable directory ID. If another client
  renamed or moved that directory, the browser adopts its current canonical
  path.

The browser selects the first row after loading a folder. A selected directory
shows its path, revision, and modification time. A selected file additionally
shows its exact logical size, media type, immutable current-version UUID, and
SHA-256 content identity. The copy buttons copy the complete UUID or digest
even when the card wraps it across lines. Every selected node also shows its
assigned tag names. Hovering a tag shows its stable UUID and vault-wide
assignment count; the bounded tag stack expands in place when a node carries
more than six.

## Download verified content

Choose **Download** on a file to retrieve its current version. Docbank first
copies the object from loose or packed storage into owner-private daemon
staging while the browser shows verified byte progress. The selected node
revision, version UUID, SHA-256 identity, and exact size must still agree before
that work starts. A concurrent replacement, move, or trash operation therefore
asks you to refresh instead of silently downloading a different document.

Only terminally verified bytes receive a short-lived, one-use browser download
ticket. The native browser save begins after that proof succeeds; cancellation
or a verification failure removes the private staging file and publishes
nothing to the browser. The ticket identifies only that prepared file, expires
after two minutes, and cannot call any other Docbank route. It is not the vault
API key or the browser session.

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

An original reference is evidence, not a live filesystem promise. It may be a
portable relative path from a watched inbox, a local path recorded by an
ordinary ingest, or an opaque reference supplied by an embedded application.
The browser does not open, validate, change, or retain that external source.

The drawer reads at most the newest 1,000 facts and says when older provenance
exists. Use `docbank provenance`, its pagination flags, or the authenticated
HTTP API for exhaustive automation. Provenance correction and external-content
pinning are not browser workflows.

## Search names and extracted text

Enter a word or phrase in the search box and press Enter. Results can match a
live document name or verified extracted text. The **Match** column identifies
which one. Name matches retain their API relevance ranking and appear before
content-only matches until you choose an explicit column sort.

Use the tag selector in the browser toolbar to require one exact assignment
for the search. Tag names are displayed for people, while the request is bound
to the tag's stable UUID so a later rename does not silently change which
definition was selected. Changing the selector reruns an active search. A text
query remains required; choosing a tag does not turn the current folder into a
tag-only listing.

![The Docbank web application showing extracted-text search results in a synthetic vault.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/v0.11.0/web-search-results.png)

*Search results display complete virtual paths and keep the same authority
inspection available from ordinary folder browsing.*

Clear the search box to return to the current directory. The selector loads at
most the first 1,000 name-sorted tag definitions and discloses when more exist.
Use `docbank search` when you need an exhaustive tag catalog, directory,
media-type, or modification-time filters, structured JSON, or another result
limit.

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

The first page is bounded to 50 events. **Load older events** follows the
API's append-stable cursor, so new activity cannot shift or duplicate the
history already being inspected. Protection status remains authoritative even
when a page contains no events. The web application does not infer protection
from an empty or non-empty timeline.

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

Pending-repack bytes have not been reclaimed. This distinction matters when a
GC report has removed unreachable catalog mappings but the vault's disk usage
has not fallen by the same amount. The percentage beside the pending total
shows how much of the current pack payload is dead, not a compression ratio or
a promise that every pack is immediately eligible for compaction.

This drawer is read-only and refreshes from the daemon's current catalog
authority. It cannot pack, garbage-collect, or repack content. Use
`docbank storage status` for structured or scripted inspection and run
`docbank storage repack` explicitly when you intend to rewrite eligible sparse
packs and retire their old files.

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

The launch page carries only the read-only session in a URL fragment. Browsers
do not include fragments in the initial HTTP request; the application removes
it from the address bar and holds it only in page memory. Requests use
`X-Docbank-Web-Session`, which the daemon accepts only for the tree, node,
search, tag-definition and assignment reads, immutable-version, provenance, audit-status, audit-history,
background-job, physical-storage-status, and verified-download preparation
used by this interface.
Download preparation may write only owner-private temporary bytes and issue
one exact, expiring file ticket; it cannot mutate document authority. The
session cannot enroll audit scopes, run independent verification, or call
mutation, backup, maintenance, configuration, or general API endpoints.

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
URL. That output contains the live session key. Do not put it in shell history,
logs, screenshots, issue trackers, or chat.

## Current boundary

Folder and search views are bounded to 1,000 rows and say when more results
exist; use the CLI or paginated HTTP API for exhaustive automation. Search has
the same name and verified extracted-text semantics as `docbank search`.
Results initially preserve the API's relevance ranking. Choosing Document,
Size, or Modified changes to that explicit column order; Document compares the
complete paths shown in search results rather than only their basenames.
Refreshing a folder resolves its stable node ID, current canonical path, and
children in one metadata snapshot, so a concurrent CLI or agent move cannot
leave the browser constructing child paths beneath an obsolete name.

The current web application does not compare versions, import, edit, revert,
prune, move, create or change tags and
assignments, trash, enroll audit scopes, run independent audit verification,
or run maintenance and backup operations.
Use the corresponding CLI or authenticated HTTP endpoint for those workflows.
Future web workflows will require deliberately expanded browser-session
permissions rather than inheriting the master API key.

If a page reports that its browser session expired or was rejected, run
`docbank web` again. Sessions deliberately do not survive daemon restart, and
the previous random `.localhost` origin is not reused.
