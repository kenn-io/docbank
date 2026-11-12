---
title: Contracts
description: Register agreements, dates, terms, and reusable parties.
---

# Contracts

## Parties

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

`party list` shows up to 100 parties by default. When a page is full, it prints
the `--offset` to use for the next page; that page may turn out to be empty.
`--json` output stays plain JSON.

Kinds are `person` and `organization`. Country is an uppercase ISO 3166-1
alpha-2 code. Display names contain 1–200 characters and legal names at most
300. Both reject control characters. Docbank rejects a new or renamed active
party when its folded display name matches another active party and returns the
existing ID. Retired entries remain available through `show` and
`list --include-retired`, but are hidden from the default list. Retired parties
cannot be edited or un-retired. Use `--clear-country` or `--clear-person` to
remove an optional field. Every command supports `--json`.

The HTTP API provides `GET` and `POST /api/v1/parties`, `GET` and `PATCH
/api/v1/parties/{party_id}`, and `POST /api/v1/parties/{party_id}/retire`.
An edit or retirement requires an `If-Match` revision obtained from the
resource's `ETag`. The CLI reads the current revision unless `--revision` is
provided. A stale revision fails without changing the party. Creation needs no
revision. Party changes are refused in vaults with active audited history.
Parties are included in metadata backup and restore. The API also rejects names made only of whitespace or control characters, and country codes must be assigned ISO 3166-1 alpha-2 codes. New person links must refer to an active person.

Duplicate detection compares display names exactly after Unicode normalization,
case folding, and trimming and collapsing whitespace. For example,
`Northwind  Ltd` matches `Northwind Ltd`. Punctuation and legal suffix variants
are not treated as duplicates. Linking a person requires an active person; an
existing link remains readable and editable after that person retires. Merging
people moves their linked parties to the survivor and advances each party
revision.

A person split does not move party links; parties moved by a merge remain linked
to the survivor.

## Contract records

Register an agreement with its dates, terms, and parties. Create the parties
first, then refer to them by ID or active display name.

```console
docbank contract create "Storage agreement" --status active --own "Example Holdings Inc" --counterparty "Northwind Storage Ltd" --expires 2027-12-31
docbank contract list --status active --expiring-before 2028-01-01
docbank contract show <contract-id>
docbank contract edit <contract-id> --summary "Annual storage" --clear-expires
docbank contract delete <contract-id>
```

Stored statuses are `draft`, `pending_signature`, `active`, `terminated`, and
`cancelled`. An active agreement reads as `expired` after its expiry date; on
the expiry date it remains active. `--as-of` chooses the date used for that
calculation, and `--expiring-before` excludes the named date for every status. Combine it with `--status active` for active renewals. Title search
with `--q` is a literal, ASCII case-insensitive substring. A contract type is
an optional lowercase label containing letters, digits, and underscores.
Each short term is limited to 8,192 bytes.

An own-party link requires an own entity. Retired parties remain on existing
contracts but cannot be newly attached. Edit party links with `--own`,
`--counterparty`, `--remove-own`, and `--remove-counterparty`. Names for removals match linked parties in the requested role, including retired parties; use an ID when a name is ambiguous. Each change
uses the contract revision. If a later change fails, the command reports the
steps already applied. Deleting a contract removes its party links, not
stored documents. Contract mutations are refused in audited vaults. Metadata
backups include contracts and their links.

The HTTP API exposes `/api/v1/contracts` and individual contracts at
`/api/v1/contracts/{contract_id}`. Mutations of existing records use the
revision in `If-Match`; a stale revision fails without changing that operation.

Contract party links use `PUT` and `DELETE` on
`/api/v1/contracts/{contract_id}/parties/{party_id}?role=own|counterparty`.
A repeated PUT is a revision-fenced no-op; deleting a missing link returns
404. An unknown party or invalid role returns `invalid_contract`, a non-own
party in the own role returns `party_not_own`, and a retired party returns
`party_retired`. List filters include type, party ID and exclusive expiry;
`limit` and `offset` select pages, with a short page indicating the end.
