---
title: Tax record storage
description: How Docbank preserves tax accounts and their initial records.
---

# Tax record storage

Docbank's store can preserve tax accounts and initial tax records alongside
[parties](contracts.md). An account identifies a filer, an optional authority,
a jurisdiction, and an owner-chosen tax kind. A record describes one matter
and may belong to an account or stand alone. These store operations are not yet
exposed through the CLI, HTTP API, MCP, or web application.

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
