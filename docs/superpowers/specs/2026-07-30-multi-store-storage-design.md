# Multiple blob stores under one metadata catalog

**Status:** Approved design
**Date:** 2026-07-30
**Kata:** `bnxr`

## Summary

One Docbank vault may use several independently located blob stores while
retaining one authoritative local metadata catalog. The initial product
supports:

- the existing writable local filesystem store as an immutable primary;
- secondary mounted-filesystem stores;
- secondary native S3-compatible stores;
- pack-oriented secondary storage with loose-object fallback;
- explicit preview-and-run placement, evacuation, repair, and salvage;
- remote-only content whose metadata remains locally browsable while its store
  is unavailable;
- multiple independently verified locations for one deduplicated blob;
- complete backup and topology-independent restore; and
- released-schema upgrades through the existing deterministic JSONL cutover.

Placement is capacity management. It is not backup, synchronization, or an
availability guarantee.

The central boundary remains unchanged: Docbank owns logical identity,
liveness, placement policy, and catalog authority. Kit owns
application-neutral byte publication, verification, reading, packing,
repacking, retirement, and backend mechanics.

## Goals

The design must let an operator move cold content from the vault disk to a NAS
or S3-compatible service without splitting document metadata across systems.
It must preserve these properties:

- document paths, tags, provenance, versions, search, and audit history remain
  available from the local catalog;
- no physical copy becomes authoritative before destination verification;
- every authority transition leaves at least one verified location;
- an unavailable store is distinct from missing or corrupt content;
- a failed or interrupted transition leaves an unauthorized orphan rather
  than false catalog authority;
- deduplicated content moves only when every logical reference permits source
  retirement;
- a successful backup contains every live blob;
- a restore does not require recreating the source machine's store topology;
- machine paths, endpoints, and credentials never enter logical JSONL or
  portable backup metadata; and
- public vaults from v0.9.0 onward remain upgradeable.

## Out of scope

The first release does not provide:

- automatic age, access-time, tag, or capacity placement policies;
- scheduled NAS or S3 synchronization;
- desired replica counts or automatic healing;
- primary-store succession;
- shared physical pools across vaults;
- bucket lifecycle or historical-version management;
- transparent failover after a stream has delivered bytes;
- cloud-provider-native backends other than S3 compatibility; or
- browser or TUI mutation controls for storage administration.

These exclusions keep the first release centered on explicit, recoverable
physical placement.

## Authority model

### One logical vault

SQLite remains the sole document and physical-catalog authority. Secondary
stores contain immutable objects and ownership metadata, never an independent
document catalog.

The authority layers are:

1. `nodes` and `content_versions` define document identity and retained
   versions.
2. `blobs` grants representation-independent membership to one content hash.
3. the store registry defines stable physical-store identities and lifecycle;
4. physical locations bind an authorized hash to a verified representation in
   one registered store.

The physical store cannot infer whether a blob is live. Kit cannot grant or
revoke Docbank authority without a catalog transaction.

### Store identity and role

Every store has:

- a random stable store ID;
- a mutable human-readable name;
- backend kind `filesystem` or `s3`;
- role `primary` or `secondary`;
- lifecycle state `active` or `draining`;
- a binding to machine-local configuration; and
- a recorded ownership epoch.

There is exactly one primary. It is a filesystem store implicitly bound to
`<vault>/blobs`. It cannot drain, detach, or be replaced in the first release.
Moving the vault to another local disk remains a backup-and-restore operation.

Backend kind and store role are independent. “Primary filesystem” is not a
third backend kind.

### Physical locations

A blob may have verified locations in several stores. Within one store it has
at most one catalog-authorized representation at a time:

- raw loose;
- zstd loose; or
- one entry in a store-scoped immutable pack.

Pending uploads, staging files, old loose copies, dead pack ranges, and retired
packs are not authority.

Each location carries a non-reusable generation token. Replacing or
re-encoding a location, packing it, repacking it, or repairing it creates a new
generation. Process-local health observations use the complete physical
identity, including this generation, so a new authority row cannot inherit an
old demotion.

