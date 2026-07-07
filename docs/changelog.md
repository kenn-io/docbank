---
title: Changelog
description: Release history.
---

# Changelog

docbank is pre-release; nothing has been versioned or announced yet.

## Unreleased

Phase 1 (Core) complete:

- Virtual tree store (SQLite, FTS5) with schema-enforced invariants
- Content-addressed blob store with durable write discipline
- Idempotent recursive import with collision suffixing and provenance
- Full core CLI: `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`,
  `search`, `trash list|empty`, `gc`, `verify`
- Trash → empty → GC deletion pipeline with `verify` integrity checking
- Inter-process vault locking (shared for commands, exclusive for `gc`)
