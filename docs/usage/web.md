---
title: Web application
description: Browse and search the local vault in a responsive, authenticated, read-only web interface.
---

# Web application

!!! info "Release availability"

    `docbank web` is newer than v0.10.1. Build from source to use it until the
    next release is tagged.

Run:

```bash
docbank web
```

Docbank starts or reconnects to the selected vault's compatible daemon and
opens its local web application. The first release is a read-only document
browser: navigate the virtual tree, sort a folder by document name, size, or
modification time, search names and extracted text, and inspect the selected
document's stable node ID, revision, current version ID, SHA-256 identity,
exact size, and media type.

The browser is another client of the authenticated HTTP API. It does not open
SQLite or the blob store, and it has no private route that the CLI or an agent
cannot use. The daemon remains loopback-only.

## Browser authentication

When Docbank opens the browser, it writes a small launch page beside the
owner-private daemon runtime record and passes only that credential-free local
file path to the operating system. Before doing so, the ownership-pinned CLI
asks the daemon to exchange its master API authority for a random,
daemon-lifetime browser session. The master key stays on that pinned connection
and never enters browser storage, a URL, or a child-process argument.

The daemon serves that session from a second listener on a newly selected
loopback port. This browser origin is independent of the configured API port
and exists for only one daemon lifetime. A process that later captures the API
port therefore cannot leave a service worker or cached script waiting for a
future browser session.

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
Refreshing a folder resolves its stable node ID, current canonical path, and
children in one metadata snapshot, so a concurrent CLI or agent move cannot
leave the browser constructing child paths beneath an obsolete name.

The current web application does not import, edit, move, tag, trash, enroll
audit scopes, or run maintenance and backup operations. Use the corresponding
CLI or authenticated HTTP endpoint for those workflows. Future web workflows
will require deliberately expanded browser-session permissions rather than
inheriting the master API key.
