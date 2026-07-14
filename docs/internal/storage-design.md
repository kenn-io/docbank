# Storage design

Docbank separates logical document identity from physical content storage.
SQLite owns the virtual tree and application policy; `go.kenn.io/kit/packstore`
owns application-neutral loose and packed storage mechanics.

## Authority model

Three layers answer different questions:

1. A `nodes` row says a document or directory exists in the virtual tree.
2. A `blobs` row says a content hash is an authorized member of the physical
   store and may be read through docbank.
3. The pack catalog says where those authorized bytes currently live: loose,
   packed, or in a lifecycle transition coordinated by Kit.

These layers must not be collapsed. Node reachability is product policy; blob
membership is docbank's physical authority boundary; offsets, reader caches,
and repacking are storage mechanics.

Stable node IDs are document identity. Paths are derived from parent/name rows
and can change or be reused. Blob hashes are content identity. Two nodes may
share a blob without sharing document identity.

!!! info "Planned — full-audit authority"
    Full-audit policy adds a fourth logical authority: sticky membership and
    append-only history decide which node/version facts can never be removed by
    ordinary maintenance. That policy remains Docbank metadata rather than a
    Kit storage concern. Its definitive contract is
    [Audited History](../architecture/audited-history.md). A logical mutation,
    its ordered history events, membership changes, and every affected scope
    chain/count/head update commit in one SQLite transaction. Canonical order
    comes from a vault-wide operation sequence plus deterministic per-operation
    event ordinals, never incidental traversal order. JSONL preserves the
    allocator high-water marks and a vault-wide allocation lineage that every
    authoritative operation appends with a random operation ID. Import verifies
    and restores that authority transactionally before any new operation can
    run; independently mutated copies therefore have distinguishable ancestry
    even when they consume the same numeric IDs. An audited operation's
    canonical mutation hash includes that operation ID, and its allocation entry
    and every affected scope chain commit the same mutation hash; import rejects
    any missing, duplicate, or mismatched cross-chain binding. Snapshot manifests
    and status/verification proofs expose scope and allocation-lineage
    count/heads together as one rollback-evidence bundle.
    Content-version IDs use random UUIDv4 values under a unique constraint,
    rather than another sequential allocator, so pruning and JSONL round trips
    cannot reuse or retarget an old agent-visible version reference.
    The same metadata-v2 bootstrap assigns non-reusable UUIDv4 identities to
    retained legacy tag and ingest records and creates the stable vault ID.
    Identical legacy provenance duplicates are canonicalized and collapsed
    before their field-derived v2 identities are assigned; distinct facts remain
    intact, and migration fixtures cover duplicates from stores and v1 imports.
    Zero-scope v2 is the portable editing/version form and contains no audit
    genesis or lineage; enabling the first scope later adds those authorities
    from the current v2 projection. Bootstrap activates a non-ignorable
    live-store fence against pre-bootstrap writers and legacy overwrite restore.
    The synced `bootstrap_pending` generation precedes the SQLite cutover;
    `v2_ready` is published only after committed authority is reverified, and
    crash recovery resumes or verifies bootstrap without reopening legacy
    access. Audit activation advances that fence again for pre-audit binaries.
    Audit baselines and final-state reconciliation include authoritative tag
    assignments and their definitions plus provenance and its referenced ingest
    records. Import validates and replays that referential closure; derived FTS,
    extraction-cache, and job rows remain outside audit hashes. Ingest and
    provenance rows are immutable; audited references are database-guarded
    retention roots. Deletion is permitted only when replayed pre-state proves
    the record wholly unprotected and the attached-metadata delta contains its
    tombstone, including in a transaction with an unrelated audited effect.
    Corrections append an immutable superseding fact, replay retains the old fact
    and derives the active leaves, and inserting an identical canonical
    provenance fact is an idempotent no-op.
    Canonical mutation ordering uses the full node/kind/scope/target/attachment
    tuple with format-versioned stable string kind codes, assigns ordinals only
    after sorting, and separately sorts every `(scope, target, baseline digest)`
    binding before hashing. All audit digests use SHA-256 over the normative
    CAE2 typed, length-framed, domain-separated encoding; golden vectors bind
    every record kind and optional-value edge case. Net topology effects always
    use `node_path`, with labels derived from committed pre/post state rather
    than ambiguous action precedence.
    Tag identities are non-reusable UUIDv4 values independent of mutable names.
    Node insertion, deletion (including cascades), and topology changes require
    a transaction-scoped audit context even when the directly touched node is
    unaudited. The mutation path
    precomputes inherited memberships and path-affecting descendant events, then
    refuses commit unless the resulting baselines, events, lineage, and scope
    heads exactly match that closure; database guards reject writes without the
    context.
    Inherited memberships are partitioned into shared baseline batches keyed by
    scope, normalized top-level target, and operation—not one baseline per
    member. Each new membership references exactly one batch, whose sorted
    member state includes the enrollment revision and current-version ID and
    whose adopted-record set is replayed atomically. Later canonical mutations
    carry operation-level member-state changes so import can reconcile revisions
    and heads with current nodes. Each batch also
    preserves deduplicated ancestor-spine topology witnesses. A vault-wide,
    hash-bound topology genesis snapshot plus every later lineage delta lets
    commit and import independently derive the exact adopted trash closure and
    compare its members, versions, and attachments with the batch. Root-scope
    legacy trash with lost ancestry uses an explicit unknown-origin sentinel
    rather than a guessed parent. Later topology mutations commit one sorted
    atomic pre/post delta and a net path-effect set with count and digest.
    Verification derives that set
    from the previous replayed topology before applying the delta, so nested
    batch moves, a writer's claim, or matching final paths cannot conceal an
    omitted descendant event. Every post-audit topology delta is also bound into
    allocation lineage even when it has no scoped effect. Active witness
    generations retire when no audited path depends on them and are recreated
    from current state on later reuse; historical generations remain immutable.
    Every witness state digest is SHA-256 over the registered CAE2 witnessed
    topology record. Sorted witness-change counts/digests are committed into
    both canonical mutations and allocation lineage.
    Shared tag or ingest records are copied identically into every baseline batch
    that references them. Their values and references come from the complete
    post-operation projection; pre-operation memberships decide only which
    nodes were already protected and how new baseline batches are partitioned.
    Audited trash-origin coordinates are immutable metadata rather than a
    nullable live foreign-key relationship; an unaudited origin can disappear
    without mutating the audited node's chain state. The nullable `trash_parent`
    locator is explicitly non-authoritative and excluded from hashes,
    final-state reconciliation, and guards; the immutable origin record is
    authoritative. Enabling the first scope first syncs an `audit_pending`
    layout generation containing every preallocated operation/lineage identity
    and other non-derivable preview input, commits and revalidates enrollment,
    then syncs
    `audit_ready`; recovery resumes a rolled-back enrollment or verifies a
    committed one while pre-audit binaries remain fenced. Database write guards
    prevent bypass through legacy mutation, GC, or backup paths, and the audited
    layouts stay outside legacy restore publication paths. The enable preview
    separately discloses that scope-specific content protection activates
    vault-wide retention of topology tombstones and authoritative tag,
    ingest, and provenance metadata for replay. The planned audited restore
    workflow inspects an existing
    target under its hierarchy lock and accepts overwrite only when the
    snapshot's stable vault ID, scope-chain prefixes, and allocation-lineage
    prefix preserve every promise and consumed identity. High-water comparison
    alone is not proof: divergent copies are rejected even when their counters
    happen to match.

