---
last_edited: 2026-09-12
title: Embed in Go
description: Own one or more independently rooted Docbank vaults inside a Go application, with CGO or pure-Go SQLite.
---

# Embed in Go

Use `go.kenn.io/docbank` to own a document vault inside your Go process.
Your application can store, version, retrieve, and back up documents without
starting or discovering a daemon. The embedded API uses the same tree, content
checks, storage rules, and exclusive vault lock as standalone Docbank.

Use an embedded vault when the application itself should own document lifecycle.
Use the HTTP API when independent processes need to share one standalone vault.

## Create a vault service

Each root is an independent archive. One process may open several
non-overlapping roots at once. The same root, an ancestor, or a descendant is
rejected while another daemon, embedded vault, or restore target owns it, even
when a filesystem alias spells that tree differently.

```go
package archive

import (
    "bytes"
    "context"
    "io"

    "go.kenn.io/docbank"
)

func StoreSession(ctx context.Context, root string, sessionID string, jsonl []byte) error {
    vault, err := docbank.New(ctx, docbank.Config{Root: root})
    if err != nil {
        return err
    }
    defer vault.Close()

    _, err = vault.Put(
        ctx,
        "/sessions/"+sessionID+".jsonl",
        bytes.NewReader(jsonl),
        docbank.PutOptions{MediaType: "application/x-ndjson"},
    )
    if err != nil {
        return err
    }

    content, err := vault.OpenContent(ctx, "/sessions/"+sessionID+".jsonl")
    if err != nil {
        return err
    }
    defer content.Reader.Close()
    _, err = io.Copy(io.Discard, content.Reader)
    if err != nil {
        return err
    }
    return content.Reader.Verify()
}
```

An embedded application can append an origin learned after ingestion with the
same immutable fact model:

```go
appended, err := vault.AppendProvenance(ctx, receipt.Node.ID,
    docbank.ProvenanceAppendOptions{
        IfRevision: receipt.Node.Revision,
        Source: docbank.ProvenanceSource{
            Kind: "agent", Description: "triage", Reference: "opaque://laptop/report.txt",
        },
    })
```

The result contains the updated node, live path, appended fact, and receipt.
Set a positive `IfRevision` to require the node revision you inspected;
zero makes the operation unconditional.

To correct an active caller-supplied fact on the same node, set `Supersedes`
to that fact's identity. Docbank records the source reference exactly as
supplied and never opens it.

`Put` creates missing virtual directories. Repeating the same bytes and media
type converges on the current version; changed bytes append an immutable content
version while preserving the node ID. Supply `PutOptions.Expected` when the
caller already knows the SHA-256 and byte count and wants Docbank to reject a
mismatched stream before granting metadata authority. Supply a positive
`PutOptions.IfRevision` when replacing content derived from an earlier read.
Docbank rejects the write with `ErrStaleRevision` if the file has changed since
that revision, including when the new bytes happen to match the current head.

Use `Create` when an application owns an immutable key and must never replace
different content already stored there. It requires the expected SHA-256 and
size. An exact retry is idempotent; a different byte identity, media type, or
node kind returns `ErrContentConflict` without appending a version:

```go
receipt, err := vault.Create(ctx, "/records/immutable.jsonl", reader,
    docbank.CreateOptions{
        MediaType: "application/x-ndjson",
        Expected: docbank.ContentIdentity{SHA256: digest, Size: size},
        Provenance: &docbank.ProvenanceSource{
            Kind:        "agent-session",
            Description: "local session archive",
            Reference:   "sessions/01K123.jsonl",
        },
    },
)
```

`CreateOptions.Provenance` lets an embedded owner record where an immutable
document came from without inventing application-specific Docbank tables. The
kind and description identify the source system; the reference is an opaque
source-local URI, archive key, path, or other stable identifier. An optional
`ModifiedAt` records the source's own timestamp in canonical UTC form. Docbank
does not interpret the reference or grant it retention authority.

