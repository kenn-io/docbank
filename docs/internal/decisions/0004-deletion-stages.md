# ADR-0004: Trash, metadata deletion, and byte reclamation are separate

- **Status:** Accepted
- **Date:** 2026-07-06
- **Decision source:** Phase 1 core

## Context

Users need immediate name reuse and recovery from accidental deletion, while
content-addressed bytes may be shared by multiple nodes or prior versions.
Combining tree deletion with physical deletion would make recovery fragile and
reachability races dangerous.

## Decision

Deletion has three explicit stages:

1. Trash a node or subtree while retaining restore metadata.
2. Empty eligible trash roots to permanently remove tree metadata.
3. Garbage-collect blob rows and physical bytes only when application
   reachability says they are unreferenced.

Permanent stages are dry-run by default. Loose reclaimed bytes and packed bytes
that are only logically dead are reported separately.

## Consequences

- Trashed content remains reachable and blocks GC.
- Emptying trash does not claim disk space was reclaimed.
- GC must serialize with mutations across the database/filesystem boundary.
- New reference types must define reachability before their schema ships.

## Alternatives rejected

- Delete bytes with the tree node: unsafe for deduplication, versions, and
  recovery.
- Treat trash as a hidden path only: does not preserve subtree roots and
  original-parent behavior cleanly.

## Public architecture

[Storage](../../architecture/storage.md) ·
[Concurrency & Locking](../../architecture/locking.md)
