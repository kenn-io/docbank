# Backup Repository Format

On-disk format reference for repositories created by the `backup` and `pack`
packages — layout, object encodings, versioning, crash-consistency, and the
freeze protocol. It documents the invariants an implementation must preserve,
in enough detail to audit a repository by hand or reimplement a reader.

The engine is application-agnostic: an application supplies its own database
filename, content-directory name, and schema-specific stats/content-path
logic through the `App` interface (`app.go`). Application metadata can be
represented either by incremental SQLite page maps or by a portable logical
stream supplied through `MetadataSource` and rebuilt through
`MetadataRestorer`. The engine treats the
manifest's application version and stats payload as opaque bytes — it
records them at create and byte-compares them at restore. Everything below
applies uniformly to every application built on this engine.

## Design Goals

- **Local-first, tool-agnostic.** A repository is a plain directory of write-once files. Any file-sync tool can replicate it; no server or database is required to read it.
- **Content-addressed and deduplicated.** Every stored object is a blob named by the SHA-256 of its (uncompressed, unencrypted) content. Identical content is stored once, across snapshots and across data types.
- **Crash-safe by construction.** A snapshot exists if and only if its manifest file exists, and the manifest is written last. There is no repair step and no journal.
- **Verifiable.** Every container and metadata object carries integrity checksums; full verification re-derives every referenced blob's identity from its bytes.
- **Versioned at every level.** Readers refuse what they cannot safely interpret rather than guessing.

## Repository Layout

```
<repo>/
  config.toml              # repo identity + format versions (plain TOML)
  snapshots/
    <snapshot-id>.mvmanifest   # JSON manifest, written LAST
  packs/
    <aa>/<ulid><ext>       # sealed blob containers (~32 MB target), aa = first ID byte;
                           # ext is the application-chosen extension (App.PackFileExtension)
  indexes/
    <ulid>.mvidx           # immutable blob -> (pack, offset) indexes
  locks/                   # exclusive.json / shared-<ulid>.json
  staging/                 # temp files; atomically renamed into place
```

All multi-byte integers in every binary object are **little-endian**. All timestamps are UTC.

## Versioning Model

Compatibility is enforced at three levels, all of which must pass:

