---
title: Parties
description: Keep reusable entities and counterparties in Docbank.
---

# Parties

A party is a person or organization you refer to in your records. Create one
entry for each entity or counterparty, then reuse its stable ID. Mark your own
entities with `--own`. You can optionally connect a party to a person already
in Docbank; an organization does not need a person record.

```console
docbank party create "Northwind Storage Ltd" --kind organization --country CA
docbank party create "Example Holdings Inc" --kind organization --own
docbank party list --kind organization --own
docbank party show <party-id>
docbank party edit <party-id> --legal-name "Northwind Storage Limited"
docbank party retire <party-id>
```

Kinds are `person` and `organization`. Country is an uppercase ISO 3166-1
alpha-2 code. Display names contain 1–200 characters and legal names at most
300. Docbank rejects a new or renamed active party when its folded display
name matches another active party and returns the existing ID. Retired entries
remain available through `show` and `list --include-retired`, but are hidden
from the default list. Use `--clear-country` or `--clear-person` to remove an
optional field. Every command supports `--json`.

The HTTP API provides `GET` and `POST /api/v1/parties`, `GET` and `PATCH
/api/v1/parties/{party_id}`, and `POST /api/v1/parties/{party_id}/retire`.
An edit or retirement requires an `If-Match` revision obtained from the
resource's `ETag`. The CLI reads the current revision unless `--revision` is
provided. A stale revision fails without changing the party. Creation needs no
revision. Party changes are refused in vaults with active audited history.
Parties are included in metadata backup and restore.