## Immutable content and mutable organization

Canonical blobs are immutable SHA-256 objects. Rewriting bytes in place would
make the path lie about content identity, surprise every deduplicated reader,
and lose crash-safe history. All logical mutation therefore happens in SQLite:
moves, renames, trash, restore, and future content-pointer replacement.

The schema enforces the invariants that every writer must obey:

- exactly one root through a partial unique index on a constant expression;
- live sibling names are unique while trashed names do not reserve a path;
- file nodes have blob hashes and directories do not;
- foreign keys prevent deleting a blob row while a node or version references
  it; and
- node IDs use `AUTOINCREMENT` and are never recycled into a dangling external
  reference.

Store code adds validation that SQL cannot express economically: Unicode NFC
normalization, rejection of empty/dot/slash/NUL names, ancestry checks for
cycle prevention, revision preconditions, and size agreement.

## Durable write ordering

The ingest invariant is **bytes before reference**:

1. Open the source without following a final symlink and confirm the opened
   object is a regular file.
2. Stream and hash it into the Kit store.
3. Kit writes staging bytes, fsyncs, renames to the canonical loose path, and
   fsyncs the containing directory.
4. Only after durable publication may the SQLite transaction insert the blob,
   node, ingest, and provenance rows.

A crash after step 3 but before step 4 leaves an untracked physical object. It
is harmless because no `blobs` row authorizes it; GC's physical scan can remove
it. Reversing the order could commit a node whose bytes vanish after power
loss, which is not recoverable through metadata.

