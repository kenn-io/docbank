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
type.

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
even when the card wraps it across lines.

## Search names and extracted text

Enter a word or phrase in the search box and press Enter. Results can match a
live document name or verified extracted text. The **Match** column identifies
which one. Name matches retain their API relevance ranking and appear before
content-only matches until you choose an explicit column sort.

![The Docbank web application showing extracted-text search results in a synthetic vault.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/v0.11.0/web-search-results.png)

*Search results display complete virtual paths and keep the same authority
inspection available from ordinary folder browsing.*

Clear the search box to return to the current directory. The web application
searches names and extracted text only; use `docbank search` when you need the
directory, tag, media-type, or modification-time filters, structured JSON, or
another result limit.

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
`X-Docbank-Web-Session`, which the daemon accepts only for the tree, node, and
search reads used by this interface. It cannot call mutation, backup,
maintenance, configuration, or general API endpoints.

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

The current web application does not import, edit, move, tag, trash, enroll
audit scopes, or run maintenance and backup operations. Use the corresponding
CLI or authenticated HTTP endpoint for those workflows. Future web workflows
will require deliberately expanded browser-session permissions rather than
inheriting the master API key.

If a page reports that its browser session expired or was rejected, run
`docbank web` again. Sessions deliberately do not survive daemon restart, and
the previous random `.localhost` origin is not reused.
