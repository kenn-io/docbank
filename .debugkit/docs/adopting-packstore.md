# Adopting packed content storage

This guide is for applications moving a content-addressed file tree onto
Kit's mixed loose-and-packed storage. It describes the application boundary,
the required transaction ordering, and the choices that remain application
policy. The on-disk pack representation is documented separately in the
[backup repository and pack format](../backup/FORMAT.md).

## Ownership boundary

Kit owns physical mechanics:

- canonical loose paths and immutable pack paths;
- verified loose publication and mixed loose/packed reads;
- pack validation, descriptor caching, and reader retirement;
- pack, repair, repack, and unpack operations; and
- crash-ordered import of compatible packs during restore.

The application owns authority and policy:

- which hashes are live members of the content store;
- the database schema and transactions recording pack mappings;
- application mutations and cross-process exclusion;
- maintenance scheduling and run budgets; and
- object, container, scratch, concurrency, and retention limits.

A file's presence never grants authority. `Resolver.Resolve` must report both
current membership and the optional authoritative pack mapping. Kit verifies
that a catalog mapping exactly matches the immutable footer before exposing
packed bytes.

## Layout and construction

`Layout` reserves `packs/` beneath the content root. Applications must reject
logical content paths that collide with this subtree. Loose objects use the
canonical `<first-two-hash-characters>/<lowercase-sha256>` layout.

Create one process-local `Coordinator` and share it between application
mutations and the `Maintainer`:

```go
limits := packstore.DefaultLimits()

layout, err := packstore.NewLayout(contentRoot, packstore.LayoutOptions{
	Staging: packstore.StagingSameDirectory,
})
if err != nil {
	return err
}

coordinator := packstore.NewCoordinator()
maintainer, err := packstore.NewMaintainer(catalog, layout, packstore.MaintainerOptions{
	Limits:      limits,
	Coordinator: coordinator,
	Store:       packstore.StoreOptions{ReaderSlots: 16},
})
if err != nil {
	return err
}

store := maintainer.Store()
loose, err := packstore.NewLooseStore(layout)
if err != nil {
	return err
}
```

Close the maintainer during shutdown and report any descriptor-close error.

`Coordinator` is intentionally process-local. The application must acquire
its own cross-process database/storage lock before acquiring a coordinator
lease. It must not upgrade or reenter a lease.

## Catalog contract

An application catalog implements `packstore.Catalog`. Its methods form the
authority boundary and must preserve these meanings:

- `Resolve` returns a stable membership decision and, when packed, the exact
  footer metadata authorized for that hash.
- Inventory methods return internally consistent snapshots. Preserve the
  order of `ListUnpacked` when it carries useful physical-locality hints.
- `ListReferences` sets `Complete` to false if any application reference could
  not be classified. Kit can still pack known-valid members, but will suppress
  deletion that depends on complete reachability.
- `RecordPack` and `AdoptPack` atomically add the pack record and selected
  mappings after Kit has durably published and verified the pack.
- `CommitRepack` atomically replaces exactly the expected source mappings.
  A concurrent catalog change must fail the compare-and-swap rather than
  silently moving a different live set.
- `ClearPackMetadata` removes packed authority only after `Unpack` has durably
  materialized every live object loose.

Catalog transactions must not grant authority to a staging path or roll back
to a retired physical representation. A database failure after physical
publication can leave an unreferenced immutable pack; later maintenance can
verify and adopt or remove it safely.

`packstore/packstoretest.MemoryCatalog` is a useful reference model for adapter
and lifecycle tests. Production adapters still need database-specific tests
for transaction atomicity, compare-and-swap behavior, incomplete reference
inventories, and retry after a failed commit.

## Loose writes and application mutations

Publish content before committing application membership. Hold a mutation
lease across the physical write and the application transaction so maintenance
cannot race the transition:

```go
lease, err := coordinator.AcquireMutation(ctx)
if err != nil {
	return err
}
defer lease.Release()

result, err := loose.Write(ctx, src, packstore.WriteOptions{
	Durability:   packstore.DurablePublication,
	Dedup:        packstore.VerifyFullHash,
	ExpectedHash: hash,
	ExpectedSize: size,
	SizeKnown:    true,
	MaxBytes:     looseWriteLimit,
})
if err != nil {
	return err
}

// Commit application membership for result.Hash in one database transaction.
```

