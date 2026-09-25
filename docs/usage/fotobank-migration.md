---
title: Fotobank migration inventory
description: Inspect a stopped Fotobank source before moving photo ownership into Docbank.
---

# Fotobank migration inventory

Docbank can inspect a stopped Fotobank install or a recovery archive before a
photo migration. Inventory records counts, the pinned source schema identity,
rebuildable embedding generations, and a deterministic owner-map template. It
does not import files or assign Docbank owners.

## Choose a source

For an install, supply the absolute path to Fotobank's `fotobank.sqlite` catalog and
the absolute embedded Docbank vault root:

```bash
docbank photos migrate fotobank inventory \
  --catalog-path /path/to/fotobank.sqlite \
  --vault-root /path/to/fotobank-vault \
  --owner-map-path /path/to/fotobank-owner-map.json
```

For a recovery archive, supply its repository root. The latest snapshot is
used unless an exact snapshot ID is supplied:

```bash
docbank photos migrate fotobank inventory \
  --archive-root /path/to/recovery-repository \
  --snapshot-id 20260925T120000Z-0123456789abcdef0123456789abcdef \
  --owner-map-path /path/to/fotobank-owner-map.json
```

The owner-map path must be absolute and new. Docbank creates it exclusively
with private permissions. Keep it outside the source and the Docbank
destination. The command uses the daemon, so the CLI never opens either
SQLite database.

## Admission and report

Install inventory acquires Fotobank's catalog lifetime lock and the embedded
vault hierarchy lock. It admits a clean catalog with the pinned schema
fingerprint and opens both databases through immutable connections. A 32-byte
SQLite WAL header is safe; WAL frames cause an immediate refusal. The embedded
Docbank schema must be version 16 with the expected `vault_metadata` and
`blobs` columns.

Archive inventory reads only the verified `application/catalog.sqlite` extra
from the selected Kit snapshot. It records the snapshot ID and the supported
Docbank metadata format. It does not restore the archive or claim an archive
lease.

The JSON response contains the run UUID, source identity, report, owner-map
template, and output path. The report includes owner, asset, file, byte,
album and album-membership, share, checkout and checkout-entry, AI-result,
hidden-setup, vector, and capacity counts. Current Fotobank file bytes and
unique embedded Docbank blob bytes are separate measurements; retained older
versions can make the latter larger. Embedding generations are marked
rebuildable because Docbank will construct its own vector index after migration.

## Review saved runs

```bash
docbank photos migrate runs list --limit 20
docbank photos migrate runs show <run-id>
```

Runs are immutable and contain canonical report and owner-map JSON. Absolute
source and output paths are kept out of durable run history. The later
migration step will fill destination owner IDs in the map; inventory leaves
that map empty.

## MCP and HTTP

The HTTP routes are `POST /api/v1/migrations/fotobank/inventories`,
`GET /api/v1/migrations/runs`, and
`GET /api/v1/migrations/runs/{run_id}`. Browser sessions cannot use them.

MCP exposes `list_migration_runs` and `show_migration_run` as read tools.
`inventory_fotobank` requires the separate process flag
`docbank mcp --allow-migration-writes`. A failed transport call is not
replayed; inspect the saved run before attempting another inventory.
