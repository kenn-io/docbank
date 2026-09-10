---
title: Interactive terminal browser
description: Browse documents, inspect authority, storage, backups, and permanent history, and safely move or restore recoverable trash from the TUI.
---

# Interactive terminal browser

Browse and search your vault from the terminal, inspect document details, and
move documents to or from recoverable trash. Start the terminal user interface
(TUI) with:

```bash
docbank tui
```

The TUI uses the same authenticated daemon API as the CLI. It starts or reuses
the daemon, which owns the database and stored content. Before each request,
the TUI finds or restarts a compatible daemon. Leaving the TUI open does not
keep an otherwise idle daemon running. See [Daemon](../architecture/daemon.md).

![The Docbank TUI showing physical storage inventory, two content stores, and two synthetic backup recovery points.](https://docbank.ai/assets/generated/tui-multi-store-storage.png)

## Browse and inspect documents

The main view is a full-width document table. At ordinary terminal widths it
shows each document's name, type, size, and UTC modification time; search results
also identify the match kind when space permits. Narrow terminals progressively
hide secondary columns so the document name remains useful.

Press <kbd>i</kbd> to leave the table temporarily and inspect the selected
document's complete stable node selector, path, revision, modification time,
and—when it is a file—its immutable version, SHA-256 identity, exact size, and
media type. The same authority view lists every assigned tag name with its
stable UUID, so renames remain distinguishable from identity. Long authority
values wrap rather than truncate.

## Read permanent history

Press <kbd>a</kbd> on any selected node to open its permanent audited history.
The timeline is newest first and shows when each event was recorded, what
happened, and the primary path, version, or attached-metadata change. Press
<kbd>Enter</kbd> to inspect the complete immutable event ID, operation and scope
IDs, revisions, path states, version identities, and typed tag or provenance
details. Nodes outside an audit scope are identified plainly rather than shown
with an empty or invented timeline.

## Inspect jobs, storage, and backups

Press <kbd>J</kbd> to inspect daemon-owned background work without leaving the
document view. The activity screen shows each stable job name, whether it is
running, completed, failed, or cancelled, and its start and finish times.
Inspecting a job exposes its complete terminal failure text. Refresh asks the
current compatible daemon for a new snapshot; closing the screen returns to the
same document selection. The current job report shows lifecycle status, not
completion percentages.

Press <kbd>O</kbd> for a read-only operational summary. It separates logical
catalog authority from physical loose-file and pack inventory, including live
packed content and dead packed payload awaiting an explicit repack. The same
screen lists the configured backup repository's recovery points with their
creation time, tag, snapshot ID, file count, and newly added bytes. Storage
status remains useful when no backup repository is configured; the two
independent results report their own errors.

## Trash and restore documents

Press <kbd>x</kbd> to review moving the selected live node to recoverable
trash. The confirmation names the escaped path, stable node ID, and exact
revision that will be changed; a concurrent change is rejected rather than
silently targeting newer state. The dialog also says plainly that the node
remains restorable and no content bytes are reclaimed.

Press <kbd>T</kbd> to browse independently restorable trash roots, newest
first. Enter opens a second revision-bound confirmation. A successful restore
reports the actual live path selected by the daemon after collision suffixing
or origin-parent fallback. Restoration does not guess or promise the old path.
Permanent deletion and physical reclamation are deliberately absent from the
TUI; use the preview-first CLI or authenticated HTTP workflows when that is
really intended.

## Keyboard controls

| Key | Action |
|-----|--------|
| <kbd>↑</kbd>/<kbd>k</kbd>, <kbd>↓</kbd>/<kbd>j</kbd> | Move between documents |
| <kbd>Enter</kbd> or <kbd>→</kbd> | Open the selected directory |
| <kbd>Enter</kbd> on a file, or <kbd>i</kbd> | Inspect complete document authority |
| <kbd>x</kbd> | Review moving the selected revision to recoverable trash |
| <kbd>T</kbd> | Browse and restore recoverable trash roots |
| <kbd>a</kbd> | Browse the selected node's permanent audited history |
| <kbd>J</kbd> | Inspect daemon background jobs and failures |
| <kbd>O</kbd> | Inspect storage inventory and backup recovery points |
| <kbd>←</kbd>, <kbd>Backspace</kbd>, or <kbd>Esc</kbd> | Return to the parent directory or leave search results |
| <kbd>/</kbd> | Search live names and extracted text |
| <kbd>s</kbd> | Cycle the sort column: name, size, and modification time |
| <kbd>v</kbd> | Reverse the current sort direction |
| <kbd>r</kbd> | Refresh the current directory or search |
| <kbd>?</kbd> | Show keyboard help |
| <kbd>q</kbd> or <kbd>Ctrl-C</kbd> | Quit |

Within audited history, <kbd>n</kbd>/<kbd>→</kbd> loads the next older page and
<kbd>p</kbd>/<kbd>←</kbd> returns to a cached newer page. Escape returns to the
same directory or search result and selected document. Each page is bounded to
100 events; the heading reports its position in the complete history.

## Search and result limits

Search follows [the same rules as `docbank search`](searching.md). Name matches
precede content-only matches, and content is available only for supported documents
whose current bytes completed verified extraction. Results say whether the
match came from the name or content. Relevance order remains the search default;
pressing <kbd>s</kbd> opts into a column sort, and cycling through the columns
returns to relevance. The TUI loads at most 1,000 directory entries or search
hits and says when more exist. Use CLI or HTTP pagination to list complete directories. Search
has no continuation cursor; narrow a truncated query as described in
[Searching](searching.md#how-do-i-handle-incomplete-results).

## Current limits

The search box accepts text only. Use `docbank search` or HTTP for tag,
media-type, directory, and modification-time filters. Saved query and
highlight definitions are managed through the
[HTTP API](searching.md#save-complete-query-intent-over-http).

Other mutations, permanent deletion, permanent-audit enrollment, independent
verification, backup creation/verification/restore, and storage maintenance
remain outside this interface. Use their ordinary CLI commands or authenticated
HTTP endpoints.

The <kbd>O</kbd> operations screen keeps storage and backup loading
independent. Its storage section lists every physical store's role, backend
kind, observed health, authoritative object count, logical and stored bytes,
affected live documents, and any sole-authority objects without a readable
alternative. It is read-only and does not expose binding paths, endpoints,
credentials, ownership epochs, takeover, repair, or placement controls.
