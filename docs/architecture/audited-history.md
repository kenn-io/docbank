---
title: Audited History
description: The planned permanent, tamper-evident history contract for protected directory scopes, content versions, backup, agents, the TUI, and the web portal.
---

# Audited history

!!! info "Planned — not yet implemented"
    Docbank does not yet record content replacements or audited history. This
    page is the definitive contract the Phase 2b implementation will follow.
    Current commands retain only the current file content and the metadata
    described elsewhere in these docs.

Full audit is an opt-in promise for records whose history matters more than
easy reclamation: tax documents, contracts, regulated work, or an external
application's archival collection. Its enrollment baseline adopts every
current node and every content version Docbank still retains, then it records
every authoritative change and content version from that point forward.
Ordinary commands cannot prune, empty, garbage-collect, or otherwise erase
that protected history. Enrollment cannot reconstruct changes or versions
that were already discarded before the baseline.

Enrollment is also **permanent in v1**: membership is additive-only, there is
no `audit disable`, and no ordinary command destroys audited history. Enabling
a scope is an irreversible commitment — every protected version's bytes remain
reachable forever — so every enablement surface treats it as a deliberate
two-step act, never a side effect.

This is deliberately stronger than an ordinary version-retention policy.
Ordinary policy may eventually keep the newest *N* versions or discard old
ones after a chosen age. Full audit never applies those limits.

## Scope identity and sticky membership

An audit scope attaches to a directory's stable node ID, not its current path.
Renaming or moving the directory therefore moves the scope with it. Enabling a
scope runs as a daemon job behind the mutation gate and atomically records:

- the scope and its initial chain state;
- one shared baseline batch containing the directory, every live descendant,
  every discoverable pre-enrollment trash root whose recorded origin ancestry
  reaches that directory, and all descendants of those trash roots;
- every content version Docbank still retains for those nodes;
- every authoritative attachment for those nodes: tag assignments with their
  stable tag definitions, plus provenance records with their referenced ingest
  records; and
- explicit membership for every node in that batch.

V1 scopes have no separate mutable display name. Their user-visible identity is
the target directory's current path plus the stable scope UUID and target node
ID; a moved or renamed target therefore needs no second label to reconcile.
CLI, agent, TUI, web, JSONL, pending recovery, and backup surfaces all use that
same identity. Adding aliases later would require an explicit portable schema
and audited transition rather than an unbound UI field.

The baseline batch is an immutable canonical snapshot, not only an
implementation scan. Its digest covers the scope and target IDs; every adopted
node and membership; their enrollment-time state and immutable trash-origin
state; and every complete content-version record and authoritative attachment
retained at enrollment. Attached records use canonical stable-ID and field
ordering, so tag definitions, assignments, provenance, and referenced ingest
facts produce the same digest on every platform. The first scope-chain entry
commits that digest. Later mutations append events; they never rewrite the
frozen batch records.

For every adopted node, the batch's `member_state` records the exact
post-operation node revision and current content-version ID; directories use an
absent version. A file's current ID must resolve to exactly one version in that
same batch, while every other retained version is historical. Replay initializes
the scope's member-authority projection from these records rather than guessing
the head from version timestamps or introducing revisions.

The batch also captures the minimal **path-topology projection** needed to
derive every adopted member's canonical path at that boundary. It contains a
deduplicated, canonically ordered topology record for each member and every
ancestor on its live or immutable trash-origin spine: stable node ID, parent ID,
name, immutable file/directory kind, canonical creation/modification/trash
timestamps, and live, trash, or tombstone state. A known spine ends at the vault
root; the unknown-origin representation defined below ends at its
domain-separated sentinel. These witness records participate in the batch
digest. Witnessing an unaudited ancestor does not enroll it, protect its
content, or give it history of its own; it preserves only the historical
topology on which an audited member's path depends.

Baseline cardinality is **one shared baseline batch per
`(scope_id, enrollment_target_node_id, operation_id)`**, not one baseline per
member. Initial scope enablement creates exactly one batch whose target is the
scope directory. A later operation creates one batch for each scope and
top-level subtree target that gains members. Overlapping scopes therefore get
separate batches even for the same target. If one operation names overlapping
targets within a scope, it first normalizes them to minimal non-overlapping
roots; a redundant descendant target folds into its nearest selected ancestor.

Each batch contains the canonically sorted set of nodes newly acquiring that
scope plus the complete post-operation version, trash-origin, and authoritative
attachment closure adopted for those nodes. Every new membership stores one
immutable reference to that shared batch. A node already in the scope is not
included or re-baselined; it receives the ordinary transition event instead.
No membership can appear in two batches for the same scope and operation.

Every baseline binding produces exactly one enrollment event, never one event
per adopted member. Its `node_id` and `target_node_id` both equal the batch's
enrollment target, and its `baseline_digest` equals that binding's digest. The
kind is `audit_enroll` when the operation creates the scope and `audit_inherit`
when it adds a batch to an existing scope. A newly created target therefore gets
one `audit_inherit` event per inherited-scope batch in addition to its
baseline-bound `node_create` and optional `content_create` events; its other
adopted members get no separate inheritance events. Import derives this
one-to-one event set from the sorted bindings and pre-operation scope set before
assigning event ordinals and rejects any missing, extra, or mismatched event.

Attachment values and references always come from the complete
**post-operation** projection. The pre-operation membership projection decides
only whether a node was already protected and how newly acquired memberships
are partitioned into batches; batches already assembled in the same operation
never influence either decision. Each batch independently includes the complete
post-operation tag definition and ingest/provenance record snapshot referenced
by its newly adopted members, even when another batch in that operation includes
the same stable record. Replay deduplicates identical stable identities and
rejects differing copies. Batch construction order therefore cannot change a
digest or decide which batch owns shared metadata.

The metadata-v2 bootstrap described in
[Editing and Versions](editing-and-versions.md) creates the stable vault ID and
portable version, tag, and ingest identities before any editing or audit writer
can run. Baseline and mutation hashes include the vault ID as domain separation,
and deterministic JSONL plus backup manifests preserve it. A restored copy
remains recognizably the same logical vault even when published at another
filesystem location.

Trash is detached from the live tree, so current parentage alone is not enough.
The first audit genesis and each later trash transition may seed an origin edge
from the then-available `trash_parent`, but the replayed vault-wide last-known
origin graph is authoritative for every post-activation enrollment. Closure
follows that graph, including through another trash root adopted by the same
baseline batch; a later locator clear cannot remove the edge. Adoption freezes
the applicable coordinates as immutable audit origin records. This closes the
escape where a file is trashed immediately before its directory is enrolled and
then emptied.
If earlier permanent deletion already erased the origin ancestry, Docbank cannot
infer that the remaining trash once belonged to the scope; the preview reports
the unresolved trash root without claiming it as a member.

The vault root is the exception: every node in the vault necessarily originated
beneath it. Enrolling the root adopts every detached trash root, descendant,
and retained version even when its recorded origin ancestry is missing. Root
enrollment therefore has no unresolved trash that remains eligible for later
emptying.

An adopted legacy trash root with lost ancestry receives one canonical
**unknown-origin** record. Its CAE2 nested record has kind `unknown_origin`, the
trash root's stable node ID, an absent parent, and an optional retained
origin-name byte string. It is never silently
replaced with a guessed parent. Replay gives it the non-resolving canonical
history path `@trash/unknown/<node-id>` and appends descendant names beneath
that path; user interfaces may show the retained origin name as a label but
must not present it as a recovered location. Restore places it at `/` using the
retained name, or deterministic `restored-<node-id>` when no name survived,
subject to the ordinary sibling-collision rules. JSONL preserves this record
verbatim. Non-root enrollment cannot infer membership from an unknown origin,
so those roots remain explicitly unresolved in its preview; root enrollment
adopts them all under this representation.

Once a node is audited, its trash event also persists immutable audit origin
metadata independent of the operational `trash_parent` foreign key: normally a
known parent ID and name, or the explicit unknown-origin record for adopted
legacy trash. Node IDs are never reused. `trash_parent` is a non-authoritative,
repairable locator: it is excluded from canonical event/baseline hashes, final
state reconciliation, and audit write guards. For a known origin, a non-null
locator must resolve to the immutable origin ID; null is valid after that parent
disappears. Deleting an unaudited origin directory may therefore clear the
locator without rewriting the protected origin coordinates, baseline digest,
or chain. Restore tries a known immutable parent ID and falls back to `/` when
that node does not resolve to a live directory—including when it is missing,
trashed, or no longer a directory—while history continues to show the original
intended location. Unknown origins use the explicit behavior above.

The policy is vault metadata, not `config.toml`. It therefore participates in
the same transactional authority, JSONL export, backup, and restore as the
documents it protects.

Because enrollment is irreversible, every enablement surface — CLI, API
clients and agents, TUI, and web — requires the same two-step shape. Preview
returns the baseline inventory, storage impact, unresolved trash origins, and
a **vault-wide metadata-retention disclosure** described below, plus a
short-lived server-issued token bound to the scope ID, baseline digest, vault
preview generation, and—on first activation—the exact topology- and
attached-metadata-genesis digests behind that disclosure. Enablement requires
that token and an explicit acknowledgment of both the scope promise and
vault-wide disclosure; it fails if the token is expired, belongs to another
scope, or the baseline or either genesis projection changed. A client therefore
cannot bypass review by calling the execution operation directly.

Preview tokens are one-use and held by the issuing daemon. Any intervening
authoritative mutation, successful enablement, or pre-activation change to a
genesis input advances the vault preview generation and invalidates every
outstanding token, even for a disjoint scope. Genesis inputs include repairable
trash locators: although a later locator clear is non-authoritative, before
genesis it can change the origin edge or unknown-origin record retained
permanently. This deliberately favors a simple exact review boundary over
concurrent enablement. Of concurrent executions, only the first matching token
can commit.
Expiration or daemon restart discards tokens without changing the vault, and
the client must preview again after `audit_preview_stale`.

