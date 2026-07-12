# Architecture decision records

Decision records preserve context that would otherwise be reconstructed from
old plans, review threads, and commits. They are internal and never published.

## Index

| ID | Decision | Status |
|----|----------|--------|
| [ADR-0001](0001-content-addressed-bytes-virtual-tree.md) | Content-addressed bytes under a mutable virtual tree | Accepted |
| [ADR-0002](0002-daemon-owns-the-vault.md) | The daemon is the sole vault owner | Accepted |
| [ADR-0003](0003-node-identity-and-concurrency.md) | Stable IDs and scoped revisions govern mutations | Accepted |
| [ADR-0004](0004-deletion-stages.md) | Trash, metadata deletion, and byte reclamation are separate | Accepted |
| [ADR-0005](0005-kit-physical-storage-boundary.md) | Kit owns physical storage mechanics; docbank owns policy | Accepted |
| [ADR-0006](0006-single-user-trust-boundary.md) | Security is scoped to a local single-user vault | Accepted |
| [ADR-0007](0007-schema-compatibility-after-v0.1.md) | Released vaults require explicit schema compatibility | Accepted; machinery pending |

## Lifecycle

Use one of these statuses:

- **Proposed** — under active review; not yet a constraint.
- **Accepted** — current work must conform.
- **Superseded by ADR-NNNN** — a later decision replaces it.
- **Deprecated** — retained for history but no longer relevant.

Accepted records are append-stable. Clarify wording, evidence, or links in
place, but do not change the historical decision. A material reversal gets a
new record, and the prior record's status changes to “Superseded by …”.

## Record shape

Each record contains status and date, then context, decision, consequences,
alternatives, and links to maintained public architecture. Keep implementation
checklists in kata or a transient plan, not in the decision record.
