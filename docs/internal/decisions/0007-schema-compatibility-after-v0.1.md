# ADR-0007: Released vaults require explicit schema compatibility

- **Status:** Accepted; migration machinery pending
- **Date:** 2026-07-12
- **Decision source:** post-v0.1 architecture audit
- **Implementation tracking:** kata `7q8z`

## Context

Before the first release, schema rebuild migrations carried risk without a
beneficiary because no released vault needed upgrading. v0.1.0 ended that
condition. Store startup still reapplies idempotent `CREATE ... IF NOT EXISTS`
statements and has no schema-version ledger or ordered migration runner.

## Decision

No release may require an incompatible change to an existing table, column,
trigger, or index until versioned, transactional forward migrations exist.
Migration support must ship before the first incompatible writer or reader, not
as repair work afterward.

Additive schema statements that are genuinely compatible may continue through
bootstrap, but they do not substitute for an explicit version contract.

## Consequences

- Schema review must distinguish additive compatibility from rebuild changes.
- CI needs old-vault-to-new-binary fixtures when migration machinery lands.
- Downgrade behavior and backup-before-migrate policy must be decided with the
  migration implementation.
- The earlier “no migrations before first release” decision is expired, not an
  ongoing rationale.

## Alternatives rejected

- Continue relying on `IF NOT EXISTS`: silently leaves old tables with an old
  shape.
- Detect incompatibility only after a query fails: turns upgrade into recovery.
- Mutate schema without version tracking: cannot prove ordered, one-time
  application.

## Public architecture

[Integrity & Threat Model](../../architecture/integrity.md)