If the database transaction fails, the durable loose object is an authority-free
orphan and can be reconciled later. Do not delete it as transaction rollback
unless the application can prove that no concurrent member references it.

`looseWriteLimit` is application ingestion policy and is independent of the
packed-maintenance `limits.BlobBytes` value. Reusing the maintenance limit here
is appropriate only when the application deliberately gives loose and packed
objects the same ceiling.

## Buffered and streaming reads

Existing callers can retain `Store.Open` when they need a seekable result, or
`Store.ReadBounded` when they need an all-or-nothing byte slice. Those APIs
verify complete packed content before returning it.

Use `Store.OpenStream` for incremental consumption. A successful read of a
prefix is not verification. Trust the complete object only after terminal
`io.EOF`, a successful `Verify`, or `Verified` returning true. Always observe
the `Close` error:

```go
stream, size, err := store.OpenStream(ctx, hash)
if err != nil {
	return err
}
_, copyErr := io.Copy(dst, stream)
verifyErr := stream.Verify()
closeErr := stream.Close()
if err := errors.Join(copyErr, verifyErr, closeErr); err != nil {
	return err
}
_ = size // authoritative decoded length
```

Closing early does not drain the stream and returns incomplete verification.
Cancellation and integrity failures are terminal and sticky.

Authorized loose streams preserve `Store.Open` availability and are not
capped by `Limits.BlobBytes`; streaming them does not require an object-sized
allocation. Packed streams still enforce configured raw, stored, container,
footer, and decoder-window limits. Use `ReadBounded` or application policy when
a loose read needs an explicit work ceiling.

When output will become authoritative, copy into a caller-owned private
staging file with `Store.CopyVerified`. That method verifies the source but
does not sync, close, publish, or update the catalog. The caller must perform
those steps in order:

```text
CopyVerified -> flush/sync -> close -> no-clobber publish -> sync directory
             -> commit application authority
```

Never stream directly over an existing authoritative destination. A late hash,
CRC, decode, cancellation, or disk error may arrive after a prefix was written.

### Direct pack migration

Direct `pack` callers can keep `Writer.Append`, `AppendEncoded`, and
`Reader.ReadBlob` when whole-object memory and verification-before-return are
required. Opt into bounded-memory writing with `Writer.AppendStream`, or
prepare entries concurrently with `PrepareBlob` and append the resulting
`PreparedBlob` values in deterministic order with `AppendPrepared`.

Use `Reader.OpenBlob` for streaming plain reads. Legacy single-segment frames
can still require a decoder window as large as the blob. The supplied `Entry` must
come from that reader's immutable `Entries` result; caller-constructed offsets
are rejected. `BlobReader` has the same terminal verification and early-close
contract as `Store.OpenStream`. Use `ReaderOptions` and `BlobReaderOptions` to
bound container, footer, entry, raw, stored, and decoder-window work. Use
`AppendStreamOptions` to bound write scratch instead of relying only on format
maxima.

## Maintenance

`Maintainer.Pack`, `Repack`, and `Unpack` acquire maintenance leases
internally. All application paths that change membership or physical content
must use mutation leases from the same coordinator.

`PackOptions.MaxBytes` is checked after each appended object. `Pack` can
therefore overshoot by one object's raw size, then reports `BudgetExhausted`.

`RepackOptions.MaxBytes` is checked only between selected source packs.
`Repack` can overshoot by the live raw bytes of an entire source pack. It sets
`BudgetExhausted` only when another source remains and reaches the next budget
check; crossing the budget while rewriting the final source leaves the flag
false. Treat `BytesRepacked` as the amount actually committed and the flag as
an indication that another source was skipped, not as proof that the numeric
budget was or was not crossed.

Use finite budgets for scheduled work and inspect all returned statistics
instead of assuming every eligible object was processed.

Maintenance preserves authority ordering:

- packing publishes and verifies a sealed pack before catalog adoption, then
  removes exact unchanged loose sources;
