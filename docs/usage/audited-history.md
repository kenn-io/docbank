---
title: Permanent Audited History
description: Permanently retain every version and recorded change beneath a reviewed directory scope.
---

# Permanent audited history

Full audit is for directories whose history matters more than reclaiming their
old bytes: tax records, contracts, long-lived agent archives, or an
application-owned collection. Enabling it makes a permanent promise:

- every node and retained content version in the reviewed scope becomes
  protected;
- new children inherit that protection;
- moving or trashing a protected node does not shed it;
- supported content, tree, provenance, and tag changes are appended to a
  tamper-evident history; and
- ordinary version pruning and permanent trash deletion cannot erase that
  authority.

This is stronger than ordinary version history. An unaudited file keeps all
versions by default, but `versions prune` may deliberately release old ones.
An audited version is a permanent reachability root. Docbank has no `audit
disable` command.

First activation also permanently retains one vault-wide metadata snapshot:
node names and topology plus tag, assignment, ingest, and provenance records,
including records outside the selected directory. Those unrelated documents
do not become audit members and their content versions are not protected by
the scope, but the enrollment-time metadata remains part of the evidence.

## Review before enabling

Enrollment is always a separate preview and execution. Preview does not change
the vault:

```bash
docbank audit enable /taxes
```

It reports:

- the stable target node and proposed scope identities;
- directories, files, and existing versions that will become protected;
- logical and unique blob bytes retained by those versions;
- unresolved retained trash origins, if any;
- the vault-wide topology and attached metadata, including records outside the
  selected scope, that will be permanently retained as audit evidence;
- the baseline digest and exact projected JSONL audit growth; and
- a one-use token with a ten-minute expiration.

The vault-wide evidence counts matter even when `/taxes` is a small subtree.
The allocation lineage commits the surrounding topology and attached metadata
needed to prove identities and detect rollback; it does not enroll unrelated
documents into the scope, but that enrollment-time metadata snapshot remains
permanent.

Read the preview before executing the command it prints:

```bash
docbank audit enable \
  --run \
  --token <preview-token> \
  --acknowledge-permanent-retention
```

The daemon recomputes the complete enrollment while holding the mutation gate.
If any document, path, version, tag, provenance fact, or allocator state changed
after preview, execution returns `audit_preview_stale` and changes nothing.
Preview again rather than retrying the old token. Tokens are consumed by one
execution attempt and disappear when the daemon restarts.

For automation, add `--json`. A caller may preview by stable directory
selector instead of path (the older `--node-id` form remains available):

```bash
docbank audit enable id:42 --json
```

## Inspect protection

Vault-wide status identifies the lineage, allocation head, scope target,
baseline, membership count, and current scope-chain head:

```bash
docbank audit status
docbank audit status --json
```

Supply a live path or stable node ID to inspect sticky membership:

```bash
docbank audit status /taxes/2026/return.pdf
docbank audit status id:57 --json
```

An empty timeline is not proof that a node is protected; use `audit status` and
require `protected: true` plus its scope and baseline identities.

## Read a node's history

Read canonical events for one protected document or directory by its live path:

```bash
docbank audit history /taxes/2026/return.pdf
docbank audit history /taxes/2026/return.pdf --json
```

Use a stable node ID when a document has moved, or while it is in trash:

```bash
docbank audit history id:57
```

Events are newest first. Each one identifies its immutable event and operation,
scope, recording time, origin, node revision before and after, and the fields
relevant to that event. Path events include old and new coordinates plus their
`live` or `trash` state; trash coordinates use the separate
`@trash/known/...` or `@trash/unknown/...` domain rather than pretending to be
live virtual paths. Content events include prior and resulting version IDs.
Tag and provenance events include their stable attachment identity and complete
before/after attachment state. Human output summarizes those changes. JSON
returns the complete typed projection.

The default page contains at most 50 events. Continue an older timeline with
the opaque cursor printed by human output or returned as `next_cursor` in JSON:

```bash
docbank audit history --node-id 57 --limit 50 --cursor <next-cursor> --json
```

The cursor is bound to the stable node and does not shift when newer events are
recorded. Do not parse or construct it. A cursor from another node returns
`invalid_audit_cursor`; a node outside every audit scope returns
`audit_not_enrolled`.

A protected node adopted during first enrollment may have no node-specific
events until its first later change. That empty timeline does not weaken its
baseline protection; `audit status` is the membership authority.

## Read a scope's history

Use the stable scope ID reported by `audit status` to read every canonical
event across its protected members:

```bash
docbank audit history --scope <scope-id>
docbank audit history --scope <scope-id> --limit 100 --json
```

Scope history answers “what changed anywhere under this permanent promise?”
without walking each member separately. Each event includes its stable node ID,
and human output prints that copyable `id:N` selector. The response also carries
the scope target, baseline, member count, entry count, and current chain head so
the timeline stays attached to its evidence authority.

Pagination uses the same newest-first ordering as node history. A scope cursor
is opaque, bound to that stable scope ID, and remains stable when later events
are appended. Reusing it with another scope returns `invalid_audit_cursor`.

## Verify the permanent evidence

Run the audit-specific verifier when you need a compact proof of the permanent
promise rather than a scan of unrelated vault content:

```bash
docbank audit verify
docbank audit verify --json
docbank audit verify --json > audit-evidence.json
docbank audit verify --expected audit-evidence.json
```

The command independently replays canonical history against the current node,
version, membership, topology, tag, and provenance projections. It then reads
every unique blob retained by protected history through catalog authority and
recomputes its SHA-256. Missing, corrupt, or unreadable protected bytes make the
command fail.

On success, the report contains the stable vault and allocation-lineage IDs,
the allocation entry count and head, the current operation high-water mark,
and every scope's entry count and chain head. The JSON `evidence` object is a
compact terminal bundle suitable for recording outside the vault. A later
verification can prove that exact bundle is a prefix of the current authority:
the vault and allocation-lineage identities must match, the recorded allocation
head must still exist at its recorded count, and every recorded scope head must
still exist at its recorded count. Equal chains pass, and chains with valid
later operations also pass. A missing, shorter, or divergent chain makes the
command fail with a structured evidence problem while still reporting current
metadata and protected-byte results.

`--expected` reads a successful active `audit verify --json` report, not an
unverified hand-written head. Keep that report outside the vault and retain the
corresponding verified backups. The proof cannot detect an attacker who can
replace both the vault and your separately recorded evidence; the external copy
is the trust anchor.

`docbank audit verify` hashes protected content only. `docbank verify` remains
the broader whole-vault check: it performs the same metadata and audit replay,
then hashes every cataloged blob, including content outside audit membership.

## What remains usable

Protected documents remain working documents. Docbank records direct creation
and filesystem ingest, verified content replacement and reversion, in-scope
moves and renames, reversible trash and restore, and tag creation, assignment,
rename, and deletion. These operations commit their metadata and history
together; a history failure rolls the visible change back.

`rm` remains a soft delete. The protected node stays in recoverable trash and
can be restored. Physical pack and repack maintenance also remains available
because it changes representation without removing logical authority, and GC
can remove only blobs that no content version retains.

Execution of `trash empty --run` and `versions prune --run` is currently
refused once audit authority exists. Their dry runs remain useful for impact
inspection. There is deliberately no exceptional audit-destruction command.

## Current boundary

The current public workflow enables the first audit scope in a vault. A second
`audit enable` returns `audit_already_enabled`; adding overlapping scopes is not
yet exposed. Status, mutation recording, JSONL export/import, and backup capture
all preserve the first scope's authority.

!!! info "Planned"
    Additional scope enrollment and rich TUI/web history views are not
    implemented yet. Backup restore already revalidates the portable JSONL
    authority before publishing a vault.