The wire token is the unpadded base64url encoding of a cryptographically random
32-byte secret. Its stored digest is SHA-256 over the registered CAE2
`preview_token` record containing that secret and the exact scope, target,
vault ID, baseline digest, preview generation, operation ID, lineage ID, and
optional topology- and attached-metadata-genesis digests. Both genesis digests
are present for first activation and both are absent after lineage already
exists; mixed presence is invalid. Token validation decodes the secret,
reconstructs that record from server-held state, and compares the digest; the
raw secret never enters `audit_pending`, JSONL, or a backup.

For first activation, preview preallocates the random operation and lineage IDs,
operation sequence, event timestamp, and other non-derivable inputs used by its
baseline digest. It constructs the complete genesis projections, computes their
registered digests, and keeps those inputs in the server-side token state. The
displayed retention counts and serialized-byte estimates are deterministically
derived from those exact projections. Execution recomputes the baseline,
genesis digests, and disclosure under the mutation gate before accepting the
token. Expiration before execution discards the unused identities. Once
execution accepts the token, the same values and digests move into the durable
`audit_pending` record before the daemon discards its ephemeral token state.

Membership is **sticky**. A member moved outside the directory remains audited;
otherwise moving a file out, deleting it, and moving it back would be a purge
escape. A file or subtree moved or restored into an audited directory is
enrolled with one shared baseline batch per newly acquired scope in the same
transaction as that move or restore. Each batch applies the same origin-ancestry
closure as initial enrollment: it adopts the live subtree, all still-retained
versions, and every detached trash root whose recorded origin ancestry reaches
the newly enrolled subtree, including those roots' descendants, versions, and
authoritative attachments not already protected in that scope's pre-operation
state. Each batch's enrollment binding commits its canonical digest. New
children inherit every audit scope that protects their parent. That includes
children created beneath a sticky member directory after
it has moved outside the scope's current path: the protected directory continues
carrying the promise. Nested and overlapping scopes are allowed, and membership
is additive rather than replacing an earlier promise.

An inherited baseline batch is the canonical **post-operation** snapshot. For a
scope that already protected a node before the operation, replay applies the
normal pre-state-to-post-state mutation event. For a scope an existing node
joins because of that same move or restore, replay installs the post-operation
batch and does not also apply the triggering transition. The enrollment record
retains the operation identity and cause, but protection begins at that
post-state boundary. If a node was already in one scope and joins another, the
first scope receives the transition while the new scope receives only its shared
batch.

Creation is the deliberate exception because the creation itself is part of the
promised history. A node created beneath an audited parent emits `node_create`
for every inherited scope; a file also emits `content_create`. Those
**baseline-bound creation events** are committed alongside the shared
post-operation batch. Replay validates their absent pre-state, exact post-state,
topology delta, new content version, and inherited scope against that batch, but
does not apply them a second time to the member projection. A moved or restored
pre-existing node never fabricates creation events. Replay installs every
membership and adopted record in a batch atomically from its one binding; it
does not synthesize per-member baselines. One scope-chain entry may commit both
transition and batch categories in canonical order without omitting or
double-applying the mutation.

Path history follows a **path-affecting closure**, not only directly mutated
members. Renaming, moving, trashing, or restoring any directory finds every
audited descendant whose derived path changes, even when the directory itself
is unaudited. The same metadata transaction emits one scoped **net path event**
for each affected `(scope_id, member_node_id)`, containing the old path, new
path, old/new live-or-trash state, and the operation-level topology-delta
digest. Its event kind is always `node_path`, regardless of whether the atomic
delta combined rename, move, trash, restore, or several nested causes. Clients
derive human labels such as “restored” from the committed pre/post state; no
action-kind precedence affects hashing. A member affected by several causes
therefore receives one net event, not colliding per-cause events.

Every transaction represents all of its topology changes as one canonical
operation-level delta. The delta contains the complete pre/post record for each
directly changed node and is sorted by stable node ID and canonical field bytes.
Duplicate changes to one node and a cyclic or otherwise invalid final graph are
rejected. Sorting defines encoding, not execution order: `POST /batch/move` and
other multi-change operations are evaluated from one pre-state to one final
post-state and replay installs the delta atomically. Nested moves are therefore
unambiguous and independent of request, map, or traversal order.

The path-topology projection evolves with that delta. Moving a protected
subtree beneath a previously unwitnessed unaudited ancestry first records the
new ancestor-spine records needed by replay. The canonical net path-effect list
contains
`(scope_id, member_node_id, old_path, new_path, old_state, new_state)` and is
sorted by those fields' canonical byte encodings in that order. The state tokens
are `live` and `trash`. The mutation commits the topology delta and the effect
list's count and digest; each net event must carry exactly the same six values
plus that delta digest.

Canonical history paths are opaque byte strings derived from the replayed
topology, never host paths produced by `filepath`, Unicode normalization, or
display escaping. A node-name component must be nonempty, must not equal `.` or
`..`, and must contain neither NUL (`0x00`) nor slash (`0x2f`). Slash is the only
separator, separators are never doubled, and a trailing slash is invalid except
that the live vault root is exactly the single byte `/`. Every other live path
is `/` followed by its root-to-node components joined with `/`.

A trash path has a separate domain. A known-origin trash root is
`@trash/known/` followed by the root-to-origin-parent components, its immutable
origin name, and any descendant components, all joined with single `/` bytes;
the vault root contributes no empty component. An unknown-origin trash root is
`@trash/unknown/` followed by the adopted trash root's stable node ID in
shortest unsigned base-10 ASCII—no sign or leading zero—and then any descendant
components. Its optional retained origin name is display-only and never enters
the path bytes. A `path_state` with state `live` accepts only the `/` form;
state `trash` accepts only an `@trash/known/` or `@trash/unknown/` form. A known
spine must terminate at the vault root, while the unknown form always uses the
trash-root ID rather than the affected descendant ID. Baseline construction,
event creation, replay, and import independently derive these exact bytes and
reject a supplied path that differs.

Historical witness generations and deltas are immutable, while the **active
witness projection** is derived. A witness generation is keyed by node ID and
the operation that introduced it. It is active only while at least one audited
member's current path traverses that generation. A delta that removes the last
such dependency retires the active pointer without deleting history. If any
hashed `topology_node` field changes while the dependency remains—including a
`modified_at` touch that does not change a path—the same operation retires the
old generation and creates a replacement from the exact post-delta state. A
single operation may therefore contain a retire/create pair for one node. Every
scope with a member still depending on that witness commits the canonical
mutation even when the net path-effect list is empty.

Later changes to a retired node still appear as topology deltas in allocation
lineage but require no scoped path event. If an audited path depends on that
node again, the guarded operation records a new witness generation for its
current state before publication. Replay deterministically derives retire-only,
create-only, rotation, or no-change from the pre/post dependency and node-state
projections. Final-state reconciliation compares current nodes only with active
generations; historical generations are checked at their original replay
boundary rather than treated as stale current state.

Witness generations introduced inside an enrollment baseline are bound by that
batch's digest and canonical baseline binding; they do not also appear in a
witness-change list. Import installs them only after recomputing and accepting
the batch. This exemption is per witness creation, not per operation: when one
transaction contains baseline enrollment and ordinary transitions, every
creation or retirement not contained in a baseline remains in the operation's
canonical witness-change list, sorted by
`(node_id, witness_generation_operation_id, action_code)`. Creation records
also commit the canonical witnessed-state digest. The canonical mutation and
allocation-lineage entry both commit the list's count and digest, and import
derives the expected changes from the topology delta before accepting either
binding. Metadata format version 2 freezes the action codes as the lowercase
ASCII tokens `create` and `retire`; unknown codes are rejected. A later witness
generation can therefore neither be altered nor inserted after the fact without
changing both chains.

For a rotation, `retire` names the old generation's introducing operation and
has no state digest; `create` names the current operation as the new generation
and hashes the replay-derived post-delta `topology_node`. The two records have
distinct canonical sort keys. Omitting either half, retaining two active
generations for the same node, or rotating when the post-state is byte-identical
is invalid.

Authoritative attached metadata has the same replay boundary. Enabling the
first scope records a vault-wide, canonically sorted genesis projection of every
tag definition and assignment plus every ingest and provenance record. Its
record count and digest are committed by allocation-lineage genesis. Every
later transaction that changes that authority records one canonical
**attached-metadata delta** containing the complete pre/post record for each
changed entity, sorted by `(record_kind_code, CAE2(stable_identity))`. A
missing side
uses the format's absent-record sentinel, so tag deletion includes definition
and assignment tombstones while rename, assignment, provenance addition, and
supersession have unambiguous transitions. Metadata-format version 2 freezes
the record-kind codes as `ingest`, `provenance`, `tag_assignment`, and
`tag_definition`; a tag assignment's stable identity is `(tag_id, node_id)`.

This is one simultaneous pre-state-to-post-state delta, not a lossy summary of
an imperative edit sequence. A transaction may touch each attached-metadata
identity at most once. Batch normalization rejects duplicate touches before
writing, and database guards roll back a helper or direct statement sequence
that touches the same identity again. Rename-away/rename-back and
unassign/reassign in one transaction are therefore invalid rather than encoded
as an unchanged record. An equal pre/post record is omitted and cannot claim an
event.

The allocation-lineage entry always commits the attached-metadata delta's count
and digest or an explicit no-attached-metadata-change marker. When the operation
has audited effects, its canonical mutation commits the identical count and
digest. Import replays the delta from the genesis projection, derives the exact
memberships and scopes affected by each transition, and requires the resulting
fan-out events and audited/no-audited mutation marker. Thus an unaudited tag
change still advances independently verifiable authority, while an omitted
unassign/reassign or rename-away/rename-back cannot hide behind an unchanged
final projection.

Fan-out is derived mechanically from that simultaneous transition:

- a tag definition's absent-to-present, changed-name, and present-to-absent
  transitions produce `tag_define`, `tag_rename`, and `tag_delete`; their
  candidate nodes are the union of assignments to that tag in the pre- and
  post-operation projections;
