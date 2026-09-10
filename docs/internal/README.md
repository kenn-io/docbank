# Internal design documentation

Use these guides to decide where a code change belongs and which contracts it
must preserve. They explain package ownership, design rationale, rejected
approaches, and constraints for contributors. Update the owning page when a
design changes.

The public [architecture overview](../architecture/overview.md) explains the
product model and links to each public contract. This internal directory adds
contributor guidance and is excluded from the public Zensical site.

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
- [CSV to PDF conversion](csv-pdf.md) — bounded local conversion, receipts,
  and the separate upload-authorization boundary.

## Documentation boundary

- **User and agent guides:** shipped capabilities, exact contracts, and current
  limitations.
- **Public architecture:** the user-visible model and durable design intent;
  future contracts appear only inside explicit planned callouts.
- **Internal design:** current mechanics and rationale for agents and
  developers, including consequences and constraints.
- **Roadmap:** one high-level public product-status view.
- **Work tracking:** lives outside this design tree and owns work items,
  ordering, ownership, blockers, and completion state.

There is no separate decision ledger. If the design changes, revise the
relevant living page so a new contributor can learn the current system without
replaying historical records. Git history preserves the older state. These
pages must not become a second task ledger.
