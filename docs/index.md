---
title: Docbank documentation
description: Install Docbank, then find the guide for importing, searching, exporting, automating, or backing up a vault.
---

# Docbank documentation

Docbank keeps documents in a vault on your own machine. The vault holds the
stored files, every saved version, and the catalog that records where each
document lives.

If you are new, start with [setup](setup.md) and then the
[ten-minute quickstart](quickstart.md). For an overview of the product, see
[docbank.ai](https://docbank.ai) or the [visual tour](tour.md).

## What do you want to do?

| Task | Start here |
| --- | --- |
| Install Docbank or build it from source | [Setup](setup.md) |
| Import a folder of documents | [Importing documents](usage/importing.md) |
| Archive a folder from another machine | [Push a folder](usage/pushing.md) |
| Move, rename, or tag documents | [Organizing and tagging](usage/organizing.md) |
| Find documents by name, text, or filters | [Searching](usage/searching.md) |
| Search by meaning | [Processing search](usage/search.md) |
| Extract a document's text and read it | [Document processing](usage/document-processing.md) |
| Read email and follow attachments | [Web email reader](usage/web.md#read-archived-email) |
| Browse photos and organize albums | [Photos](usage/photos.md) |
| Export documents as a verified ZIP | [Export bundles](usage/export-bundles.md) |
| Import review packages or stamp PDF pages | [Web package import](usage/web.md#import-load-files) · [Bates export](usage/web.md#stamp-selected-pages-with-bates-labels) |
| Export search counts for a date range, with evidence | [Search exports](usage/search-exports.md) |
| Work in a browser or terminal | [Web application](usage/web.md) · [Terminal browser](usage/tui.md) |
| Restore a deleted document or reclaim space | [Trash, garbage collection, and repack](usage/trash-and-gc.md) |
| Create and test a backup | [Backup and restore](usage/backup.md) |
| Keep a permanent record of changes | [Audited history](usage/audited-history.md) |
| Add storage or move stored content | [Multi-store storage](usage/storage.md) |
| Maintain or upgrade a vault | [Vault lifecycle](usage/lifecycle.md) |
| Diagnose a failure | [Troubleshooting](troubleshooting.md) |

## Build an integration

- [Docbank for agents](agents.md) helps you choose an interface.
- [MCP setup](usage/mcp.md) connects a local agent to the daemon.
- [Agent integration](agents/integration.md) covers authentication, verified
  transfers, and conflicting edits.
- [Embed in Go](embedding.md) shows how an application can own its own vault.
- [Document understanding in Go](document-understanding.md) covers the packages
  for text preparation, optical character recognition (OCR), and embeddings.
- [Document processing](architecture/document-processing.md) explains how the
  vault stores processing results and which workers the daemon runs.

## Look up a command, endpoint, or setting

Use the [CLI reference](cli-reference.md) for commands and flags, the
[HTTP API reference](architecture/http-api.md) for requests and errors, and
[configuration](configuration.md) for settings.

[How Docbank works](architecture/overview.md) introduces the storage model and
links to the architecture pages that define it.

## Check capabilities and releases

The [capability guide](capabilities.md) lists what Docbank does today. The
[roadmap](roadmap.md) lists what is planned. The [changelog](changelog.md)
records each published release.

Docbank is alpha software. Keep your own copies of anything irreplaceable, and
verify a backup before you rely on it.

Docbank is licensed under the [Apache License, Version 2.0](license.md).
