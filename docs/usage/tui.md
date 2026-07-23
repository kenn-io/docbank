---
title: Interactive terminal browser
description: Browse and sort documents, search names and extracted text, inspect stable identity, and read permanent audited history from a daemon-backed TUI.
---

# Interactive terminal browser

Run:

```bash
docbank tui
```

The TUI is a read-only view of the same authenticated daemon API used by the
ordinary CLI. It never opens SQLite or the blob store and cannot bypass the
vault's exclusive owner. Starting it reuses or starts the daemon in the normal
way. A TUI session may outlive the background daemon's idle window, so each
bounded interaction rediscovers or restarts a compatible daemon before issuing
its request; leaving the terminal open does not pin an otherwise idle process.

The main view is a full-width document table. At ordinary terminal widths it
shows each document's name, type, size, and UTC modification time; search results
also identify the match kind when space permits. Narrow terminals progressively
hide secondary columns so the document name remains useful.

Press <kbd>i</kbd> to leave the table temporarily and inspect the selected
document's complete stable node selector, path, revision, modification time,
and—when it is a file—its immutable version, SHA-256 identity, exact size, and
media type. Long authority values wrap rather than truncate.

Press <kbd>a</kbd> on any selected node to open its permanent audited history.
The timeline is newest first and shows when each event was recorded, what
happened, and the primary path, version, or attached-metadata change. Press
<kbd>Enter</kbd> to inspect the complete immutable event ID, operation and scope
IDs, revisions, path states, version identities, and typed tag or provenance
details. Nodes outside an audit scope are identified plainly rather than shown
with an empty or invented timeline.

| Key | Action |
|-----|--------|
| <kbd>↑</kbd>/<kbd>k</kbd>, <kbd>↓</kbd>/<kbd>j</kbd> | Move between documents |
| <kbd>Enter</kbd> or <kbd>→</kbd> | Open the selected directory |
| <kbd>Enter</kbd> on a file, or <kbd>i</kbd> | Inspect complete document authority |
| <kbd>a</kbd> | Browse the selected node's permanent audited history |
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

Search has the same semantics as `docbank search`: name matches precede
content-only matches, and content is available only for supported documents
whose current bytes completed verified extraction. Results say whether the
match came from the name or content. Relevance order remains the search default;
pressing <kbd>s</kbd> opts into a column sort, and cycling through the columns
returns to relevance. The first interface loads at most 1,000
directory entries or search hits and says when more exist; use the CLI or HTTP
pagination for exhaustive automation.

Mutations, permanent-audit enrollment and independent verification, backup,
and storage maintenance remain outside this interface. Use their
ordinary CLI commands or authenticated HTTP endpoints. Later TUI work can add
those workflows without creating a privileged path into the vault.
