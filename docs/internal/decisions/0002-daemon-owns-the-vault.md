# ADR-0002: The daemon is the sole vault owner

- **Status:** Accepted
- **Date:** 2026-07-08
- **Decision source:** Phase 2a daemon-first infrastructure

## Context

The original CLI opened the store per command and coordinated through shared
and exclusive locks. Agents and a later UI would have required parallel access
paths or repeated store ownership logic, while maintenance spans both SQLite
and filesystem operations.

## Decision

Exactly one daemon opens SQLite and the physical blob store. It holds the vault
flock exclusively for its lifetime. CLI commands and agents are HTTP clients of
the same authenticated loopback API; no data command opens the vault directly.

All starter paths converge on a version- and protocol-compatible daemon.
Discovery-only status and stop operations remain permissive so an incompatible
daemon can still be found and replaced.

## Consequences

- One process is the authority for storage, maintenance gating, and startup
  cleanup.
- Every client capability must exist in the HTTP API.
- A second daemon fails immediately instead of waiting on a lifetime lock.
- The runtime record becomes part of same-user discovery and authentication,
  but not archive data.

## Alternatives rejected

- Keep direct CLI store access: creates a privileged client path and preserves
  cross-process filesystem races.
- Let each client embed the store: duplicates lifecycle and compatibility logic.

## Public architecture

[Daemon](../../architecture/daemon.md) ·
[Concurrency & Locking](../../architecture/locking.md)
