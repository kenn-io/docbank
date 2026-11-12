---
title: Tax accounts and records
description: How Docbank preserves tax accounts and their initial records.
---

# Tax accounts and records

Docbank preserves tax accounts and initial tax records alongside
[parties](contracts.md). An account identifies a filer, an optional authority,
a jurisdiction, and an owner-chosen tax kind. Manage accounts through the CLI
or HTTP API. Tax kinds accept up to 256 UTF-8 bytes before trimming and NFC
normalization, and up to 64 Unicode characters afterward. Identifiers are
limited to 256 UTF-8 bytes; each account note is limited to 8,192 UTF-8 bytes. Record operations are not yet exposed through those interfaces,
MCP, or the web application.

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