### Dynamic read preference

Preferred location is not stored per blob. Docbank orders candidates in Go
from:

1. a usable binding and accepted ownership observation;
2. configured store priority;
3. primary before secondary when priorities tie; and
4. stable store and representation identity as deterministic tie-breakers.

Availability is observed runtime state. It never rewrites durable authority
merely because a network, mount, or credential provider is temporarily
unavailable.

## Deployment bindings

Machine-local `config.toml` contains named store-binding profiles. A profile
holds:

- filesystem root; or
- S3 endpoint, region, bucket, prefix, and credential-provider reference;
- runtime read priority; and
- backend tuning that does not affect logical identity.

For example:

```toml
[store_bindings.archive_nas]
kind = "filesystem"
path = "/mnt/docbank-archive"
priority = 100

[store_bindings.archive_s3]
kind = "s3"
endpoint = "https://s3.example.invalid"
region = "us-east-1"
bucket = "documents"
prefix = "vaults/main"
credential_profile = "docbank-archive"
priority = 200
```

The catalog associates a store ID with a profile name. The profile name and
its contents are deployment state, not portable authority.

Bindings load once at daemon startup. After editing a profile, the operator
runs `docbank daemon restart`. A command against a profile unknown to the
running daemon returns a typed stale-configuration error with that instruction.
Embedded applications close and reopen their exclusively owned vault after
changing bindings.

Changing a path, endpoint, bucket, or prefix cannot silently retarget
authority. The configured namespace must present the expected ownership marker
and epoch.

Secrets enter through standard credential providers such as environment
configuration, shared profiles, workload identity, or an owner-private secret
source. They never enter SQLite, runtime records, logs, HTTP response bodies,
browser sessions, logical JSONL, placement manifests, or backups.

## Namespace ownership and fencing

### Exclusive namespace

Each filesystem root or S3 bucket prefix belongs exclusively to one vault/store
binding. A NAS or bucket may host many vaults under distinct roots or prefixes.
Two vaults do not share one physical pool.

The namespace marker contains:

- marker format version;
- vault ID;
- store ID; and
- a random ownership epoch.

The catalog records the epoch expected for that binding.

Canonical-path and filesystem-identity overlap checks catch ordinary
configuration mistakes. They are fast-fail diagnostics, not the ownership
invariant: two mount aliases may reach the same export. The marker and epoch
are authoritative.

### Registration and takeover

Binding an empty namespace creates its marker and first epoch. Reattaching an
existing namespace requires the matching marker and epoch.

Taking over a namespace is explicit. Takeover writes a new epoch to the marker
before recording it in the adopter's catalog. A crash between those steps
fences both parties and requires an explicit retry; it never leaves two valid
owners.

S3 marker changes use conditional object writes. Filesystem marker replacement
is durable and atomic but not a true cross-host compare-and-swap. Racing
filesystem takeovers are unsupported operator behavior; the last marker writer
wins and the loser fences at its next observation.

Losing an epoch fences operations against that store, not the daemon. Primary
and unaffected secondary stores continue operating.

### Fencing limits on S3

Every destructive admission performs a fresh marker check. S3 cannot make a
marker check atomic with deletion of another object. An explicit takeover can
therefore race a destructive request admitted by the previous owner after its
last marker check.

Takeover is an operator-coordinated recovery action, not a concurrent handoff
protocol. The new epoch blocks future admissions, while verification detects
damage caused by an already in-flight request.

## Registration

`docbank storage add <name> --binding <profile>` is preview-first. Preview
reports:

- generated store ID;
- backend, role, and namespace;
- capability checks;
- ownership marker and epoch state;
- obvious path or identity overlaps;
- empty registration, matching reattachment, or takeover classification; and
- consequences for an existing owner.

Execution is separate and requires explicit takeover acknowledgment when
applicable.

Registration requires an empty namespace or a valid marker. S3 registration
also requires:

