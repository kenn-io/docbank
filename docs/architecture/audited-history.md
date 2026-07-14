---
title: Audited History
description: The planned indelible-history contract for protected directory scopes, content versions, backup, agents, the TUI, and the web portal.
---

# Audited history

!!! info "Planned — not yet implemented"
    Docbank does not yet record content replacements or audited history. This
    page is the definitive contract the Phase 2b implementation will follow.
    Current commands retain only the current file content and the metadata
    described elsewhere in these docs.

Full audit is an opt-in promise for records whose history matters more than
easy reclamation: tax documents, contracts, regulated work, or an external
application's archival collection. It retains every authoritative change and
every file version under a protected scope. Ordinary commands cannot prune,
empty, garbage-collect, or otherwise erase that history.

This is deliberately stronger than an ordinary version-retention policy.
Ordinary policy may eventually keep the newest *N* versions or discard old
ones after a chosen age. Full audit never applies those limits.

## Scope identity and sticky membership

An audit scope attaches to a directory's stable node ID, not its current path.
Renaming or moving the directory therefore moves the scope with it. Enabling a
scope runs as a daemon job behind the mutation gate and atomically records:

- the scope and its initial chain state;
- a baseline of the directory and every existing descendant; and
- explicit membership for every node in that baseline.

The policy is vault metadata, not `config.toml`. It therefore participates in
the same transactional authority, JSONL export, backup, and restore as the
documents it protects.

Membership is **sticky**. A member moved outside the directory remains audited;
otherwise moving a file out, deleting it, and moving it back would be a purge
escape. A file or subtree moved into an audited directory is enrolled with a
baseline in the same transaction as the move. New children inherit every audit
scope that protects their parent. Nested and overlapping scopes are allowed,
and membership is additive rather than replacing an earlier promise.

An external application or pseudo-folder uses the same model: an integration
projects its collection onto a stable Docbank directory/scope reference. The
core schema remains application-neutral and never contains product-specific
tables or path-name matching rules.

## What becomes history

The audit stream records successful authoritative mutations:

- initial creation or ingest;
- content replacement and reversion;
- rename and move, including old and new parent/name coordinates;
- tag changes and provenance additions;
- trash and restore; and
- audit enrollment and later inherited membership.

Each mutation has an immutable event identity, time, stable node ID, resulting
node revision, operation, canonical post-change state, and the relevant prior
state. The origin distinguishes import, API/CLI mutation, or daemon job. A
caller-supplied agent label may be useful provenance, but is not presented as a
verified human identity while Docbank remains a single-user system.

Reads, searches, verification runs, extraction-cache refreshes, and physical
loose/pack movement are not document changes. They may have operational logs,
but do not create document-history events.

### Content versions

A document node remains the stable identity while its content pointer changes.
Every changed head receives a stable content-version identity recording the
blob hash, size, media type, time, and node revision that introduced it. The
previous head remains an immutable version. Reverting creates a new head that
references the chosen old bytes; it never removes the intervening history.

Blob deduplication remains valid. Two versions or nodes may reference the same
SHA-256 object, but they remain distinct historical facts. Re-submitting bytes
that leave all authoritative state unchanged is a no-op, not a fabricated
version.

## Tamper evidence and the guarantee boundary

Canonical mutation records are hashed. Each affected audit scope appends a
chain entry containing that mutation hash and its previous chain head; the
scope authority records the expected entry count and current head. This lets
verification and import detect changed, reordered, duplicated, or truncated
history without duplicating the canonical mutation payload for overlapping
scopes.

The guarantee is application-enforced and tamper-evident. It protects against
ordinary Docbank commands, API clients, maintenance, software mistakes, and
incomplete metadata restores. It cannot make bytes metaphysically indelible to
the operating-system account that can rewrite SQLite, packs, the executable,
and every backup. Backup manifests and externally recorded audit-chain heads
provide stronger independent evidence.

## Deletion and storage maintenance

`docbank rm` remains a reversible trash operation for audited nodes. Restore
continues to work normally, and both transitions appear in history.

An executed `trash empty` selection that contains any audited member fails as
one operation. It does not silently skip protected nodes or partially empty
the selection. Version pruning likewise refuses audited versions. Dry runs
report which scopes and nodes prevent deletion.

Every current and historical content hash protected by audit remains a blob
reachability root, so GC cannot revoke its catalog authority or unlink its
loose bytes. Repack is allowed because physical pack identity is not part of
the audit promise: it must copy and verify every protected live blob before it
can publish replacement mappings or retire sparse source packs.

