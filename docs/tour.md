---
title: Visual Tour
description: See Docbank's real web and terminal interfaces running against synthetic document vaults.
---

# Visual tour

These are the actual Docbank interfaces backed by temporary synthetic vaults,
not mockups. The names, contents, hashes, identifiers, storage history, and
audit history were generated for the captures.

## Browse and identify documents

Browse a sortable table and inspect a document without opening it. The details
include its current path, stable ID, revision, version ID, SHA-256 content hash,
tags, origin records, and permanent-audit status. The browser receives limited
credentials that last for one daemon run. See [Web application](usage/web.md)
for controls and authentication.

![The Docbank web application browsing a synthetic vault and showing the selected document's stable authority.](https://docbank.ai/assets/generated/web-vault-browser.png)

## Organize independently of folders

Group documents across folders with tags. Each tag keeps its UUID when its
name changes. The browser keeps its color and groups slash-separated names.
You can manage tags in the browser; people and agents use the
same daemon API, which rejects changes based on an outdated revision. See
[Organizing & Tagging](usage/organizing.md).

![The Docbank web application managing a synthetic vault's stable tag catalog.](https://docbank.ai/assets/generated/web-tag-catalog.png)

## Verify permanent history

Permanently retain a directory's versions and recorded changes by enabling an
audit scope. Verification replays that history and checks the protected
content hashes. Save its evidence report outside the vault to compare against
a later check. [Permanent Audited History](usage/audited-history.md) explains
the irreversible retention rules.

![The Docbank web application showing independently verified permanent audit evidence for a synthetic vault.](https://docbank.ai/assets/generated/web-audit-evidence.png)

## See where document bytes are stored

Inspect the built-in local store and configured filesystem or S3-compatible
stores. The read-only browser view distinguishes database records from bytes
on disk. It also reports store health, content with only one copy, and affected
live documents. See [Multi-store Storage](usage/storage.md) for placement and
recovery commands.

![The Docbank web application showing the primary and a secondary physical store for a synthetic vault.](https://docbank.ai/assets/generated/web-multi-store-storage.png)

## Operate from a terminal

Use the terminal user interface (TUI) to browse, search, inspect document IDs
and history, and move documents to or from recoverable trash. Its operations
screen loads storage and backup results independently, so an unavailable backup
repository does not hide vault status. See the
[terminal browser guide](usage/tui.md) for keys and limits.

![The Docbank TUI showing physical storage inventory, two content stores, and two synthetic backup recovery points.](https://docbank.ai/assets/generated/tui-multi-store-storage.png)

Continue with the [Quickstart](quickstart.md), or choose a task from
[Capabilities](capabilities.md).
