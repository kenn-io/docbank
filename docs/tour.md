---
title: Visual tour
description: Screenshots of the Docbank web app and terminal browser, taken against synthetic vaults.
---

# Visual tour

Every screenshot on this page shows the real Docbank interface running against
a temporary vault. All the data in them is synthetic: the names, contents,
hashes, identifiers, storage history, and audit history were generated for the
captures.

## Browse and identify documents

The web app lists a folder as a sortable table. Select a document to see its
details without opening it: current path, stable ID, revision, version ID,
SHA-256 content hash, tags, origin records, and permanent-audit status. The
browser gets limited credentials that last for one daemon run. See
[Web application](usage/web.md) for controls and authentication.

![The Docbank web application showing a folder of a synthetic vault, with the selected document's details in a side panel.](https://docbank.ai/assets/generated/web-vault-browser.png)

## Organize with tags

Tags group documents across folders. A tag keeps its UUID and its color when
you rename it, and the browser groups tags whose names share a slash-separated
prefix. You can manage tags in the browser. People and agents use the same
daemon API, which rejects a change made against an outdated revision. See
[Organizing and tagging](usage/organizing.md).

![The Docbank web application listing the tags defined in a synthetic vault.](https://docbank.ai/assets/generated/web-tag-catalog.png)

## Review a selection

Check a range of documents, then tag them all in one atomic step or download
the checked rows as CSV. The sidebar groups the tools for browsing, review,
export, and vault maintenance. On a narrow screen, the same tools are in the
navigation menu. See
[selection and shortcuts](usage/web.md#select-documents-on-this-page).

![A selected document range and its available actions](https://docbank.ai/assets/generated/web-page-selection.png)

## Keep a search and its results

The query editor accepts field and Boolean expressions. You can save a
complete query and reuse sets of highlight terms. Running a lexical query
freezes its rows, counts, and facets, and each frozen row keeps the version
that matched when the query ran. See
[frozen queries](usage/web.md#work-with-a-frozen-query).

![A frozen query workspace with fixed facets and document versions](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

## Read extracted text and search by meaning

Before you consent to processing, the plan shows what each configured provider
will receive. Once the job finishes, read its rendition: the extracted Markdown
that Docbank verified and stored for that version. With embeddings configured,
search gains Semantic and Hybrid modes. **Find similar** compares stored
embeddings and calls no provider. See
[processing](usage/document-processing.md) and [search](usage/search.md).

![A processing plan listing each provider, its destination, and the outputs Docbank will keep](https://docbank.ai/assets/generated/web-document-processing-plan.png)

![Search results with text excerpts and semantic matches](https://docbank.ai/assets/generated/web-natural-1440.png)

## Read email with its attachments

Import an MBOX file or a Google Takeout archive. Read each message as HTML or
plain text, with decoded or raw headers. From a message you can open its
attachments, and from an attachment its parent message, each at the version
that was imported together. See
[archived email](usage/web.md#read-archived-email).

![An archived email with decoded headers and an HTML body](https://docbank.ai/assets/generated/web-email-reader.png)

## Download the versions you reviewed

Start an export from a page, from checked rows, or from a frozen query. The
preview lists the original files and stored outputs the bundle will contain.
Download the verified ZIP when it is ready. A search report for a date range
has two downloads: the CSV of counts and a frozen evidence ZIP. See
[document bundles](usage/export-bundles.md) and
[search reports](usage/search-exports.md).

![A verified export with its download receipt](https://docbank.ai/assets/generated/web-export-ready.png)

![Search counts and evidence downloads for selected versions](https://docbank.ai/assets/generated/selected-report-result.png)

## Exchange packages and label pages

Import a load-file package with its files, supplied text, metadata, and page
maps. Once a package is sealed, you can preview and reserve sequential Bates
labels, then export the selected PDF pages with the labels stamped on them. See
[load-file import](usage/web.md#import-load-files) and
[Bates export](usage/web.md#stamp-selected-pages-with-bates-labels).

![A load-file package preview](https://docbank.ai/assets/generated/web-load-file-import.png)

![A completed Bates-stamped PDF export](https://docbank.ai/assets/generated/web-bates-export.png)

## Verify permanent history

Enabling an audit scope on a directory keeps its versions and recorded changes
permanently. Verification replays that history and checks the hashes of the
protected content. Save the evidence report outside the vault so you can
compare it with a later check. Enabling a scope cannot be undone.
[Permanent audited history](usage/audited-history.md) explains the retention
rules.

![The Docbank web application showing the result of verifying permanent audit evidence in a synthetic vault.](https://docbank.ai/assets/generated/web-audit-evidence.png)

## See where document bytes are stored

The storage view lists the built-in local store and any filesystem or
S3-compatible stores you have configured. It is read-only. It separates what
the database records from the bytes actually on disk, and reports store health,
content that has only one copy, and the live documents a problem affects. See
[Multi-store storage](usage/storage.md) for placement and recovery commands.

![The Docbank web application showing the primary and a secondary physical store for a synthetic vault.](https://docbank.ai/assets/generated/web-multi-store-storage.png)

## Work from a terminal

The terminal user interface (TUI) browses and searches the vault, shows
document IDs and history, and moves documents to and from the trash. Its
operations screen loads storage and backup results independently, so vault
status still appears when a backup repository is unavailable. See the
[terminal browser guide](usage/tui.md) for keys and limits.

![The Docbank TUI showing physical storage inventory, two content stores, and two synthetic backup recovery points.](https://docbank.ai/assets/generated/tui-multi-store-storage.png)

Next, try the [quickstart](quickstart.md) or pick a task from the
[capability guide](capabilities.md).
