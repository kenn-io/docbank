---
title: Visual Tour
description: See Docbank's real web and terminal interfaces running against synthetic document vaults.
---

# Visual tour

These are the actual Docbank interfaces backed by temporary synthetic vaults,
not mockups. The names, contents, hashes, identifiers, storage history, and
audit history were generated for the captures.

## Browse one shared document authority

The local web application combines a sortable document table with the selected
node's current path, stable ID, revision, immutable version, SHA-256 identity,
tags, provenance, and permanent-audit status. The browser receives scoped,
daemon-lifetime credentials rather than the daemon's master API key.

![The Docbank web application browsing a synthetic vault and showing the selected document's stable authority.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/v0.11.0/web-vault-browser.png)

## Organize independently of folders

Tags form a shared vocabulary with stable UUIDs and revision-protected
definitions. People can manage the catalog or a selected document's
assignments in the web application; agents use the same bounded daemon API.

![The Docbank web application managing a synthetic vault's stable tag catalog.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/web-tag-catalog/web-tag-catalog.png)

## Verify permanent history

An audited scope permanently retains its protected versions and supported
changes. Independent verification replays the history, re-hashes protected
content, and returns terminal scope heads that can be recorded outside the
vault for later comparison.

![The Docbank web application showing independently verified permanent audit evidence for a synthetic vault.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/web-audit-evidence/web-audit-evidence.png)

## See where physical authority lives

The document catalog remains authoritative while verified content can occupy a
fixed local primary and configured filesystem or S3-compatible secondaries.
The web view is deliberately read-only: it separates logical authority,
physical inventory, store health, sole copies, and live-document impact without
exposing deployment paths or credentials.

![The Docbank web application showing the primary and a secondary physical store for a synthetic vault.](https://raw.githubusercontent.com/kenn-io/docbank/f1475f1a97d4d2d5b2bb2625ff2f5f97150a9625/.superpowers/screenshots/web-multi-store-storage.png)

## Operate from a terminal

The daemon-backed TUI provides analytical tree and search views, complete
document identity, permanent history, recoverable trash and restore, job
status, and a read-only operational screen. Storage and backup results load
independently so one unavailable repository does not hide live-vault status.

![The Docbank TUI showing physical storage inventory and two synthetic backup recovery points.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/tui-storage-backup/tui-storage-backup.png)

Continue with the [Quickstart](quickstart.md), or choose a task from
[Capabilities](capabilities.md).
