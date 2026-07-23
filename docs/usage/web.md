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

When Docbank opens the browser, it places the daemon's effective API key in the
URL fragment. Browsers do not include fragments in HTTP requests. The
application consumes the key into tab-scoped session storage and immediately
removes the fragment from the address bar before making an API request. Every
vault read then carries the key in `X-Api-Key`.

Closing the tab ends that browser session. The lock button removes the stored
key immediately. An ephemeral daemon key also becomes invalid when that daemon
stops; run `docbank web` again and the ordinary daemon-first lifecycle starts
or reconnects to the compatible owner.

For an explicitly configured `[server] api_key`, the unlock screen accepts the
same key. It still stays in session storage rather than durable browser
storage.

Use `docbank web --no-browser` only when another local program must open the
URL. That output contains the live session key. Do not put it in shell history,
logs, screenshots, issue trackers, or chat.

## Current boundary

Folder and search views are bounded to 1,000 rows and say when more results
exist; use the CLI or paginated HTTP API for exhaustive automation. Search has
the same name and verified extracted-text semantics as `docbank search`.

The current web application does not import, edit, move, tag, trash, enroll
audit scopes, or run maintenance and backup operations. Use the corresponding
CLI or authenticated HTTP endpoint for those workflows. Future web workflows
will remain ordinary API clients rather than gaining a privileged data path.
