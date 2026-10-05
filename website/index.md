# Find the right document. Keep the whole record.

Bring files, email, photos, and recordings into a vault you control. Search their text, review the exact saved version, and export the records you need without losing the originals.

Docbank is open source under Apache-2.0, for Linux, macOS, and Windows.

## Install

On macOS or Linux:

```sh
curl -fsSL https://docbank.ai/install.sh | sh
```

On Windows:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

Then [try the quickstart](/docs/quickstart/) or read the [setup guide](/docs/setup/).

## Read the file behind the result

Browse folders, follow search matches, and inspect saved versions in the local
web app. Check a range of documents, apply a tag to the selection, or export
the rows as CSV.

[![Docbank's labeled sidebar, folder breadcrumbs, and document inspector in a synthetic vault](https://docbank.ai/assets/generated/web-vault-browser.png)](https://docbank.ai/assets/generated/web-vault-browser.png)

The Docbank web app with synthetic documents. [Take the visual tour](/docs/tour/).

## Find the passage you remember

Search names and extracted text, then narrow the results by folder, tag, file
type, or date. A configured embedding service adds search by meaning. Open the
retained text to check the source.

- **Keep a useful search.** Save a complete query and reuse highlight terms.
  Browse a frozen result set whose rows, counts, and filters stay fixed while
  documents change.
- **Find related documents.** Compare a selected file with the other loaded
  versions using stored embeddings. Finding similar documents makes no provider call.
- **Read email with its attachments.** Import MBOX or Google Takeout archives.
  Read HTML or plain text, inspect headers, and follow attachments back to the
  exact parent message.
- **Find a moment in a recording.** Import a transcript or use a configured
  Docling service to transcribe supplied WAV and MP3 audio. Search results can
  identify matching time intervals.

[Search and filter documents](/docs/usage/searching/) ·
[Configure search by meaning](/docs/usage/search/) ·
[Group and browse photos](/docs/usage/photos/)

![Configured document search showing retained text excerpts in the web app](https://docbank.ai/assets/generated/web-natural-1440.png)

Search results show the text and source behind a match.

## Take the records you need

Preview an export before starting it. Docbank fixes the selected versions and
verifies the files it puts in the download.

- **Download a checked bundle.** Export selected originals and available retained
  outputs as a ZIP. Later edits do not replace the versions in your plan.
- **Support a search report.** Count search terms across all documents, import
  collections, or selected versions. Download the CSV with a frozen evidence ZIP.
- **Exchange review packages.** Import files with their review metadata and page
  maps. Export frozen selections as DAT packages with PDFs or images, or CSV
  packages with native files.
- **Label selected pages.** Preview and reserve a Bates range: sequential labels
  stamped on exported PDF pages. Reserved numbers cannot be reused after abandonment.

[Verified exports](/docs/usage/export-bundles/) ·
[Search reports](/docs/usage/search-exports/) ·
[Load-file packages](/docs/usage/web/#import-load-files)

![A completed verified export ready to download from a frozen query](https://docbank.ai/assets/generated/web-export-ready.png)

Download the versions included in the reviewed export.

## Choose what leaves your vault

Review the provider, destination, and content it will receive before granting
processing consent. Use supported local tools, a service you operate, or a hosted provider.

- **Read what was extracted.** Processing retains a rendition: the extracted
  Markdown for one saved version. Read it alongside the original and check
  processing coverage without starting another job.
- **Keep the original available.** Stored files remain retrievable without
  processing. New vaults have no processing profiles. Network processing after
  a restore requires fresh consent.

Provider and format support varies by interface. Start with the
[processing guide](/docs/usage/document-processing/) and
[provider configuration](/docs/usage/configuration/). Anonymous usage telemetry
is on by default; [see what it sends and how to turn it off](/docs/configuration/#anonymous-usage-telemetry).

## Keep earlier versions. Test your backup.

Moving or renaming a document keeps its ID. Replacing its content adds a version.
Checksums let Docbank detect changed bytes when you retrieve a file.

- **Undo a mistaken deletion.** Restore documents from trash. Permanent deletion
  and reclaiming disk space are separate actions.
- **Recover on your own storage.** Create incremental backups, verify them, and
  restore a separate vault before you need it. Add filesystem or S3-compatible
  stores as your collection grows.

[Saved versions](/docs/architecture/editing-and-versions/) ·
[Backup and restore](/docs/usage/backup/) ·
[Permanent audited history](/docs/usage/audited-history/)

## Give your tools the same records

The CLI, web app, terminal browser, HTTP API, and local MCP server use one
daemon. Agents read the same document IDs and exact versions you do. A Go
application can own a separate vault in its own process.

| Interface | Use it to |
| --- | --- |
| CLI | Import, search, export, and maintain documents from scripts. |
| Web | Read, select, tag, process, and export documents visually. |
| TUI | Browse, search, and inspect processing from the keyboard. |
| MCP | Connect local agents with explicit opt-ins for processing and export writes. |
| HTTP | Integrate through authenticated requests. |
| Go | Own a vault and its backup lifecycle inside an application. |

[Connect an agent](/docs/usage/mcp/) or [embed in Go](/docs/embedding/).

## Start with a folder of documents

Docbank is alpha software. Keep independent copies of irreplaceable material and
verify backups before relying on them.

It does not synchronize folders across devices, create public share links, or
provide collaborative editing.

[Try the quickstart](/docs/quickstart/) or [follow one document](/guide/).

[Meet the contributors](https://github.com/kenn-io/docbank#contributors).