- an assignment's absent-to-present or present-to-absent transition produces
  `tag_assign` or `tag_unassign` for its node;
- deleting a tag therefore records the definition tombstone and every cascading
  assignment tombstone, producing both `tag_delete` and `tag_unassign` for each
  pre-assigned audited member. Creating and assigning a tag in one transaction
  similarly produces both `tag_define` and `tag_assign`; and
- a new provenance fact produces `provenance_add`, or
  `provenance_supersede` when it carries a supersession edge, for its attached
  node. An ingest-record insertion alone has no scoped event; it becomes part of
  an enrollment baseline or the referenced input to a provenance transition.

For every candidate node, events fan out to each scope that protected it in the
pre-operation membership projection. A scope acquired by an existing node in
the same operation receives only its canonical post-operation baseline, never
duplicate transition events. A node created in the operation receives the
baseline-bound `node_create` and, for a file, `content_create` events defined
above in every inherited scope. The complete event sort key keeps definition
and assignment events distinct. These rules also govern combined
rename/assignment operations, so neither SQL cascade order nor request order
changes the audited effects.

### Guarded mutation closure

Once audit is enabled, inserting or deleting a node or changing any node's
parent, name, live/trash state, `created_at`, `modified_at`, or `trashed_at`
requires a guarded audit transaction even when the directly changed node is not
yet a member. The transaction reads the relevant pre-state under the
single-writer mutation gate and materializes an expected-effect set before it
changes authoritative node state:

- every node in an inserted or reparented subtree must retain its existing
  sticky memberships and, evaluated top-down, acquire the union of scopes
  carried by its post-operation parent;
- newly acquired memberships must be partitioned into exactly one shared
  post-operation baseline batch per normalized `(scope, target)` pair, and every
  new membership must reference exactly one such batch;
- from the frozen pre-state and final post-state, each batch must derive the
  complete detached-trash closure whose available origin ancestry reaches its
  newly adopted targets, recursively through other detached roots. The expected
  candidate set is the live subtree plus that closure and every descendant.
  After subtracting nodes that already carry the scope, the expected batch is
  exactly the remaining newly adopted nodes, every still-retained version, and
  every authoritative attachment for those nodes;
- every audited descendant whose derived path changes through an ancestor must
  have exactly one old-path/new-path event for each scope that protected it in
  the pre-operation state. A scope inherited by an existing node in this
  operation receives only its post-operation baseline binding; a newly created
  node also requires the exact baseline-bound creation events described above;
- the canonical operation-level topology delta, any newly required
  ancestor-spine witness creations, retirements, or state rotations, and the
  sorted net path-effect count and digest must describe exactly that same
  descendant-event set and active witness projection; and
- the canonical mutation, allocation-lineage entry, affected scope entries, and
  resulting count/heads must cover exactly those expected effects.

Tag, assignment, ingest, and provenance statements use the same guarded audit
context after activation. Before commit, their exact canonical
attached-metadata delta is compared with the rows changed by the transaction;
the replayed pre-state must match, immutable-record rules must hold, and the
derived affected audited members must match the emitted fan-out events and
mutation marker. A helper cannot omit an audited tag event merely because a
later change restores the same tag projection.

Database write guards reject the topology statement unless its transaction has
registered an audit operation context. Before commit, the shared mutation path
compares the materialized expectations with memberships, baselines, events,
lineage, and scope heads actually written. It also compares each baseline's
members, versions, and attachments with the derived trash-origin closure; a
missing detached root or extra adopted record rolls the whole transaction back.
Direct SQL, a helper that forgets inherited membership, and an
unaudited-ancestor rename that omits descendant events therefore fail rather
than creating a purge or history escape.

The same guards cover direct and cascading node deletion. Hard-deleting an
unaudited trash root after audit activation is allowed only when the protected
closure is empty and the transaction records every deleted subtree node as a
tombstone in its atomic topology delta and allocation-lineage entry. The
tombstone preserves the node's creation time, last parent/name/origin, and prior
trash time while setting `modified_at` to the deletion operation's canonical
timestamp. An audited member or missing tombstone aborts the whole deletion.
`trash empty` cannot use an unguarded legacy `DELETE` path for otherwise
eligible trash.

Verification and JSONL import independently enforce the same closure. Every
live parent/child edge must give the child at least the parent's memberships.
Import starts from the independently hash-bound vault-wide topology genesis and
replays every allocation-lineage topology delta to the enrollment boundary.
That projection contains every live and detached node plus each available or
unknown trash origin, not merely roots selected by the baseline writer. Import
derives the expected origin closure from that complete pre/post projection, then
requires exact equality with the batch's members, versions, and authoritative
attachments. An omitted detached root therefore cannot disappear through a
later `trash empty` and still leave an internally valid stream.

For a topology mutation, the verifier derives the affected memberships and
their old/new paths from the **previous** replayed path-topology projection and
the canonical operation-level delta before it trusts any claimed descendant
event. The derived canonical net list, count, and digest must exactly match both
the path-effect commitment and the scoped events; only then does replay install
the whole delta atomically. A missing event therefore remains detectable even
when another change in the same batch or a later mutation happens to restore or
otherwise mask the same final path.

Every node insertion or deletion and every parent, name, or trash-state mutation
after audit activation has a topology-delta record in its
allocation-lineage entry, even when the derived membership and net path-effect
sets are empty. Import replays that delta against the vault-wide topology and
active witness projections before accepting either a canonical mutation hash or
the no-audited-mutation marker. A lineage entry may claim no audited mutation
only when replay derives no membership, baseline, attachment, witness, or
scoped-event effect; omitting both a topology delta and its required audit
effects is therefore not a valid encoding of an ancestor change.

This replay authority has an intentional privacy and storage consequence that
is broader than the enrolled scope. First activation snapshots the complete
vault topology and authoritative attached metadata. Thereafter, lineage retains
names, parentage, trash origins, deletion tombstones, tag definitions and
assignments, and ingest/provenance values for every authoritative vault mutation,
including unaudited nodes. Ordinary deletion does not remove those historical
metadata values from JSONL exports or backups. Full audit does **not** retain an
unaudited file's content bytes or versions solely for this reason, but its
vault-wide metadata trail persists with the audit authority.

Every enablement preview states this distinction in plain language and reports
whether vault-wide lineage is newly activated or already active. On first
activation it inventories the topology and attached-metadata genesis by record
kind and estimated serialized bytes; confirmation explicitly accepts permanent
metadata retention outside the selected scope. CLI/agent JSON exposes the same
structured counts and acknowledgment requirement, and TUI/web clients may not
hide it behind a generic confirmation dialog.

An external application or pseudo-folder uses the same model: an integration
projects its collection onto a stable Docbank directory/scope reference. The
core schema remains application-neutral and never contains product-specific
tables or path-name matching rules.

## What becomes history

The audit stream records successful authoritative mutations:

- initial creation or ingest;
- content replacement and reversion;
- rename and move, including old and new parent/name coordinates;
- tag-definition and assignment changes, plus provenance additions and
  supersessions;
- trash and restore; and
- audit enrollment and later inherited membership.

Each mutation has an immutable event identity, time, stable node ID, resulting
node revision, operation, canonical post-change node state including its
authoritative timestamps, attached-metadata state, and the relevant prior state.
Changing a shared tag definition emits an event for every audited member
carrying that tag, across all affected scopes.
The origin distinguishes import, API/CLI mutation, or daemon job. A
caller-supplied agent label may be useful provenance, but is not presented as a
verified human identity while Docbank remains a single-user system.

Chain order is authoritative; wall-clock time is not. Event times are
canonical UTC values reported by the daemon's local clock, not trusted or
externally attested timestamps. Verification proves their recorded order and
integrity, not that the host clock was correct.

Reads, searches, verification runs, extraction-cache refreshes, and physical
loose/pack movement are not document changes. They may have operational logs,
but do not create document-history events. FTS rows, extracted-text cache rows,
and background-job state are derived or operational data, not authoritative
attachments, and do not enter baseline or mutation hashes.

### Attached metadata lifecycle

Tag definitions and assignments are mutable authority, so their create, rename,
assign, unassign, and delete operations emit the canonical fan-out events
described above whenever an audited member is affected.

A tag's stable identity is an opaque UUIDv4 generated from the operating
system's cryptographic random source, stored canonically under a unique
constraint, and never selected by a caller or reused. The tag name is mutable
and not identity. Deleting a tag removes its live definition and assignments but
does not erase its UUID from audit baselines or events; recreating the same name
always receives a new UUID. JSONL preserves tag UUIDs verbatim, so a stale tag
reference becomes not-found rather than silently naming a later tag. Import
rejects non-canonical or duplicate tag UUIDs.

Ingest and provenance records are different: their fields are immutable after
insertion. Every ingest record has an opaque UUIDv4 stable identity generated by
the operating system's cryptographic random source, stored canonically under a
unique constraint, never accepted from a caller, and never reused after
deletion. The metadata-v2 bootstrap assigns such an identity to every retained
legacy ingest record in the same daemon-exclusive transaction as the other
portable identities; a legacy integer row ID may remain an internal key but is
never the JSONL, provenance, or audit identity.

All filesystem-derived values represented as CAE2 `bytes` share one migration
rule: node names, known/unknown trash-origin names, provenance `original_path`,
and ingest `source_desc`. They are opaque bytes rather than UTF-8 text because
POSIX filesystems can contain arbitrary non-NUL path bytes. Human clients
display valid UTF-8 and escape other bytes; hashes and JSONL preserve the exact
sequence.

Direct-database bootstrap reads each corresponding SQLite value as raw bytes
without text coercion. A v1 JSONL import requires valid UTF-8 JSON and paired
Unicode escapes for every such string, then uses the exact UTF-8 encoding of the
decoded value as v2 bytes. Malformed input is rejected transactionally. An
already-created v1 JSONL stream is authority only for the text it contains—raw
bytes a legacy exporter had already replaced cannot be reconstructed. The
bootstrap/import report gives per-field converted counts and separately lists
records whose imported text contains U+FFFD as potentially legacy-lossy; that
code point remains valid data and is never silently changed. Fixtures cover
invalid-UTF-8 direct SQLite values, non-ASCII and U+FFFD v1 JSONL values,
malformed JSON strings, and v2 byte-exact round trips for every field kind.

