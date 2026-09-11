# Keep a document from import to recovery

Import a document, save changes, and recover it later. These steps explain what
Docbank keeps and which choices belong to you. Processing steps apply only when
you configure or build that workflow.

1. [Import the file](#ingest)
2. [Keep its identity through changes](#identify)
3. [Choose what may be processed](#authorize)
4. [Turn a document into usable text](#render)
5. [Prepare content for meaning-based retrieval](#embed)
6. [Find the document again](#retrieve)
7. [Keep processing outputs separate](#replace)
8. [Work through your preferred interface](#serve)
9. [Verify a recovery copy](#prove)

<a id="ingest"></a>

## Import the file

Docbank copies the file into your vault and leaves the source untouched. Browse
the copy in a familiar folder tree. Docbank records a checksum so it can check
that the saved bytes have not changed.

![Synthetic Docbank vault after import](https://docbank.ai/assets/generated/web-vault-browser.png)

[Importing documents](/docs/usage/importing/)

<a id="identify"></a>

## Keep its identity through changes

Each document has a stable node ID. Moving or renaming it changes the path, not
the ID. Replacing its content saves a new version; retained earlier versions
remain available to inspect or download.

![Retained versions and download action](https://docbank.ai/assets/generated/web-retained-version-download.png)

[Editing and versions](/docs/architecture/editing-and-versions/)

<a id="authorize"></a>

## Choose what may be processed

Before using a processing provider, choose where document content may go and
which task is allowed. Configured daemon workers and applications using the Go
packages have different setup requirements. Follow the configuration or package
guide for the path you use.

[Processing configuration](/docs/configuration/)

<a id="render"></a>

## Turn a document into usable text

Optical character recognition (OCR) extracts text from images or scans. A
rendition is a text or Markdown representation of one saved source version. Go
applications can use the document packages to prepare this output while keeping
the original.

[Document understanding in Go](/docs/document-understanding/)

<a id="embed"></a>

## Prepare content for meaning-based retrieval

An embedding is a numeric representation used to compare meaning. The Go
packages split text into bounded inputs and describe the model and settings
used. Configured daemon workers can process eligible embedding jobs. This does
not add a semantic mode to the search command.

[Embedding configuration](/docs/configuration/)

<a id="retrieve"></a>

## Find the document again

Search document names and extracted text, then narrow results with tags,
folders, media types, or modification dates. Open the matching document or
download a saved version. The search guide defines the filters and result
limits.

![Search results in a synthetic vault](https://docbank.ai/assets/generated/web-search-results.png)

[Searching](/docs/usage/searching/)

<a id="replace"></a>

## Keep processing outputs separate

Text, previews, embeddings, and indexes are derivatives: outputs made from saved
documents. Docbank records their source versions separately from the originals.
Processing and retention rules determine when those outputs can be replaced or
removed.

[Processing and retention](/docs/architecture/overview/)

<a id="serve"></a>

## Work through your preferred interface

Use the command line, web app, terminal browser, or HTTP API through the local
daemon. Agents use the same authenticated requests and revision checks. A Go
application can instead own a separate vault in its own process.

[Docbank for agents](/docs/agents/)

<a id="prove"></a>

## Verify a recovery copy

Create an incremental backup, verify its content, and restore it into a separate
vault. Backups retain document content, metadata, and saved history. Test the
restored copy before you need it for recovery.

![Recorded history in a synthetic vault](https://docbank.ai/assets/generated/web-audit-evidence.png)

[Backup and restore](/docs/usage/backup/)

Start with the [quickstart](/docs/quickstart/), then use the
[task guides](/docs/) for commands, automation, storage, and recovery.
