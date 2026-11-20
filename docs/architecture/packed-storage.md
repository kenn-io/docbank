---
title: Loose and packed content
description: The shared Kit packed content layer, and what Docbank itself remains responsible for.
---

# Loose and packed content

Docbank first stores new content as individual **loose objects** and later
packs eligible objects into sealed, immutable **pack files**. Because the
catalog chooses which stored copy satisfies each read, packing never changes
document or content identity.

Docbank uses the shared `go.kenn.io/kit/packstore` engine for writes, reads,
and physical maintenance, the same engine msgvault uses without changing its
pack format or migration behavior. Existing raw loose files stay valid
indefinitely and need no conversion.

When a new loose object is at least 4 KiB, Docbank compresses it with zstd if
that saves at least 10% of its size; otherwise it stores the raw bytes. Docbank
makes this choice automatically, and standalone users do not pick a compression
format.

You can inspect and manage packed storage through the CLI or the embedded Go
API:

- `docbank storage status` reports loose and packed inventory through the
  authenticated daemon API.
- `docbank storage pack` moves authorized loose content into immutable packs,
  with an optional work budget. In embedded mode, an application that exclusively owns a
  vault can run the same packing and reconciliation pass with `Vault.Pack`
  without bypassing the catalog or Kit's maintenance coordinator.
- `docbank storage repack` compacts eligible sparse packs and retires dead pack
  files. Embedded owners also get bounded `GarbageCollect`, `Verify`, and
  `Repack` passes.

Startup never rewrites blob bytes. Configured daemon scheduling applies only to
bounded packing, while garbage collection, verification, and repacking run only
when you request them.

Large collections of small files are expensive to enumerate, copy, and restore.
msgvault uses immutable pack files as the steady-state storage format for
attachments. Docbank reuses that implementation instead of maintaining a
separate pack index, reader cache, recovery state machine, and repacker.

`kit/packstore` builds on the low-level `kit/pack` format to provide a mixed
loose-and-packed content-addressed store, so you can pack gradually and recover
cleanly from interrupted work. Docbank caps new loose-object admission at
64 TiB to match Kit's logical backup-object limit, while keeping packing,
packed reads, and packed restore capped at 64 MiB as an independent application policy. Any
admitted object larger than 64 MiB stays loose, remains readable and eligible
for backup, and is reported as deferred during pack maintenance rather than
prepared for packing.

Loose compression is a write policy, not a separate content identity. When an
embedded `Config` enables compression, Docbank finishes the candidate zstd
stream and keeps it only if it meets the configured minimum size and savings
threshold; otherwise it publishes the raw bytes. Existing raw objects are not
rewritten. The catalog records the chosen encoding and stored byte count, while
the canonical identity stays the SHA-256 digest of the decoded logical bytes.

## Ownership boundary

Kit owns the low-level storage mechanics that must behave identically in both
Docbank and msgvault:

- Canonical loose and pack paths, hash validation, and mixed-storage reads.
- Bounded pack-reader caching and safe reader retirement.
- Staging cleanup, orphan reconciliation, packing, unpacking, and repacking.
- Crash ordering, verification, cancellation, work budgets, and maintenance
  statistics.
- Safe physical deletion after a transactional mapping replacement.

Each application owns the policy that gives those mechanics meaning:

- Its schema, migrations, and SQL queries.
- The rules that define whether a content hash is live.
- Transactional mapping changes and compare-and-swap checks.
- Product-specific retention and deletion rules.
- Daemon scheduling, commands, logging, and backup compatibility adapters.

This split matters because the two applications decide reachability
differently. msgvault derives liveness from attachment content and thumbnail
references, whereas in Docbank a row in `blobs` grants physical read authority
and current GC keeps that row while any live node, trashed node, or recorded
prior version points to it. Therefore, Kit takes an application-supplied
catalog and stays out of schema and garbage-collection policy. Physical
maintenance never decides application-level liveness; it acts only on the
catalog authority that Docbank's tree, trash, version, and retention rules have
already marked reachable or unreachable.

## Consequences for docbank

Docbank owns only its catalog adapter, daemon wiring, released-schema cutover
policy, and end-to-end verification, without forking Kit's reader cache,
reconciliation, or repacker. Raw and zstd loose files remain both recovery
paths and staging forms before packing. Both encodings share the same logical
SHA-256 and decoded size, while status and GC report their physical stored
bytes. Reads, backups, verification, and packing decode and verify the logical
bytes before granting authority. Streaming reads run in bounded memory. If a
caller asks for a seekable handle to compressed loose content, Kit may have to
create a private decoded temporary file, so prefer the streaming API when you
read sequentially.

When Docbank writes an object eligible for compression, it privately stages
both the full raw object and the zstd candidate before picking one to publish.
Plan for roughly the raw size plus the compressed size of temporary disk space
for each concurrent write. If a write fails or is canceled, Docbank removes
those private staging files without granting metadata authority.

`RepairContent` verifies trusted bytes against an existing logical SHA-256
identity without changing any nodes or content versions. If the blob already
has loose authority, it keeps its raw or zstd encoding so publication and
catalog replacement stay crash-safe. If authority is packed or missing, it
follows the configured loose-compression policy. Bytes left behind in retired
immutable packs are reclaimed only when you run a later repack pass.

The two size limits exist for different reasons:

- **64 TiB admission ceiling.** Matches the largest logical object Kit backup
  can represent with bounded chunk recipes. Backup splits objects larger than
  64 MiB into chunks while preserving their whole-file hashes. Snapshots that
  use recipes require a version-5 reader, and existing snapshots stay readable.
  Restore writes these large objects back as complete loose files.
- **64 MiB packed-content limit.** Bounds the scratch space needed during pack
  preparation. A 1 GiB pack candidate could need about 2.004 GiB of scratch
  space before frame overhead.

Measured 1 GiB loose streaming and backup workloads stay within the recorded
memory envelope, though that does not measure pack preparation at 1 GiB.
Raising the packed-content limit would require representative measurements of
temporary space, file descriptors, throughput, cancellation, and restore
behavior. Keep in mind that active streams can temporarily exceed the idle
reader cache's file-descriptor count.

Large loose objects carry the same filesystem tradeoff that packing solves for
small files. An incompressible large object can require roughly its raw size in
the live vault, again in the backup repository, and again in a simultaneous
restore target.

Because `blobs` separates logical membership from physical layout, Docbank can
add logical features without changing physical pack authority.

## Why unpack is not an operator command

Kit keeps an unpack operation because shared storage tests, migrations, and a
future purpose-built recovery tool may need to materialize packed content as
loose files. Docbank does not expose it as a normal API or CLI command.

Packing exists to avoid enumerating, backing up, and restoring thousands of
small files. A general unpack command would bring that problem back, could
temporarily require disk space for both representations, and would push an
internal storage format onto users. Recovery belongs in verified backup and
restore or in a dedicated repair workflow; having a low-level Kit operation is
not by itself a product use case.

The daemon can run configured automatic packing, applying a finite byte budget
through the same maintenance gate as a manual pack. Garbage collection and
repacking run only when you request them, and external content references are
not a current operator capability. Backup, replacement, repair, reversion, and
maintenance all work through the catalog and content-hash boundary instead of
private pack internals.

Next, see [Storage](storage.md) for the schema and blob-store invariants
beneath this layer, or
[Trash, garbage collection, repack, and verify](../usage/trash-and-gc.md) for
the operator commands above it.
