# Keep a document from import to recovery

Import a document, save changes, and recover it later. These steps explain what
Docbank keeps and which choices belong to you. Processing steps apply only when
you configure or build that workflow.

1. [Import the file](#ingest)
2. [Keep its identity through changes](#identify)
3. [Choose what may be processed](#authorize)
4. [Turn a document into usable text](#render)
5. [Prepare content for search by meaning](#embed)
6. [Find the document again](#retrieve)
7. [Export the versions you reviewed](#export)
8. [Keep processing outputs separate](#replace)
9. [Work through your preferred interface](#serve)
10. [Verify a recovery copy](#prove)

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

Choose a configured profile and review its plan for the selected version. The plan names each provider, destination, and kind of content it receives. Grant consent only after reviewing those disclosures. A restored vault needs fresh consent before network processing.

![The processing plan for one saved version](https://docbank.ai/assets/generated/web-document-processing-plan.png)

[Processing and consent](/docs/usage/document-processing/)

<a id="render"></a>

## Turn a document into usable text

A rendition is the extracted Markdown retained for one saved source version. Run configured processing from the web app, TUI, CLI, HTTP API, or Go API, then read that text without replacing the original. Optical character recognition (OCR) can extract text from images or scans when a supported provider is configured.

![Verified retained Markdown from a saved document](https://docbank.ai/assets/generated/web-document-rendition.png)

[Document processing](/docs/usage/document-processing/)

<a id="embed"></a>

## Prepare content for search by meaning

An embedding represents content as numbers for comparison. Configure an embedding service, review its disclosure, and process the versions you want to search. The web app and TUI offer Semantic and Hybrid search. Finding similar documents uses stored embeddings locally, with no provider call.

![Similar documents found from stored embeddings](https://docbank.ai/assets/generated/web-similar-1440.png)

[Processing search](/docs/usage/search/)

<a id="retrieve"></a>

## Find the document again

Search document names and extracted text, then narrow results with tags, folders, file types, or dates. Save useful queries and highlight sets. Run a lexical query to browse a frozen result set whose rows, counts, and facets stay fixed while documents change.

![A frozen query with exact versions and fixed facets](https://docbank.ai/assets/generated/web-snapshot-workspace.png)

[Searching](/docs/usage/searching/)

<a id="export"></a>

## Export the versions you reviewed

Select documents or use the whole frozen query. Preview which originals and
retained outputs will be included, then build a verified ZIP. Later changes
do not substitute newer files. For dated search counts, download a CSV with
its frozen evidence ZIP.

![A verified export ready to download](https://docbank.ai/assets/generated/web-export-ready.png)

[Verified export bundles](/docs/usage/export-bundles/)

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
daemon. Local agents can connect with `docbank mcp` and use the same exact
versions and revision checks. A Go
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

[Meet the contributors](https://github.com/kenn-io/docbank#contributors).
