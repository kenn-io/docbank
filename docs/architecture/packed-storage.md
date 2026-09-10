---
title: Loose & Packed Content
description: The shared Kit packed-CAS layer and docbank's application-owned authority boundary.
---

# Loose and packed content

Docbank first stores new content as individual **loose objects**. Packing later
combines eligible objects into sealed, immutable **pack files**. The catalog
selects the stored copy for each read, so packing does not change document or
content identity.

Docbank uses the shared `go.kenn.io/kit/packstore` engine for writes, reads, and
physical maintenance. msgvault uses the same engine without changing its pack
format or migration behavior. Existing raw loose files remain valid
indefinitely and need no conversion.

New loose objects of at least 4 KiB use zstd when it saves at least 10% of their
size. Otherwise Docbank keeps the raw bytes. Docbank manages this choice;
standalone users do not select a compression format.

`docbank storage status` exposes loose and packed inventory through the
authenticated daemon API, and `docbank storage pack` explicitly moves authorized
loose content into immutable packs with an optional work budget. An application
that exclusively owns an embedded vault can invoke the same packing and
reconciliation pass through `Vault.Pack`; it cannot bypass the catalog or Kit's
maintenance coordinator. `docbank storage repack` compacts eligible sparse packs
and retires dead pack files. Embedded owners also have bounded `GarbageCollect`,
`Verify`, and `Repack` passes. Startup never implicitly rewrites blob bytes, and
configured daemon scheduling applies only to bounded packing. Garbage
collection, verification, and repacking remain explicit operator actions.

Large collections of small files are expensive to enumerate, copy, and restore.
msgvault uses immutable pack files as the steady-state storage format for
attachments. docbank reuses that work rather than growing an independent pack
index, reader cache, recovery state machine, and repacker.

`kit/packstore` sits above the low-level `kit/pack` format. It provides a mixed
loose-and-packed content-addressed store, so migration can be gradual and
interrupted work remains recoverable. Docbank explicitly caps new loose-object
admission at 4 GiB while keeping packing, packed reads, and packed restore at 64
MiB. These are application policies rather than inherited Kit defaults, so
upgrading the shared engine cannot silently raise either one. An admitted object
above 64 MiB remains loose, readable, and eligible for backup; pack maintenance
reports it as deferred instead of attempting to prepare it.

Loose compression is a write policy, not a new content identity. When an
embedded `Config` enables it, Docbank finishes the candidate zstd stream and
keeps it only when the configured minimum size and savings threshold are met;
otherwise it publishes the raw form. Existing raw objects are not rewritten. The
catalog records the selected encoding and stored bytes, while the canonical
identity remains SHA-256 over decoded logical bytes.

## Ownership boundary

Kit owns mechanics that must behave identically in both applications:

- canonical loose and pack paths, hash validation, and mixed-storage reads;
- bounded pack-reader caching and safe reader retirement;
- staging cleanup, orphan reconciliation, packing, unpacking, and repacking;
- crash ordering, verification, cancellation, work budgets, and maintenance
  statistics; and
- safe physical deletion after a transactional mapping replacement.

Each application retains the policy that gives those mechanics meaning:

- its schema, migrations, and SQL queries;
- the definition of whether a content hash is live;
- transactional mapping changes and compare-and-swap checks;
- product-specific retention and deletion rules; and
- daemon scheduling, commands, logging, and backup compatibility adapters.

This boundary matters because the applications have different reachability
rules. msgvault derives liveness from attachment content and thumbnail
references. In docbank, a row in `blobs` grants physical read authority. Current
GC keeps that row while any live node, trashed node, or recorded prior version
refers to it. Kit therefore accepts an application-supplied catalog and does not
own either application's schema or garbage-collection policy. Physical
maintenance therefore cannot choose application-level liveness. It can only act
on the catalog authority that docbank's tree, trash, version, and retention
rules have already made reachable or unreachable.

## Consequences for docbank

Docbank owns only its catalog adapter, daemon wiring, released-schema cutover
policy, and end-to-end verification. It does not fork Kit's reader cache,
reconciliation, or repacker. Raw and zstd loose representations remain recovery
paths and staging representations before packing. Both names identify the same
logical SHA-256 and decoded size. Status and GC report their physical stored bytes;
reads, backup, verification, and packing decode and verify the logical bytes
before granting authority. Streaming reads remain bounded-memory. A caller
that needs a seekable handle to compressed loose content may require Kit to
create a private decoded temporary file, so sequential consumers should prefer
the streaming API.

An eligible compressed write privately stages both the complete raw object and
its zstd candidate before choosing which one to publish. Temporary-space
planning must therefore allow roughly the raw size plus the compressed size for
each concurrent write; cancellation and failed writes remove those private
candidates without granting metadata authority.

`RepairContent` verifies trusted bytes against one existing logical SHA-256
identity without changing nodes or content versions. Existing loose authority
keeps its raw or zstd encoding so publication and catalog replacement remain
crash-safe. Packed or missing authority follows the configured loose-compression
policy. Retired immutable pack bytes are reclaimed only by a later repack pass.

The limits serve different purposes:

- The 4 GiB admission ceiling matches Kit's format-v1 raw-object ceiling. Every
  admitted object can therefore participate in backup.
- The 64 MiB packed-content limit bounds pack preparation. A 1 GiB pack
  candidate could require about 2.004 GiB of scratch space before frame overhead.

Measured 1 GiB loose streaming and backup workloads stay within the recorded
memory envelope. That does not measure pack preparation at 1 GiB. Raising the
packed-content limit requires representative measurements of temporary space,
file descriptors, throughput, cancellation, and restore behavior. Active streams
can temporarily exceed the idle reader cache's descriptor count.

Large loose objects retain the filesystem tradeoff that packing solves for
small-object collections. In the incompressible case, one large object can
require roughly its raw size in the live vault, again in the backup repository,
and again in a simultaneous restore target.

The `blobs` membership boundary also lets logical features evolve without
changing physical pack authority.

## Why unpack is not an operator command

Kit retains an unpack primitive because shared storage tests, migrations, and a
future purpose-built recovery tool may need to materialize packed content loose.
Docbank intentionally does not expose that primitive as a normal API or CLI
operation.

Packing exists to avoid the enumeration, backup, and restore cost of thousands
of small files. A general unpack command would recreate that problem, could
temporarily require space for both representations, and would make users manage
an implementation format that docbank should own. Recovery belongs in verified
backup/restore or a concrete repair workflow; the existence of a low-level Kit
operation is not by itself a product use case.

Configured automatic packing is a current daemon capability. It applies a
finite byte budget through the same maintenance gate as explicit packing;
garbage collection and repacking remain deliberate operator actions. External
content references are not a current operator capability. Backup, replacement,
repair, reversion, and maintenance use the catalog and content-hash boundary
rather than private pack internals.

Next: [Storage](storage.md) documents the schema and blob-store invariants
beneath this layer; [Trash, GC, Repack & Verify](../usage/trash-and-gc.md) is
the operator workflow above it.
