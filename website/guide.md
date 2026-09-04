# The document authority lifecycle

One document moves through nine explicit boundaries. Exact source bytes remain
authoritative at every stop.

1. [Ingest](#ingest)
2. [Identify](#identify)
3. [Authorize](#authorize)
4. [Render](#render)
5. [Embed](#embed)
6. [Retrieve](#retrieve)
7. [Replace](#replace)
8. [Serve](#serve)
9. [Prove](#prove)

## Ingest

Docbank copies bytes into the vault without making the source path part of
document identity. The virtual tree gives operators a familiar hierarchy while
retained content stays addressable by checksum.

![Synthetic Docbank vault after document
ingestion](https://docbank.ai/assets/generated/web-vault-browser.png)

[Importing documents](/docs/usage/importing/)

## Identify

A stable node identity survives moves and renames. Replacement creates an
immutable content version, preserving checksums and history instead of silently
changing what an identifier means.

![Docbank retained version history and download
action](https://docbank.ai/assets/generated/web-retained-version-download.png)

[Editing and versions](/docs/architecture/editing-and-versions/)

## Authorize

Before processing, the operator chooses where document bytes may go and which
outputs may be retained. Authorization governs a task; it does not transfer
source authority to the processor.

![Source authority above authorized, version-bound processing
steps](https://docbank.ai/assets/authority-ledger.svg)

[Document understanding](/docs/document-understanding/)

## Render

OCR and rendition providers turn the source into normalized Markdown and text
through an enforceable contract. A rendition is a governed derivative of one
exact version; it can improve retrieval or be regenerated without becoming the
original record.

![Derivatives retained beneath an exact authoritative source
version](https://docbank.ai/assets/authority-ledger.svg)

[Understanding outputs](/docs/document-understanding/)

## Embed

Chunks, embedding generations, and vector indexes enter the same catalog as
every other derivative: fingerprinted, bound to provider and model identity,
and rebuildable from retained evidence. The catalog records what produced
every vector.

![A source version feeding rendition, chunk, embedding, and index derivatives
behind a consent boundary](https://docbank.ai/assets/intelligence-pipeline.svg)

[Embedding plans](/docs/document-understanding/)

## Retrieve

Hybrid search fuses lexical and semantic signals, optionally expands and
reranks, and explains each ranked result. The fence holds: every answer keeps
the document, version, and evidence behind it. An index helps locate the
record; it does not define it.

![Docbank ranked search over a synthetic document
catalog](https://docbank.ai/assets/generated/web-search-results.png)

[Searching](/docs/usage/searching/)

## Replace

Consent can be revoked and derived state is disposable. Purge renditions,
embeddings, and indexes, re-derive them through a different provider, or
restore them from backup with no provider in the loop. The originals are never
touched.

![Consent, derivation, retrieval, and purge cycling around a source that stays
exact](https://docbank.ai/assets/derivative-cycle.svg)

[Backup and restore](/docs/usage/backup/)

## Serve

The CLI, web application, TUI, HTTP API, embedded Go package, and agent tools
operate over the same vault model. Each surface inherits the same identity,
revision, and integrity boundaries.

![CLI, web, TUI, HTTP, Go, and MCP interfaces sharing one Docbank
vault](https://docbank.ai/assets/interface-map.svg)

[Docbank for agents](/docs/agents/)

## Prove

Verification checks content and history before an operator needs them.
Incremental snapshots restore exact bytes, metadata, and retained history
without relying on a processing or search provider.

![Docbank evidence view for a protected synthetic document
scope](https://docbank.ai/assets/generated/web-audit-evidence.png)

![Verified snapshot restoring a vault without a processing
provider](https://docbank.ai/assets/recovery-flow.svg)

[Backup and restore](/docs/usage/backup/)

Continue with the [quickstart](/docs/quickstart/) or open the complete [operating
documentation](/docs/).