The dedup fast path validates that an existing canonical object is structurally
eligible. It does not rehash same-sized bytes on every duplicate ingest because
that doubles common-path I/O without systematically protecting existing
references. Full content validation belongs to `verify`, which covers every
authorized blob.

## Ingest convergence

Bulk ingest is intentionally restartable. For a destination name, the ingester
scans the base name and numeric collision candidates. A candidate with the same
blob hash is a skip; candidates with different content are preserved; the next
free name receives the new node.

Each successful file gets its own metadata transaction. Source errors are
collected and the batch continues, so rerunning after permissions or mount
problems converges without discarding prior progress. The destination directory
is ID-based once resolved so a concurrent move cannot cause later files to
recreate an old path.

Preflight and import share one exclusion matcher and explicit-root-symlink
model. Preflight stops at filesystem metadata: it never opens a regular file,
hydrates cloud content, creates a destination, records an ingest, or writes a
blob. It therefore predicts selection and size-policy outcomes, not future
readability. Detailed findings and extension groups are bounded while their
aggregate counts remain complete, preventing an adversarial tree from creating
an unbounded API response.

## Trash, permanent deletion, and GC

Deletion is deliberately three-stage:

1. Trash stamps a subtree and records original parent/name recovery context.
2. Trash empty permanently removes eligible tree metadata.
3. GC removes unreachable blob authority and any loose physical content.

The third stage removes loose files immediately. For packed content it only
makes the immutable range logically dead; a separate repack maintenance pass
rewrites live ranges and retires sparse source packs. No removal command folds
that physical rewrite into logical deletion.

Trashed nodes remain reachable. Permanent node deletion may make a blob row a
GC candidate, but it does not itself claim disk space. GC must consider live
nodes, trashed nodes, and `node_versions`; new logical reference types must be
added to reachability before their schema is usable.

!!! info "Planned — full-audit maintenance"
    Full-audit membership is sticky and protected historical versions remain
    reachability roots. A trash root containing any audited member is excluded
    explicitly from trash-empty eligibility and reported separately, while
    eligible unrelated roots can still be removed. GC cannot revoke protected
    blob authority. Repack may replace physical mappings only through Kit's
    verified publication ordering; audit does not pin a particular loose file
    or pack container.

GC and ingest cross SQLite/filesystem boundaries. The daemon maintenance gate
prevents a mutation from deduplicating against bytes between GC's reachability
decision and physical deletion.

Loose bytes can be removed immediately and reported as reclaimed. Removing a
packed mapping makes its immutable range logically dead; disk space is pending
repack and must be reported separately.

## Kit boundary

Kit owns:

- durable loose publication and canonical paths;
- mixed loose/packed reads;
- pack reader caching and reader-safe retirement;
- staging cleanup, orphan reconciliation, pack, unpack, and repack mechanics;
- physical deletion ordering, cancellation, and maintenance budgets.

Sequential reads use Kit's verified stream rather than its buffered
read-seek compatibility API. Bytes from either a loose object or a pack are
provisional until the reader reaches terminal EOF or `Verify` succeeds. An
early `Close` deliberately reports incomplete verification and never drains
the remaining object in the background. HTTP delivery, single-node and
vault-wide verification, and backup capture must therefore consume through
that terminal boundary; existence probes retain the buffered open-and-close
path because they ask about catalog authority, not fresh byte evidence.

Docbank deliberately separates two policies. New local and remote writes may
admit one loose object through 4 GiB, matching the format-v1 backup ceiling.
Kit's packed-read, maintenance, and packed-restore `BlobBytes` limit remains
64 MiB. Kit v0.8 keeps
catalog-authorized larger loose objects available through the same verified
`OpenStream`; they remain eligible for backup but not packing. Do not add a
second Docbank loose-stream adapter or mistake the packed limit for a
read-availability boundary. Raising either application policy needs downstream
measurements first: pack preparation can use about 2.004 times raw size in
scratch per concurrent object, and active stream leases can temporarily raise
open descriptors above the idle reader-cache bound.

Repack commits replacement mappings before retiring an old immutable pack. If
Kit returns `ErrPackRetirementDeferred`, the catalog change must not be rolled
back: the old pack is now an untrusted physical orphan, not authority. Docbank
reports the condition as retryable cleanup. After external readers or Windows
file locks release it, the operator runs `storage pack`; Kit's orphan
reconciliation verifies current authority and removes the redundant source.