Each provenance fact has a stable identity derived from its canonical immutable
fields, including that ingest UUID, and may carry an immutable `supersedes`
identity. A correction appends a new fact that supersedes the currently active
fact for the same node,
backed by a new ingest record when its source description changes; it never
updates or erases the old row. The superseded fact remains visible in history,
while the current provenance projection selects the unsuperseded leaves.
Constraints require the target to exist on the same node, permit at most one
direct successor, and reject cycles. Replay applies the supersession edge, and
baseline hashing plus import preserve and validate the full graph and its active
projection.

Legacy v1 permits duplicate byte-for-byte provenance rows. Before assigning v2
fact identities, bootstrap canonicalizes their fields and collapses each equal
`(node_id, ingest_uuid, original_path, original_mtime)` group to one fact in
deterministic tuple order. The derived stable identity belongs to that fact, not
to a legacy SQLite row ID; duplicates are semantically the idempotent no-op that
v2 enforces. Non-identical facts remain distinct. This normalization and the
number of collapsed rows are part of the bootstrap report, and migration tests
cover direct legacy stores plus v1 import streams containing duplicates.

Re-adding a byte-for-byte identical canonical provenance fact is an idempotent
no-op, not a duplicate row or event. Adding or superseding a fact on an audited
member emits an event in the insertion transaction. Database constraints and
audit write guards reject update or deletion of provenance attached to an
audited member and reject deletion of any ingest record it references. Those
records are permanent metadata retention roots just like the member's versions.
Ordinary policy may delete facts belonging only to unaudited nodes; later
enrollment adopts only facts still retained in its baseline batch. A deletion
set must be referentially closed: every fact whose `supersedes` points at a
deleted fact is deleted in the same transaction, and deleting an ingest requires
deleting every fact that references it. Every member of that closure must remain
wholly unprotected, and the post-state may contain no dangling edge.

### Content versions

A document node remains the stable identity while its content pointer changes.
Every head has a stable content-version identity recording the blob hash,
size, media type, time, and node revision that introduced it. Initial ingest
creates the first version and the node references it as its current version.
Replacement and reversion each create a new version and atomically advance that
reference; the previous head remains an immutable version. A reversion also
names the selected historical version UUID. That source must already belong to
the same node, and its blob hash, size, and media type must exactly match the new
head; the new version still receives its own UUID, timestamp, and introducing
revision. Enrollment adopts all existing version identities rather than
assigning new ones. Reverting may reference the same bytes as another version,
but its source identity makes the user's selection unambiguous and never removes
the intervening history.

A content-version ID is an opaque UUIDv4 generated from the operating system's
cryptographic random source and stored in canonical lowercase form under a
unique constraint. A collision retries with a new UUID. It is never derived
from a node ID, blob hash, timestamp, or sequential allocator, and a deleted
unaudited version ID remains globally non-reusable by contract. Writers neither
accept caller-selected version IDs nor intentionally issue a prior value; the
UUID's random namespace makes accidental reuse negligible. JSONL preserves the
UUID verbatim and import validates its canonical form and uniqueness. A stale
reference to a pruned version therefore becomes not-found; it cannot silently
retarget a later version after export/import.

A metadata transaction may create at most one content version for a given node.
Every native version records that transaction's immutable operation ID, and
the pair is unique among records whose `introduced_operation_id` is present.
Legacy absent values do not participate in that partial uniqueness rule. For a
node protected in the pre- or post-operation scope projection, that version has
exactly one canonical content transition whose event fans out identically under
the membership rules above: pre-protected scopes receive it, and scopes
inherited in the same operation receive it only for creation. Every copy has the
same event kind, pre/post version IDs, and optional source version. Creation uses
`content_create`; an existing-node update without a selected source uses
`content_replace`; and `content_revert` is valid only when the caller selected
the verified non-current source version. A second content touch, a second
native version for the node/operation, mixed replace/revert kinds across scopes,
or a new audited head without its complete fan-out aborts the transaction.

The immutable native version itself stores that transition kind and optional
source-version ID, so intent remains authoritative even when the node is wholly
unaudited and emits no scoped event. `content_create` and `content_replace`
require the source absent; `content_revert` requires it present and resolving to
the verified non-current version described above. Version-level import requires
that source to be a different retained version of the same node with matching
blob hash, size, and media type. A source with a known node revision must have a
smaller revision than the revert; only a migrated source whose revision is
actually absent uses the legacy ordering exemption. Cycles are invalid. Audited
replay additionally proves the source existed in the operation's pre-state. A
retained revert version keeps its source version as a metadata and
blob-reachability dependency.
Unaudited pruning must therefore remove the complete dependent closure or leave
the source intact; audited retention permits neither removal.

A wholly unaudited node still records the introducing operation ID on its native
version but emits no scoped event; this preserves the stated boundary that
allocation lineage does not retain unaudited content. Later enrollment adopts
that complete version record in its baseline.

Blob deduplication remains valid. Two versions or nodes may reference the same
SHA-256 object, but they remain distinct historical facts. Re-submitting bytes
that leave all authoritative state unchanged is a no-op, not a fabricated
version.

## Tamper evidence and the guarantee boundary

Each audited authoritative operation has exactly one canonical mutation record.
Its hash includes the stable vault ID, operation sequence, random operation ID,
canonical operation timestamp, origin, optional agent label, ordered node
events, every sorted enrollment-baseline binding, and any canonical
operation-level topology delta, net path-effect count/digest, and witness-change
count/digest and attached-metadata-delta count/digest. Each affected
audit scope appends a chain entry containing that mutation hash and its previous
chain head; the scope authority records the expected entry count and current
head. This lets verification and import detect changed, reordered, duplicated,
or truncated history without duplicating the canonical mutation payload for
overlapping scopes.

Attribution is operation-level authority, not an incidental event field. Every
canonical mutation carries exactly one `recorded_at`, `origin`, and optional
`agent_label` tuple even when its event list is empty, as for a witness-only
rotation. The mutation-level `origin` is restricted to the exact ASCII tokens
`api`, `cli`, `import`, or `job` regardless of whether events exist. Every event
in the mutation must repeat that exact tuple. A native content version introduced
by the operation uses the same `recorded_at`, and every direct post-state node
timestamp required to equal the operation time uses that value. An unknown
mutation origin, conflicting event attribution, or a missing eventless
attribution tuple is invalid before hashing.

Enrollment is a canonical mutation whose hash includes every sorted baseline
batch binding. Verification, JSONL import, and restore recompute each batch
digest from its immutable snapshot and member records before accepting the
scope-chain entry. They then verify or replay subsequent mutations in canonical
order and reconcile the resulting final state with current node, membership,
version, tag-assignment, tag-definition, provenance, and referenced-ingest
metadata for every audited member. Current mutable state is never substituted
for enrollment-time inputs.
Baseline membership, version, or authoritative attachment metadata therefore
cannot change independently of the recorded scope head, while valid later
changes do not invalidate it.

The authoritative mutation and its audit effects are one SQLite transaction.
Node state, content-version records, authoritative attachment changes, history
events, membership changes, every affected scope-chain entry, and each scope's
expected count/head either all commit or all roll back. Durable blob publication
may precede that transaction; a rollback can leave only an unreferenced blob
eligible for later GC, never a new document head without matching history.

An authoritative operation is exactly one committed SQLite metadata
transaction. It receives one monotonically increasing vault-wide
operation-sequence number and one cryptographically random operation ID; neither
identity is shared with another transaction. Before hashing, its events are
sorted by the complete tuple
`(node_id, event_kind_code, scope_id, target_node_id,
attachment_kind_code, attachment_identity)`. IDs use their canonical byte
encoding, and an absent field uses a fixed empty sentinel that sorts before a
present value. Attachment identities compare by the complete CAE2 bytes of the
registered typed identity record; there is no ad hoc tuple concatenation.
Emitting two events with the same complete key is an invariant violation rather
than an invitation to preserve discovery order.

### Normative audit hash encoding

Every metadata-v2 audit digest uses SHA-256 over the **CAE2** typed binary
encoding below. JSONL is only a transport representation; whitespace, object
key order, escaping choices, and decimal rendering never enter a hash.

Let `F(b)` be an unsigned 64-bit big-endian byte length followed by `b`, and let
`U(n)` be an unsigned 64-bit big-endian integer. A record is exactly:

```text
F("docbank-audit") || U(2) || F(record_kind) || U(field_count) ||
    each F(field_name) || value, sorted by field_name ASCII bytes
```

`record_kind` and `field_name` are the lowercase ASCII tokens fixed by the v2
schema. Every declared field appears exactly once; an optional field uses the
absent value rather than disappearing. Unknown, missing, or duplicate fields
are invalid. Values have one-byte type tags followed by:

| Tag | Value | Encoding |
| --- | --- | --- |
| `00` | absent | no payload |
| `01` / `02` | false / true | no payload |
| `03` | unsigned integer | `U(n)` |
| `04` | signed integer | eight-byte big-endian two's-complement |
| `05` | bytes | `F(raw_bytes)` |
| `06` | text | `F(exact_UTF-8_bytes)` |
| `07` | timestamp | `F(YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ)` |
| `08` | UUID | 16 canonical parsed bytes |
| `09` | SHA-256 digest | 32 raw bytes |
| `0a` | list | `U(count)` followed by each complete typed value |
| `0b` | nested record | `F(the complete CAE2 record)` |

In the schema registry below, `?T` is a field of type `T` whose absent form uses
tag `00`, and `[T]` is a list. Tokens, field names, and types are exact; changing
any of them requires a later metadata format. `state` is text restricted to
`live`, `trash`, or `tombstone`, and `action` is text restricted to the stable
codes already defined for that record.

