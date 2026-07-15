# Internal design documentation

This directory is the definitive agent/developer description of **how docbank
works and why**. It is living documentation: update it in place whenever the
implementation or its rationale changes.

It is intentionally excluded from the public Zensical site. Public pages under
`docs/architecture/` explain product behavior and stable boundaries to users;
these internal pages include package ownership, rejected approaches, change
constraints, and implementation seams.

## Design map

- [Storage design](storage-design.md) — virtual-tree authority, immutable
  content, ingest ordering, deletion reachability, packed storage, and schema
  compatibility.
- [Daemon and API design](daemon-api-design.md) — sole vault ownership,
  discovery, authentication, revisions, path operations, maintenance gating,
  and errors.
- [Development guide](development.md) — where changes belong, which
  cross-layer contracts must move together, and how design documentation stays
  current.

## Documentation boundary

- **User and agent guides:** shipped capabilities, exact contracts, and current
  limitations.
- **Public architecture:** the user-visible model and durable design intent;
  future contracts appear only inside explicit planned callouts.
- **Internal design:** current mechanics and rationale for agents and
  developers, including consequences and constraints.
- **Roadmap:** one high-level public product-status view.
- **Kata:** the sole home for work items, ordering, ownership, blockers, and
  completion state.

There is no separate decision ledger. If the design changes, revise the
relevant living page so a new contributor can learn the current system without
replaying historical records. Git history preserves the older state. These
pages must not become a second task ledger.
