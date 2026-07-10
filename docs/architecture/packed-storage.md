---
title: Packed Storage Foundation
description: The planned shared packed-CAS layer for docbank and msgvault.
---

# Packed storage foundation

!!! info "Planned"
    docbank currently stores each blob as a loose file. The packed storage
    described here will be implemented only after the shared engine has been
    extracted into `go.kenn.io/kit` and msgvault has adopted it without a
    behavior change.

Large collections of small files are expensive to enumerate, copy, and restore.
msgvault is addressing the same problem by making immutable pack files the
steady-state storage format for attachments. docbank should reuse that work
rather than grow an independent pack index, reader cache, recovery state
machine, and repacker.

The intended foundation is a new kit package, provisionally `kit/packstore`,
above the existing low-level `kit/pack` format. It will provide a mixed
loose-and-packed content-addressed store, so migration can be gradual and
interrupted work remains recoverable.

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
references. docbank must account for live and trashed nodes, prior versions,
and eventually references held by external applications. Kit therefore accepts
an application-supplied catalog or transaction adapter; it does not own either
application's schema or garbage-collection policy.

## Extraction sequence

1. Complete and verify msgvault's full lifecycle: bounded packing, logical
   deletion, physical repacking, reader retirement, backup overlap, unpacking,
   and crash recovery.
2. Extract the proven filesystem and lifecycle behavior into kit behind
   application-neutral types and narrow catalog interfaces.
3. Move msgvault onto the kit package without changing its observable behavior.
   This is the compatibility proof for the extraction.
4. Add docbank's catalog and reachability adapter, then migrate docbank from
   loose-only storage to the same mixed loose/packed engine.
5. Build any first-class msgvault integration with docbank on content hashes and
   the shared storage contract instead of private pack internals.

The extraction should happen after msgvault's physical repacker is complete:
reader retirement and transactional pack replacement are the most demanding
parts of the interface. Extracting earlier would either omit them or freeze an
interface before its hardest behavior has been exercised.

## Consequences for docbank

Feature work that does not encode a physical blob format can continue while
the extraction is underway. Editing and version semantics, tags, watched
inboxes, text extraction, provenance, and the external-reference retention
decision all remain useful inputs to docbank's reachability adapter.

Docbank should defer its own pack metadata schema, cache, reconciliation, and
repacking implementation. Once the kit layer is ready, docbank's work should be
limited to its application adapter, daemon wiring, migration policy, and
end-to-end verification. The existing loose store remains the recovery and
gradual-migration path rather than being discarded.