Nested record schemas are:

| `record_kind` | Exact fields |
| --- | --- |
| `unknown_origin` | `node_id:u64`, `parent_id:?u64` (always absent), `name:?bytes` |
| `known_origin` | `node_id:u64`, `parent_id:u64`, `name:bytes` |
| `topology_node` | `node_id:u64`, `parent_id:?u64`, `name:bytes`, `node_kind:text`, `state:state`, `origin:?record`, `created_at:timestamp`, `modified_at:timestamp`, `trashed_at:?timestamp` |
| `content_version` | `version_id:uuid`, `node_id:u64`, `blob_hash:digest`, `size:u64`, `media_type:?text`, `recorded_at:timestamp`, `node_revision:?u64`, `version_origin:text`, `introduced_operation_id:?uuid`, `transition_kind:?text`, `source_version_id:?uuid` |
| `tag_definition` | `tag_id:uuid`, `name:text` |
| `tag_assignment` | `tag_id:uuid`, `node_id:u64` |
| `ingest` | `ingest_id:uuid`, `started_at:timestamp`, `source_kind:text`, `source_desc:bytes` |
| `provenance_identity` | `node_id:u64`, `ingest_id:uuid`, `original_path:?bytes`, `original_mtime:?timestamp`, `supersedes:?digest` |
| `provenance` | `identity:digest`, `node_id:u64`, `ingest_id:uuid`, `original_path:?bytes`, `original_mtime:?timestamp`, `supersedes:?digest` |
| `tag_definition_identity` | `tag_id:uuid` |
| `tag_assignment_identity` | `tag_id:uuid`, `node_id:u64` |
| `ingest_identity` | `ingest_id:uuid` |
| `provenance_identity_ref` | `identity:digest` |
| `event_identity` | `operation_id:uuid`, `event_ordinal:u64` |
| `member_state` | `node_id:u64`, `node_revision:u64`, `current_version_id:?uuid` |
| `member_state_change` | `node_id:u64`, `prior_revision:u64`, `resulting_revision:u64`, `prior_current_version_id:?uuid`, `resulting_current_version_id:?uuid` |
| `witnessed_state` | `node:topology_node` |
| `witness` | `node_id:u64`, `generation_operation_id:uuid`, `state_digest:digest` |
| `baseline_binding` | `scope_id:uuid`, `target_node_id:u64`, `baseline_digest:digest` |
| `topology_change` | `node_id:u64`, `pre:?topology_node`, `post:?topology_node` |
| `path_state` | `path:bytes`, `state:state` |
| `path_effect` | `scope_id:uuid`, `member_node_id:u64`, `old:path_state`, `new:path_state` |
| `witness_change` | `node_id:u64`, `generation_operation_id:uuid`, `action:action`, `state_digest:?digest` |
| `attached_metadata_change` | `record_kind:text`, `stable_identity:record`, `pre:?record`, `post:?record` |
| `audit_event` | `event_id:digest`, `operation_id:uuid`, `node_id:u64`, `event_kind:text`, `scope_id:uuid`, `target_node_id:?u64`, `attachment_kind:?text`, `attachment_identity:?record`, `source_version_id:?uuid`, `event_ordinal:u64`, `recorded_at:timestamp`, `prior_node_revision:u64`, `resulting_node_revision:u64`, `prior_current_version_id:?uuid`, `resulting_current_version_id:?uuid`, `origin:text`, `agent_label:?text`, `pre:?record`, `post:?record`, `topology_delta:?digest`, `baseline_digest:?digest` |

In that table, `record` means one complete nested CAE2 record of the applicable
registered kind. An attached-metadata change permits only `tag_definition`,
`tag_assignment`, `ingest`, or `provenance`; an event's pre/post kinds are fixed
by `event_kind` (`path_state` for `node_path`, `content_version` for content
events, and the matching attached record for tag/provenance events). A
`topology_node.origin` permits only `known_origin` or `unknown_origin`.
An attached-metadata identity uses `tag_definition_identity`,
`tag_assignment_identity`, `ingest_identity`, or `provenance_identity_ref` as
selected by `record_kind`; event attachment identity uses the same matching
record. No other identity record is valid.

Origin presence and identity are exact. The unique vault root is a live
directory with absent `parent_id`, empty `name`, and absent `origin`. Every
other live node has a present live-directory parent, a valid non-root name, and
absent `origin`. A current trash root has state `trash`, an operational
`parent_id` equal to the vault root, a valid non-root name, and exactly one
origin record. That origin's `node_id` must equal the enclosing
`topology_node.node_id`. A `known_origin` has a different positive `parent_id`
and a valid non-root `name`; its replayed last-known parent graph must be acyclic
and terminate at the vault root. An `unknown_origin` has the registered absent
parent and an absent or valid non-root retained name. The known name and any
present unknown retained name equal the enclosing trash root's topology name
byte-for-byte. Neither form may appear on the vault root, a live node, or a
trash descendant.

Every trash descendant has absent `origin`, a parent in the same detached trash
subtree, and the same non-null `trashed_at` as its unique trash root. No live
node may descend from a trash node. A tombstone is valid only as the post-side
of a deletion delta with a live or trash pre-side for the same node. It copies
that pre-side's parent, name, kind, creation time, trash time, and origin
verbatim, while its modification time is the deletion time. Consequently only
a tombstoned former trash root may retain an origin, and it retains the matching
node ID and known/unknown presence exactly; tombstoning cannot add, remove, or
rewrite origin authority. Baseline, genesis, delta, replay, JSONL import, and
restore validation reject every record that violates these relationships before
using it for trash closure or hashing.

`content_version.version_origin` is one of the exact ASCII tokens `native` or
`legacy_v1`. A native record requires both `node_revision` and
`introduced_operation_id` present, plus a `transition_kind` of
`content_create`, `content_replace`, or `content_revert`. Revert requires
`source_version_id` present; the other two kinds require it absent. Only a
`legacy_v1` record produced by the metadata-v2 bootstrap may omit the revision;
every legacy record requires operation ID, transition kind, and source version
absent, while the bootstrap's version for a node's then-current content is also
`legacy_v1` but retains its known revision. Import rejects invalid presence, a
duplicate present `(node_id, introduced_operation_id)` pair among native
records, and every unknown origin token before hashing or installing version
authority.

Every `topology_node` carries `node_kind` as one of the exact ASCII tokens `file`
or `dir`, plus canonical UTC `created_at` and `modified_at`. Kind and creation
time are immutable after insertion. Creation sets both timestamps to the
operation timestamp. A direct authoritative node mutation sets post-state
`modified_at` to that operation's timestamp; derived descendant path effects do
not alter the descendant timestamp. Live state requires `trashed_at` absent,
trash state requires it present and equal to the trash operation's timestamp,
and restore returns it to absent. A tombstone preserves the pre-state kind,
creation time, and `trashed_at` value (present or absent) and uses the deletion
operation's timestamp as `modified_at`. Import rejects any other presence
pattern or a delta that changes kind or `created_at`. Because these fields are
inside `topology_node`, baseline, genesis, witness, node-create, and
topology-delta hashes all bind them.

Event payload presence is exact:

| Event codes | `pre` / `post` | Other required optional fields |
| --- | --- | --- |
| `audit_enroll`, `audit_inherit` | both absent | `target_node_id` and `baseline_digest` present |
| `content_create` | absent / `content_version` | `source_version_id` and all topology/baseline fields absent |
| `content_replace` | `content_version` / `content_version` | `source_version_id` and all topology/baseline fields absent |
| `content_revert` | `content_version` / `content_version` | `source_version_id` present; all topology/baseline fields absent |
| `node_create` | absent / `topology_node` | `topology_delta` present |
| `node_path` | `path_state` / `path_state` | `topology_delta` present |
| `tag_define`, `tag_assign` | absent / matching tag record | attachment kind/identity present |
| `tag_rename` | `tag_definition` / `tag_definition` | attachment kind/identity present |
| `tag_delete`, `tag_unassign` | matching tag record / absent | attachment kind/identity present |
| `provenance_add` | absent / `provenance` | attachment kind/identity present |
| `provenance_supersede` | `provenance` / `provenance` | attachment kind/identity present |

Fields not required by the selected row are absent, except that `agent_label`
and both current-version fields follow the node rules below for every event.
`event_id`, `operation_id`,
`scope_id`, `node_id`, `event_kind`, `event_ordinal`, `recorded_at`,
`prior_node_revision`, `resulting_node_revision`, and `origin` are always
present. For each pre/post side where the node is a file, its corresponding
current-version field is present and names that side's head; for a directory or
a side where the node does not yet exist, it is absent. Node creation uses prior
revision zero and an absent prior version. Enrollment events still describe the
vault-wide operation's actual pre/post node state even though a newly acquired
scope installs only the post-operation baseline. `origin` is one of
the exact ASCII tokens `api`, `cli`, `import`, or `job`; `agent_label` is
unverified caller text. The tag “matching record” is `tag_definition` for
define/rename/delete and `tag_assignment` for assign/unassign. Import rejects
any other presence pattern before hashing.

Every event's `recorded_at`, `origin`, and `agent_label` must equal the enclosing
`canonical_mutation` fields byte-for-byte. For the optional label, equality
distinguishes absent from present-but-empty. Scope fan-out repeats the tuple
unchanged; no scope, event kind, importer, or user interface may rewrite it.
The native content version introduced by a content event has the same
`recorded_at`, and node timestamps governed by the operation-time rules above
use it as well. Import verifies these bindings before event sorting or hashing.

For `content_revert`, `source_version_id` must resolve in the pre-operation
version projection to a retained, non-current version belonging to `node_id`.
Its blob hash, size, and media type must equal the post version's corresponding
fields, while the post version has a distinct newly allocated ID and the
operation's resulting revision. Replay and import verify that relationship
before event sorting or hashing. Every other event kind requires
`source_version_id` absent.

