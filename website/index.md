# Find the right document. Keep the whole record.

Docbank is a document vault that runs on your own machine. It imports files,
email, photos, and recordings, makes their text searchable, and keeps every
version you save. When you need to hand records to someone else, it exports the
versions you checked.

Docbank is open source under Apache-2.0 and runs on Linux, macOS, and Windows.

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

## Browse and read your documents

The local web app shows the vault as folders. Open a document to read it, see
its earlier versions, and check where it came from. Select several documents to
tag them together or download the list as CSV.

[![The Docbank web app with a folder of documents on the left and the selected document's details on the right](https://docbank.ai/assets/generated/web-vault-browser.png)](https://docbank.ai/assets/generated/web-vault-browser.png)

The web app with a vault of synthetic documents. [Take the visual tour](/docs/tour/).

## Search by what a document says

Search covers file names and the text Docbank extracts from each document.
Narrow the results by folder, tag, file type, or date. Connect an embedding
service to search by meaning as well. Each result shows the matching text, so
you can check it against the original.

- **Saved and frozen searches.** Save a query, or a set of terms to highlight,
  and use it again later. Freeze a query's results to keep the same documents,
  counts, and filters while the vault changes underneath.
- **Similar documents.** Pick a file and Docbank ranks the others in the
  current view by similarity. It compares embeddings already stored in the
  vault and sends nothing to a provider.
- **Email.** Import an MBOX file or a Google Takeout archive. Read each message
  as HTML or plain text, look at its headers, and move between a message and
  its attachments.
- **Recordings.** Import a transcript, or have a Docling service you configure
  transcribe WAV and MP3 audio. Search results can point to the part of the
  recording that matched.

[Search and filter documents](/docs/usage/searching/) ·
[Configure search by meaning](/docs/usage/search/) ·
[Group and browse photos](/docs/usage/photos/)

![Search results in the web app, each with an excerpt of the matching text](https://docbank.ai/assets/generated/web-natural-1440.png)

Each search result shows the text that matched.

## Export what you reviewed

Docbank shows what an export will contain before it builds anything. The export
uses the versions you selected, even if those documents change afterwards, and
Docbank verifies every file it puts in the download.

- **ZIP bundles.** Download the selected originals as one ZIP. Where Docbank
  holds extracted text, page images, or email PDFs for those versions, the
  bundle can include them.
- **Search reports.** Count search matches over a date range across all
  documents, chosen imports, or a selection. The CSV of counts comes with a ZIP
  of the evidence behind them, so someone else can check the numbers.
- **Load-file packages.** Import a load file with its documents, metadata, and
  page maps. Export a frozen selection as a DAT package with PDFs or images, or
  as a CSV package with the native files.
- **Bates labels.** Stamp sequential Bates labels on the PDF pages you export.
  Docbank reserves the range before it stamps, and never issues a reserved
  number again, even if you abandon the export.

[Verified exports](/docs/usage/export-bundles/) ·
[Search reports](/docs/usage/search-exports/) ·
[Load-file packages](/docs/usage/web/#import-load-files)

![The export drawer reporting a verified ZIP that is ready to download](https://docbank.ai/assets/generated/web-export-ready.png)

A finished export, verified and ready to download.

## You decide what leaves the vault

Extracting text or building embeddings can mean sending content to another
service. Before that happens, Docbank shows a plan that names each provider,
where it runs, and what it will receive. Nothing is sent until you consent. You
can use local tools, a service you host, or a hosted provider.

- **Extracted text you can read.** Processing saves the extracted text as
  Markdown, tied to the version it came from. Read it next to the original, and
  see which documents have been processed without starting another job.
- **Processing is optional.** A new vault has no processing profiles, and you
  can always retrieve your files without them. After you restore from a backup,
  network processing needs your consent again.

Supported providers and formats differ between interfaces. See the
[processing guide](/docs/usage/document-processing/) and
[provider configuration](/docs/usage/configuration/). Docbank sends anonymous
usage telemetry by default.
[See what it sends and how to turn it off](/docs/configuration/#anonymous-usage-telemetry).

## Versions, trash, and backups

Moving or renaming a document does not change its ID. Saving new content adds a
version and keeps the old one. Docbank records a checksum for every file and
uses it to detect changed bytes when you retrieve the file.

- **Trash.** A deleted document goes to the trash, and you can restore it from
  there. Deleting it permanently and reclaiming the disk space are separate
  steps.
- **Backups and storage.** Backups are incremental. Verify one, then restore it
  into a separate vault to test it before you need it. As the collection grows,
  add filesystem or S3-compatible stores.

[Saved versions](/docs/architecture/editing-and-versions/) ·
[Backup and restore](/docs/usage/backup/) ·
[Permanent audited history](/docs/usage/audited-history/)

## Use it from a terminal, a browser, or an agent

The CLI, web app, terminal browser, HTTP API, and local MCP server all talk to
one daemon, so they see the same documents, IDs, and versions. A Go application
can instead own a separate vault in its own process.

| Interface | Use it to |
| --- | --- |
| CLI | Import, search, export, and maintain a vault from scripts. |
| Web | Read, select, tag, process, and export documents in a browser. |
| TUI | Browse, search, and check on processing from the keyboard. |
| MCP | Connect a local agent. Processing and export writes stay off until you enable them. |
| HTTP | Integrate other software through authenticated requests. |
| Go | Run a vault and its backups inside your own application. |

[Connect an agent](/docs/usage/mcp/) or [embed in Go](/docs/embedding/).

## Before you start

Docbank is alpha software. Keep your own copies of anything irreplaceable, and
verify a backup before you rely on it.

It does not synchronize folders across devices, create public share links, or
provide collaborative editing.

[Try the quickstart](/docs/quickstart/) or [follow one document](/guide/).

[Meet the contributors](https://github.com/kenn-io/docbank#contributors).