- repacking commits replacement mappings before retiring source packs; and
- unpacking durably publishes every live loose object before atomically
  clearing packed metadata.

`Store.RetirePack` does not change catalog authority. On Windows or when an
external reader holds the file, physical removal can return
`ErrPackRetirementDeferred`. Treat it as retryable after readers release their
handles; do not restore old catalog mappings.

## Limits and capacity planning

Defaults remain deliberately conservative:

| Limit | Default | Controls |
| --- | ---: | --- |
| `BlobBytes` | 64 MiB | raw and stored bytes admitted by `ReadBounded`, packed `OpenStream`, and maintenance |
| `PackBytes` | 128 MiB | one pack container |
| `FooterBytes` | 8 MiB | footer parsing and allocation |
| `PackEntries` | 100,000 | entries parsed from one pack |

Streaming capability does not change these defaults. Raising `BlobBytes`
without a compatible `PackBytes` still leaves large objects unpackable. Keep
the four limits coherent with target pack size, expected object distribution,
and restore inputs. Larger authorized loose objects remain readable and stay
loose until packing policy admits them. The buffered compatibility path
`Store.Open` does not enforce these limits; callers requiring an allocation
ceiling must choose `ReadBounded` or packed `OpenStream` instead.

Plain entry preparation can temporarily hold the raw scratch file plus a
worst-case zstd candidate: approximately `2.004 * raw size` plus fixed frame
overhead. Scratch use and codec buffers multiply with concurrent preparation.
Place scratch on storage with an explicit capacity policy and include cleanup
of exact-owned staging files in recovery.

`ReaderSlots` bounds idle cached descriptors, not all live descriptors. Each
active stream holds a lease even after eviction, so budget file descriptors as
`ReaderSlots + maximum concurrent streams`, plus unrelated application use.
Packed `OpenStream` and `ReadBounded` reads also enforce a decoder-window limit
derived from the blob policy. A legacy format-v1 frame whose declared window
exceeds that limit fails with a typed limit error. Direct `pack` readers with
zero `ReaderLimits.WindowBytes` allow windows up to 4 GiB on 64-bit systems,
so a legacy single-segment frame can require memory proportional to its full
raw size. The decoder window ceiling remains 512 MiB on 32-bit systems.
The buffered compatibility path `Store.Open` uses that default as well.

Format v1 represents raw lengths up to 4 GiB. Its encrypted frames use
whole-entry authenticated encryption and cannot safely expose streamed
prefixes; streaming APIs return `pack.ErrStreamUnsupported`. Continue using
buffered APIs within explicit limits for encrypted repositories.

## Restore import

`PrepareImport` validates, copies, durably publishes, reopens, and verifies the
compatible subset of source packs without granting read authority. The caller
must then:

1. inspect `PreparedImport.Stats`;
2. materialize every fallback through an authenticated, hash-verifying loose
   path;
3. finish all other staged database mutations;
4. call `PreparedImport.Commit` against that staged database's
   `RestoreCatalog` transaction;
5. close the staged database and durably flush its final state, including the
   committed packed authority; and
6. publish the restored application.

A fallback is a compatibility decision, never an integrity bypass. Retry uses
the same pack IDs and requires `ReplaceRestoredPacks` to be idempotent.

## Adoption checklist

Before enabling packed storage or raising object limits:

- pin the intended Kit release and retain a rollback-compatible application
  schema;
- test the real catalog adapter's transactions and exact-set replacement;
- run end-to-end loose write, pack, mixed read, repack, unpack, backup, verify,
  and loose/packed restore flows;
- test cancellation, source mutation, corruption, full disks, catalog commit
  failures, and process restart at authority boundaries;
- run race tests and native Windows retirement/retry tests when Windows is a
  supported target;
- verify representative existing archives before changing write policy;
- measure heap/RSS, scratch use, descriptors, throughput, and copy
  amplification at expected concurrency; and
- raise `BlobBytes` and related limits only from those measurements.

An initial upgrade should keep the 64 MiB packed-maintenance default.
Capability, loose-object policy, and packing policy can then be rolled out
independently.
