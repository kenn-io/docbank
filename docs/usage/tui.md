---
title: Interactive terminal browser
description: Browse documents, inspect their details, storage, backups, and permanent history, and move documents to or from trash in the terminal.
---

# Interactive terminal browser

You can browse and search your vault, inspect document details, and move
documents to or from recoverable trash right from your terminal. To start the
terminal user interface (TUI), run:

```bash
docbank tui
```

The TUI talks to the same authenticated daemon API as the CLI and starts or
reuses the daemon that owns your database and stored content. Before each
request, it finds or restarts a compatible daemon, though leaving the TUI open
does not keep an otherwise idle daemon running. For more on how the daemon
works, see [Daemon](../architecture/daemon.md).

![The Docbank TUI showing physical storage inventory, two content stores, and two synthetic backup recovery points.](https://docbank.ai/assets/generated/tui-multi-store-storage.png)

## Browse and inspect documents

The main view opens as a full-width document table. At ordinary terminal widths,
it shows each document's name, type, size, and UTC modification time, and search
results also show the match kind when there is room. On narrow terminals,
secondary columns hide first so the document name stays readable.

To leave the table temporarily and inspect the selected document's authority,
press <kbd>i</kbd>. The detail view shows the document's complete stable node
selector, path, revision, and modification time. For a file, it also displays
the version, SHA-256 identity, exact size, and media type. Every assigned tag
name appears alongside its stable UUID so you can tell a renamed tag from a
different tag, and long values wrap instead of truncating.

## Read permanent history

To open a node's permanent audited history, select it and press <kbd>a</kbd>.
The timeline lists events newest first, showing when each event was recorded,
what happened, and the primary path, version, or attached-metadata change. To
see the full record for an event, press <kbd>Enter</kbd>; this view shows the
complete event ID, operation and scope IDs, revisions, path states, version
identities, and typed tag, provenance, or photo details. For photos, the state
shows authored values and confirmed field names before and after edits and undo,
including confirmation of an empty caption. If a node is outside an audit scope,
the TUI labels it as such instead of showing an empty timeline.

## Inspect jobs, storage, and backups

To check daemon-owned background work without leaving the document view, press
<kbd>J</kbd>. The activity screen reports worker activity and lifecycle status,
showing each stable job name, whether it is idle, running, completed, failed,
or cancelled, and its start and finish times. Workers waiting for work appear
as idle, and the running count includes only work in progress. When you inspect
a job, you see its complete final failure text. Refreshing asks the daemon for
a new snapshot, and closing the screen returns you to the same document
selection.

To open a read-only operational summary, press <kbd>O</kbd>. This screen
separates what the catalog records from the physical loose-file and pack
inventory, including live packed content and dead packed payload waiting for a
repack. It also lists recovery points from the configured backup repository,
with each point's creation time, tag, snapshot ID, file count, and newly added
bytes. Storage status still appears when no backup repository is configured.
The two results are independent and report their own errors.

## Trash and restore documents

To review moving the selected live node to recoverable trash, press
<kbd>x</kbd>. The confirmation dialog names the escaped path, stable node ID,
and exact revision to change, and it notes that the node remains restorable and
no content bytes are reclaimed. If the node changes concurrently, the daemon
rejects the request instead of applying it to newer state.

To browse independently restorable trash roots from newest to oldest, press
<kbd>T</kbd>. Pressing <kbd>Enter</kbd> opens a second revision-bound
confirmation. When a restore succeeds, the TUI reports the live path the daemon
selected after collision suffixing or origin-parent fallback. A restore does
not promise the old path. The TUI does not offer permanent deletion or physical
reclamation; use the preview-first CLI or authenticated HTTP workflows for
those operations.

## Browse load-file packages

To browse received and produced packages, press <kbd>K</kbd>. From there, press
<kbd>Enter</kbd> on a completed package to view its members, or press
<kbd>l</kbd> to look up an exact Bates label within the selected package. Label
results display the label set, provenance, and page number when verified.

Each page holds at most 250 packages or members, or 100 label matches, and the
displayed counts describe the current page. A `+` indicates that another page
is available. Press <kbd>n</kbd> to replace the current rows with the next
page, or press <kbd>r</kbd> to refresh from the first page.

You can move through any of these lists with <kbd>↑</kbd>/<kbd>↓</kbd> or
<kbd>k</kbd>/<kbd>j</kbd>. Use <kbd>PgUp</kbd>/<kbd>PgDn</kbd> to scroll by a
screen, <kbd>Home</kbd>/<kbd>End</kbd> to select the first or last row on the
current page, and <kbd>Esc</kbd> to leave label results, members, or the
package browser.

![The terminal browser listing imported load-file packages](https://docbank.ai/assets/generated/tui-packages.png)

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
| <kbd>K</kbd> | Browse load-file packages, their members, and exact Bates labels |
| <kbd>←</kbd>, <kbd>Backspace</kbd>, or <kbd>Esc</kbd> | Return to the parent directory or leave search results |
| <kbd>/</kbd> | Search live names and extracted text |
| <kbd>s</kbd> | Cycle the sort column: name, size, and modification time |
| <kbd>v</kbd> | Reverse the current sort direction |
| <kbd>r</kbd> | Refresh the current directory or search |
| <kbd>?</kbd> | Show keyboard help |
| <kbd>q</kbd> or <kbd>Ctrl-C</kbd> | Quit |

Inside audited history, press <kbd>n</kbd> or <kbd>→</kbd> to load the next
older page, and <kbd>p</kbd> or <kbd>←</kbd> to return to a cached newer page.
Pressing <kbd>Esc</kbd> returns you to the same directory or search result and
selected document. Each page holds at most 100 events, and the heading reports
your position in the complete history.

## Search and result limits

TUI search follows [the same rules as `docbank search`](searching.md). Name
matches appear before content-only matches, and content search covers only
supported documents whose current bytes completed verified extraction. Each
result indicates whether the match came from the name or the content. Results
sort by relevance by default; press <kbd>s</kbd> to switch to a column sort,
and keep cycling through the columns to return to relevance.

The TUI loads at most 1,000 directory entries or search hits and lets you know
when more exist. To list complete directories, use CLI or HTTP pagination.
Search has no continuation cursor. To handle a truncated query, narrow it as
described in [Searching](searching.md#how-do-i-handle-incomplete-results).

## Current limits

The search box accepts text only. For tag, media-type, directory, and
modification-time filters, use `docbank search` or the HTTP API. You can manage
saved query and highlight definitions through the
[HTTP API](searching.md#save-complete-query-intent-over-http).

While searching, press <kbd>Tab</kbd> to cycle through **Names and text**,
**Auto**, **Lexical**, **Semantic**, and **Hybrid**. **Auto** uses **Hybrid**
when an embedding binding is available and uses the lexical API mode otherwise;
**Semantic** and **Hybrid** both require a binding. To toggle reranking when
your profile permits it, press <kbd>Ctrl-R</kbd>. Changing either the search
mode or reranking reruns your latest submitted query, even while results are
still loading. Base rows stay visible while reranking runs, and if reranking
fails, the TUI keeps those rows and displays the cause. Processing failures
fall back to **Names and text** and show the failure.

![Configured text and semantic search in the terminal browser](https://docbank.ai/assets/generated/tui-natural-search.png)

Other mutations, permanent deletion, permanent-audit enrollment, independent
verification, backup creation, verification, or restore, and storage
maintenance are not available in the TUI. Use their CLI commands or
authenticated HTTP endpoints instead.

The <kbd>O</kbd> operations screen loads storage and backups independently. Its
storage section lists each physical store's role, backend kind, observed
health, authoritative object count, logical and stored bytes, affected live
documents, and any sole-authority objects without a readable alternative. The
screen is read-only and does not expose binding paths, endpoints, credentials,
ownership epochs, takeover, repair, or placement controls.