For a newly created document, the node, first content version, ingest record,
and provenance fact commit as one metadata transaction. If any part fails,
none gains authority. An exact `Create` retry succeeds only when its active
provenance also matches, so a caller cannot accidentally claim source evidence
that was never recorded. Superseding that matching fact makes the original
request return `ErrContentConflict` unless another active fact still matches.
A request matching the corrected provenance remains idempotent when the
content and media type are unchanged.

`AppendProvenance` may supersede active caller-supplied facts on the same node,
including facts supplied through `CreateOptions.Provenance`. Operational CLI
and watched-folder ingest facts cannot be superseded, because they keep
re-ingest idempotent; append newly learned origins alongside them instead.
Use `Provenance` to inspect the bounded newest-first history:

```go
page, err := vault.Provenance(ctx, receipt.Node.ID, docbank.ProvenanceOptions{
    Limit: 100,
})
if err != nil {
    return err
}
for _, fact := range page.Items {
    // fact.SourceReference remains the exact opaque identifier supplied above.
    _ = fact
}
```

The node, live path, total, and page come from one read snapshot. `Path` is
empty for a trashed node. Provenance records origin; it does not prevent trash
empty, version pruning, or garbage collection. Applications must manage any
retention policy for external references themselves.

`ContentIdentity` always describes decoded document bytes, regardless of
whether those bytes are stored raw, zstd-compressed, or in a pack. SHA-256 is
the canonical logical identity, not a digest of a physical storage file.

## Extract and read source metadata

`EnsureSourceMetadata` verifies and processes one immutable content version
with Docbank's current local extractor, then returns its typed metadata. The
result carries the complete `ContentVersion`, so callers can bind the facts to
the exact SHA-256 and node version they describe:

```go
metadata, err := vault.EnsureSourceMetadata(ctx, receipt.Version.ID)
if err != nil {
    return err
}
for _, field := range metadata.Fields {
    // Interpret fields by namespace, key, and typed value.
    _ = field
}
```

`EnsureSourceMetadata` runs locally and returns when extraction finishes.
It processes only the requested version and reuses the current extractor's
result when one exists. It does not start background workers.