For each `(operation_id, node_id)`, content events must have exactly one of the
three content kinds. Every scoped copy must carry the same kind, pre/post version
records, source-version ID, node revisions/current heads, recorded time, origin,
and agent label; only `scope_id`, `event_id`, and the canonically derived
`event_ordinal` vary. The post record's `introduced_operation_id` must equal the
event operation ID. The post record must be the one new native version and the
node's resulting current head. Import derives the required scope fan-out from
pre/post membership and rejects a missing, extra, or mixed-kind event.
The event kind and `source_version_id` must also equal the post version's
immutable `transition_kind` and `source_version_id` fields.

An attachment identity must equal the identity derived from its event payload.
Define/assign/add use the post record; delete/unassign use the pre record; tag
rename requires the same tag ID in pre and post and uses that ID. A
`provenance_supersede` event uses the **post/new** fact's
`provenance_identity_ref`; pre and post must name the same node,
`post.supersedes` must equal `pre.identity`, and the reference must equal
`post.identity`. A `provenance_add` reference likewise equals `post.identity`.
Import verifies these relationships before event sorting or hashing.

`member_state_changes` is one operation-level simultaneous list against a
single vault-wide audited-member projection, not one entry per event or scope.
Replay freezes the pre-operation projection and scope set, requires every
`prior_*` value for an already audited node to match, and validates all events
for the operation against the same pre/post change. It then applies each such
node's state change once. Every event for that node in a pre-existing scope
carries those same prior/result values. A direct node-row mutation
advances the revision by exactly one; a derived descendant-path event or shared
tag-definition fan-out leaves it unchanged. Current-version identity changes
only for content create/replace/revert and must resolve to the corresponding
post content-version record. If revision and current version are unchanged, no
state-change entry is emitted and the event carries equal prior/result values.

Only after those changes validate does replay install each newly acquired
scope's post-operation baseline. A node already audited in one scope therefore
advances once in the vault projection before another scope adopts the resulting
state; the new scope gets no transition event. A newly created node's
baseline-bound creation events are verified as the historical cause of that
post-state but produce no `member_state_change` and are not applied after
installation. A node first audited by several baselines in the same operation
is installed once from their required-identical post member state. Replay and
import finally require the projected revision and current-version ID of every
audited member to equal the current node table.

Hashed top-level record schemas are:

| `record_kind` | Exact fields |
| --- | --- |
| `enrollment_baseline` | `vault_id:uuid`, `scope_id:uuid`, `target_node_id:u64`, `operation_id:uuid`, `cause:text`, `members:[u64]`, `member_states:[member_state]`, `nodes:[topology_node]`, `versions:[content_version]`, `attachments:[record]`, `witnesses:[witness]` |
| `topology_genesis` | `vault_id:uuid`, `lineage_id:uuid`, `nodes:[topology_node]` |
| `attached_metadata_genesis` | `vault_id:uuid`, `lineage_id:uuid`, `records:[record]` |
| `topology_delta` | `operation_id:uuid`, `changes:[topology_change]` |
| `path_effect_list` | `operation_id:uuid`, `topology_delta:digest`, `effects:[path_effect]` |
| `witness_change_list` | `operation_id:uuid`, `changes:[witness_change]` |
| `attached_metadata_delta` | `operation_id:uuid`, `changes:[attached_metadata_change]` |
| `event` | `event:audit_event` |
| `canonical_mutation` | `vault_id:uuid`, `operation_sequence:u64`, `operation_id:uuid`, `grouping_id:?uuid`, `recorded_at:timestamp`, `origin:text`, `agent_label:?text`, `events:[audit_event]`, `member_state_changes:[member_state_change]`, `baselines:[baseline_binding]`, `topology_delta:?digest`, `path_effect_count:u64`, `path_effect_digest:?digest`, `witness_change_count:u64`, `witness_change_digest:?digest`, `attached_metadata_change_count:u64`, `attached_metadata_change_digest:?digest` |
| `scope_chain_entry` | `vault_id:uuid`, `scope_id:uuid`, `entry_count:u64`, `previous_head:?digest`, `mutation_hash:digest` |
| `allocation_genesis` | `vault_id:uuid`, `lineage_id:uuid`, `previous_head:?digest`, `node_id_high_water:u64`, `operation_sequence_high_water:u64`, `topology_count:u64`, `topology_digest:digest`, `attached_metadata_count:u64`, `attached_metadata_digest:digest` |
| `allocation_entry` | `vault_id:uuid`, `lineage_id:uuid`, `previous_head:digest`, `operation_sequence:u64`, `operation_id:uuid`, `allocated_node_ids:[u64]`, `node_id_high_water:u64`, `operation_sequence_high_water:u64`, `has_audited_mutation:bool`, `mutation_hash:?digest`, `has_topology_change:bool`, `topology_delta:?digest`, `has_witness_change:bool`, `witness_change_count:u64`, `witness_change_digest:?digest`, `has_attached_metadata_change:bool`, `attached_metadata_change_count:u64`, `attached_metadata_change_digest:?digest` |
| `preview_token` | `secret:bytes`, `vault_id:uuid`, `scope_id:uuid`, `target_node_id:u64`, `baseline_digest:digest`, `preview_generation:u64`, `operation_id:uuid`, `lineage_id:uuid`, `topology_genesis_digest:?digest`, `attached_metadata_genesis_digest:?digest` |
| `audit_pending` | `vault_id:uuid`, `scope_id:uuid`, `target_node_id:u64`, `preview_token_digest:digest`, `preview_generation:u64`, `baseline_digest:digest`, `operation_id:uuid`, `lineage_id:uuid`, `operation_sequence:u64`, `grouping_id:?uuid`, `recorded_at:timestamp`, `origin:text`, `agent_label:?text`, `cause:text`, `node_id_high_water:u64`, `operation_sequence_high_water:u64`, `topology_genesis_digest:digest`, `attached_metadata_genesis_digest:digest` |

The four `has_*` booleans are the normative allocation-entry no-change markers.
`false` requires the paired digest absent and count zero where a count exists;
`true` requires a
present digest and a positive count, except that a topology delta has no
separate count. In `canonical_mutation`, each count/digest pair uses zero plus
absent as its empty marker and positive plus present otherwise. Contradictory
combinations are invalid.

List order is also part of the registry: member IDs use unsigned numeric order;
allocated-node IDs preserve intrinsic allocation order; topology nodes/changes
use `node_id`; versions use
`(node_id, version_id)`; member states and state changes use `node_id`;
attachments and attached-metadata changes use
`(record_kind, CAE2(stable_identity))`; witnesses use
`(node_id, generation_operation_id)` and witness changes add `action`;
path effects use `(scope_id, member_node_id, old.path, new.path, old.state,
new.state)`; events use the complete event tuple defined below; and baseline
bindings use `(scope_id, target_node_id, baseline_digest)`. Genesis records use
the corresponding attachment or topology order. Duplicate set keys are invalid.

Text must be valid UTF-8 and receives **no Unicode normalization**; the exact
stored byte sequence is authoritative. A field that can contain opaque
filesystem bytes uses the bytes type instead. Empty text/bytes and absent are
therefore distinct. Timestamps are UTC with exactly nine fractional digits.
Lists representing sets are sorted by the tuple named for that record before
encoding; intrinsically ordered lists retain their specified order. Maps and
floating-point values are forbidden.

In metadata JSONL v2, a schema field of CAE2 type `bytes` is an unpadded
base64url JSON string; its registered field type distinguishes it from text.
The empty string encodes empty bytes and JSON `null` encodes an absent optional
bytes value. Export always emits this canonical spelling, and import rejects
padding, non-url alphabet characters, non-zero trailing bits, or any decoded
value whose re-encoding differs. This rule applies equally to node names,
filesystem paths, trash-origin names, and ingest source descriptions.

The digest of a provenance identity is
`SHA-256(CAE2("provenance_identity", fields))`; `supersedes` participates, so
an otherwise identical correction that points to a prior fact has a distinct
identity. The stored `provenance.identity` must equal that recomputed digest.
Likewise, `event_id` is the SHA-256 digest of the registered CAE2
`event_identity` record containing `operation_id` and `event_ordinal`, and
import recomputes it.

A witness `state_digest` is the SHA-256 digest of the registered
`witnessed_state` record containing the exact corresponding `topology_node`.
A baseline witness must match the unique topology node in that same baseline.
A later `create` witness change must match the node's replay-derived post-delta
topology state; `retire` requires an absent `state_digest` and an existing active
generation. When that node remains a dependency and its state changes, the same
list requires the matching retire/create rotation described above. Import
recomputes these relationships before accepting the witness list or either chain
binding.

The digest of a baseline, event, topology delta, net path-effect list,
witness-change list, attached-metadata delta, canonical mutation, scope-chain
entry, allocation genesis/entry, topology genesis, attached-metadata genesis, or
preview-token/audit-pending record
is `SHA-256(CAE2(record_kind, fields))` using its distinct lowercase kind token.
A scope-chain entry includes the stable vault and scope IDs, entry count,
optional previous head, and mutation digest. An allocation entry includes every
field and explicit no-change marker specified below. Allocation genesis encodes
its registered `previous_head` field as absent, never as an all-zero digest; its
digest is the required `previous_head` of the first allocation entry. Composite
records embed child digests with the digest type; no implementation hashes
unframed concatenated values.
Format-v2 golden vectors cover every record kind, absent versus empty values,
Unicode byte distinctions, integer limits, and timestamp encoding; export,
import, verification, and restore must all reproduce them byte-for-byte.

Kind codes are stable lowercase ASCII tokens frozen by the metadata-format
version, not implementation-assigned ordinals. Metadata format version 2 event
codes are:
`audit_enroll`, `audit_inherit`, `content_create`, `content_replace`,
`content_revert`, `node_create`, `node_path`,
`provenance_add`, `provenance_supersede`, `tag_assign`, `tag_define`,
`tag_delete`, `tag_rename`, and `tag_unassign`; attachment codes are
`provenance` and `tag`. Canonical bytewise token order determines sorting. A
later format may add codes but never remaps an existing token, and import rejects
an unknown or non-canonical code for the declared format version.

