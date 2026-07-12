# ADR-0006: Security is scoped to a local single-user vault

- **Status:** Accepted
- **Date:** 2026-07-08
- **Decision source:** integrity model and Phase 2a API hardening

## Context

Docbank is a personal archive whose daemon and vault run with one user's
privileges. Treating it as an internet-facing multi-tenant service would add
controls that do not protect against the actor who already owns the account,
while obscuring the real risks: crashes, accidental damage, stale state, and
unauthenticated local access.

## Decision

The vault is rooted in a tightened 0700 directory. The daemon binds only to
loopback and always enforces an API key, generating and publishing a same-user
ephemeral key in the runtime record when none is configured. Remote access uses
an external secure tunnel to loopback.

The implementation defends against corruption, stale process records, PID
reuse, and accidental symlinks at content objects. An adversary already able to
rewrite the user's vault or race their processes is outside the threat model.

## Consequences

- Plain loopback HTTP is acceptable; non-loopback binds are rejected.
- Rate limiting, tenant isolation, and app-owned TLS are not current product
  requirements.
- Local ingest may read daemon-host paths but remains loopback-fenced.
- Integrity checks prioritize systematic detection over expensive incidental
  checks on every deduplication.

## Alternatives rejected

- Permit keyed LAN binds over plain HTTP: exposes credentials and content on
  the wire.
- Model hostile same-user filesystem races: cannot create a meaningful boundary
  once the attacker has the user's privileges.

## Public architecture

[Integrity & Threat Model](../../architecture/integrity.md) ·
[HTTP API](../../architecture/http-api.md)