- strong, repeatable read-after-write behavior;
- immutable reads;
- ranged reads;
- multipart upload;
- conditional marker replacement;
- bounded listing;
- deletion; and
- complete destination read-back.

Registration performs conformance probes and rejects endpoints known not to
meet this contract. Eventual-consistency retry windows do not weaken
publication authority.

## Placement policy

### Local-first ingestion

Every new ingest publishes to primary first. Ordinary document creation never
depends on NAS or S3 availability. Explicit placement may transfer the content
later.

Large remote-only ingestion that bypasses local staging is outside the first
release. The operator must have enough temporary primary capacity to admit new
content.

### Explicit control

Placement is initiated only by preview-and-run actions. There is no automatic
hot/cold policy.

The first selector accepts a live path or stable node ID. A selected file or
directory includes:

- every retained version of the selected node and its current live subtree;
- trashed-but-not-emptied nodes whose recorded trash-origin ancestry lies
  within that subtree; and
- every retained version belonging to those trashed nodes.

Unknown-origin trash belongs to root selection only. Docbank never guesses it
into a narrower directory.

Reference closure considers every retained content version in the vault,
including trash-held content outside the selection.

### Deduplicated reference closure

Placement acts on physical hashes, but source retirement is reference-aware.
If selected content shares a hash with an unselected live reference, Docbank
may add and verify the destination location but keeps the required source
location.

Preview reports every class that reduces reclamation. Content identity is not
duplicated merely to manufacture separate placement behavior.

### Permanent audit

Permanent audit promises retention authorization and history integrity, not
continuous availability.

The default placement policy treats audit membership as pinning content to
primary. Retiring the last primary location of an audited blob requires an
explicit remote-only acknowledgment in preview and apply.

After that acknowledgment, audited content may be remote-only. Docbank still
never authorizes its deletion through ordinary maintenance, but cannot prevent
external deletion by bucket lifecycle, storage administrators, compromised
credentials, or device failure. Complete backups are the durable recovery
backstop.

## Placement preview

A placement preview reports:

- selected nodes, versions, unique hashes, and logical bytes;
- destination representation and expected transfer bytes;
- loose bytes reclaimable immediately;
- complete-pack bytes reclaimable immediately;
- bytes made dead inside retained packs;
- bytes pending a later repack;
- shared references preventing source retirement;
- audit-protected primary pins;
- audit-protected hashes requiring remote-only acknowledgment;
- destination locations already present;
- unavailable or damaged source candidates;
- required local scratch capacity; and
- known destination capacity.

The preview token binds a canonical digest and aggregate counts for:

- store registry generation;
- source and destination;
- selected logical authority;
- deduplicated candidate set;
- complete reference closure; and
- audit acknowledgment.

The token does not enumerate millions of hashes. Apply recomputes and
revalidates the bound facts.

## Copy and authority handoff

Network or device copying does not hold the daemon's logical mutation gate for
its duration.

For each object:

1. Kit opens a catalog-authorized source and pins it through verification.
2. Kit publishes into private destination staging or an incomplete multipart
   operation.
3. Kit verifies decoded size and SHA-256 while copying.
4. The destination backend publishes the immutable object.
5. Kit reads the published destination back completely and verifies it.
6. A short Docbank catalog transaction revalidates blob membership, selection,
   reference closure, and audit policy.
7. The transaction adds destination authority and, when still permitted,
   revokes source authority. It must leave at least one verified location.
8. Kit retires revoked source bytes through reader-aware cleanup.

If references change during transfer, the destination may remain as an
additional verified copy while the source stays authoritative. The receipt
reports the reduced reclamation.

Cancellation before a catalog commit creates no new authority. Cancellation
after an individual handoff preserves that completed handoff and returns an
exact resumable receipt.

## Crash and cleanup model

Every interruption reduces to catalog truth plus unauthorized physical debris:

- before destination verification: private staging or incomplete multipart
  data;
- after verification but before catalog commit: an unauthorized destination
  orphan;