The zero-based position after that sort becomes `event_ordinal`. The canonical
total order is therefore
`(operation_sequence, event_ordinal)`, independent of filesystem walk, map
iteration, or SQL query order. Each affected scope appends one chain entry that
commits the operation's ordered event hashes. Enrollment-baseline bindings are
encoded as `(scope_id, target_node_id, baseline_digest)` and sorted by canonical
scope ID, target node ID, then digest bytes before the canonical mutation hashes
them. Overlapping scopes can therefore enroll the same subtree without query or
map order affecting the result.

Canonical topology-delta records are sorted separately by changed node ID and
their complete pre/post field bytes, then hashed as one atomic delta. Net path
events are unique by `(scope_id, member_node_id)` within the operation and carry
that delta digest, so multiple or nested batch-move causes cannot collide under
the event key or acquire an incidental replay order.

A higher-level command or job that spans transactions—recursive ingest is the
important example—may assign one UUIDv4 grouping ID to all of its operations.
The grouping ID is created once for that command or job, recorded in and hashed
with each audited canonical mutation, but it never substitutes for the
per-transaction operation ID, serves as the allocation-lineage identity, or
changes canonical ordering. Clients may collapse related operations visually
while verification continues to check each transaction independently.

Enabling the first scope also creates a vault-wide allocation-lineage genesis
that commits a cryptographically random 128-bit lineage ID and the existing
node-ID and operation-sequence allocator high-water marks immediately before
enrollment. The genesis also commits separate counts and digests for a complete,
canonically sorted topology snapshot containing every extant node's stable ID,
parent, name, file/directory kind, live/trash state, canonical node timestamps,
and available or unknown trash-origin record, and for the complete
attached-metadata projection defined above. JSONL carries every genesis record,
and import recomputes both digests before deriving the first enrollment. This
vault-wide authority—not a batch's claimed member or
attachment list—establishes which detached roots and authoritative metadata
existed at the cutover.
For every detached root, genesis freezes its available origin edge as
last-known topology. A later operational `trash_parent` locator clear remains
non-authoritative: it creates no delta and does not alter the replay projection.
The frozen edge therefore survives for closure derivation, so a still-retained
root can be associated with a scope enrolled later; an origin already absent at
genesis uses the unknown-origin representation. A later trash transition may
establish a new last-known origin as part of its authoritative trash-state
delta, independently of the repairable locator.

That same transaction then appends the enrollment as the first ordinary lineage
entry. Copies that enable audit independently therefore start different
lineages even when they inherited the same vault ID and allocator state. Every
authoritative operation from that enrollment onward, audited or not, appends one
allocation-lineage entry in its transaction. An entry commits
its previous lineage head, operation sequence, a cryptographically random
128-bit operation ID, the ordered node IDs allocated by the operation, both
resulting allocator high-water marks, either that operation's canonical
mutation hash or an explicit no-audited-mutation marker, and either its
topology-delta digest or an explicit no-topology-mutation marker, plus either its
witness-change count/digest or an explicit no-witness-change marker, and either
its attached-metadata-delta count/digest or an explicit
no-attached-metadata-change marker. Every JSONL
topology delta resolves to exactly one lineage entry with the same operation ID
and digest; an entry for a topology-changing transaction cannot carry the
no-topology marker. Every witness-change list likewise resolves to its lineage
entry and, when an audited mutation exists, to the identical count/digest in
that mutation hash. The random operation ID is generated once for that
transaction and is the same value hashed into the canonical mutation. It makes
independently mutated copies diverge even when they consume the same numeric IDs
in the same sequence position. The lineage records allocation identity,
ancestry, replayable topology facts, and authoritative attached-metadata
transitions required by audit verification, not an unaudited operation's file
content.

The guarantee is application-enforced and tamper-evident. It protects against
ordinary Docbank commands, API clients, maintenance, software mistakes, and
incomplete metadata restores. It cannot make bytes metaphysically indelible to
the operating-system account that can rewrite SQLite, packs, the executable,
and every backup. Backup manifests and externally recorded evidence bundles—
the stable vault ID, every scope count/head, and the allocation-lineage
count/head—provide stronger independent evidence.

### Live-store downgrade fence

Creating the first audit scope permanently raises the vault's required
live-store feature level through a second crash-safe cutover. After revalidating
the preview under the mutation gate and exclusive vault lock, Docbank durably
publishes and parent-syncs a non-ignorable `audit_pending` layout generation
containing the accepted scope and target node IDs, preview-token digest and
generation, baseline digest, preallocated operation and lineage IDs, operation
sequence, optional grouping ID, event timestamp, origin and optional agent
label, fixed enrollment cause, both allocator high-water marks, and the accepted
topology- and attached-metadata-genesis digests. Each must equal SHA-256 over the
exact registered CAE2 genesis record that the transaction will install, using
the pending vault/lineage identity and accepted sorted inventory. These are
every nondeterministic or externally supplied input to genesis, baseline,
events, and the first mutation;
the pending record carries the SHA-256 digest of its registered CAE2
`audit_pending` form. This happens before beginning or committing the SQLite
enrollment transaction, so every supported pre-audit v2 binary fails store open
before any
audit authority exists. Only an audit-aware recovery path can open the pending
generation.

The transaction creates topology and attached-metadata genesis, allocation
lineage, the scope, baseline, memberships, and first chain entry atomically.
After commit, Docbank reopens a pinned read snapshot, verifies the complete
cross-bound authority, then atomically publishes and syncs `audit_ready` before
serving any data, maintenance, export, backup, or restore operation. A crash
before `audit_pending` is durable leaves ordinary v2 authority unchanged. After
it is durable, legacy access stays blocked: recovery either finds no enrollment
transaction, recomputes the still-frozen accepted baseline, and resumes it, or
finds the complete committed transaction and revalidates it before publishing
`audit_ready`. Any mismatch or partial authority is a hard recovery error. A
non-crash transaction failure may return to `v2_ready` only after proving that
no audit record committed and durably publishing that rollback; it then reports
that enablement did not occur and requires a new preview.

The audit-pending and ready layouts remain outside pre-audit restore cleanup and
publication paths, so an old `backup restore --overwrite` cannot replace them
with an editing-only or legacy database. A marker an old binary can ignore is
not sufficient. The release test matrix exercises every pending/commit/ready
crash boundary, then opens and attempts overwrite restore with each supported
pre-audit binary and requires clean refusal with no file or metadata changes.

The database also enforces the protection boundary independently of API
routing. Constraints and write guards reject deletion or mutation of audited
nodes, memberships, versions, chain state, and protected blob reachability
unless the current audit-aware writer performs the complete authorized
transaction. Raw legacy trash-empty and GC statements must fail rather than
cascade or omit protected state. The daemon protocol check remains useful for
client replacement, but is not the live-store downgrade defense. The
non-authoritative `trash_parent` locator is the narrow exception: foreign-key
cleanup may clear it because immutable audit origin metadata, not that locator,
is the protected fact.

The same protection applies to indirect topology effects. Node insertion,
direct or cascading deletion, and parent/name/state or authoritative timestamp
updates require the guarded operation context whenever any scope exists. This
includes a parent revision/`modified_at` touch caused by child creation or move
and a content replacement's node `modified_at` change; each appears in the same
canonical node-state delta even when no path changes. Commit-time validation
proves the complete inherited membership and path-affecting descendant closure
and matches every deleted node to its topology tombstone. A row need not already
be audited for its write to be guarded.

Once any audit scope exists, database guards likewise cover every insert,
update, direct delete, and cascading delete of tag definitions, tag assignments,
ingest records, and provenance records, whether or not the affected node is
audited. Each changed identity must match exactly one record in the registered
attached-metadata delta; a second touch, an unregistered change, or a cascade
missing one of its assignment tombstones aborts the transaction. Commit-time
validation replays the complete simultaneous delta, enforces immutable
ingest/provenance rules, derives its audited fan-out, and compares the lineage,
canonical mutation, events, and scope heads actually written. There is no
unguarded SQL path for unaudited metadata after activation.

## Deletion and storage maintenance

`docbank rm` remains a reversible trash operation for audited nodes. Restore
continues to work normally, and both transitions appear in history.

A trash root is protected when it or any descendant belongs to an audit scope;
the whole root is then outside the eligible `trash empty` deletion set. Dry
runs and executions both report eligible roots separately from protected roots,
with the stable IDs of every protecting scope. Execution deletes exactly the
reported eligible set and leaves protected roots intact; this is an explicit
selection boundary, not a silent partial success. An audited item can therefore
remain out of the live tree without permanently preventing cleanup of unrelated
trash. Version pruning that directly targets an audited version is refused with
`audit_protected` and all blocking scope IDs.

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

Deterministic JSONL is the portable metadata authority. The editing/identity
bootstrap introduces `docbank-metadata` format version 2 and backup manifest
identifier `docbank-metadata-jsonl-v2`; existing version 1 remains the
pre-bootstrap format and cannot represent stable content versions or audit
records. A **zero-scope v2** stream is valid: it preserves the stable vault ID,
content versions and current-version references, and stable tag, ingest, and
provenance identities, but contains no audit genesis, allocation lineage,
scope, membership, baseline, mutation, or chain record. Once the first audit
scope is enabled, the same format additionally requires the complete audit
authority below. Every version 2 stream includes:

- the stable vault ID used to domain-separate audit hashes;
- the node-ID allocator high-water mark;
- every stable content-version record and node current-version reference;
- authoritative tag definitions and assignments, every retained ingest record,
  and every provenance record.

When at least one audit scope exists, version 2 additionally includes:

- audit scopes and their expected chain count/head;
- the operation-sequence allocator high-water mark;
- the vault-wide allocation-lineage genesis, complete topology-genesis and
  attached-metadata-genesis snapshots with their digests, entries, count, and
  head;
- sticky node memberships, shared enrollment-baseline batches, and each
  membership's immutable batch reference;
