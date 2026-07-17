# Application retention references

!!! info "Planned"

    This page defines planned application-owned retention authority. The
    current store has no retention-owner or retention-reference records.

## Purpose and non-goals

An application retention reference gives a calling application durable
authority over one exact immutable content version. It prevents docbank from
removing the version's metadata or bytes while the reference is active, even
when trash-empty would otherwise remove the trashed subtree that contains that
version.

Retention is not a second virtual tree, a path alias, a directory-history
scope, or an export lease. It does not copy bytes, change content identity, or
make application metadata authoritative. It is also not permission for a
remote application to force physical destruction: releasing the last
application reference only removes that application's authority, after which
ordinary docbank tree, trash, backup, and maintenance policy still applies.
The design does not introduce detached version tombstones; protected versions
remain attached to their node, and trash-empty refuses to remove that subtree.

This design is intentionally separate from the planned full-audit directory
scopes described in [Audited History](../architecture/audited-history.md).
Retention names exact versions selected by an application; it must not inherit
unrelated versions merely because they once occupied the same directory.

## Current authority and reachability

The current store has interlocking layers of authority:

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

A persisted retention owner has:

| Field | Meaning |
|---|---|
| `owner_id` | Stable random UUIDv4 allocated by docbank. |
| `authority_kind` | `daemon` or `embedded`; determines whether a secret is required. |
| `display_name` | Normalized, bounded, human-readable application name. |
| `secret_verifier` | Versioned password-KDF verifier for daemon owners; absent for embedded owners. |
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
| `released_by` | Optional `owner` or `administrator` actor kind; never a credential. |

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

An active reference protects the exact `content_versions` row it names. That
version continues to protect its authoritative `blobs` row; the blob row
continues to control loose or packed read authority. Retention does not bypass
these layers or introduce a second physical catalog.

| Operation | Required retention behavior |
|---|---|
| Metadata or version pruning | Refuse removal of a retained exact version and identify every blocking owner and reference. |
| Trash | A node may move into recoverable trash; its active references and exact versions remain unchanged. |
| Trash empty | Preflight every selected subtree. If any exact version is retained, make no deletions and report every blocking root, owner, operation, reference, and version. |
| GC | Exclude active references and their exact-version blobs from candidates; retained trash remains ordinary version authority rather than a detached tombstone. |
| Pack | A retained blob may move from loose storage into a verified immutable pack without changing version or reference identity. |
| Repack | Copy every retained live member and commit its replacement mapping before retiring source-pack authority. |
| Verify | Report a missing or corrupt retained version, blob row, loose object, or packed member as integrity failure. |
| Logical export | Include retention owners, reference records, and the exact-version closure needed to validate them. |
| Backup | Include retention authority and every required object in the pinned snapshot. |
| Restore | Verify and publish required objects before restored retention authority becomes visible. |

Trash-empty's all-or-nothing preflight is deliberate. Skipping protected roots
while deleting others would make a maintenance result depend on a caller
noticing a secondary blocker list. The dry run and execution return the same
complete blocker model; execution proceeds only when the selected set has no
active reference.

Retention acquisition and release are ordinary metadata mutations. Operations
that can remove logical or physical authority—trash-empty, GC, and pack
retirement—remain maintenance operations. They serialize through the existing
daemon gate and the equivalent embedded mutation coordinator. Backup capture
keeps the preservation side for its complete content stream, so maintenance
cannot invalidate the pinned metadata snapshot after ordinary writes resume.

## Verification and corruption semantics

A retained object is required content. Missing or mismatched retained bytes
are corruption, not collectible garbage or an ignorable application error.

Verification evaluates each active reference from owner through physical
content:

1. the owner and reference records are structurally valid;
2. the exact content version exists;
3. its recorded hash and size equal the reference's expected evidence;
4. the corresponding authoritative blob row exists with the same size;
5. the catalog resolves an authorized loose or packed representation; and
6. a complete verified read yields the expected size and SHA-256.

Vault-wide verification reports all retained-reference failures alongside
ordinary metadata and physical-content findings. It never downgrades a missing
retained object to an unreachable row, dead pack member, or removable orphan.
An inactive released reference is historical reconciliation state and is not a
reachability root, but its structural fields must remain valid.

GC and pack maintenance consume a complete reference inventory. If malformed
retention metadata prevents the store from proving that inventory complete,
destructive maintenance stops rather than assuming the malformed record is
inactive.

## Logical export, backup, and restore

Retention authority and its exact-version closure are portable logical state.
Backups and restores preserve that authority without archiving plaintext
application credentials.

### Deterministic logical authority