## Current resource envelope

`internal/backupapp/resource_benchmark_test.go` is the reproducible downstream
gate for the real SQLite catalog and Docbank adapters:

```bash
go test -tags fts5 ./internal/backupapp -run '^$' \
  -bench '^BenchmarkDocbank' -benchtime=1x -benchmem -count=1
```

The peak-RSS figures use a prebuilt test binary so compilation is outside the
measurement. On macOS, reproduce the full and 1 GiB loose-only runs with:

```bash
go test -c -tags fts5 -o /tmp/docbank-resource.test ./internal/backupapp
/usr/bin/time -l /tmp/docbank-resource.test -test.run '^$' \
  -test.bench '^BenchmarkDocbank' -test.benchtime=1x -test.benchmem -test.count=1
/usr/bin/time -l /tmp/docbank-resource.test -test.run '^$' \
  -test.bench '^BenchmarkDocbankLoose' \
  -test.benchtime=1x -test.benchmem -test.count=1
```

Record `maximum resident set size`; other operating systems need their
equivalent external process measurement rather than Go's cumulative `B/op`.

The current darwin/arm64 baseline on an Apple M4 Max with Go 1.26.4 and Kit
v0.8.0 is:

| Workload | Throughput | Heap allocated per operation | Additional stream descriptors |
| --- | ---: | ---: | ---: |
| verified loose read, 64 MiB | 2,570 MB/s | 12,944 bytes | 1 |
| verified packed read, 64 MiB | 2,159 MB/s | 15,928 bytes | 1 |
| write + pack + sparse repack, 64 MiB total | 145 MB/s | 75,589,880 bytes cumulative | — |
| snapshot + verify + loose restore, 64 MiB, one job | 633 MB/s | 71,131,056 bytes cumulative | — |
| durable loose write, 1 GiB | 295 MB/s | 49,624 bytes | — |
| verified loose read, 1 GiB | 2,639 MB/s | 12,944 bytes | 1 |
| snapshot + verify + loose restore, 1 GiB, one job | 950 MB/s | 49,987,056 bytes cumulative | — |

A prebuilt benchmark binary running all seven workloads sequentially peaked at
109,953,024 bytes (104.9 MiB) resident. Running only the three 1 GiB loose
workloads peaked at 49,348,608 bytes (47.1 MiB) resident. The full
suite retains allocator and codec high-water state from prior maintenance, so
the larger number is the appropriate whole-process capacity baseline. `B/op`
for compound maintenance and backup rows is cumulative allocation across
several streaming stages, not peak live heap; the external RSS measurements
are the process envelopes.

Resource policy still needs capacity beyond RSS. Incompressible preparation at
the 64 MiB ceiling can require about 128.256 MiB of scratch for one object.
Docbank serializes maintenance, so pack/repack currently has one preparation in
flight; future backup concurrency must multiply scratch and codec windows by
its explicit job count. The mixed reader keeps at most 16 idle pack descriptors,
and each concurrent loose or packed stream can add one descriptor until EOF or
`Close`. Cancellation and early-close cleanup remain mandatory race-tested
gates, not benchmark outcomes.

The 1 GiB benchmarks exercise a representative large object through Docbank's
production admission path, Kit's durable loose writer, the mutation and SQLite
authority boundary, native
verified `OpenStream`, and the real backup adapters. They do not admit the
object to packing. They are bounded-memory evidence supporting the current
4 GiB loose-ingestion ceiling while packed maintenance stays at 64 MiB, not a
measurement of the ceiling itself or a performance guarantee. A large object
still needs roughly its raw size for live storage, repository storage, and a
simultaneous restore target in the incompressible case.

Any proposal to change admission or maintenance limits, reader slots, or
backup concurrency must rerun this suite on representative target hardware and
revise this envelope.

Docbank owns:

- the SQLite schema and catalog adapter;
- the meaning of blob membership and liveness;
- transactional mapping changes and compare-and-swap policy;
- trash, version, provenance, and future external-reference semantics;
- daemon commands, scheduling, logging, and product output.

## Logical metadata portability

