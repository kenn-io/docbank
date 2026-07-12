# ADR-0001: Content-addressed bytes under a mutable virtual tree

- **Status:** Accepted
- **Date:** 2026-07-06
- **Decision source:** initial design and Phase 1 implementation

## Context

Documents need durable, deduplicated storage without freezing their user-facing
names or locations. A filesystem tree makes physical placement and organization
the same decision, so a rename rewrites storage paths and a duplicate filename
can obscure content identity.

## Decision

Store immutable bytes by SHA-256 and represent directories, names, paths,
trash state, provenance, and later version history in SQLite. A node ID is the
durable document identity; a path is derived, mutable addressing.

Publish a blob durably before committing a metadata reference. Reorganization
changes SQLite rows, never blob bytes.

## Consequences

- Duplicate content shares physical storage.
- Moves and renames are transactional metadata operations.
- The database and blob store form one archive and must be snapshotted together.
- Editing cannot mutate a blob in place; it requires a new blob and version
  metadata.
- GC needs application-owned reachability rather than filesystem traversal.

## Alternatives rejected

- Use the host filesystem tree as the canonical organization: couples user
  layout to physical storage and makes transactional invariants difficult.
- Store mutable file bytes behind stable paths: loses content identity and
  makes history and deduplication ambiguous.

## Public architecture

[Architecture Overview](../../architecture/overview.md) ·
[Storage](../../architecture/storage.md)