- after catalog commit but before source deletion: an unauthorized redundant
  source object;
- during cleanup: a retryable orphan-cleanup finding; and
- after catalog rollback: no destination authority and unchanged source
  authority.

Cleanup acts only within a currently owned namespace, rechecks the marker for
each destructive batch, and recognizes only Kit's canonical key grammar.
Unknown objects are reported and preserved.

Catalog authority never depends on a namespace listing being complete.
Inventory is bounded and resumable.

## Pack-granularity accounting

Location authority is per blob/store, while disk reclamation occurs at loose
object or pack-container granularity.

Moving selected entries from a mixed pack revokes only those mappings. The pack
remains while any authoritative entry still refers to it. Audit-pinned and
movable entries may coexist.

Placement does not silently repack the residual source. It reports:

- immediate loose reclamation;
- immediate complete-pack reclamation;
- newly dead packed bytes; and
- bytes recoverable through the existing repack workflow.

Whole-store evacuation can retire a pack when its final authoritative entry
moves.

Pack identity is scoped to a store: `(store ID, pack ID)`. Copying one immutable
pack to another store therefore creates another physical location with the same
pack ID but distinct store identity.

## Reader-safe retirement

Retirement always goes through Kit:

- open filesystem readers retain pinned descriptors;
- Windows sharing failures become deferred cleanup rather than catalog
  rollback;
- pack readers use lease-and-retire semantics;
- an in-flight S3 GET may finish after object deletion; and
- failed physical deletion becomes retryable cleanup.

No cleanup failure invalidates destination authority already committed.

## Read resolution

Docbank loads blob membership and all authorized locations in one metadata
snapshot. Kit receives an ordered candidate set rather than one location.

Kit tries the next candidate when an open fails before payload delivery. Once
a stream has delivered provisional bytes, a late integrity failure cannot
switch sources transparently without buffering the complete object. The caller
receives the integrity failure from byte zero.

### Runtime health

Kit keeps process-local observations keyed by:

- store;
- blob;
- representation;
- pack and offset where applicable;
- immutable backend identity; and
- location generation.

A missing or corrupt observation immediately demotes that candidate. A later
read will select the next usable candidate. Demotion clears only after:

- explicit verification succeeds;
- repair publishes and verifies a replacement; or
- catalog authority changes generation.

Transient network and credential errors affect store availability rather than
content integrity.

Restart intentionally forgets these demotions and may try a damaged location
once. Durable repair findings and verification reports carry evidence; runtime
health never becomes a second authority database.

Ordinary reads use bounded cached marker and availability observations. They do
not issue an S3 marker request for every blob. Startup, periodic health checks,
explicit refresh, and backend failures update the cache. Destructive
admission and authority handoff always perform a fresh marker check.

## Typed physical outcomes

The storage boundary exposes:

- `not_found`: no logical blob membership;
- `store_unavailable`: missing binding, network failure, unavailable
  credentials, or unreachable backend;
- `store_fenced`: ownership identity or epoch mismatch;
- `physical_missing`: an available, correctly owned store lacks an authorized
  object;
- `physical_corrupt`: framing, length, checksum, or decoded hash verification
  fails; and
- `physical_authority_missing`: logical membership has no catalog location.

If no candidate succeeds, headline precedence is:

1. `physical_corrupt`;
2. `physical_missing`;
3. `store_fenced`; and
4. `store_unavailable`.

`physical_authority_missing` is determined before candidate attempts. The
structured result retains every attempted candidate and cause regardless of
headline precedence.

An ordinary read may succeed through a lower-ranked valid copy after another
location fails. The damaged location remains a repair finding; fallback does
not certify it.

## Degraded mode and fenced recovery

The daemon starts with unavailable, misconfigured, unbound, or fenced
secondary stores. Metadata, search, browsing, and unaffected content remain
available.

`storage status` reports each store's:

- role and lifecycle;
- binding and observed state;
- fencing cause;
- last observation;
- authoritative objects and bytes;
- loose, packed, dead, and pending-repack bytes;
- redundant locations; and
- blobs with no usable candidate.

