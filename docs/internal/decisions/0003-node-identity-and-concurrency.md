# ADR-0003: Stable IDs and scoped revisions govern mutations

- **Status:** Accepted
- **Date:** 2026-07-08
- **Decision source:** Phase 2a HTTP API

## Context

Paths change and can be reused. Agents also make decisions across multiple HTTP
turns, so a read followed by a mutation can otherwise overwrite a concurrent
change. A global tree revision would invalidate unrelated work across the
vault.

## Decision

Node IDs are canonical and never reused. ID-addressed move, trash, and restore
require the node revision in `If-Match`; a stale decision fails with 412.

Path-addressed move and trash are a separate one-shot contract. They resolve
the path and mutate within one store transaction, so no client-side revision is
accepted or needed. Paths on trash responses are recovery context, not identity.

## Consequences

- Agent retries must re-read and reconsider intent after a stale revision.
- Unrelated node changes do not invalidate an in-flight decision.
- Path operations are safe against a resolve-then-mutate race but intentionally
  mean “whatever this path names when the transaction runs.”
- API errors need stable machine-readable codes.

## Alternatives rejected

- Use paths as identity: renames and name reuse can redirect later work.
- Use a global tree ETag: creates unnecessary conflicts at personal-archive
  scale.
- Require revisions on path operations: would reintroduce a separate resolution
  window or require a misleading pre-read.

## Public architecture

[HTTP API](../../architecture/http-api.md) ·
[Agent Integration Guide](../../agents/integration.md)
