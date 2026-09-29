---
title: Fotobank migration inventory
description: Inspect a stopped Fotobank source before moving photo ownership into Docbank.
---

# Fotobank migration inventory

Docbank can inspect a stopped Fotobank install or a recovery archive before a
photo migration. Inventory writes two files: a report with counts, the pinned
source schema identity and rebuildable embedding generations, and a
deterministic owner-map template. It does not import files or assign Docbank
owners.

## Choose a source

For an install, supply the absolute path to Fotobank's `fotobank.sqlite` catalog and
the absolute embedded Docbank vault root:

```bash
docbank photos migrate fotobank inventory \
  --catalog-path /path/to/fotobank.sqlite \
  --vault-root /path/to/fotobank-vault \
  --output-dir /path/to/fotobank-inventory
```

For a recovery archive, supply its repository root. Docbank reads the latest
snapshot:

```bash
docbank photos migrate fotobank inventory \
  --archive-root /path/to/recovery-repository \
  --output-dir /path/to/fotobank-inventory
```

The output directory must be absolute and sit outside the source and the
Docbank vault. Docbank creates it if needed and writes `report.json` and
`owner-map.json` there. Both files are new and private; the command fails if
either already exists. The command uses the daemon, so the CLI never opens
either SQLite database.

## Admission and report

Install inventory acquires Fotobank's catalog lifetime lock and the embedded
vault hierarchy lock. It admits a clean catalog with the pinned schema
fingerprint and opens both databases through immutable connections. A 32-byte
SQLite WAL header is safe; WAL frames cause an immediate refusal. The embedded
Docbank schema must be version 16 with the expected `vault_metadata` and
`blobs` columns. A refused inventory writes no files.

Archive inventory reads only the verified `application/catalog.sqlite` extra
from the latest Kit snapshot. It records the snapshot ID and the supported
Docbank metadata format, and takes unique blob bytes from the snapshot
manifest. It does not restore the archive or claim an archive lease.

The report includes owner, asset, file, byte, album and album-membership,
share, checkout and checkout-entry, AI-result, hidden-setup, vector, and
capacity counts. Current Fotobank file bytes and unique embedded Docbank blob
bytes are separate measurements; retained older versions can make the latter
larger. Embedding generations are marked rebuildable because Docbank will
construct its own vector index after migration.

The owner-map template lists each source owner. The later migration step
will fill destination owner IDs in the map; inventory leaves them empty.
The command prints the report and both file paths as JSON.

## HTTP

The HTTP route is `POST /api/v1/migrations/fotobank/inventories`. It is
operator only; browser sessions cannot use it.