1. **Repository level.** `config.toml` records `repo_id` (a lowercase-hex UUID; readers refuse any other shape, because the ID is embedded verbatim in local cache filenames), `format_version` (what wrote it), and `min_reader_version` (the oldest format a reader must understand). `Open` refuses a repository whose `min_reader_version` exceeds the reader's supported version, with an explicit error telling the caller to upgrade the reader. A future format change that old readers can safely ignore bumps only `format_version`; a change they cannot safely ignore also bumps `min_reader_version`. An application may also raise a repository's `min_reader_version`, up to the supported version, to keep older builds from opening that repository; reader version 7 is a repository gate only and carries no layout, so a later layout change needs a higher version.
2. **Object level.** Every binary object begins with a 4-byte magic and a version field, and every decoder rejects an unknown magic or version. A reader can therefore never misparse an object from a future format as if it were current.
3. **Snapshot level.** Each manifest records its own `format_version`, `min_reader_version`, and the application version string that wrote it (wire key `msgvault_version`, frozen for compatibility across every application built on this engine), so compatibility can evolve per-snapshot within one repository (for example, when a future version introduces encrypted snapshots alongside existing plaintext ones). Version 2 marks snapshots whose attachment population records storage paths beyond the canonical `<aa>/<hash>` derivation: version-1 readers placed every restored attachment at the canonical path and would materialize a database pointing at files that do not exist, so they must refuse these snapshots. Snapshots whose recorded paths are all canonical keep version 1. Version 3 marks snapshots whose application metadata is a portable logical blob rather than SQLite page-map chains. Version 4 marks snapshots with application-defined auxiliary artifacts. Version 5 adds chunk recipes for content and portable metadata; version 6 extends recipes to operational extras (see [Large Logical Objects](#large-logical-objects)). A manifest whose `min_reader_version` a reader accepts must contain only fields that reader knows: the content-derived ID covers only known fields, so an unknown field would otherwise ride along in an authenticated manifest, and readers refuse it as forged rather than ignore it.

Integrity is separate from versioning: every metadata object ends with a SHA-256 trailer over everything before it, checked before any field is interpreted, and pack entries carry CRC32-C over the stored bytes.

## Blob Identity

```
BlobID = SHA-256(plaintext content)
```

The ID is always computed over the raw content — before compression, before any future encryption. Compression and encryption are storage transforms recorded per-entry in the pack; they never change identity. This is what makes deduplication stable across compression-level changes and future format evolution.

## Pack Files

Blobs are appended into pack files sealed at a ~32 MB target. A sealed pack is never modified.

The pack file format is identified by its `MVPK` header magic, not by the file's name: the
file extension is application-chosen (`App.PackFileExtension`), and `.kpack` is the
recommended convention. An application must keep its chosen extension fixed for the life of a
repository — packs are located by `<packID><ext>`, so changing it strands previously written
packs — and, for encrypted repositories, renaming a pack file also breaks it: the pack ID
derived from the filename (basename minus extension) participates in the footer's AAD.

```
header:   "MVPK" | version u8 (=1) | flags u8
frames:   one frame per blob, concatenated
footer:   entry table | footer trailer ("KPVM" magic, SHA-256 over footer region)
```

Each footer entry records the blob ID, offset, stored length, raw length, CRC32-C of the stored bytes, and per-blob flags (`compressed`, `encrypted`). Each frame is either the raw content or a zstd frame: compression (level 3 by default, `zstd_level` configurable 1–19) is kept only if it saves at least 3%, so already-compressed content (most media attachments) is stored raw rather than burning CPU for nothing. Raw blob size is capped at 4 GiB (`maxRawLen`), and readers reject stored lengths beyond that bound plus a small overhead allowance before allocating.

Reading a blob verifies, in order: the footer trailer hash (at open), the entry CRC over stored bytes, then — after decompression — that SHA-256 of the result equals the blob ID.

Plain format-v1 frames may be consumed incrementally without changing their
encoding. In that mode, bytes read before terminal EOF are an unverified
prefix; the CRC, decoded length, trailing-data check, and blob identity become
authoritative only when the stream reaches its successful terminal result.
Closing early is not success. Encrypted format-v1 frames retain whole-entry
authentication and therefore remain buffered; safely streaming encrypted
prefixes requires a future chunk-authenticated format.

## Index Objects (`.mvidx`, magic `MVIX`)

Immutable mappings from blob ID to pack location, written once per `create` after its packs are sealed:

```
"MVIX" | version u16 (=1) | entry_count u32 |
entries: blob_id [32] | pack_ulid [16] | offset u64 | stored_len u64 | flags u8   (65 bytes each)
SHA-256 trailer
```

Entries are strictly sorted by blob ID; decoders reject unsorted or duplicate entries. Readers load the union of all index files. An index file orphaned by an interrupted backup (index written, manifest never written) is safe by construction: indexes are only ever written after their packs are durably sealed, so an orphan references real, valid blobs and deduplicating against it is correct.

## Page-Hash Objects (magic `MVHK` keyframe / `MVHD` delta)

The incremental-capture state: the truncated SHA-256 (first 16 bytes) of every 4 KB database page.

```
keyframe: "MVHK" | version u16 | page_size u32 | page_count u64 | hashes (page_count x 16) | trailer
delta:    "MVHD" | version u16 | page_size u32 | new page_count u64 | entry_count u32 |
          pages (u64 each, strictly ascending) | hashes (entry_count x 16) | trailer
```

Applying a delta resizes to the new page count (growth zero-fills, shrinking truncates) and patches the listed pages. All count and size fields are validated overflow-safely against the actual body length before any allocation.

## Page-Map Objects (magic `MVMK` keyframe / `MVMD` delta)

Where each database page's content lives, as sorted, non-overlapping runs:

```
"MVMK"/"MVMD" | version u16 | page_size u32 | page_count u64 | blob_count u32 |
blob table (32-byte blob IDs) | run_count u32 |
runs: start_page u64 | page_count u32 | blob_index u32 | blob_offset u64   (24 bytes each)
SHA-256 trailer
```

A keyframe must cover `[0, page_count)` with no gaps; deltas are sparse. Delta application unions the blob tables, subtracts the delta's intervals from the base runs (splitting runs with byte-exact offset adjustment), and merges — a linear sweep over both sorted run lists. Materializing a snapshot's map and concatenating the referenced page ranges reproduces the database file byte-for-byte; the end-to-end test asserts exactly that.

**Capture grouping:** contiguous dirty ranges of ≥ 256 pages become dedicated blobs, split at 1024 pages (4 MiB); smaller scattered ranges are grouped into shared blobs of at most 1024 pages.

**Keyframe cadence:** a snapshot writes fresh keyframes (instead of deltas) when the chain would exceed 30 deltas or when the accumulated deltas' stored size exceeds the previous keyframe's, bounding both chain-walk depth and wasted space. Chain walks independently enforce cycle detection and the depth bound, so corrupted parent links fail deterministically.

## Portable Metadata

A version-3 snapshot may replace the database page-map and page-hash chains
with one application-defined logical metadata blob:

```json
"metadata": {
  "format": "application-defined-format",
  "blob": "<sha256>",
  "bytes": 1234
}
```

The blob is stored in the same content-addressed packs as every other backup
object. `format` identifies the logical serialization and is interpreted only
by the application. Kit verifies its declared length and SHA-256 identity but
does not parse or migrate it. A portable manifest must not also contain a
database page map: one snapshot has exactly one metadata authority.

Portable metadata is a first-class durable representation, not an intermediate
upgrade format. On restore, `MetadataRestorer` streams the logical artifact
into a Kit-owned private scratch path and constructs the application's current
runtime database. Kit prefers private repository staging when its resolved
directory is disjoint from the restore target, otherwise it uses a resolved
system temporary directory. Kit then copies the closed file through its held
target-root descriptor into unpublished staging. The application never receives
a path whose resolution depends on the caller-supplied target. This allows the
archive representation to remain stable while runtime schemas evolve. The
restorer must consume the stream through verified EOF, finish and close the
database, and leave no SQLite sidecars before Kit can publish it. Existing
SQLite-page snapshots remain readable, and a repository may contain both kinds.
A SQLite capture following a portable snapshot starts a fresh page-map keyframe
because there is no prior page chain to inherit.

The selected scratch filesystem—normally repository staging, or system
temporary storage when the target contains repository staging—must have
capacity for the complete rebuilt runtime database. Confining it into the target
requires one complete sequential copy, so the target must simultaneously have
capacity for its unpublished copy. This deliberate scratch cost keeps the
application callback independent of the caller-supplied target path on every
supported platform.

## Auxiliary Artifacts

A version-4 or newer snapshot may carry a bounded, name-sorted list of
application-defined artifacts alongside either metadata representation. Each
manifest entry records a canonical name, an opaque format identifier, byte
length, blob identity, and SHA-256 digest. The artifact bytes use the same
content-addressed packs and verification path as every other snapshot object.

For portable metadata, the artifact list comes from the same pinned
`MetadataSnapshot`; for SQLite capture it comes from the pinned `FrozenView`.
Kit opens and streams each artifact exactly once before releasing that view.
It interprets neither the format nor the bytes.

Quick verification proves every artifact resolves through the repository
index and pack footer. Full verification reads it and re-derives its length
and SHA-256. Restore performs the same content verification, proves the staged
database, and then delivers the complete bytes to
`AuxiliaryTarget.StageAuxiliary`. Staging must not expose the replacement
state. If cancellation, extras promotion, database publication, durability
sync, or the final auxiliary commit fails, Kit invokes `Rollback` with a
bounded context independent of caller cancellation. `Commit` runs only after
the restored target is published, synced, and released from restore
coordination. A missing target or staging error fails while the restored
database remains unpublished.

## Large Logical Objects

Version-5 snapshots split new known-size content and portable metadata larger
than 64 MiB into ordered chunks of at most 64 MiB. Each chunk is an ordinary
hash-addressed pack entry; pack format v1 and its frame limits are unchanged.
Content hashes still identify the complete original file.

`attachments.recipes` lists the hashes of content recipe objects. Portable
metadata uses the optional `metadata.recipe` field. Each recipe is JSON:

```json
{"version":1,"blob":"<whole-object-sha256>","bytes":67108865,"chunks":[{"blob":"<chunk-sha256>","bytes":67108864},{"blob":"<chunk-sha256>","bytes":1}]}
```

A recipe has at most 1,048,576 chunks and 128 MiB of encoded metadata, bounding a
logical object at 64 TiB. Chunk lengths must be positive and sum to the object
length. Metadata recipes must match the manifest's whole-object identity and
length. Snapshots with recipes require reader version 5; older readers refuse
them. Existing snapshots and small-object encodings remain readable.

Recipe decoding validates each chunk before retaining it and stops at the
chunk-count limit. The encoded byte limit alone cannot bound the memory used
by an array of many short or invalid entries.

Capture reads chunked objects sequentially with one reusable chunk buffer and
checks the whole-file hash before publishing a manifest. Chunked files reserve
one 64 MiB buffer against the capture budget, allowing ordinary file workers to
continue alongside them. Ordinary files use one parallel worker pool for the
entire capture. Directory reads use the file's stat size to select chunking.
Unknown-size `ContentSource` references reuse their recorded sizes from the
parent snapshot when available. Otherwise they retain the parallel single-blob
path and its 4 GiB limit; sources must declare larger sizes on first capture.
Incremental snapshots reuse chunks and recipes by hash; each snapshot carries
recipes for its current content population.

Existing whole content blobs within the 4 GiB frame limit are reused after
verifying the source again. They do not acquire recipes or force a snapshot to
require reader version 5. A snapshot may contain both whole blobs and chunked
objects; only objects with recipes use chunk reconstruction.

Quick verification checks recipe hashes and every referenced chunk's index and
pack footer. Full verification reads logical objects serially and reports bytes
after each 64 MiB read. Full verification and restore verify each chunk's bytes
and the concatenated object's length and hash through terminal EOF. Prune follows
recipe references, keeping their chunks reachable. Restore rebuilds chunked
content as complete loose objects even when small objects restore into managed
packs. The rebuilt metadata database remains unpublished on verification failure.
Auxiliary artifacts retain their separate size limit.

Version-6 snapshots extend the same recipe format to operational extras larger
than 64 MiB. `extras.recipes` lists their recipe hashes; the extras tree still
records each complete file's hash, length, path, and mode. Capture streams each
file through one chunk buffer and checks its length against the opened file's
size. If the bytes read differ from that opening size, the entire backup fails,
including for small extras. Callers must provide stable files or consistent
database snapshots; matching length alone does not detect in-place writes.
Files at most 64 MiB retain their single-blob representation and do not raise
the minimum reader version. Restore, full verification, and prune follow
extras recipes just as they do content recipes. A snapshot containing chunked
extras requires reader version 6, since older readers do not follow
`extras.recipes`. Once such a snapshot exists, older readers cannot list
snapshots, create backups, prune, or select the latest snapshot for restore or
verification; verifying all snapshots also fails. Restore and verification of
an older supported snapshot still work when its ID is supplied explicitly.
Upgrade every reader before writing chunked extras to a shared repository.

Restore stages each extra as a complete temporary file before replacing its
destination. Allow temporary disk space for all extras being restored; during
an overwrite restore, their old files remain until the replacements are staged.

Extras capture has no pre-recorded content hash. Existing extras larger than
64 MiB are stored as chunks on their first capture with this writer, even when
an older snapshot holds the complete file as one blob. That blob remains until
the older snapshot is forgotten and its unreferenced data is pruned. Subsequent
captures reuse identical chunks by hash.

## Attachment Lists (magic `MVAL`)

```
"MVAL" | version u16 (=1) | entry_count u32 |
entries: content SHA-256 [32] | size u64   (40 bytes, first-seen order)
SHA-256 trailer
```

A snapshot's manifest references one or more list blobs whose union is exactly the attachment population of that snapshot. In the common append-only case, a snapshot inherits its parent's list blobs and adds one new segment; when the live set has shrunk (attachments were deleted), the snapshot writes one fresh full list instead, so the union invariant holds in both directions. Attachment content is re-read and re-hashed at every capture — from the attachments directory, or from an application-supplied `ContentSource` when `CreateOptions.ContentSource` is set — and content whose bytes no longer match the recorded hash fails the backup rather than being stored wrong. The wire format is identical either way; the source only changes where capture acquires bytes.

## Reading selected snapshot files

`Repo.OpenSnapshot(ctx, snapshotID, packExtension)` reads an exact snapshot
without restoring its database or content store. `Object(hash)` and
`Extra(name)` return the logical hash and size of files listed in that snapshot.
Objects found only in another snapshot or an orphaned pack are not members.
Recipe records, chunks, and portable metadata are not separate members unless
an attachment list or extras tree also lists them as files.

Opening a reader loads the repository-wide blob index, the snapshot's complete
file lists, and all chunk recipes, even when copying one file. Each new reader
repeats this metadata work.

`CopyObject` and `CopyExtra` stream and verify the complete file, including every
chunk and the whole-file hash and length. A failure can leave partial bytes in
the destination: discard all output unless the copy and destination close
succeed. These methods do not publish or remove the caller's output files.

A reader holds a shared repository lease and caches at most 64 open packs and
their verified footers. Use it sequentially and close it promptly, preferably
after copying one group of files and before parsing them. Metadata lookups still
work after close; copying requires an open reader. A fresh reader reloads pack
placement because prune can relocate objects between reads.

Backups and ordinary maintenance need an exclusive lease. Opening a reader
during either operation returns `ErrRepoLocked`; retry later. A backup waits up
to 60 seconds for existing readers, then returns the same retryable error.
Forced maintenance can invalidate reader leases. A lease also becomes stale
after 30 minutes without a heartbeat, for example while the machine sleeps,
allowing backups and maintenance to proceed. Missing or damaged files fail the
copy. Never switch snapshots or force access in response to a read failure.
Lease heartbeat files require a writable repository even when only reading
archive payloads. Attaching a reader does not retain a snapshot against future
forget operations.

For example, copy one extra to a private temporary file, release the lease,
then pass that file to the application for inspection:

```go
func copyExtra(ctx context.Context, repo *backup.Repo, snapshotID, ext, name string) (string, error) {
    reader, err := repo.OpenSnapshot(ctx, snapshotID, ext)
    if err != nil {
        return "", err
    }
    file, err := os.CreateTemp("", "snapshot-extra-*")
    if err != nil {
        return "", errors.Join(err, reader.Close())
    }
    _, copyErr := reader.CopyExtra(ctx, name, file)
    if err := errors.Join(copyErr, file.Close(), reader.Close()); err != nil {
        return "", errors.Join(err, os.Remove(file.Name()))
    }
    // The caller can inspect the verified file now, then remove it.
    return file.Name(), nil
}
```

## Snapshot Manifests

A manifest is indented JSON with a fixed field set: format versions, `snapshot_id`, `parent_id`, `created_at` (RFC 3339 UTC), capture options, database geometry and page-map/hash-map chain heads, attachment lists and totals, extras tree, exclusions, stats, the packs and index added, duration, and bytes added.

**Snapshot ID derivation:**

```
snapshot_id = <UTC yyyymmddTHHMMSSZ> + "-" + first 32 hex chars (128 bits) of
              SHA-256(compact JSON of the manifest with snapshot_id = "")
```

The ID is content-derived: identical content at the same second produces the same ID, and any change to the manifest changes it. Readers recompute the ID from the manifest body on load and refuse a mismatch with the filename or embedded `snapshot_id`, so a renamed, corrupted, or forged manifest is rejected; the 128-bit digest keeps crafting a different manifest with the same ID computationally infeasible. `create` additionally enforces **strictly monotonic timestamps** per repository (bumping past the parent's second when two snapshots land within one second), so lexicographic ID order is chronological order and parent selection is deterministic.

**Trust model.** The content-derived ID is tamper-evidence, not cryptographic authentication: it is an unkeyed hash binding a manifest's content to its filename, so corruption, renames, and serving different content under a known ID are all detected — but an actor who can write to the repository can add a wholly new, internally consistent snapshot (packs, index, and manifest with a correctly derived ID) that readers accept, and an empty `SnapshotID` restores whatever snapshot sorts latest. Restoring from storage that other principals can write therefore requires pinning the expected snapshot ID through a trusted channel (`RestoreOptions.SnapshotID`); repository encryption (planned, below) closes this fully by binding every object to a key the repository does not store.

Manifests hash Go's canonical struct-order JSON encoding, and the manifest contains no map-typed fields, so serialization is fully deterministic. This is a deliberate reason the format uses JSON rather than a schema-compiled encoding such as protobuf: protobuf serialization is not canonical across implementations or library versions, which would break content-derived IDs, and its silently-ignore-unknown-fields evolution model is the opposite of what a backup format wants — unknown data must be refused via explicit versioning, not skipped. JSON manifests are also inspectable with nothing but `cat` and `jq`, which matters when debugging a decade-old repository.

## Crash Consistency

Every repository file is published atomically: written to `staging/`, fsynced, renamed into place, parent directory synced. Pack publication additionally refuses to replace an existing file (sealed packs are immutable and located by ID, so a name collision must fail the seal, never overwrite a pack existing snapshots reference). Within one `create`, the write order is:

1. Pack files sealed (durable),
2. Index object written,
3. Manifest written **last**.

A crash at any point leaves either a complete snapshot or no snapshot, never a
manifest referencing missing data. A failed capture can leave sealed packs even
when a source fails whole-file hash or length verification. `Abort` discards only
the open pack; sealed packs and any index written before the manifest remain
unreferenced and reclaimable by `Prune`.

## Locking

`create` holds an exclusive lock; `verify` holds a shared lock (concurrent verifies allowed). Locks are JSON files under `locks/` recording hostname, PID, operation, and acquisition time; holders refresh the file mtime every 30 seconds after re-verifying they still own the file, and locks older than 30 minutes are reaped as stale. Acquisition uses a plant-then-recheck handshake on both sides to close the create/verify race window, and release re-reads the file and removes it only if every field still matches the holder's own record.

## Freeze Protocol

To capture a transactionally consistent database image while the application's database-owning process (for example, a daemon) keeps running, the SQLite page-capture path does the following:

1. `OpenFrozenSession` calls `FreezeCoordinator.Begin`, which the application implements to pause conflicting writes against the live database — for example, an authenticated same-host call into a daemon's serial operation gate — and returns once the gate is held. The application is expected to bound this with its own watchdog so a crashed capture cannot wedge the gate forever.
2. It opens its own SQLite connection, runs `PRAGMA wal_checkpoint(TRUNCATE)` (with bounded retries) until the WAL is empty, then pins a read transaction — from this point the main database file bytes cannot change under it.
3. It immediately calls `FreezeCoordinator.End`. The gate is released and normal writes resume; the pinned transaction alone keeps the file image stable for the page scan. Database geometry, statistics, and content locators are all read inside the pinned transaction (`App.FrozenView`).

The freeze window is therefore milliseconds-to-seconds regardless of archive size. `Create` refuses to run unfrozen against a live database owner: an application whose `FreezeCoordinator.Begin` cannot resolve the owner should fail rather than risk a torn read.

For portable metadata capture, Kit holds the same coordinator gate while
`MetadataSource.OpenSnapshot` establishes an application-defined stable view,
then releases the gate. The metadata stream, content references, and stats must
all come from that one view, which remains open until metadata capture finishes.

## Restore

`Restore` materializes one snapshot into a target directory as a usable copy of the application's data. For a page-map snapshot, the database is written run-by-run at `page × page_size`. For a portable snapshot, the application consumes the verified logical metadata stream and builds its current runtime database at Kit's private staging path. Content files are written at the storage paths the restored database records for each hash (applications may namespace paths beyond the loose `<hash[:2]>/<hash>` layout; paths are re-validated as local before writing), and captured extras at their recorded relative paths and file modes (tree entry paths are re-validated as local and traversal-free before writing). It refuses a non-empty target unless `Overwrite` is set.

Restore is destructive only after the source has proven itself, in two layers. A preflight runs before the target is touched: the snapshot's map chains must materialize, every referenced blob (pages, attachments, extras) must resolve through the index, and the extras tree's paths must pass restore's locality, reserved-name, and collision rules. Failures the index cannot reveal — unreadable or corrupt pack bytes — are covered by ordering instead: the database is materialized and page-verified in a staging temp, attachments are then read (each re-deriving its SHA-256) and written to their content-addressed paths, extras are read and staged as temp siblings of their final paths, and the restored manifest statistics are reproduced against the staging temp. Callers may additionally request SQLite's full `PRAGMA integrity_check`; this can be expensive for large databases and is distinct from restore's cryptographic page verification. Only after those checks do the staged extras get renamed over their live counterparts and the database published: the target's stale SQLite sidecars are set aside (renamed, so a failed publish puts them back rather than stranding the old database without its WAL), the temp is renamed over the existing database, and the asides are removed. An `--overwrite` target's live database and extras files therefore survive any content or proof failure up to that final swap; partial attachment writes are benign because the paths are content-addressed — a write to a path the live tree already uses is byte-identical, and a write to a new path is an orphan the live database never references. One caveat: that argument holds only for the canonical `<hash[:2]>/<hash>` layout. An application that namespaces attachment paths (reader version 2) can record different content at the same path across snapshots, so a failed overwrite restore may leave such a path already rewritten; no current application does this, and an application adopting namespaced paths onto live overwrite targets should derive the path from the content hash to stay in the benign case. Content-path derivation and the checks both read the staging temp, never the not-yet-replaced database.

Restore is self-proving, in layers. During materialization every blob read re-derives its SHA-256 identity (the pack reader's normal contract) and every database page is additionally checked against the snapshot's page-hash map before it is written — so a page-map bug cannot silently place correct bytes at the wrong offset. After materialization the restored database reproduces the manifest's recorded stats (via `App.RestoredStats`) through exactly the queries capture ran inside the freeze window. Unless `SkipIntegrityCheck` is set, it also passes `PRAGMA integrity_check`; callers restoring large databases may omit that SQLite scan without disabling cryptographic page or blob verification. The end-to-end test further proves the restored file is byte-identical to the live database as it existed at capture time, including for parent snapshots restored from an incremental chain. All files, and the directory entries naming them, are fsynced before Restore reports success. Pack reads are grouped by pack with a `Jobs` worker bound (1 = strictly serial for spinning-disk repositories); serial and parallel restores produce byte-identical trees. Restoring an old SQLite-page snapshot for use with a newer application version still goes through the application's normal schema migration at first open. A portable snapshot instead rebuilds the current runtime schema during restore, so runtime database migrations are not part of that archive's compatibility contract.

`BeforePublication` receives `TargetDir` together with a private scratch
`DBPath` outside that namespace. Kit prefers repository staging when its
resolved directory is disjoint from the target, otherwise uses a resolved
system temporary directory, and refuses the callback when neither is outside.
It copies the unpublished database into that scratch before invoking the
callback, so replacing the target directory cannot redirect callback writes.
The callback must finish its update, checkpoint and close SQLite, and leave no
`-wal`, `-shm`, or `-journal` sidecar. Kit rejects any other output, copies the
exact closed regular file back through the held target-root descriptor, then
runs the normal integrity and statistics proof before canonical publication. A
callback error or invalid output therefore leaves the canonical database
unpublished.

`RestoreOptions.LooseCompression` opts into Kit's canonical loose-file
encoding for every attachment that is not imported as a pack, including
chunked objects. It requires one `<hash[:2]>/<hash>` path per object and a
`BeforePublication` callback. The callback receives verified `LooseContent`
receipts and must record their encoding and stored length in the unpublished
catalog. Compressible content uses the existing `.zst` representation; the
backup format and reader version do not change. Extras and the database are
unaffected. Existing verified objects are reused without changing encoding.
Overwrite repair removes damaged alternates. Valid alternate encodings remain
until the new catalog is durably published, then Restore removes and syncs them.
Memory is bounded while streaming, but each in-flight loose write temporarily
stages both raw bytes and a compressed candidate before keeping one.

### Optional packed-content restore

Repository attachment membership is representation-neutral: the snapshot's
attachment lists and the content paths derived from the restored database are
the liveness authority whether bytes are restored loose or in packs. A pack
file or footer entry alone never grants application read authority. Without a
`PackedContentTarget`, restore follows the fully-loose path described above.

With a packed-content target, restore may copy compatible plain version-1
repository packs once into the target store's sharded `packs/<aa>/<id>.mvpack`
layout. The source pack remains immutable and in place. Before a copied pack
can receive catalog authority, restore validates the whole container, footer,
entry count, version, flags, and encoding settings against the target's
configured `packstore.Limits`. It verifies each snapshot-selected entry against
the footer metadata, decodes and CRC-checks the stored frame, and checks its raw
size and SHA-256 identity. The application catalog records the immutable totals
for the whole footer, but authorizes only the selected hashes that are both live
in this snapshot and eligible for packed storage. Imported pack `CreatedAt` is
the restore time, so age-based maintenance does not immediately churn freshly
restored packs.

Compatibility is deliberately per pack and per selected entry. A pack falls
back to loose restore when its container, footer, or entry count exceeds the
configured target limits, when its otherwise recognizable version or encoding
settings are unsupported, or when the target filesystem cannot atomically
publish an immutable pack. A selected entry larger than the configured
`BlobBytes` ceiling falls back independently while eligible siblings in the
same copied pack can remain packed. An unsupported application pack extension
keeps all content loose and commits an empty packed-authority replacement.
Unsupported pack settings keep the affected packs loose; when no pack is
compatible, the same replacement contains no pack records or hash mappings.
Whole-pack limit fallback verification has separate conservative hard ceilings
of 8 MiB of footer data and 100,000 entries; a larger footer fails restore
before scanning or scratch-file creation instead of doing unbounded verifier
work. These checks protect availability for pathological or damaged inputs and
do not add per-entry bookkeeping to ordinary verification.
These are compatibility outcomes, not integrity waivers: every declined hash
must still pass the ordinary encoding-aware loose read, size check, and SHA-256
verification before restore can succeed. Corruption, selection/footer metadata
mismatch, or a same-ID destination with different bytes is a hard failure, not
a fallback.

Before packed attachment restoration starts, the application supplies a live
mutation lease from the same process-local `packstore.Coordinator` used by
every maintainer that can adopt, repack, or remove target-store content. The
application must acquire its broader operation gates before that lease; it
transfers sole ownership of the lease to Restore, which validates it and holds
it across pack publication, loose fallbacks, extras, database checks, database
publication, and the final durability sync. Restore releases the lease on every
success and failure path, joining a release failure with the primary restore
error. `OpenRestoreCatalog` and `ReplaceRestoredPacks` run under the existing
lease and must not reacquire or otherwise reenter that Coordinator. A
restore without a `PackedContentTarget` does not acquire a packed-store lease.

Publication and authority follow one crash-safe order:

1. Stream the source pack to a private target staging file, sync it, and close
   every handle.
2. Publish without replacement by hard-linking the staging file to its final
   sharded name, remove the staging link, and sync the containing directory.
3. Reopen and verify the final file. An existing byte-identical final file is
   safe to reuse on retry; validating that collision may require hashing the
   whole container.
4. Materialize and durably sync every loose fallback.
5. In one application-owned transaction against the unpublished staged
   database, replace all packed catalog records and selected mappings.
6. Close the SQLite staged-catalog connection and remove its exact `-wal`,
   `-shm`, and `-journal` sidecars.
7. Reopen the main staged database file read-write, verify that the opened
   handle still names the inspected regular file, sync it, re-check that the
   staged path still names that same regular file, and close the handle.
8. Run the optional SQLite integrity check, reproduce the manifest statistics,
   then publish the database last.

The staged catalog mutation must leave the database structurally valid and
must not change the application's `RestoredStats` payload. An overwrite
target's old visible database therefore survives any failure through catalog
replacement and database checks. A crash before catalog replacement can leave a valid
uncataloged pack, but it grants no read authority; maintenance may adopt,
remove, or retain it according to application liveness. Retry never depends on
that orphan surviving: it either reuses a byte-identical final pack or fails
closed on a collision, then replaces authority idempotently. A crash after the
catalog transaction but before database publication changes only the staged
database, not the visible one.

The first path component `packs` is reserved inside the content directory only
when packed restore is enabled, preventing restored loose paths from colliding
with the production pack subtree. In a fully-loose restore it remains an
ordinary application path.

This path primarily removes per-blob file creation and associated filesystem
or antivirus scanning for the packed subset. It does not eliminate read I/O:
restore still streams each copied container and hashes every selected blob,
and an existing-final retry can hash the entire container. A mixed restore also
pays a complete content-directory durability traversal after loose fallback
materialization and before packed authority is committed.

On Windows, durability follows Kit's established policy: regular files are
flushed, handles are closed before publication and reopening, and directory
sync is a documented no-op. Atomic no-clobber publication uses a hard link on
both Windows and Unix. If a filesystem cannot provide that operation, a new
pack falls back loose instead of using a replacement rename with a race window.
`RestoreResult` exposes packed and loose blob counts, imported pack count, and
structured fallback reasons so callers can report both the achieved layout and
why content stayed loose.

## Verification Model

`verify` enumerates every blob a manifest can reach — page-map chains and their blob tables, hash-map chains, attachment lists and every listed content hash, the extras tree and its entries — and checks each against the index and packs. Quick mode proves structure (references resolve, objects decode, packs exist); full mode additionally reads every referenced blob and re-derives its SHA-256 identity, compares each attachment list and extras tree entry's recorded size against the blob's actual content length (restore refuses a mismatch, so verify must flag it), confirms materialized page/hash maps match the manifest's recorded geometry with full coverage, and checks every page-map run against its blob's actual bytes — the run must fit inside the blob and each mapped page must hash to the page-hash map's entry, exactly as restore's materialization requires. Extras tree paths are held to restore's rules in both modes: escaping or reserved-overlapping paths and case-folded path collisions are Problems, not restore-time surprises. Capture enforces the same rules, so a snapshot with such paths is never written in the first place; verify's check exists for trees written by other tools or tampered after the fact. Problems are collected, not fail-fast, and each names the snapshot, blob, and pack involved.

## Removing Recovery Points

`Forget` removes explicitly selected snapshot manifests under the exclusive
repository lock. `DryRun` validates the same selection and reports the removal
order without deleting manifests. Duplicate IDs are ignored; unknown IDs fail
before deletion. Removing the last recovery point requires `AllowEmpty`, which
is independent of lock recovery's `ForceUnlock`.

Forget rejects a symlinked `snapshots` directory and keeps its validated directory
handle for manifest reads, removals, and directory syncing. Waiting for active
repository readers honors cancellation and releases the waiting writer's lock;
it does not interrupt an operating-system filesystem call already in progress.

A retained SQLite incremental snapshot still needs its parent manifests through
the first keyframe. Forget rejects a selection that would break that chain.
Portable metadata snapshots and SQLite keyframes are self-contained: their
`ParentID` does not keep an older manifest alive. Selected children are removed
before their parents, with the snapshots directory synced after each deletion
(subject to the Windows directory-sync limitation described above). Cancellation
or an error can leave a partially completed selection; the result reports which
manifests were removed. A sync error means the last removal may not be durable.

Forget leaves packs and indexes untouched. It does **not** erase historical
content or reclaim disk space. Run `Prune` separately to reclaim unused storage.

## Reclaiming Repository Space

`Prune` takes the exclusive repository lock and collects the content needed by
every remaining snapshot. It reuses the quick verifier's traversal of SQLite
page/hash-map chains, portable metadata, auxiliary artifacts, attachment lists
and content, and extras trees and files. An unreadable or incomplete reference
walk stops cleanup before any index or pack is removed. No live application
database or source content directory is needed.

Prune deletes wholly unused packs, including orphan packs left by interrupted
backup or cleanup runs. A pack with **less than 50% live encoded payload** is
rewritten with only its referenced blobs. Exactly-half-live and mostly-live
packs remain untouched. The ratio excludes pack framing and footer bytes;
duplicate copies not selected by the current index count as unused. Prune does
not choose snapshots to forget. An empty repository has no live blobs, so all
recognized packs are eligible after an explicit `Forget` with `AllowEmpty`.

Replacement content passes through verified streams and bounded-memory blob
preparation. Scratch storage must accommodate one blob's preparation plus all
replacement packs while the old packs remain present. Reference maps and pack
footers still use memory proportional to their entry counts. Unchanged payloads
receive structural checks, not a full reread; use full `Verify` for an integrity
scrub. Only plain repositories are currently supported, as with `Open`.

Publication follows this order:

1. Seal and sync replacement packs.
2. Write and sync one merged index containing every live blob.
3. Remove every previously inventoried index, syncing the index directory.
4. Remove obsolete packs, syncing each affected shard directory.

The index loader unions **all** index files. During index retirement, old and
new indexes may select either copy; both packs remain present until every old
index removal is durable. This does not depend on a new index ID sorting last.
After interruption, a retry recomputes liveness and can collect redundant or
unindexed packs. Manifests are never rewritten; their original `NewPacks` and
`NewIndex` fields describe capture history, not current content locations.

Cleanup rejects symlinked repository subdirectories and symlinks in the pack
tree. Destructive removals and directory syncs use retained directory handles.
Other repository clients must honor the same lock; concurrent out-of-band
filesystem modification is not supported. Directory syncing follows the
Windows limitation described above. Cancellation stops between operations and
during streamed copying, but cannot interrupt an already-blocked OS file call.

`DryRun` performs the same reference and selection checks without writing or
removing packs or indexes. `BytesToRemove` counts complete old pack files;
`LiveBytesToRewrite` counts their currently encoded live payload, **not** an
exact prediction of replacement pack size or net savings. `BytesRemoved` and
`BytesWritten` report actual pack-file work, which may be partial on error.
These counts exclude indexes and temporary scratch; a sync failure can leave
the last removal non-durable.

Unused bytes inside mostly-live packs remain until those packs qualify for
rewriting. Cleanup is not secure erasure and cannot remove copies retained by
filesystem snapshots or storage hardware.
Applications choose their own retention schedule and pass explicit snapshot IDs.

## Current Limitations

- Repository encryption is not yet implemented; the format reserves flags and fields for it (`encryption` in the repo config, the `encrypted` blob flag, crypter parameters threaded through the code as nil).
- Default pruning leaves unused bytes inside packs that are at least half live.
- The runtime database produced by restore must currently be SQLite because
  packed-content catalog replacement and final database checks operate on it.
  The archived metadata itself may be an application-defined portable
  serialization.
- The application's write gate is held only through the freeze protocol (checkpoint plus read-transaction pin), not through content capture. An operation that deletes content files while a backup is still capturing can therefore delete a file the frozen database still references; the backup then fails loudly with a read or hash error and can be retried after the deletion completes. This is a deliberate trade: holding the gate — and with it every write — for the full capture window would be far more disruptive than a rare retryable backup failure. A snapshot that completed is unaffected: it captured every file it references.

## Roadmap (settled design, not yet implemented)

The following behaviors were designed alongside the shipped format — the format hooks for them already exist — and are recorded here as the binding intent for the follow-up phases.

A restore-check verification mode performing full restore materialization and
database checks against scratch space, without writing a target, remains planned.

**Encryption.** Initializing a repository with encryption enabled generates a random 256-bit repository key; every blob, footer, index, and manifest is encrypted with XChaCha20-Poly1305, with the AAD binding each ciphertext to its identity (blob ID, or object role plus ID). The repository key is wrapped with [age](https://age-encryption.org) to one or more recipients (scrypt passphrase and/or X25519 identities) in `keys/master.age`; adding, removing, or rotating recipients rewraps the key without rewriting objects. `config.toml` stays plaintext by necessity; tampering yields detectable failures, not silent corruption. Key loss is unrecoverable by design. Blob IDs remain plaintext-content hashes but appear only inside encrypted metadata.

**Full compaction.** An explicit mode that rewrites every mixed pack, regardless of its live fraction, remains a follow-up. It would reclaim more unused bytes at the cost of more I/O and temporary storage.

**Performance follow-ups.** Two accepted deferrals from review: detecting a same-page-size `VACUUM` by delta-ratio anomaly (warn that a keyframe would be cheaper), and a streaming page-map merge for memory-constrained hosts. Further out: an export mode (one self-contained archive file), WAL shipping for point-in-time recovery, native remote backends, and application-scheduled backups.