There will be **no audit-destruction command in v1**. If a real need later
justifies one, it must be a separate daemon-exclusive recovery workflow—not a
flag on `rm`, `trash empty`, `gc`, version pruning, the TUI, or the normal web
portal. It must identify one scope by stable ID, produce a dry-run impact
inventory, require a deliberately difficult interactive confirmation, and stay
out of ordinary agent guidance.

## JSONL, backup, and restore

Deterministic JSONL is the portable authority for audit metadata. The metadata
format will be versioned to include:

- audit scopes and their expected chain count/head;
- sticky node memberships and enrollment baselines;
- canonical mutation records and per-scope chain entries; and
- stable content-version records referencing every retained blob.

Export orders those records deterministically from one pinned metadata
snapshot. Import into a fresh current-schema store validates IDs, membership
topology, event order, chain hashes and heads, node revisions, and every
protected blob reference in one transaction. Unknown audit records, missing
events, dangling versions, or a history downgrade fail the import; they never
produce a current-tree-only restore.

Portable backup capture includes every blob reachable from a current audited
head or historical version. Incremental snapshots may reuse existing backup
packs, but each snapshot's logical metadata describes the complete audit
authority at that point. Verify and restore must reproduce identical scope IDs,
memberships, event count/heads, content hashes, and deletion protections across
loose and packed source vaults.

## One history model, several clients

The daemon API owns one bounded, cursor-paginated representation for audit
scope status, events, content versions, comparisons, and chain verification.
CLI, agents, TUI, and web clients consume that same model; none opens SQLite or
the blob store directly.

The event order is canonical and total, while clients project it in three
useful ways:

- a **scope timeline** aggregates changes to every sticky member;
- a **node timeline** follows one stable document or directory across paths;
  and
- a **version history** filters to content heads and reversions.

Interactive clients show newest first by default, but cursors preserve stable
forward/backward traversal and chain verification reads canonical order.
Mutations that share an operation identity—such as one subtree ingest or move—
may be visually grouped without collapsing their individual node events.

Comparison is type-aware but never invents semantic equivalence. Plain text
and canonical metadata can render inline or side by side. Images and PDFs may
render side by side when a safe viewer is available. Office and unknown binary
formats start with hash, size, media type, and metadata changes plus verified
download/open actions; richer format-aware comparison can be added later.

### CLI and agents

Planned CLI concepts are `audit enable`, `audit status`, `history`, `versions`,
and `audit verify`. Machine output uses stable scope, node, event, and version
IDs and explicit protection state. A refused deletion returns a structured
error naming the blocking scope rather than relying on prose parsing.

### TUI

The TUI is a focused operator browser with three coordinated panes:

1. the virtual tree with audited-scope and sticky-membership badges;
2. the selected scope/node's ordered changes and content versions; and
3. event detail showing path transitions, metadata, hashes, and verification.

```text
┌─ Tree ─────────────┐ ┌─ History ──────────┐ ┌─ Event / Version ─────────┐
│ ▾ taxes       [A]  │ │ content replaced   │ │ 2026-07-14T09:42:11Z      │
│   ▾ 2025      [A]  │ │ moved              │ │ /inbox/w2.pdf → /taxes/… │
│     w2.pdf     [A]  │ │ baseline enrolled  │ │ sha256:…  verified       │
│     return.pdf [A]  │ │                    │ │ compare · open · verify  │
└────────────────────┘ └────────────────────┘ └───────────────────────────┘
```

Selection in the tree drives the history pane; selecting an event or version
drives detail. Scope and node views are switchable without losing the selected
stable node. Filtering, comparison, external open, and chain verification are
first-class actions. Policy enablement may show a dry-run baseline inventory,
but exceptional destruction is absent.

It can render concise text or metadata differences. Rich PDF, office, image,
and binary comparison opens an external tool or directs the operator to the web
portal rather than overloading a terminal UI.

### Web portal and kit-ui

The web portal is the primary human history experience. It adds filterable
timelines, side-by-side version and metadata comparison, scope/member views,
chain and backup evidence, and progress for verification or restore jobs.
Reusable tree, timeline, diff, evidence, and job components belong in kit-ui
when they are application-neutral enough for Msgvault and later tools.

The primary layout keeps the virtual tree and current document context visible
while a history workspace supplies filters, compare selection, and an evidence
drawer. A scope dashboard summarizes enrolled nodes, protected current and
historical bytes, chain status, latest verification, and snapshots known to
contain the scope. Enabling audit is a reviewed workflow: preview the baseline
inventory and storage impact, name the scope, then confirm enrollment.

Neither UI presents exceptional audit destruction as an ordinary action.
