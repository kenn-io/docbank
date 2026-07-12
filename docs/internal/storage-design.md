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

Docbank currently uses 64 MiB for two distinct policies: new local and remote
writes, and Kit's packed-read, maintenance, and packed-restore `BlobBytes`
limit. Kit v0.8 keeps catalog-authorized oversized loose objects available
through the same verified `OpenStream`; they remain eligible for backup but not
packing. Do not add a second Docbank loose-stream adapter or mistake the packed
limit for a read-availability boundary. Raising either application policy needs
downstream measurements first: pack preparation can use about 2.004 times raw
size in scratch per concurrent object, and active stream leases can temporarily
raise open descriptors above the idle reader-cache bound.

Repack commits replacement mappings before retiring an old immutable pack. If
Kit returns `ErrPackRetirementDeferred`, the catalog change must not be rolled
back: the old pack is now an untrusted physical orphan, not authority. Docbank
reports the condition as retryable cleanup. After external readers or Windows
file locks release it, the operator runs `storage pack`; Kit's orphan
reconciliation verifies current authority and removes the redundant source.

Docbank owns:

- the SQLite schema and catalog adapter;
- the meaning of blob membership and liveness;
- transactional mapping changes and compare-and-swap policy;
- trash, version, provenance, and future external-reference semantics;
- daemon commands, scheduling, logging, and product output.

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