The current metadata JSONL format gains deterministic records for retention
owners and references. Owner records sort by owner UUID. Reference records sort
by owner UUID and reference key. The header and relational validation bind
their counts into the same format-v1 authority as nodes, versions, and blobs.

Logical records preserve stable owner IDs, authority kind, display names,
creation/last-use/disabled state, reference and operation keys, exact version
IDs, expected hash and size, metadata, active/released state, and release actor
kind. They do not contain plaintext owner secrets or password-KDF verifiers.
Those values are credential material rather than portable content authority.

Every imported active reference must resolve to an imported exact version
whose hash and size match the reference and an imported authoritative blob row
of the same size. Released references must still resolve to their historical
owner and immutable identity fields. Import validates the complete relational
closure transactionally before replacing a pristine target's authority.

### Backup capture

A pinned backup snapshot includes owner/reference records and every blob row
required by its exact versions. Existing backup capture already includes all
authoritative blob rows, which is broader than GC reachability; retention makes
the reason for preserving the selected versions explicit in logical metadata.
The preservation gate remains held until every required loose or packed object
has been copied and verified.

The manifest fidelity proof gains an owner count plus active and released
reference counts. Repository verification follows active references through
their versions and content and reports every missing or mismatched retained
object. A backup is not valid merely because its JSONL parses while retained
bytes are absent.

### Restore publication and credentials

Restore preserves stable owner and reference identities so application retry
and release keys continue to converge. It stages metadata and content, checks
the complete active-reference closure, and reports all missing or mismatched
retained objects together before publication. Verified loose or packed bytes
are published before the restored database grants retention authority; a
failed restore leaves the live target absent or unchanged.

Embedded owners need no credential recovery: exclusive filesystem access to
the restored vault remains their authority. Restored daemon owners start with
their references intact but ordinary owner authentication disabled until the
administrator rotates or re-provisions a secret. Restore documentation and
the terminal report list that requirement. The restore never invents a secret,
copies a source verifier, or releases content because credentials are absent.

## Daemon API, embedded API, and CLI contract

The daemon and embedded surfaces share domain request, response, and error
semantics. Authentication differs only at the adapter boundary.

### Domain surface

Public Go types represent owners, exact identities, acquisition members and
results, stable pages, release reports, blockers, and typed failures. One
backend-neutral retention interface is implemented by the embedded vault
adapter and the public daemon client. Consumers do not need mode-specific
retention logic.

Embedded calls select their auto-provisioned owner through the vault handle.
Daemon data calls continue to require the effective vault API key. Ordinary
retention calls additionally send the owner UUID and owner secret in dedicated
authorization headers, never in a URL, query string, request body, log field,
or RFC 7807 detail. Administrative owner creation, rotation, disabling,
inspection, and override use the vault administrator authority.

The API-key holder remains ultimate administrator under docbank's documented
local, loopback-only, single-user trust model. Owner secrets prevent accidental
or ordinary cross-application retention operations; they are not a claim that
an application given the administrator key is isolated from administrator
routes.

The Huma-described route family provides:

- administrator creation, secret rotation, disabling, and inspection of
  owners;
- owner-scoped verified batch acquisition;
- owner-scoped paginated listing, optionally filtered by operation ID;
- owner-safe and administrator views of references blocking one exact version;
- owner-scoped idempotent batch release; and
- explicit administrator release of an abandoned owner with a complete
  report.

All routes use bounded request sizes and bounded pages. Administrative release
and trash-empty blockers include stable IDs and non-secret display context,
not credentials or document bytes.

### Compatibility and errors

The authenticated vault-information response advertises a retention contract
revision and capabilities for owner management, verified batch acquisition,
enumeration, release, and blocker reporting. A public client verifies those
capabilities when connecting. If an operation indicates contract drift, it
refreshes capabilities once and then returns the same typed backend-
incompatible error rather than exposing an unexplained `404` or decoding
failure.

Stable RFC 7807 codes include `retention_identity_mismatch`,
`retention_idempotency_conflict`, `retention_owner_unauthorized`,
`retention_owner_disabled`, `retention_blocked`, and
`backend_incompatible`, alongside the existing `not_found` and validation
codes. The public client maps them to the domain errors defined above. An
owner-authenticated endpoint uses the same unauthorized response for an
unknown owner, disabled owner, or bad secret; the more specific disabled state
is visible only to an administrator or an already established local adapter.

### CLI contract

`docbank retention list` is the primary operator view. It supports owner,
operation, exact-version, active/released, and age filters and reports owner
name/ID, operation ID, reference key, version ID, expected hash and size,
creation/release time, and state. Human and JSON output never include secrets
or verifiers.

