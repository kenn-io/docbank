# Application retention references

!!! info "Planned"

    This page defines planned application-owned retention authority. The
    current store has no retention-owner or retention-reference records.

## Purpose and non-goals

An application retention reference gives an external application durable
authority over one exact immutable content version. It prevents docbank from
removing the version's metadata or bytes while the reference is active, even
when no live or trashed virtual-tree node would otherwise keep that version
reachable.

Retention is not a second virtual tree, a path alias, a directory-history
scope, or an export lease. It does not copy bytes, change content identity, or
make application metadata authoritative. It is also not permission for a
remote application to force physical destruction: releasing the last
application reference only removes that application's authority, after which
ordinary docbank tree, trash, backup, and maintenance policy still applies.

This design is intentionally separate from the planned full-audit directory
scopes described in [Audited History](../architecture/audited-history.md).
Retention names exact versions selected by an application; it must not inherit
unrelated versions merely because they once occupied the same directory.

## Current authority and reachability

The current store has two related layers of authority:

- `internal/store/schema.sql` records stable file nodes and their immutable
  byte history in `content_versions`. Every live file head is one of those
  rows, and prior versions remain in the same table.
- `internal/store/gc.go` treats a `blobs` row as unreachable only when no
  `content_versions` row names its hash. Consequently, both live heads and
  prior versions retain their bytes today.
- `internal/store/trash.go` moves a node subtree into recoverable trash without
  deleting its versions. Trash-empty hard-deletes selected roots; foreign-key
  cascades then remove their nodes and version rows, allowing an otherwise
  unreferenced blob to become a later GC candidate.
- `internal/store/pack_catalog.go` and Kit's pack maintainer change the
  physical representation and catalog location of authorized blobs. Pack and
  repack do not define logical content reachability or change a blob's SHA-256
  identity.
- `internal/backupapp/metadata.go` exports the complete logical metadata
  authority, while the backup application captures every authoritative
  `blobs` row through the pinned snapshot. Backup reachability is deliberately
  broader than current GC reachability.
- `internal/api/server.go` and `internal/api/middleware.go` enforce one
  effective vault-wide API key, configured or generated per daemon run. That
  credential authenticates a caller to the daemon; it does not identify an
  application owner or partition one caller's durable references from
  another's.
- No current schema, embedded API, daemon route, client method, or CLI command
  records or operates on application retention owners and references.

The design below adds a third logical root without weakening the existing
ones. A version is removable only when no node/version authority, application
retention authority, pinned snapshot, or other documented lifecycle authority
requires it.

## Owner authority

Retention ownership distinguishes independent applications in daemon mode
while preserving filesystem authority for embedded vaults. Both modes expose
the same behavioral operations and typed outcomes.

## Retention reference identity

A reference has stable caller-provided idempotency identity and names one
exact content version together with its expected hash and byte size.

## Atomic operations and idempotency

Acquisition and release are batch operations. They are verified, atomic, and
safe to retry after a caller loses the response.

## Lifecycle integration

Retention is part of docbank's logical reachability model. Every operation
that can remove metadata authority or physical content must account for active
references under the same maintenance and snapshot-ordering rules as existing
tree authority.

## Verification and corruption semantics

A retained object is required content. Missing or mismatched retained bytes
are corruption, not collectible garbage or an ignorable application error.

## Logical export, backup, and restore

Retention authority and its exact-version closure are portable logical state.
Backups and restores preserve that authority without archiving plaintext
application credentials.

## Daemon API, embedded API, and CLI contract

The daemon and embedded surfaces share domain request, response, and error
semantics. Authentication differs only at the adapter boundary.

## Operator recovery

Vault administrators can inspect application retention authority and recover
from abandoned or disabled external owners without pretending that released
content has already been physically reclaimed.

## Conformance requirements

One behavioral suite exercises embedded and daemon-backed implementations,
including failure atomicity, retry convergence, lifecycle roots, and typed
error equivalence.

## Security and failure analysis

The design treats application credentials, crash ordering, interrupted
maintenance, and restore publication as authority boundaries. No failure may
silently turn required content into garbage or grant one ordinary owner
authority over another owner's references.
