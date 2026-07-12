---
title: Packed Storage Foundation
description: The shared Kit packed-CAS layer and docbank's application-owned authority boundary.
---

# Packed storage foundation

The shared engine is implemented in `go.kenn.io/kit/packstore`, and msgvault
has adopted it without changing its pack format or migration behavior. docbank
now uses the same engine for durable loose publication, catalog-authorized
mixed reads, and physical lifecycle coordination. Existing vaults require no
conversion: ordinary writes still land loose and both representations are
valid indefinitely.

!!! info "Planned — operator surface"
    Authenticated daemon operations for status, explicit packing, repacking,
    and unpacking have not landed yet. Startup never performs an implicit
    migration, and automatic background maintenance remains a later step.

Large collections of small files are expensive to enumerate, copy, and restore.
msgvault is addressing the same problem by making immutable pack files the
steady-state storage format for attachments. docbank should reuse that work
rather than grow an independent pack index, reader cache, recovery state
machine, and repacker.

`kit/packstore` sits above the low-level `kit/pack` format. It provides a mixed
loose-and-packed content-addressed store, so migration can be gradual and
interrupted work remains recoverable. Its default maintenance limit is 64 MiB
per blob; larger documents remain loose and readable rather than forcing an
unbounded in-memory pack operation.

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
references. In docbank, a row in `blobs` grants physical read authority; live
and trashed nodes, prior versions, and eventually references held by external
applications decide whether GC may remove that row. Kit therefore accepts an
application-supplied catalog and does not own either application's schema or
garbage-collection policy.

## Adoption sequence

1. **Complete:** prove msgvault's pack, logical-delete, repack, reader-retirement,
   backup-overlap, unpack, and crash-recovery lifecycle.
2. **Complete:** extract that lifecycle into Kit behind application-neutral
   catalog interfaces and migrate msgvault without observable behavior change.
3. **Complete:** add docbank's SQLite catalog, mixed reader, durable loose
   writer, GC integration, and the Kit catalog conformance suite.
4. **Next:** expose daemon-first maintenance operations, then reuse the same
   catalog in Kit backup and direct packed restore.
5. **Later:** build first-class external integrations on content hashes and the
   shared storage contract rather than private pack internals.

## Consequences for docbank

Docbank owns only its catalog adapter, daemon wiring, migration policy, and
end-to-end verification. It does not fork Kit's reader cache, reconciliation,
or repacker. The existing loose representation remains both the recovery path
and the representation for blobs above the bounded maintenance limit.

The `blobs` membership boundary also lets feature work proceed independently.
Editing and versions, tags, watched inboxes, text extraction, provenance, and
external references change logical reachability without changing physical pack
authority. An eventual external pin can keep a blob row alive without pinning a
virtual-tree node against normal trash operations.
