---
title: Interactive terminal browser
description: Browse the virtual tree, search document names and extracted text, and inspect stable document identity from a daemon-backed TUI.
---

# Interactive terminal browser

!!! info "Release availability"

    `docbank tui` is newer than v0.10.1. Build from source to use it until the
    next release is tagged.

Run:

```bash
docbank tui
```

The TUI is a read-only view of the same authenticated daemon API used by the
ordinary CLI. It never opens SQLite or the blob store and cannot bypass the
vault's exclusive owner. Starting it reuses or starts the daemon in the normal
way.

The left pane shows the current virtual directory or a bounded set of search
results. The right pane shows the selected document's stable node selector,
path, kind, revision, modification time, and—when it is a file—its current
immutable version, SHA-256 identity, raw size, and media type.

| Key | Action |
|-----|--------|
| <kbd>↑</kbd>/<kbd>k</kbd>, <kbd>↓</kbd>/<kbd>j</kbd> | Move between documents |
| <kbd>Enter</kbd> or <kbd>→</kbd> | Open the selected directory |
| <kbd>←</kbd>, <kbd>Backspace</kbd>, or <kbd>Esc</kbd> | Return to the parent directory or leave search results |
| <kbd>/</kbd> | Search live names and extracted text |
| <kbd>r</kbd> | Refresh the current directory or search |
| <kbd>q</kbd> or <kbd>Ctrl-C</kbd> | Quit |

Search has the same semantics as `docbank search`: name matches precede
content-only matches, and content is available only for supported documents
whose current bytes completed verified extraction. Results say whether the
match came from the name or content. The first interface loads at most 1,000
directory entries or search hits and says when more exist; use the CLI or HTTP
pagination for exhaustive automation.

Mutations, permanent-audit enrollment and history, backup, and storage
maintenance deliberately remain outside this first interface. Use their
ordinary CLI commands or authenticated HTTP endpoints. Later TUI work can add
those workflows without creating a privileged path into the vault.