A fenced store does not count as usable destination coverage for retiring
another copy.

The old owner may:

- reclaim ownership through explicit takeover;
- recover from another verified location or verified backup;
- perform operator-acknowledged read-only salvage;
- detach the local binding while retaining inaccessible catalog evidence; or
- unregister only after no live authority depends on the store.

### Salvage

Salvage uses stale catalog coordinates but does not claim ownership. It performs
no publication, deletion, listing cleanup, or marker change against the fenced
store.

It streams expected immutable content into primary, verifies decoded size and
SHA-256, then grants new primary authority. Concurrent cleanup by the current
owner may make salvage fail, but cannot make wrong bytes authoritative.

## Verification

### Vault verification

Quick verification checks metadata relations, catalog shape, and markers but
cannot claim content integrity.

Full verification reads decoded content and records every verified,
unavailable, fenced, absent, or corrupt physical location. Reports aggregate by
store and keep bounded detailed findings.

Verification never treats an inaccessible store as proof of loss.

### Permanent-audit verification

Audit verification has three top-level outcomes:

- **Complete:** history, metadata, and every required protected location
  verifies. Fresh terminal evidence may be recorded.
- **Incomplete:** no violation is observed, but at least one protected
  location is unavailable or fenced. No fresh terminal evidence is recorded.
- **Violated:** a checked protected location is absent or corrupt, or protected
  content has no physical authority.

Violated dominates incomplete. A violated report still lists unavailable
locations.

Violations are further classified:

- repairable from live authority when another location verifies;
- repairable from backup only when a selected snapshot's actual content was
  independently verified; or
- unrecoverable within the checked recovery set.

Prior terminal evidence remains historical and is not shown as current after
an incomplete or violated attempt.

Full audit verification reads every protected catalog location and may incur
full S3 egress. Backup needs one verified candidate per live hash, not every
redundant location.

## Kit boundary

Kit gains application-neutral physical-backend and multi-location mechanics:

- filesystem and S3-compatible backends;
- marker operations;
- immutable loose and pack publication;
- ordered candidate reads;
- verified copy and repair;
- store-scoped pack, repack, and cleanup;
- reader-aware retirement;
- runtime candidate health; and
- typed physical outcomes.

Docbank retains:

- store registry and lifecycle;
- logical liveness;
- reference-aware placement;
- audit placement rules;
- preview and apply;
- backup completeness;
- restore mapping;
- API and operator policy; and
- schema authority.

Kit source, documentation, APIs, and tests remain application-neutral and do
not name Docbank.

Kit's released single-layout `Catalog`, `Layout`, `Store`, and `Maintainer`
interfaces remain supported. Multi-location behavior is added through new
interfaces and adapters rather than breaking existing Kit consumers. Docbank
moves to the new substrate after an independently released Kit version is
available.

## Kit backend mechanics

### Core contract

A backend accepts immutable relative object identities rather than absolute
paths or URLs. It supplies:

- marker read and replacement;
- loose publication;
- pack publication;
- sequential and ranged reads;
- reader-safe retirement;
- bounded inventory;
- staging cleanup;
- capability checks; and
- optional capacity information.

Cross-store work acquires per-store coordination leases in stable store-ID
order. Unrelated stores need not serialize their byte transfer.

### Namespace grammar

Kit reserves canonical relative keys for:

- ownership marker;
- raw and zstd loose objects;
- immutable packs;
- private staging and multipart operation records; and
- backend-format metadata.

Ownership epochs fence access but do not rename every content object.

### Filesystem

The filesystem backend generalizes existing Kit behavior:

- pin the root without following redirections;
- stage privately;
- fsync bytes;
- atomically publish;
- fsync containing directories;
- use reader-safe retirement; and
- recheck ownership before destructive admission.

### S3-compatible storage

S3 publication:

