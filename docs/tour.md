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

## Review a selection

Check document ranges, apply tags atomically, and download the checked rows as
CSV. The sidebar groups browsing, review, export, and vault maintenance. On
narrow screens, open the navigation menu to reach the same tools. See
[selection and shortcuts](usage/web.md#select-documents-on-this-page).

![A selected document range and its available actions](https://docbank.ai/assets/generated/web-page-selection.png)

## Keep a search and its results

Use the query editor for field and Boolean expressions. Save the complete
query, reuse highlight terms, and run a lexical query to freeze its rows,
counts, and facets. A frozen row keeps the version selected when the query ran.
See [frozen queries](usage/web.md#work-with-a-frozen-query).

![A frozen query workspace with fixed facets and document versions](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

## Read retained text and search by meaning

Review what configured providers receive before granting processing consent.
Follow the job and read its verified rendition: the extracted Markdown retained
for that source version. Configured embeddings add Semantic and Hybrid search.
**Find similar** compares stored embeddings without calling a provider. See
[processing](usage/document-processing.md) and [search](usage/search.md).

![A reviewed processing plan disclosing provider destinations and retained outputs](https://docbank.ai/assets/generated/web-document-processing-plan.png)

![Search results with text excerpts and semantic matches](https://docbank.ai/assets/generated/web-natural-1440.png)

## Read email with its attachments

Import MBOX or Google Takeout archives, read HTML or plain-text bodies, and
inspect decoded or raw headers. Follow attachment documents and parent messages
at their exact versions. See [archived email](usage/web.md#read-archived-email).

![An archived email with decoded headers and an HTML body](https://docbank.ai/assets/generated/web-email-reader.png)

## Download the versions you reviewed

Preview original files and retained outputs from a page, checked rows, or a
frozen query. Download the verified ZIP when it is ready. Dated search reports
have separate CSV and frozen evidence downloads. See
[document bundles](usage/export-bundles.md) and [search reports](usage/search-exports.md).

![A verified export with its download receipt](https://docbank.ai/assets/generated/web-export-ready.png)

![Search counts and evidence downloads for selected versions](https://docbank.ai/assets/generated/selected-report-result.png)

## Exchange packages and label pages

Import load-file packages with their files, supplied text, metadata, and page
maps. For a sealed package, preview and reserve sequential Bates labels before
exporting the selected PDF pages. See [load-file import](usage/web.md#import-load-files)
and [Bates export](usage/web.md#stamp-selected-pages-with-bates-labels).

![A load-file package preview](https://docbank.ai/assets/generated/web-load-file-import.png)

![A completed Bates-stamped PDF export](https://docbank.ai/assets/generated/web-bates-export.png)

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