- baseline ancestor-spine witness generations, later witness-change lists,
  atomic topology deltas, attached-metadata deltas, and their canonical net
  path-effect lists, counts, and digests;
- immutable audited trash-origin parent IDs and names, including canonical
  unknown-origin records;
- canonical mutation records and per-scope chain entries; and
- every audit-protected historical blob reference.

Import accepts the zero-scope form only when every audit-specific record is
absent. It validates the vault and stable record identities, node current-version
references, the complete retained ingest set, and every provenance-to-ingest
reference without inventing an audit lineage. An unreferenced ingest record is
still portable authority and cannot be dropped merely because an ingest run
created no provenance facts. The first later `audit enable` creates genesis from
that imported current projection.
Conversely, any scope or other audit record requires exactly one valid genesis
and the full cross-bound authority; a partial audit form is rejected.

Export orders those records deterministically from one pinned metadata
snapshot. Import into a fresh current-schema store validates IDs, membership
topology, event order, chain hashes and heads, node revisions, every protected
blob reference, and the referential closure of authoritative attachments in one
transaction. It reconstructs every enrollment baseline batch, verifies its
digest and member set, replays attached-metadata changes, and requires the
resulting tag and provenance projection to equal the imported current state
before accepting the chain. It also recomputes the complete event sort keys and
sorted baseline-binding lists; an event ordinal, duplicate key, kind code, or
digest order that is not canonical is rejected. Every membership must resolve
to exactly one batch
that created it with the same scope and operation; initial memberships resolve
to the scope's sole enablement batch. Imported batches must use normalized
non-overlapping targets, contain exactly their declared newly adopted members,
and never assign one member to two batches for the same scope and operation.
Ingest/provenance updates are always invalid because those records are
immutable. A deletion is valid only when replay of the pre-operation projection
proves that the record belongs solely to unaudited nodes, is not retained by any
baseline or historical protected reference, and—for an ingest—has no remaining
provenance reference. A deleted provenance fact may have no retained incoming
`supersedes` edge; every dependent fact must be included in the same protected-
closure check and deletion delta. The canonical attached-metadata delta must
contain every tombstone. These rules are unchanged when the same transaction has
an unrelated audited effect: the deletion emits no scoped event, while the
canonical mutation still binds the complete delta. Import rejects a deletion
that fails those pre/post referential checks, a missing tombstone, or any current
provenance fact whose ingest or supersession target is absent.

Import builds the vault-wide topology projection from its digested genesis
snapshot and the complete attached-metadata projection from its independently
digested genesis snapshot, then derives active path witnesses from baseline
records. Before applying each later atomic topology delta, it derives the
complete affected member set, old/new paths, and witness changes from the prior
projections; verifies the claimed canonical lists, counts, digests, and scoped
events; and only then installs the post-state. It rejects missing dependency
witnesses, impossible pre-states, duplicate changed-node records, extra or omitted members,
and final replayed topology or active witnesses that differ from current node
authority—including parent, name, live/trash state, `created_at`, `modified_at`,
`trashed_at`, file/directory kind, and immutable trash-origin state.

At each operation, import also applies the canonical attached-metadata delta to
the prior replayed projection. It rejects an impossible pre-state, missing or
extra changed record, mutable provenance/ingest record, non-canonical
tombstone, or final tag, assignment, provenance, or ingest projection that
differs from current authority. From that verified transition and the replayed
memberships it derives the exact scoped fan-out events and whether a canonical
audited mutation is required.

Unknown audit records, internally missing or reordered events, altered baseline
state, dangling versions or attachments, or inconsistent heads fail the import;
they never produce a current-tree-only restore.

Import verifies the allocation lineage from its vault-specific genesis through
its declared count/head. Each entry's operation sequence must advance from the
previous tail, its node-ID high-water mark may only stay equal or increase, and
every allocated node ID must agree with that node-allocator transition. The
verified tail—genesis when it has no later entry—must agree with both exported
high-water marks. The operation-sequence high-water mark may exceed the greatest
audit event sequence because unaudited operations still append lineage entries.
Import restores both allocators and their lineage in the same transaction as
metadata authority; the next operation advances from that exact tail rather
than reusing a gap.

Import also cross-checks the authorities one-to-one. Every canonical mutation
must have exactly one allocation-lineage entry with the same operation sequence,
operation ID, and mutation hash; a lineage entry marked as unaudited must have no
canonical mutation. Independently, every topology delta must have exactly one
lineage entry with the same operation ID and delta digest, and the no-topology
marker is valid only when no topology delta exists for that operation. Every
witness-change list must match the allocation entry's count/digest and, when a
canonical mutation exists, the identical mutation-hash input; the no-witness
marker is valid only for an empty derived list. Replay must agree with the
entry's audited/no-audited marker. Every attached-metadata delta must likewise
match the lineage entry's count/digest and, for an audited operation, the
identical canonical-mutation input; the no-change marker is valid only when no
authoritative attached-metadata record changed. Every affected scope-chain
entry must commit that same mutation hash. Mixing a valid scope history from one
branch with a valid allocation lineage from another therefore fails before
publication.

A fresh import with no trusted reference cannot distinguish a coherently
rewritten and re-chained stream from an original one. Downgrade or rollback is
detectable only when the operation is given a trusted evidence bundle from a
prior snapshot manifest or external record: the stable vault ID, every expected
scope count/head, and the allocation-lineage count/head. Restore and verification
check the complete bundle rather than only audit heads. Independent evidence
remains necessary against an attacker who can replace the repository, manifest,
and history together.

Portable backup capture includes every blob reachable from a current audited
head or historical version. Incremental snapshots may reuse existing backup
packs, but each snapshot's logical metadata describes the complete audit
authority at that point. Every snapshot manifest records the complete evidence
bundle, including allocation-lineage count/head even when the latest operations
were unaudited. Verify and restore must reproduce identical scope IDs,
memberships, event count/heads, allocation lineage, content hashes, and deletion
protections across loose and packed source vaults. The comparison also includes
the complete authoritative attachment projection for every audited member and
the replayed path-topology projection with every path-effect commitment.

Normal overwrite restore is forward-only for an existing audited target. While
holding the target hierarchy lock and before cleanup, restore reads the target's
vault ID, verified scope heads, and verified allocation-lineage count/head. The
selected snapshot must carry the same vault ID and, for every existing scope,
prove that the chain at the existing entry count has exactly the existing head;
its final count may be equal or greater. It must make the same prefix proof for
the target's allocation lineage. The snapshot's final allocator high-water
marks must equal its verified lineage tail and cannot be lower than the target's.
A missing scope, shorter chain, divergent audit or allocation-lineage prefix,
pre-audit snapshot, or different vault ID is rejected with `audit_protected`
before publication.

High-water comparison alone is deliberately insufficient. Copies restored from
one snapshot share a vault ID and may independently consume the same numeric
node IDs and operation sequences; their random operation IDs produce different
lineage heads at the first mutation. Neither copy can then overwrite the other
through the normal restore command, even if every scope head still matches.
One branch may overwrite another only when the target's complete allocation
lineage is an exact prefix of the snapshot's lineage, proving that every target
allocation has the same ancestry and meaning in the incoming store.

An older, unrelated, or divergent snapshot may still be inspected or restored
to a fresh directory because that does not erase an existing promise. Replacing
an audited target with anything that does not preserve or extend all of its
audit and allocation lineages belongs only to the exceptional recovery
workflow. The ordinary overwrite form of `docbank backup restore` rejects it.

## One history model, several clients

The daemon API owns one bounded, cursor-paginated representation for audit
scope status, events, content versions, comparisons, and chain verification.
CLI, agents, TUI, and web clients consume that same model; none opens SQLite or
the blob store directly. Status and terminal verification proofs expose the
stable vault ID, every scope count/head, and allocation-lineage count/head as one
evidence bundle suitable for external recording and later expected-state checks.

The event order is canonical and total, while clients project it in three
useful ways:

- a **scope timeline** aggregates changes to every sticky member;
- a **node timeline** follows one stable document or directory across paths;
  and
- a **version history** filters to content heads and reversions.

Interactive clients show newest first by default, but cursors preserve stable
forward/backward traversal and chain verification reads canonical order.
Events committed in one metadata transaction share an operation identity. A
multi-transaction command such as recursive ingest shares only its grouping ID;
clients may visually group either level without collapsing individual node
events or presenting the group as one atomic mutation.

Comparison is type-aware but never invents semantic equivalence. Plain text
and canonical metadata can render inline or side by side. Images and PDFs may
render side by side when a safe viewer is available. Office and unknown binary
formats start with hash, size, media type, and metadata changes plus verified
download/open actions; richer format-aware comparison can be added later.

### CLI and agents

Planned CLI concepts are `audit enable`, `audit status`, `audit history`,
`audit verify`, and the general `versions` command. `audit enable` previews its
baseline inventory plus structured vault-wide metadata-retention counts and
returns a server-issued preview token by default; a separate execution supplies
that token and the explicit disclosure acknowledgment because enrollment is
permanent.
Machine output uses stable scope, node, event, and version IDs and explicit
protection state. `audit status --json` and `audit verify --json` include the
complete evidence bundle rather than reporting only per-scope heads. The
`audit history` command and node-timeline API return
`audit_not_enrolled` for a node outside every audit scope rather than presenting
an empty timeline as complete. A refused protected mutation returns
`audit_protected` with a list of every blocking scope rather than relying on
prose parsing.

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
first-class actions. Policy enablement shows the dry-run baseline inventory and
vault-wide metadata-retention disclosure, then requires the separate explicit
confirmation backed by the preview token; exceptional destruction is absent.

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
inventory, storage impact, and vault-wide metadata-retention disclosure; review
the target's current path and stable IDs; then acknowledge both retention
boundaries and confirm enrollment. V1 has no separate scope-name field.
The confirmation consumes the server-issued preview token and must refresh the
inventory if relevant vault state changed.

Neither UI presents exceptional audit destruction as an ordinary action.