1. uploads to incomplete multipart state or a canonical-but-unauthorized key;
2. completes publication;
3. reads the complete destination object back;
4. verifies framing and every adopted blob's decoded size and SHA-256; and
5. permits catalog authority only after verification.

Provider checksums and ETags are transport evidence, not content identity.
Multipart ETags are never treated as hashes.

Pack reads use ranges for footer/index and selected entries. Every returned
blob still undergoes decoded identity verification. Pack caches include store
identity in their keys.

Whole-pack reuse never grants authority based only on pack framing or source
CRCs. Every destination entry receiving authority is decoded and verified from
the published destination pack.

### Streaming, seeking, and scratch

Streaming is preferred and bounded-memory.

A seekable remote read materializes and verifies the complete object into an
owner-private temporary file. It performs scratch preflight against the
object's full logical size, including loose objects up to the 4 GiB admission
ceiling, and returns a typed insufficient-scratch error rather than consuming
the disk unexpectedly.

Pack, repack, and remote publication also report scratch requirements before
starting. An implementation may stream pack construction into multipart
upload, but complete destination verification remains mandatory.

### Loose fallback

Secondary stores are pack-oriented, not pack-only. Blobs above the current
packing ceiling remain authoritative loose objects after placement. Repair and
exceptional migration may also publish loose content.

### Repair exception

Ordinary publication is conditional and immutable. Repair is the deliberate
exception because a corrupt object may already occupy the canonical key.

Repair:

- performs a fresh ownership check;
- atomically replaces the canonical filesystem object or uses an unconditional
  S3 PUT;
- reads and verifies the replacement;
- swaps catalog authority with a new location generation; and
- retains an independently verified source until commit.

A corrupt pack entry is never overwritten in place. Repair publishes a loose
replacement or a new pack, swaps authority, and leaves the damaged range as
dead packed bytes pending repack.

### Provider inventory

S3 listing supports status and orphan reconciliation only. A partial or failed
listing suppresses destructive conclusions.

Versioned buckets may retain historical provider versions after Docbank
deletes the current key. Status distinguishes catalog reclamation from
provider-billed reclamation. Docbank does not manage lifecycle rules.

## Storage schema

The next schema separates `blobs` membership from physical location.

Conceptually it contains:

- `blob_stores`;
- `blob_locations`;
- `blob_packs` keyed by `(store_id, pack_id)`;
- store-scoped pack-entry detail; and
- mechanical pack usage projections.

`blobs` retains only representation-independent fields such as hash, logical
size, creation time, and packing eligibility.

Foreign keys and uniqueness constraints enforce relational shape. Go validates
representation detail, lifecycle transitions, location coverage, reference
closure, and retirement eligibility.

SQL triggers may maintain mechanical aggregate projections. They do not encode
placement or document policy.

## Portable metadata

`docbank-metadata-jsonl-v1` remains the logical authority format. Physical
locations do not enter document history or audit hashes.

Backup adds a separate deterministic placement artifact containing:

- source store ID;
- display name;
- backend kind;
- source role;
- per-hash source store IDs; and
- aggregate counts and bytes.

It excludes:

- paths;
- endpoints;
- bucket names and prefixes;
- credentials;
- ownership epochs;
- pack IDs and offsets;
- physical encodings; and
- backend staging state.

The artifact makes explicit restore mapping usable without trusting source
physical representation.

## Backup

Backup acquires its preservation lease before pinning metadata or placement.
Both snapshots are derived under that lease, preventing an authority handoff
from landing between them.

The lease blocks location revocation, pack/repack mapping changes, GC, and
physical retirement for captured content. Logical document mutations may
continue through SQLite's WAL.

For each live hash, backup reads one usable candidate through terminal
verification. It does not download every redundant location.

If no candidate verifies:

- backup returns the structured exhausted-candidate evidence;
- no successful snapshot is published; and
- prior snapshots remain untouched.

Remote-only content may therefore require full NAS or S3 egress for every new
snapshot. An unavailable redundant store does not block backup when another
copy verifies. An unavailable sole location does.