Owner create and rotate commands reveal a server-issued secret exactly once
and clearly state that it cannot be recovered. Disable and administrative
release require explicit owner identity; release prints the complete affected
reference report and does not claim bytes were reclaimed. All standalone CLI
commands remain daemon HTTP clients.

## Operator recovery

Vault administrators can inspect application retention authority and recover
from abandoned or disabled external owners without pretending that released
content has already been physically reclaimed.

- **Lost daemon-owner secret:** active references remain roots. The
  administrator rotates the secret, reconfigures the application, and may
  inspect the owner before allowing ordinary release.
- **Disabled application** — authentication stops but references remain
  active. The administrator may rotate and re-enable it or explicitly release
  it after reviewing every affected reference.
- **Application decommissioning:** the operator lists active references by
  owner and operation, disables the owner, and uses the administrator release
  operation. Owner and released-reference records remain as reconciliation
  history; hard deletion is not part of ordinary recovery.
- **Stale operation ID:** the application or administrator enumerates the
  complete stable batch. An identical acquisition or release safely resumes;
  changed membership is rejected as an idempotency conflict.
- **Restore:** daemon owners retain their identities and references but require
  explicit secret rotation before ordinary use. Embedded owners resume through
  filesystem authority and have no recoverable secret to lose.

Operator output distinguishes releasing retention authority from deleting a
node, emptying trash, reclaiming a loose object, and retiring dead packed
space. Each later action follows its existing dry-run and maintenance contract.

## Conformance requirements

One behavioral suite exercises embedded and daemon-backed implementations,
including failure atomicity, retry convergence, lifecycle roots, and typed
error equivalence.

The shared suite proves at least these semantics against both adapters:

- one invalid member makes an acquisition batch persist nothing;
- an identical acquisition retry after a simulated lost response returns the
  existing records without duplication;
- an operation-ID replay with a changed, missing, or additional member is an
  idempotency conflict;
- releasing active, already released, and unknown keys converges to the same
  terminal state;
- ordinary owners cannot enumerate or release another owner's references;
- embedded and daemon calls return the same typed category for every defined
  failure; and
- a retained exact version remains readable through trash, GC, pack, repack,
  verify, backup, and restore.

The upload-to-retention window has an explicit lifecycle scenario:

1. upload a synthetic file and record its exact version, SHA-256, and size;
2. leave it live without acquiring an application reference;
3. run GC dry-run and execution and prove the version is not a candidate;
4. pack it, exercise eligible repack maintenance, and verify an exact-version
   read;
5. trash and restore its node and prove the bytes survive;
6. acquire a retention reference and replay the identical acquisition;
7. trash the node and prove trash-empty refuses the subtree while naming the
   blocking reference; and
8. release the reference, rerun eligible destructive maintenance, and verify
   that the outcome follows the remaining node/version authority.

Steps 2–5 prove today's live-version authority protects the interval before
retention acquisition. Steps 6–8 prove the new application root and its
release boundary. The suite must exercise real loose and packed content rather
than replacing lifecycle behavior with an in-memory mock.

## Security and failure analysis

The design treats application credentials, crash ordering, interrupted
maintenance, and restore publication as authority boundaries. No failure may
silently turn required content into garbage or grant one ordinary owner
authority over another owner's references.

The required recovery behavior at each commit boundary is:

| Boundary | Recovery rule |
|---|---|
| Bytes and live version are durable; retention batch is absent | Existing live node/version authority preserves the content. Retrying acquisition verifies and records the batch. |
| Retention batch committed; caller lost the response | The identical operation and key set returns the existing acquisition result. |
| Release committed; caller lost the response | Retrying release returns active-to-released and already-released results without restoring authority. |
| Backup snapshot pinned while release races | The pinned transaction retains its captured reference view, and the preservation gate prevents maintenance from removing required content until capture ends. |
| Restore interrupted before publication | Only private staged state changes; the live target remains absent or unchanged. |
| Restored metadata would become visible before required bytes | Publication is forbidden. Aggregate verification fails while the staged target remains private. |
| Administrator releases an abandoned owner | The result names every released reference. Content remains subject to ordinary tree and maintenance authority; no GC or destruction claim is implied. |

An acquisition transaction never precedes durable version authority, so it
cannot create a reference to bytes that are still only a temporary upload.
Conversely, content upload must not delete its live operation-owned node until
the caller has either acquired retention or explicitly cancelled the
operation. The live node is the bridge across that interval.

Owner disabling, secret loss, and application decommissioning preserve active
references by default. Only an authenticated owner release or explicit
administrator release changes reachability. This fail-closed posture may leave
diagnosable retained content; it must never produce silent data loss.
