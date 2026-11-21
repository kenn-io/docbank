---
title: Docbank documentation
description: Install Docbank, then find the guide for importing, searching, exporting, automating, or backing up a vault.
---

# Docbank documentation

Docbank stores your documents in a vault on your own machine, keeping the
files, every saved version, and the catalog that tracks where each document
lives.

If you are new to Docbank, start with [setup](setup.md) and work through the
[ten-minute quickstart](quickstart.md). For a quick overview, visit
[docbank.ai](https://docbank.ai) or browse the [visual tour](tour.md).

## Common tasks

| Task | Start here |
| --- | --- |
| Install Docbank or build it from source | [Setup](setup.md) |
| Import a folder of documents | [Importing documents](usage/importing.md) |
| Archive a folder from another machine | [Push a folder](usage/pushing.md) |
| Move, rename, or tag documents | [Organizing and tagging](usage/organizing.md) |
| Find documents by name, text, or filters | [Searching](usage/searching.md) |
| Search by meaning | [Automatic and processing search](usage/search.md) |
| Extract a document's text and read it | [Document processing](usage/document-processing.md) |
| Quote a document passage in an issue | [Cite documents in Kata](usage/kata-evidence.md) |
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

- [Docbank for agents](agents.md) helps you choose the right interface.
- [MCP setup](usage/mcp.md) connects a local agent to the daemon.
- [Agent integration](agents/integration.md) covers authentication, verified
  transfers, and handling conflicting edits.
- [Embed in Go](embedding.md) shows how your application can own a separate
  vault.
- [Document understanding in Go](document-understanding.md) covers the packages
  for text preparation, optical character recognition (OCR), and embeddings.
- [Document processing](architecture/document-processing.md) explains how the
  vault stores processing results and which workers the daemon runs.

## Look up a command, endpoint, or setting

Check the [CLI reference](cli-reference.md) for commands and flags, the
[HTTP API reference](architecture/http-api.md) for requests and errors, and
[configuration](configuration.md) for settings. To see how the storage model
fits together, read [How Docbank works](architecture/overview.md), which links
to the detailed architecture pages.

## Check capabilities and releases

See the [capability guide](capabilities.md) for what Docbank does today, the
[roadmap](roadmap.md) for planned work, and the [changelog](changelog.md) for
published releases.

Docbank is alpha software. Keep your own copies of anything irreplaceable, and
verify a backup before you rely on it.

Docbank is licensed under the [Apache License, Version 2.0](license.md).
