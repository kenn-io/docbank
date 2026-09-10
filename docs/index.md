---
title: Docbank documentation
description: Install Docbank, organize documents, automate a vault, and verify backups.
---

# Docbank documentation

Use Docbank to keep, find, update, and recover documents in a vault you control.
A vault holds your document catalog, saved versions, and stored files.

New to Docbank? Start with [setup](setup.md), then follow the
[ten-minute quickstart](quickstart.md). For a product overview, visit
[Docbank](https://docbank.ai) or see the [visual tour](tour.md).

## What do you want to do?

| Task | Start here |
| --- | --- |
| Install Docbank or build it from source | [Setup](setup.md) |
| Import a folder of documents | [Importing documents](usage/importing.md) |
| Move, rename, or tag documents | [Organizing and tagging](usage/organizing.md) |
| Find documents by name, text, or filters | [Searching](usage/searching.md) |
| Work in a browser or terminal | [Web application](usage/web.md) · [Terminal browser](usage/tui.md) |
| Restore a deleted document or reclaim space | [Trash, garbage collection, and repack](usage/trash-and-gc.md) |
| Create and test a backup | [Backup and restore](usage/backup.md) |
| Keep a permanent record of changes | [Audited history](usage/audited-history.md) |
| Add storage or move stored content | [Multi-store storage](usage/storage.md) |
| Maintain or upgrade a vault | [Vault lifecycle](usage/lifecycle.md) |
| Diagnose a failure | [Troubleshooting](troubleshooting.md) |

## Build an integration

- [Docbank for agents](agents.md) explains which interface to use.
- [Agent integration](agents/integration.md) walks through authentication,
  verified transfers, and conflicting edits.
- [Embed in Go](embedding.md) explains how an application can own its own vault.
- [Document understanding in Go](document-understanding.md) covers text
  preparation, optical character recognition (OCR), and embedding packages.

## Look up a contract

Use the [CLI reference](cli-reference.md) for commands and flags, the
[HTTP API reference](architecture/http-api.md) for requests and errors, and
[configuration](configuration.md) for settings.

[How Docbank works](architecture/overview.md) introduces the storage model and
links to the architecture references that define it.

## Check capabilities and releases

The [capability guide](capabilities.md) describes what Docbank provides.
The [roadmap](roadmap.md) separates current capabilities from planned work.
The [changelog](changelog.md) records published releases.

Docbank is alpha software. Keep independent copies of irreplaceable material
and verify backups before relying on them.

Docbank is licensed under the [Apache License, Version 2.0](license.md).