SQLite is Docbank's runtime query and transaction engine, but its historical
page layout is not the intended long-lived backup contract. The logical
boundary is deterministic JSONL headed by `docbank-metadata` and an integer
format version. Records are emitted in dependency-stable order and deterministic
key order: blobs, nodes, ingests, node versions, provenance, tags, node tags,
and extracted text. Nodes carry parent IDs, so directory structure, trash
restore coordinates, and stable external node references survive a roundtrip.
The header carries SQLite's node `AUTOINCREMENT` high-water mark separately
from the live rows; import restores it only after proving it is at least the
maximum surviving node ID.

!!! info "Planned — editing and audited metadata v2"
    The editing/identity bootstrap will advance this boundary to
    `docbank-metadata` version 2 and `docbank-metadata-jsonl-v2`; existing
    version 1 remains the pre-bootstrap format and cannot contain stable content
    versions or audit records. Zero-scope v2 preserves editing and portable
    identities without audit genesis or lineage. Enabling the first scope adds
    the complete audit authority to that same format.

The stream excludes `nodes_fts`, `blob_packs`, and `blob_pack_index`. FTS is a
derived index rebuilt by the node insert triggers. Pack tables describe one
physical representation and must never regain authority merely because an old
metadata snapshot mentioned offsets; Kit restore verifies and publishes the
chosen loose or packed representation before installing fresh catalog mappings.

Import runs only against a pristine current-schema database, in one transaction
with deferred foreign-key checks. Unknown format versions, unknown record types
or fields, uniqueness failures, orphaned extraction rows, and dangling
references abort the transaction. Timestamps must use Docbank's canonical UTC
representation because retention queries compare their fixed-width strings
lexicographically. The exception is provenance `original_mtime`: it records an
external filesystem value using canonical UTC `RFC3339Nano`, matching ordinary
ingestion, and is never used as a retention cutoff.

Trash roots remain detached beneath the tree root in portable metadata. Their
saved `trash_parent` may be absent when the original directory was hard-deleted;
`trash_name` remains authoritative and restore then falls back to the tree root.
When a saved parent is present, import proves it is neither the trash root nor
one of its descendants, so restore cannot create a cycle from hostile or
corrupted coordinates. Every trashed node must belong to exactly one such root,
and every member of that subtree must be trashed under the root's exact
operation timestamp. This mirrors restore's timestamp-scoped update and rejects
both permanently hidden orphans and live nodes nested beneath trash.
Explicit node IDs advance SQLite's `AUTOINCREMENT` sequence, preserving the
invariant that a deleted historical ID is never silently reused.

The Kit v0.9.0 backup adapter now uses this boundary for every new snapshot:
export → verified metadata artifact → construct a fresh current-schema
database → import → checkpoint → publish verified content and fresh pack
authority → prove fidelity → atomic replacement. Historical SQLite page-map
snapshots remain restorable, but new captures cannot select that legacy path.
This does not make compatibility work disappear: each supported JSONL version
needs an explicit decoder, and physical pack authority must always travel
through Kit's separately verified publication path.

Do not move docbank SQL or reachability policy into Kit. Do not reimplement Kit
reader or lifecycle mechanics in docbank. A physical-storage bug shared by
msgvault belongs in Kit; a decision about whether a docbank reference keeps a
blob alive belongs here.

Kit owning a mechanic does not imply that docbank exposes it. In particular,
`packstore.Maintainer.Unpack` remains available for conformance tests,
migrations, and a purpose-built emergency recovery path, but is not part of the
ordinary daemon API or CLI. A public unpack operation would reverse the
small-file benefit, require transient duplicate disk capacity, and leak
physical-format selection into the product. Do not add one without a concrete
recovery workflow that cannot be served by verified backup/restore.

## Schema compatibility

Store startup runs the embedded idempotent schema in one immediate transaction
and ensures the root exists. This safely creates missing compatible tables and
indexes, but it is not a general migration system.

The pre-v0.1 freedom to change schema without migrations is over. Until kata
`7q8z` adds version tracking and transactional forward migrations, no release
may depend on an incompatible change to an existing table, column, trigger, or
index. Migration machinery must precede the incompatible reader/writer.

When migrations land, tests need real old-vault fixtures, upgrade verification,
failure rollback behavior, and a documented backup-before-migrate contract.

## Open design constraints

- External references need a liveness policy before their schema exists:
  pin blob authority, or allow dangling-reference detection in the referrer.
- Version writers must preserve bytes-before-reference ordering and add their
  rows to reachability atomically with pointer replacement.
- Pack and repack maintenance commands must remain daemon/API operations; no
  CLI path may open the physical store directly.