Every successful snapshot is complete and independently restorable.

## Restore

### Default

Default restore collapses every verified blob into a fresh built-in primary.
The target receives:

- a fresh primary store ID;
- a fresh ownership epoch;
- an implicit `<target>/blobs` binding;
- no inherited endpoints, paths, credentials, or secondary ownership; and
- a report describing collapsed source stores.

The source placement artifact is informational unless the operator supplies a
mapping.

### Explicit mapping

`docbank backup restore ... --store-map <owner-private-file>` maps source store
IDs to target binding definitions.

Every target store receives fresh target identity and epoch. A mapping may use:

- a new empty namespace; or
- explicit takeover of an existing matching namespace.

Unmapped hashes go to primary. Audit-protected hashes remain primary-pinned
unless the mapping carries the same explicit remote-only acknowledgment as
ordinary placement.

The mapping file is local daemon input. It is never placed in an HTTP request
body, backup, log, or target logical metadata.

### In-place adoption

Taking over a namespace may adopt existing immutable content without
retransfer:

1. conditional publication observes the canonical object already present;
2. Kit reads it back from the destination;
3. every adopted loose object or pack entry is independently verified; and
4. the staged target catalog grants authority.

Failed read-back causes repair from backup content. Adoption can reduce
transfer to verification egress but never skips verification.

### Staged publication

The restored SQLite database remains private until all target content and
catalog relations verify.

SQLite and S3 cannot publish atomically. Failure leaves unauthorized target
orphans under the restore's epoch. Recovery may resume or clean them before
publishing the target.

If the operator abandons a takeover restore, the original vault may explicitly
take over again with a fresh epoch. This is the recovery path for an original
owner fenced by a target that never published.

### Pack reuse

Source pack rows and offsets are never trusted.

A compatible pack may be reused only after destination publication, complete
read-back, structural validation, and per-entry verification for every adopted
hash. Otherwise restore publishes verified loose content or constructs fresh
target packs.

## Released-schema cutover

The current public storage schema is version 2. Multi-store storage uses the
next explicit version.

Upgrade follows the existing cutover:

1. recognize an exact released source layout;
2. export deterministic metadata-v1 through its retained adapter;
3. build a fresh current-schema database;
4. import and validate logical authority;
5. create the built-in primary registry;
6. translate old loose and pack mappings into primary locations;
7. validate logical and physical relations;
8. checkpoint and sync;
9. atomically publish the replacement database; and
10. retain the released source database as recovery.

There is no incremental `ALTER TABLE` ladder.

Translated physical locations are a named exception to new-publication
verification. They inherit the source schema's existing authority because
cutover transforms metadata rather than copying bytes. Post-upgrade full
verification provides the same byte assurance available before upgrade.

Source adapters and exact fixtures cover:

- the inferred v0.9.0 layout;
- every released version-2 layout;
- raw loose authority;
- zstd loose authority;
- packed authority;
- missing physical authority;
- interrupted cutover recovery; and
- both mattn and modernc SQLite.

Version-aware older binaries reject the newer schema before mutation. The
structural layout also prevents released v0.9 writes from succeeding. Direct
compatibility coverage proves v0.9 cannot mutate the new layout and every
supported released layout upgrades to current.

## Operator and agent surfaces

### Store administration

```text
docbank storage add cold --binding archive_s3
docbank storage list
docbank storage status [store] [--refresh]
docbank storage detach cold
docbank storage unregister cold
```

Add is preview-first. Detach removes the machine binding but preserves
inaccessible catalog evidence. Unregister is allowed only after the store has
no authoritative locations or active work. Optional namespace cleanup runs
before unregister and deletes the ownership marker last.

Store names are mutable. Canonical UUID selectors are identity-exclusive.

### Placement

```text
docbank storage place /archive/sessions --to cold
docbank storage place id:482 --to cold
docbank storage evacuate cold --to warm
docbank storage salvage cold --to primary
```

Every mutation previews by default and executes only with a bound preview token
and explicit run flag.

