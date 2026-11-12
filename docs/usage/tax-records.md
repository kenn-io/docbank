---
title: Tax accounts and records
description: How Docbank preserves tax accounts and their initial records.
---

# Tax accounts and records

Docbank preserves tax accounts and initial tax records alongside
[parties](contracts.md). An account identifies a filer, an optional authority,
a jurisdiction, and an owner-chosen tax kind. Manage accounts and initial records
through the CLI or HTTP API. Tax kinds accept up to 256 UTF-8 bytes before
trimming and NFC normalization, and up to 64 Unicode characters afterward.
Identifiers are limited to 256 UTF-8 bytes; each account note is limited to
8,192 UTF-8 bytes. Tax operations are not yet exposed through MCP. The web
application can read records; see
[Read records in the web app](#read-records-in-the-web-app).

## Manage an account

Create an own filer with the canonical party command, then use its ID:

```sh
docbank party create "Example Holdings Ltd." --own --json
docbank tax account add --filer FILER_ID --country ZZ --tax-kind Example --cadence annual --currency USD --json
docbank tax account list --filer FILER_ID --country ZZ --taxkind Example
docbank tax account show ACCOUNT_ID
docbank tax account edit ACCOUNT_ID --lead-days 0 --clear-effective-start
docbank tax account retire ACCOUNT_ID
```

Lists, edits, retirement and ordinary inspection mask identifiers. Only
`tax account show ACCOUNT_ID --reveal` prints the complete identifier. Empty
identifiers stay empty; one to four Unicode characters become `****`, and
longer identifiers show `****` followed by the last four characters. List
filters combine with AND; `--include-retired` includes retired accounts.
`--limit` defaults to 100 and accepts 1–1000; `--offset` is nonnegative.
A full text page gives the next offset when more accounts may exist.
Dates can be cleared with `--clear-effective-start` and `--clear-effective-end`.
Identity fields cannot be edited.

Edit and retire inspect the current revision once before writing unless you
supply `--revision`. A stale revision fails without retry; inspect again before
deciding what to do. If a mutation response is lost or invalid, the change
may have been applied: inspect the account list or account before trying again.
Creating again can add another registration. The CLI replaces an incompatible older daemon before
sending an account request. A vault with audited authority refuses account
mutations with `audit_mutation_unsupported` (HTTP 409).

## Manage an initial record

Attach a record to an account, or create a one-off record with its own filer and
jurisdiction:

```sh
docbank tax record add --account ACCOUNT_ID --kind filing --title "Example return" --direction payable --amount 1234.00 --json
docbank tax record add --filer FILER_ID --country ZZ --tax-kind Example --kind audit --title "Example review" --json
docbank tax record list --filer FILER_ID --country ZZ --taxkind Example --year 2026 --state expected --kind filing
docbank tax record show RECORD_ID
docbank tax record edit RECORD_ID --note "Example note" --due-date 2027-01-31
docbank tax record retire RECORD_ID
```

When setting `--amount`, the direction must be `payable`, `refundable`, or `nil`. On create, set `--direction` explicitly; on edit, the existing direction may already be suitable.

`list` combines filters with AND and supports `--include-retired`, `--limit`
and `--offset` as for accounts. A full text page prints a continuation offset.
Retired records remain available with `show` and cannot be edited. Account-bound
records retain their copied filer, authority, country, subdivision and tax kind;
their account and kind cannot be changed. One-off records permit identity edits.
Omitted edit fields retain their values; `--clear-authority`, `--clear-tax-year`,
`--clear-period-start`, `--clear-period-end`, `--clear-due-date`,
`--clear-lead-days`, and `--clear-amount` explicitly clear nullable values.
On create, an omitted or null `tax_year` defaults to the year of `period_end`
when provided. A period edit does not recalculate an existing tax year.

Amounts use exact decimal text in the selected currency, never floating point.
Record kinds are `filing`, `payment`, `penalty`, `notice`, `other`, `audit`,
and `review`. The direction values are `payable`, `refundable`, `nil`, and
`unknown`. The `--state` filter accepts `expected`, `gathering`, `ready`,
`filed`, `assessed`, `settled`, `amended`, `disputed`, `not_required`,
`opened`, `information_requested`, `responding`, `decision_issued`, `appealed`,
and `closed`. Lists use stable record ID order; the later deadline read will
order by date. Text and JSON outputs show `amount_minor` in the currency's
minor units: for USD, `1234.00` is `123400` minor units.

`payable` and `refundable` require an amount; `nil` requires zero; `unknown`
requires no amount. When changing the currency of a non-null amount, resend the
amount in the new currency or clear it and choose a compatible direction. Even
zero requires a resend or clear; changing only the currency's letter case does
not. Docbank never converts or rounds an amount. Records begin in their kind's
initial state and are unverified; this interface does not set those fields.

Edit and retire inspect once for the current revision unless `--revision` is
supplied, and never retry a stale write. If a response is lost or invalid, inspect
the record before retrying: the change may have been applied. The CLI replaces
an incompatible account-only daemon before record requests. Audited vaults
refuse record mutations with HTTP 409 `audit_mutation_unsupported`.

An account's identity cannot change after creation. A record attached to it
copies that identity and retains it even if the account is retired. New
attachments require a live own filer and, when present, a live organization as
the authority. A previously attached party may later change or retire without
invalidating historical records.

Accounts default to a 14-day lead time. An unset record lead time is reserved
for inheritance from its account's current value, or 14 days without an
account. Its currency snapshots the account default when created and can be
overridden. A tax year defaults to the year of the period end. Amounts are exact
nonnegative minor units; direction records whether an amount is payable or refundable.
Currencies missing from the pinned currency catalog are rejected.

Records start in their kind's initial state and unverified. Event transitions,
payments, document evidence, deadlines, and verification operations are not
available through these store operations. Mutations are refused when the vault
has audited authority. Revisions reject stale updates. Retired accounts and records are read-only
and cannot be unretired; create a new row when needed.

Metadata backups preserve accounts and records, including retired entries and
historical party references. Restore validates their fields and relationships
and installs the snapshot atomically. A snapshot from before these catalogs
restores them empty. See [backup and restore](backup.md) for vault recovery.

## Read records in the web app

Choose **Taxes** in the sidebar of `docbank web` to open the page at `/taxes`.
The page only reads; add or change records with the CLI.

- **Filer** lists **All filers**, your own parties, and any other filer of a
  listed record, such as a party that is no longer marked as your own. Retired
  parties are marked "· retired". If party names cannot be read, filers show as
  "Filer" and the start of their ID, with **Try again**.
- With all filers, the loaded records appear under one heading per filer. The
  page keeps the server's stable record order (not date order), and records
  from **Load more** join their filer's heading. Choosing a filer shows only its
  records.
- **Tax year** accepts a year from 1 to 9999; press Enter or leave the field to
  apply it. **All years** and **Clear filters** remove filters.
- Each row shows the title and form code, tax year, due date, state, and the
  amount. **Load more** fetches the next 50 records. Retired records are not
  shown; list them with `docbank tax record list --include-retired`.

The amount column shows exact minor units with the currency code and the
direction, such as `123400 USD` and **Payable**, without converting or rounding.
An amount above 9,007,199,254,740,991 minor units shows **Too large to show**;
run `docbank tax record show RECORD_ID` to see it exactly. The page never shows
account identifiers or event references, and records no usage screen while open.
