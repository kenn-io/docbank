# Follow one document through Docbank

This guide follows a single document from import to backup. Each step says what
Docbank stores and what is left for you to decide. The processing steps apply
only if you set up processing.

1. [Import the file](#ingest)
2. [Move, rename, or replace it](#identify)
3. [Review what processing will send](#authorize)
4. [Extract the text](#render)
5. [Add search by meaning](#embed)
6. [Find the document again](#retrieve)
7. [Export the versions you reviewed](#export)
8. [Processing outputs stay separate](#replace)
9. [Use any interface](#serve)
10. [Back up, then test a restore](#prove)

<a id="ingest"></a>

## Import the file

Docbank copies the file into the vault and leaves the source where it was. The
copy appears in a folder tree you can browse. Docbank records a checksum at
import, so it can tell later whether the stored bytes have changed.

![The web app showing a folder of imported documents in a synthetic vault](https://docbank.ai/assets/generated/web-vault-browser.png)

A vault of synthetic documents after import.

[Importing documents](/docs/usage/importing/)

<a id="identify"></a>

## Move, rename, or replace it

Every document has a node ID that stays the same for its whole life. Moving or
renaming the document changes its path and nothing else. Replacing its content
saves a new version, and the earlier versions you keep can still be opened or
downloaded.

![The version history of one document, with a download button for the selected version](https://docbank.ai/assets/generated/web-retained-version-download.png)

Each saved version of a document can be downloaded.

[Editing and versions](/docs/architecture/editing-and-versions/)

<a id="authorize"></a>

## Review what processing will send

Processing is set up as profiles. Choose one, and Docbank shows its plan for
the selected version: each provider, where it runs, and the kind of content it
will receive. Give consent once you have read the plan. A vault restored from a
backup needs new consent before any network processing.

![A processing plan listing the providers and the content each one will receive](https://docbank.ai/assets/generated/web-document-processing-plan.png)

The plan for one version, shown before anything is sent.

[Processing and consent](/docs/usage/document-processing/)

<a id="render"></a>

## Extract the text

Processing produces a rendition: the document’s text as Markdown, saved against
the version it came from. Start it from the web app, TUI, CLI, HTTP API, or Go
API. The original file is not changed. With a supported provider, optical
character recognition (OCR) reads text from scans and images.

![The extracted Markdown text of a saved document, shown in the web app](https://docbank.ai/assets/generated/web-document-rendition.png)

The extracted text of one saved version.

[Document processing](/docs/usage/document-processing/)

<a id="embed"></a>

## Add search by meaning

An embedding is a list of numbers that stands for a piece of content, so
similar content can be found by comparing numbers. Configure an embedding
service, review what it will receive, and process the versions you want to
search. The web app and TUI then offer Semantic and Hybrid search. Finding
similar documents compares embeddings already in the vault and calls no
provider.

![A list of documents ranked by similarity to the selected file](https://docbank.ai/assets/generated/web-similar-1440.png)

Similar documents, ranked from stored embeddings.

[Search by meaning](/docs/usage/search/)

<a id="retrieve"></a>

## Find the document again

Search file names and extracted text, then narrow the results by tag, folder,
file type, or date. Save the queries and highlight terms you use often. A
lexical (keyword) query can also be frozen: its rows, counts, and facets stay
as they were when it ran, whatever changes in the vault afterwards.

![A frozen query in the web app, with its result rows and facet counts](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

A frozen query keeps the versions it found.

[Searching](/docs/usage/searching/)

<a id="export"></a>

## Export the versions you reviewed

Select documents, or take a whole frozen query. Docbank lists the originals and
processing outputs the export will include, then builds a ZIP and verifies it.
If a document changes after you start, the export still holds the version you
selected. For search counts over a date range, download the CSV report with its
evidence ZIP.

![The export drawer reporting a verified ZIP that is ready to download](https://docbank.ai/assets/generated/web-export-ready.png)

A finished export, verified and ready to download.

[Verified export bundles](/docs/usage/export-bundles/)

<a id="replace"></a>

## Processing outputs stay separate

Text, previews, embeddings, and indexes are derivatives: outputs computed from
a saved document. Docbank stores them apart from the original and records which
version each one came from. Processing and retention rules decide when a
derivative can be replaced or removed.

![A cycle of consent, processing, search, and removal around an original that does not change](https://docbank.ai/assets/derivative-cycle.svg)

[Processing and retention](/docs/architecture/overview/)

<a id="serve"></a>

## Use any interface

The command line, web app, terminal browser, and HTTP API all go through the
local daemon. A local agent connects with `docbank mcp` and works with the same
versions and revision checks. A Go application can instead own a separate vault
in its own process.

![The CLI, web app, TUI, HTTP API, Go API, and agents all reading the same documents and versions](https://docbank.ai/assets/interface-map.svg)

[Docbank for agents](/docs/agents/)

<a id="prove"></a>

## Back up, then test a restore

Create an incremental backup and verify it. Then restore it into a separate
vault and check the result before you need it. A backup holds document content,
metadata, and version history.

![Three steps: take a snapshot, verify it, and restore it into a separate vault](https://docbank.ai/assets/recovery-flow.svg)

[Backup and restore](/docs/usage/backup/)

## Next steps

Run the [quickstart](/docs/quickstart/) to try these steps on your own files.
The [task guides](/docs/) cover commands, automation, storage, and recovery.

[Meet the contributors](https://github.com/kenn-io/docbank#contributors).