The method holds the vault's mutation lock during extraction. It verifies the
complete original, including large media files, then applies the
[format-specific parsing limits](architecture/source-metadata.md#current-format-boundary).
Concurrent `Put`, `Create`, and maintenance calls wait.
If Docbank cannot open or verify the source bytes, it returns
`ErrContentUnavailable`.

Use `SourceMetadata` for a read-only lookup. It returns `ErrNotFound` when no
result has been published. Both methods return all local fields, including
sensitive fields. Your application decides which fields it may disclose.

## Publish email attachment documents

Use `EnsureEmailMetadata` to retain the MIME inventory for an exact email
version. Then call `PublishEmailDocuments` with a
`document.EmailDocumentPublicationRequest`: an operation ID, the exact parent
node/version/hash/size, the returned generation and attachment IDs, and a
destination directory ID with its current revision. No host filesystem path
is accepted. This explicit operation does not run automatically on import.

Publication verifies retained payload bytes and creates ordinary file documents
for the full known attachment inventory in one transaction. Inline resources
are included; body alternatives and multipart containers are excluded. An
attached email becomes a child email, whose own attachments can be published
separately. The receipt says whether the MIME inventory is `complete` or
`partial`, and records unavailable, unsupported, failed, or encrypted parts
without inventing empty files.

Equal bytes share storage but get separate document occurrences. Filenames use
the safe MIME name plus operation and occurrence identifiers. An explicit
`reuse` selection may instead identify an existing child version with matching
bytes and its node revision. Raw filenames remain in the MIME inventory.
Renames, later content versions, and decoder reprocessing never move an old
relation to a different version.

Keep the operation ID and request unchanged when retrying. The same request
returns the original receipt, including after backup and restore; a changed
request conflicts. `EmailDocumentPublication` reads that receipt.
`EmailDocumentRelations` selects either an exact parent version or an exact
child version and returns current processing status with each relation. Pages
default to 100 rows, accept at most 250, include a total, and return the next
operation/order pair when another page exists. Publication accepts at most
1,000 MIME parts, 128 MiB per payload, and 256 MiB of decoded payloads. HTTP
request and response bodies are bounded to 2 MiB.

Publication itself does not prove indexing or authorize provider disclosure.
`RequestEmailDocumentProcessing` submits one receipt occurrence to the ordinary
rendition scheduler with an explicit processing profile, execution identity,
artifact policy, principal, scope, and consent classes. Existing consent must
authorize those exact inputs and retained outputs. `GrantProcessingConsent`
explicitly grants that authority; `RevokeProcessingConsent` advances the
principal/scope revocation fence. Publication and processing requests never
grant consent or inherit it from a parent email. A configured runtime must
execute that profile; an unconfigured provider remains pending. The worker
checks consent again before provider access and publication. Relation status
reports `indexed` only for a complete active lexical rendition of that exact
child version. Other states include `decoded`, `pending`, `failed`,
`unsupported`, `encrypted`, and `unavailable`. Ordinary search returns child
matches; QueryV1 does not add an email-family traversal predicate.

Trash and restore keep relations and do not cascade to children. A referenced
version cannot be permanently deleted or pruned, and a referenced MIME
inventory cannot be purged. To release those references, call
`RemoveEmailDocumentPublication` with the operation ID and exact request digest.
This removes that receipt and its relations, preserves ordinary children, and
relinquishes the operation's retry guarantee. Other receipts retain their own
references. Ordinary audit and content-retention rules still apply.

The authenticated HTTP and typed client surfaces expose the same operations:

| Operation | HTTP route |
| --- | --- |
| Publish | `POST /api/v1/email-document-publications` |
| Read receipt | `GET /api/v1/email-document-publications/{operation_id}` |
| Release receipt | `DELETE /api/v1/email-document-publications/{operation_id}` with `request_digest` |
| Read relations | `GET /api/v1/email-document-relations` with `parent_version_id` or `child_version_id` |
| Request processing | `POST /api/v1/email-document-processing` |
| Grant processing consent | `POST /api/v1/processing/consents` |
| Revoke processing consent | `POST /api/v1/processing/consents/revoke` |

These routes require the master API key and are outside the current browser
session capability. All relation and receipt DTOs live in
`go.kenn.io/docbank/document`. Consumers
can use the same exact identities for navigation and downloads without
inferring parentage from names or hashes.

## Read canonical visual previews

`EnsureVisualPreview` synchronously processes one immutable content version
when the current built-in recipe has no recorded result. It returns the active
preview; if the recipe was already recorded, another recipe's active result
stays selected. Ready results identify exact preview
bytes and dimensions; unsupported and failed results carry a stable failure
code without pretending that content is available.

```go
preview, err := vault.EnsureVisualPreview(ctx, versionID)
if err != nil {
    return err
}
if preview.State == document.VisualPreviewReady {
    content, err := vault.OpenVisualPreview(ctx, versionID)
    if err != nil {
        return err
    }
    defer content.Reader.Close()
    // Reach EOF or call Verify before trusting the bytes.
}
```

`OpenVisualPreview` uses the same verified-reader contract as original
content. It returns `ErrVisualPreviewUnavailable` for a cataloged unsupported
or failed result and `ErrNotFound` when no preview result exists.
`VisualPreview` remains a read-only lookup. Opening an embedded vault does not
start a preview worker; applications choose when to call the synchronous
producer. The built-in producer supports JPEG, PNG, GIF, still WebP, and
supported embedded JPEG previews in ARW, DNG, CR2, NEF, and RAF camera RAW
files. See [Visual previews](architecture/visual-previews.md) for format limits,
output size, and recipe selection.

## Inspect and repair stored content

`Put` and `Create` receipts include `Physical`. This field describes the raw,
zstd, or packed representation on disk. Use it to inspect storage; use
`ContentIdentity` to identify document bytes.

`Put` is an idempotent content write, not an integrity repair primitive. Kit's
structural dedup can reuse an existing canonical representation without hashing
it, and packed catalog authority can remain selected over a loose copy. When an
application has trusted bytes for a known SHA-256 and size, `RepairContent`
verifies the complete stream before replacing physical authority. It preserves
every node and historical version reference to that identity. Repairing packed
content makes a verified loose copy authoritative; a later repack reclaims the
now-dead packed bytes.

### Choose loose compression

New content remains loose until an explicit `Pack` call. The standalone daemon
selects zstd when a new loose object is at least 4 KiB and compression saves at
least 10%. Embedded vaults keep raw loose storage by default; an owner can match
the daemon policy explicitly or choose application-specific thresholds:

```go
vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    LooseCompression: docbank.LooseCompressionOptions{
        Enabled:           true,
        MinBytes:          4 << 10,
        MinSavingsPercent: 10,
    },
})
```

Docbank keeps zstd only when the logical size meets `MinBytes` and the completed
encoding saves at least `MinSavingsPercent`; otherwise it publishes raw loose
content. Enabling compression does not proactively migrate or rewrite existing
objects. `RepairContent` preserves an existing loose object's raw or zstd
encoding. It applies this policy only when trusted bytes replace packed or
missing physical authority. The zero value disables compression, preserving the
unchanged raw loose layout, and mixed raw, zstd, and packed content remains
readable through the same verified API. Receipts report the chosen physical
encoding and stored size without changing the logical SHA-256 or size.
`PutReceipt.Created` reports a new logical node, while `PhysicalCreated`
separately reports that the operation published new final loose authority.

An eligible write temporarily needs scratch space for both the raw object and
its compressed candidate before Docbank chooses one for durable publication.
`LooseBacklog` reports how much indexed loose content remains eligible for an
explicit pack pass, including both logical and physically stored bytes and a
split between raw and compressed object counts. Applications can therefore
schedule packing from physical storage growth without walking loose objects.
The report does not make packing automatic.

## Identify a vault and verify reads

`vault.ID()` returns the archive's stable UUID. JSONL backup and restore
preserve that identity even when the restored vault has a different filesystem
root; applications can therefore distinguish logical archives without treating
paths as identity.

An `OpenContent` stream is not authoritative until it reaches terminal `io.EOF`
or `Verify` succeeds. Early `Close` does not drain the stream. `Vault.Close`
waits for active operations and streams, closes storage, and releases the vault
lock.

`OpenContent` and `OpenVersionContent` wrap `ErrContentUnavailable` when the
catalog-authorized physical content cannot be opened or its physical size
disagrees with metadata. Metadata lookup failures retain their existing
`ErrNotFound`, `ErrNotFile`, or `ErrClosed` classification instead. A canceled
physical open can match both `context.Canceled` and `ErrContentUnavailable`, so
callers that distinguish cancellation should check the context error first.

`OpenVersionContentRange` selects a non-empty decoded logical byte range from
one exact immutable version. Offsets and lengths address the same logical bytes
described by `ContentVersion.BlobHash` and `Size`, regardless of whether current
physical authority is raw loose, zstd-compressed loose, packed, or secondary:

```go
part, err := vault.OpenVersionContentRange(ctx, versionID,
    docbank.ContentRangeOptions{Offset: 1 << 20, Length: 256 << 10},
)
if err != nil {
    return err
}
defer part.Reader.Close()
_, err = io.Copy(dst, part.Reader)
```

Range-read costs depend on storage:

- Raw loose content uses filesystem offsets.
- Compressed loose content is decoded to a temporary file first.
- Packed content is decoded in full into memory. Reading a small range can
  therefore need RAM for the whole decoded object.

A range outside the version's bytes returns `ErrInvalidContentRange`. The
reader returns exactly the requested length, or `io.ErrUnexpectedEOF` if the
stream ends early. It keeps the vault open until the caller calls `Close`.

A successful partial read checks the selected location, decoded size, and
range bounds. It does not verify the whole object. Use `OpenVersionContent`
to read and verify all bytes, or run maintenance verification.

## Traverse and mutate the tree

`Children` exposes the live virtual tree without materializing an unbounded
directory. Resolve a directory with `Stat`, then advance through its direct
children with `Limit` and `Offset`:

```go
manifests, err := vault.Stat(ctx, "/manifests")
if err != nil {
    return err
}
for offset := 0; ; {
    page, err := vault.Children(ctx, manifests.ID, docbank.ChildrenOptions{
        Limit:  500,
        Offset: offset,
    })
    if err != nil {
        return err
    }
    for _, child := range page.Items {
        // Inspect this bounded page.
        _ = child
    }
    offset += len(page.Items)
    if offset >= page.Total || len(page.Items) == 0 {
        break
    }
}
```

Pages contain directories first and files second, name-sorted within each kind.
A zero limit uses `DefaultChildrenLimit`; one call cannot exceed
`MaxChildrenLimit`. The total and page come from one metadata snapshot, but a
caller that needs a complete stable traversal must avoid concurrent tree
mutations between page calls.

Use `Walk` for a complete stable traversal. It pins one SQLite snapshot before
returning and yields the selected root and its descendants in bounded pages:

```go
walker, err := vault.Walk(ctx, "/sessions", docbank.WalkOptions{PageSize: 500})
if err != nil {
    return err
}
defer walker.Close()

for {
    page, err := walker.Next(ctx)
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    for _, entry := range page {
        // entry.Path is canonical within the pinned snapshot.
        _ = entry
    }
}
```

A zero page size uses `DefaultWalkPageSize`; no page can exceed
`MaxWalkPageSize`. Paths are limited to `MaxWalkPathBytes`, and absolute tree
depth is limited to `MaxWalkDepth`. Later tree changes do not enter the snapshot.

The walker loads the tree incrementally. Setup does not load the whole
subtree. Each node needs at most two sibling range seeks and one child seek.
The second sibling seek applies only when an include-trash walk finishes
the node IDs sharing one path and advances to the next name.

Always call `Walker.Close`, including after `io.EOF`. Repeated calls are safe.
It releases the read transaction, dedicated connection, and vault lease.
`Vault.Close` waits for all walkers and content readers to close.

`MovePath`, `TrashPath`, and `Restore` return the resulting node and canonical
path. Their optional positive `IfRevision` rejects stale mutations;
`IfRevision == 0` is unconditional. `EmptyTrash` previews or deletes at most a
finite number of trash roots: a zero `MaxRoots` uses
`DefaultTrashEmptyMaxRoots`, and `More` asks the owner to schedule another
batch.

Use `BatchMove` for an all-or-nothing reorganization of up to
`MaxBatchMoves` nodes. Each source is either a path resolved inside the
transaction or a stable node ID with the revision previously inspected. All
destinations are exact final coordinates whose parents resolve in the planned
final tree; an existing directory does not mean “move into.” The complete final
tree is validated before any change, so embedded applications can express file
or directory swaps and nested moves without temporary names or partial completion.

## Back up and restore an embedded vault

An application that owns a vault in-process can use the same portable,
topology-independent backup format as the daemon:

```go
repository, err := docbank.InitBackupRepository(backupRoot)
if err != nil {
    return err
}
snapshot, err := vault.CreateBackup(ctx, repository, docbank.BackupOptions{
    Tag: "before-import",
	Prepare: func(ctx context.Context) error {
		return snapshotApplicationCatalog(ctx, catalogSnapshot)
	},
	ExtraFiles: []docbank.BackupExtraFile{{
		Path: catalogSnapshot, RecordAs: "application/catalog.sqlite",
	}},
})
if err != nil {
    return err
}
proof, err := repository.Verify(ctx, docbank.BackupVerifyOptions{
    SnapshotID: snapshot.ID,
})
if err != nil {
	return err
}
if len(proof.Problems) != 0 {
	return fmt.Errorf("backup verification found %d problems", len(proof.Problems))
}
_, err = vault.RestoreBackup(ctx, repository, docbank.BackupRestoreOptions{
	SnapshotID:    snapshot.ID,
	Target:        restoreRoot,
	ProtectedRoots: applicationStorageRoots,
})
return err
```

Use `OpenBackupRepository` after process restart. `Snapshots` lists recovery
points in chronological order.

Capture prevents content reclamation for the whole snapshot. Ordinary appends
resume once Docbank fixes the SQLite view that the backup will use. Physical
maintenance waits until Docbank publishes the manifest.

To include your application's catalog in the same backup:

1. Use `Prepare` to create an immutable catalog snapshot while writes are paused.
2. Declare the file in `ExtraFiles`.
3. Keep the file unchanged until `CreateBackup` returns.

`RecordAs` is the file's relative location beneath the restored vault root.
Your application must coordinate its own writes during `Prepare`; Docbank's
freeze pauses Docbank mutations, not changes to an unrelated application
database. The callback must not call vault mutations while that lock is held.

Mark files containing credentials or tokens as `Sensitive`. Docbank rejects
sensitive files in a plaintext repository unless your application explicitly
sets `AllowPlaintextSecrets` for that backup.

Restore always uses a separate root. It rejects overlap with the live vault
or repository and checks content, SQLite integrity, and manifest statistics
before making the result available. Supply other application storage in
`ProtectedRoots`; Docbank checks those locations before restore cleanup too.
Embedded restore builds a fresh primary store. It does not recreate secondary
store placement.

If the original vault is unavailable, open the backup repository and restore
directly without initializing or opening a source vault:

```go
repository, err := docbank.OpenBackupRepository(backupRoot)
if err != nil {
    return err
}
_, err = repository.Restore(ctx, docbank.BackupRestoreOptions{
    Target: restoreRoot,
    ProtectedRoots: applicationStorageRoots,
})
return err
```

An omitted snapshot ID selects the latest recovery point. Repository restore
uses the build's default SQLite driver and restores declared host files along
with vault content. It rejects the repository, declared protected roots, and
targets held by another vault. Include any offline source or application
storage you want to preserve in `ProtectedRoots`: the repository cannot infer
their current locations. `Vault.RestoreBackup` also protects its open vault
automatically and retains that vault's configured SQLite driver.

### Remove recovery points and reclaim storage

`BackupRepository.Forget` removes explicitly selected snapshot records;
`BackupRepository.Prune` reclaims backup storage that retained snapshots no
longer need. Neither operation opens the source vault. Retention schedules and
which recovery points to keep remain the embedding application's policy.

Preview each operation before committing to it:

```go
selection, err := repository.Forget(ctx, docbank.BackupForgetOptions{
    SnapshotIDs: snapshotIDs,
    DryRun: true,
})
if err != nil {
    return err
}
// Present selection.Selected to the user. Call Forget again with DryRun false
// to apply the removal; selection.Forgotten is empty during a dry run.
fmt.Println("Selected recovery points:", selection.Selected)

cleanup, err := repository.Prune(ctx, docbank.BackupPruneOptions{DryRun: true})
if err != nil {
    return err
}
// After removing snapshots, preview cleanup again before calling Prune with
// DryRun false. PacksToRemove includes any old packs that will be rewritten.
fmt.Println("Packs selected for cleanup:", cleanup.PacksToRemove)
```

Forgetting alone does not reclaim packed bytes. It refuses to remove the last
recovery point unless `AllowEmpty` is explicit (`ErrBackupLastSnapshot`), and
refuses parents needed by retained incremental snapshots
(`ErrBackupSnapshotRequired`). Both cleanup operations use Kit's exclusive
repository lock, including dry runs; contention returns
`ErrBackupRepositoryLocked`. `ForceUnlock` is only for known abandoned locks.

Pruning removes wholly unused packs and rewrites packs with less than half
their encoded payload still needed. Mostly-live packs retain unused bytes, so
this is not full compaction or secure erasure. Planned byte counts cover old
pack files and copied payload, not exact net savings or temporary-space needs.
Inspect the returned report even when an error occurs: completed removals and
writes may be partial. Interrupted pruning can be retried. These embedded
operations do not add automatic retention or standalone CLI cleanup commands.

## Maintain physical storage

Ordinary `Put` calls publish loose content. Call `Pack` explicitly when the
embedded owner is ready to move authorized loose blobs into managed immutable
packs:

```go
report, err := vault.Pack(ctx, docbank.PackOptions{MaxBytes: 256 << 20})
if err != nil {
    return err
}
if report.More {
    // Run another bounded pass when scheduling allows.
}
```

`MaxBytes` is a soft committed raw-byte budget: the pass finishes the blob that
crosses the budget, seals its pack, and stops. Zero is unlimited. The report
includes packing, reconciliation, missing/corrupt content, and orphan cleanup
outcomes; embedded applications should surface those fields rather than treating
a nil error alone as a complete health report. Packing changes only physical
representation. `OpenContent` keeps the same verified read contract.

Embedded `GarbageCollect`, `Verify`, and `Repack` calls are resumable bounded
passes. `WorkBudget.MaxObjects == 0` uses the finite
`DefaultMaintenanceMaxObjects`; larger explicit budgets must not exceed
`MaxMaintenanceObjects`. A positive `MaxBytes` adds a soft byte bound, so one
selected object may finish after crossing it. A zero byte bound is unlimited,
but the object bound still limits each pass.

When a report has `More`, pass a non-empty `NextCursor` back in the same
operation's next `WorkBudget`. Treat cursors as opaque and operation-specific;
malformed cursors and cursors from another operation return
`ErrInvalidMaintenanceCursor`. A Repack pass can validly report more work with
an empty cursor when completed mutations themselves reduce the candidate set;
repeat it from an empty cursor. Cursors are continuation positions, not snapshot
tokens: work inserted earlier in canonical order during a cycle waits for a
later cycle started without a cursor.

The bounded embedded contract is intentionally narrower than the standalone
full-run commands. Embedded `Verify` checks a bounded page of blob bytes but
does not perform whole-catalog metadata validation. Embedded `GarbageCollect`
handles bounded unreachable catalog authority but does not enumerate untracked
filesystem files. The daemon's `verify` and `gc` commands retain those full-run
checks.

Your application decides when to empty trash and prune prior versions.
GC can reclaim a blob only after no retained reference needs it. Embedded
vaults have no background maintenance scheduler, so your application also
chooses when to resume each bounded pass.

## Choose SQLite

The build default preserves performance where CGO is available:

- CGO builds use `github.com/mattn/go-sqlite3`.
- `CGO_ENABLED=0` builds use `modernc.org/sqlite`.

An application may select either adapter explicitly:

```go
import (
    "go.kenn.io/docbank"
    "go.kenn.io/docbank/sqlite/modernc"
)

vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    SQLite: modernc.Driver{},
})
```

Use `sqlite/mattn.Driver` in a CGO build to select the CGO adapter explicitly.
The adapter is selected when the vault opens; query and transaction operations
then run directly on that driver's `database/sql` pool. Standalone backup and
restore paths use the same adapter boundary rather than silently switching
SQLite implementations.

## Ownership boundary

Do not point a daemon and an embedded application at the same root. `New` holds
the same exclusive hierarchy lock as the daemon for the entire vault lifetime
and fails if the requested root overlaps another active vault or restore target.

The standalone CLI remains daemon-first and never opens storage directly.
Embedding is a distinct application ownership mode, not a second privileged path
into a daemon-owned vault. External agents should continue to use the
[authenticated HTTP contract](agents/integration.md).
