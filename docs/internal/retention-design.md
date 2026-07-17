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

### Daemon owners

A daemon retention owner is a persisted record with:

| Field | Meaning |
|---|---|
| `owner_id` | Stable random UUIDv4 allocated by docbank. |
| `display_name` | Normalized, bounded, human-readable application name. |
| `secret_verifier` | Versioned password-KDF verifier; never the plaintext secret. |
| `created_at` | UTC creation time. |
| `last_used_at` | UTC time of the last successfully authenticated owner operation. |
| `disabled_at` | Optional UTC time after which ordinary owner authentication fails. |

The vault administrator creates an owner. The daemon returns a high-entropy
plaintext secret exactly once and persists only a versioned, salted,
memory-hard verifier. Verification is constant-time after deriving the
candidate verifier. Logs, audit details, error messages, logical exports, and
backups never contain the plaintext secret. Rotation installs a newly issued
secret and invalidates the old one atomically.

An owner-authenticated caller may acquire, enumerate, and release only that
owner's references. Authentication failure must not reveal whether an owner
ID exists, is disabled, or supplied the wrong secret. Disabling an owner stops
ordinary use but does not release its references: loss of credentials cannot
silently make content collectible.

The existing vault-wide daemon API key remains administrator authority. An
administrator may enumerate all owners and references, disable or rotate an
owner, and explicitly release an abandoned owner's references. Administrative
release produces a complete result suitable for operator review; it does not
claim that subsequent trash or GC has run.

### Embedded owners

Whoever opens an embedded vault already controls its files and holds the
exclusive hierarchy lock. Requiring a separately recoverable secret would add
stranding risk without adding an authority boundary. The embedded adapter
therefore auto-provisions a stable owner UUID for the application identity on
first use and authorizes that owner through the open vault handle. It stores no
owner secret.

The caller supplies a stable application name when selecting its embedded
owner. Reopening the same vault with that name resolves the same owner record;
opening the vault does not silently appropriate another name's references.
Administrative inspection remains possible to a process that owns the
exclusively locked embedded vault.

Owner authentication is an adapter concern. The domain operations, request
and result types, idempotency rules, and typed failures are identical in
embedded and daemon mode.

## Retention reference identity

A reference has stable caller-provided idempotency identity and names one
exact content version together with its expected hash and byte size.

| Field | Meaning |
|---|---|
| `owner_id` | Stable application-owner UUID. |
| `reference_key` | Bounded caller-chosen idempotency key, permanently unique within the owner. |
| `operation_id` | Bounded caller-chosen batch, import, or migration identifier. |
| `content_version_id` | Exact immutable docbank content-version UUID. |
| `expected_sha256` | Canonical lowercase 64-character SHA-256 of uncompressed bytes. |
| `expected_size` | Nonnegative uncompressed byte length. |
| `created_at` | UTC time at which the reference first became authoritative. |
| `metadata` | Optional bounded, non-secret application context. |
| `released_at` | Optional UTC time at which retention authority ended. |

`(owner_id, reference_key)` is the stable identity. A key is not reusable,
including after release. An identical acquisition retry returns the existing
logical record. A retry against a released record reports that terminal state
without reactivating it; a caller that intentionally begins a new retention
lifecycle allocates a new key. Reusing a key with another operation, version,
hash, or size is an idempotency conflict, not an update.

`operation_id` groups the references created by one caller-side saga and is
unique per owner as a batch identity. Reusing an operation ID requires the
same complete, order-independent set of reference keys and immutable identity
fields. A proper subset, superset, or changed member is a conflict. This makes
a lost batch response safe to retry without creating partial or duplicated
authority.

Metadata is diagnostic context only. It is bounded to a small canonical JSON
object, must not contain credentials or content, and does not participate in
content identity. The initial metadata value is immutable so a replay never
becomes an undocumented metadata update; later metadata editing, if needed,
is a separate operation with its own contract.

## Atomic operations and idempotency

Acquisition and release are batch operations. They are verified, atomic, and
safe to retry after a caller loses the response.

The backend-neutral behavioral surface is:

```text
AcquireRetentionBatch(owner, operationID, references) -> acquisition result
ListRetentionReferences(owner, operationID?, page) -> stable page
ListVersionRetention(contentVersionID, page) -> owner-safe or admin page
ReleaseRetentionBatch(owner, operationID or reference keys) -> release report
AdminReleaseOwner(ownerID) -> release report
```

### Acquisition

Acquisition validates the complete request before granting any authority. In
one transaction it:

1. authenticates and confirms the owner is active;
2. validates bounded keys, operation identity, metadata, canonical hash, and
   nonnegative size for every member;
3. resolves every exact `content_version_id`;
4. compares the version's recorded SHA-256 and size with the caller's expected
   evidence;
5. checks existing reference and operation keys for an exact replay or a
   conflict; and
6. inserts every new reference or none of them.

One missing version, identity mismatch, malformed member, unauthorized owner,
or key conflict aborts the whole batch. A response reports each requested
reference in deterministic key order and distinguishes newly acquired records
from exact existing replays. Duplicate keys inside one request are rejected
rather than silently coalesced.

### Enumeration

Ordinary listing is always scoped to the authenticated owner. Filtering by
`operation_id` cannot cross that boundary. Version-centric listing tells an
ordinary owner only about its own references; the administrator view may name
all blocking owners and references.

Pages use an opaque cursor derived from a stable unique ordering, with
`reference_key` as the final tie-breaker. Timestamps alone are never a cursor.
Released records remain enumerable so a reaper or operator can reconcile an
interrupted caller operation without treating absence as an ambiguous state.

### Release

Release selects either one complete owner-scoped operation or an explicit set
of owner-scoped reference keys. It marks active references released in one
transaction and returns every selected record in deterministic order. Already
released keys and unknown keys are successful, distinguishable no-op results;
all known active keys transition in one transaction. A request that mixes
selection modes is invalid.

Release removes only application retention authority. It does not delete the
content version, alter a tree node, empty trash, run GC, rewrite a pack, or
promise physical destruction. Administrative owner release follows the same
idempotency and reporting rules across all active references for that owner.

### Typed outcomes

Both adapters expose equivalent public error categories:

| Category | Meaning |
|---|---|
| not found | The requested exact version, reference, or owner-scoped operation does not exist. |
| identity mismatch | The expected SHA-256 or size does not match the exact version. |
| idempotency conflict | A stable key or operation ID was reused with different immutable input. |
| unauthorized owner | Owner authentication failed without disclosing which credential fact failed. |
| disabled owner | An authenticated administrative operation targeted a disabled owner, or an already authenticated session became disabled. |
| backend incompatible | The remote daemon cannot honor the required retention contract. |

Daemon RFC 7807 codes map to these public Go errors. Embedded calls return the
same errors directly. Human detail may vary, but callers can branch on the
typed category in either mode.

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