### Repair

```text
docbank storage repair cold --hash <sha256>
docbank storage repair cold --all-findings
```

Repair chooses the best verified live source by default. The operator may name
another store or an independently verified backup snapshot.

Snapshot membership alone is not repair authority. Backup repair verifies the
actual source content before publication.

### Jobs and progress

Placement, evacuation, salvage, remote verification, repair, and remapped
restore are durable jobs.

The existing surface expands:

```text
docbank jobs
docbank jobs show <operation-id>
docbank jobs cancel <operation-id>
```

Jobs expose stable kind, state, progress, cancellation, terminal receipt, and
idempotency identity. An agent that lost its session can enumerate active and
recent jobs, inspect uncertain outcomes, and retry safely.

Progress stages include:

- planning and revalidation;
- source verification;
- transfer;
- destination read-back;
- catalog commit;
- retirement;
- deferred cleanup; and
- pending repack.

Counters distinguish logical, transferred, verified, immediately reclaimed,
and pending-repack bytes.

Ordinary completed-job receipts have a documented retention window. Receipts
that become permanent audit evidence follow audit retention instead.

Daemon restart reconstructs durable job state from catalog authority and
physical staging, not from the last emitted progress message.

### HTTP and embedded Go

Master-key HTTP endpoints expose the same preview, execution, job, progress,
receipt, and cancellation contracts. Streaming responses contain structured
progress and exactly one terminal result.

Browser sessions receive read-only store health and capacity. They do not
receive binding paths, endpoints, credential profile names, ownership epochs,
takeover controls, or storage mutation access.

The daemon protocol advances so older clients cannot misinterpret multi-store
receipts.

The embedded API remains rooted at:

```go
import "go.kenn.io/docbank"
```

Embedded callers provide binding configuration programmatically and receive
Docbank store IDs, previews, jobs, and receipts. Kit catalog and backend types
do not leak through the public Docbank API.

## Conformance boundaries

The design is successful only when the same invariants hold on Linux, macOS,
and Windows and under both supported SQLite drivers.

Conformance must exercise:

- filesystem and S3 publication;
- strong-consistency capability rejection;
- loose, compressed loose, oversized loose, and packed placement;
- complete per-entry pack verification;
- reference drift between preview and commit;
- trash-held and shared-hash closure;
- default audited primary pinning and explicit remote-only acknowledgment;
- every crash boundary around publication and catalog commit;
- deferred reader-safe retirement;
- epoch mismatch, takeover, abandoned takeover, and salvage;
- mixed exhausted-candidate outcomes and precedence;
- process-local demotion and generation invalidation;
- complete backup from local and remote-only populations;
- unavailable-sole-location backup failure;
- default local restore and explicit remap;
- in-place restore adoption and repair of a bad existing object;
- exact released-schema cutovers;
- downgrade fencing; and
- bounded status, inventory, progress, and finding responses.

S3-compatible tests need both deterministic fault injection and a real
compatible service in CI. Provider-specific behavior is not inferred from an
in-memory mock alone.

## Durable invariants

The complete design reduces to these rules:

1. Logical metadata remains local and authoritative.
2. Only fully verified physical locations enter catalog authority.
3. Released-schema translation may inherit existing authority without copying
   bytes, but never invents a location.
4. Every authority handoff leaves at least one verified location.
5. Runtime availability and health are observations, not shadow authority.
6. Missing, corrupt, unavailable, fenced, and authority-free states remain
   distinct.
7. No destructive operation starts without a fresh ownership check.
8. Every failure leaves recoverable authority plus unauthorized debris, never
   false authority.
9. Reference-aware policy decides whether a source may retire.
10. Permanent audit prevents Docbank-authorized deletion but does not promise
    continuous availability or control external storage actors.
11. A successful backup contains every live blob.
12. Restore grants fresh target authority and does not inherit deployment
    secrets or ownership accidentally.
13. Kit owns byte mechanics; Docbank owns product policy.
