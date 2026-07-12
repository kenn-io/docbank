# Internal engineering documentation

This directory preserves maintained contributor context that should not ship on
the public documentation site. The build wrapper excludes it and the built-site
validator treats any leak as a failure.

## What belongs here

- [Decision records](decisions/README.md): durable choices, rejected
  alternatives, constraints, and consequences.
- Operational notes that are useful to maintainers but expose repository
  process rather than product behavior.

Public architecture belongs in `docs/architecture/`. A public page explains
how the implemented system works; an internal decision record explains why a
boundary was chosen and what would justify changing it.

Transient specs and execution plans belong in `docs/superpowers/` only while
work is active. When work ships, digest current behavior into public
architecture, preserve durable rationale in a decision record, and remove the
transient material. Git history remains the point-in-time implementation log.

## Maintenance rule

A change that contradicts an accepted decision must update the affected public
architecture and add a superseding decision record in the same PR. Do not
silently rewrite the original decision into a rationale its authors did not
make.
