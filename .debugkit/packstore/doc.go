// Package packstore provides a mixed loose-and-packed content-addressed store.
//
// Packstore owns physical storage, validation, and crash-ordered maintenance.
// Applications retain catalog membership and product reachability behind the
// Resolver and Catalog interfaces. A file or pack entry is never sufficient by
// itself to grant read authority.
//
// Loose content keeps SHA-256 over decoded logical bytes as its only identity.
// The original <shard>/<hash> raw layout remains readable and writable without
// migration. Eligible new writes may instead publish <shard>/<hash>.zst, whose
// versioned header records logical size before one zstd frame. Physical names,
// encoded lengths, and compression settings never change the logical hash or
// size reported to callers.
//
// Readers prefer the compressed name when both canonical representations exist
// and fail closed if that preferred copy is corrupt; they do not silently
// substitute the raw copy. Verified repair and maintenance reconcile valid
// duplicate representations after catalog authority is safe to advance, while
// retaining corrupt evidence for diagnosis. New compressed writes use a fixed
// 1 MiB encoder window so streaming allocations do not scale with object size.
// Readers continue accepting older compressed loose frames within the 64 MiB
// streaming frame-window ceiling.
//
// Store may read one legacy filesystem layout or an application-ordered set of
// store-scoped physical candidates. Applications retain membership and
// candidate authority through LocationResolver; BackendRegistry supplies only
// byte mechanics. Failed candidates retain typed evidence, and process-local
// Health observations demote a damaged generation without changing durable
// authority. A stream that fails after yielding bytes is never spliced to a
// second candidate; a later read prefers another less-damaged authorized
// generation when one exists.
//
// FilesystemBackend applies the same verified loose and pack mechanics to a
// store-scoped namespace. Canonical Ownership markers fence publication and
// retirement by vault, store, and epoch; ordinary reads use application-cached
// availability observations rather than performing a marker round trip.
// Move copies one candidate, reads the destination back, and returns physical
// evidence without granting catalog authority or retiring the source.
//
// Store.OpenStream exposes loose and plain packed content through one
// verification-on-EOF contract. A prefix is not authoritative: callers must
// observe terminal io.EOF, call Verify successfully, or check Verified before
// trusting the complete object. Store.CopyVerified consumes that contract into
// caller-owned private staging but deliberately does not sync, close, publish,
// or grant catalog authority. Packed streams lease cached descriptors; eviction
// and Store.Close retire them without racing active reads, and the last stream
// close releases the physical handle.
//
// Store.Open remains a seekable compatibility API. Opening compressed loose
// content through it decodes and verifies the entire object into a private
// temporary file before exposure, so streaming callers should use OpenStream.
// Applications should retain WriteResult receipts or cataloged physical paths
// for indexed statistics and scheduling. Scanning loose directories to infer
// representation or size is maintenance work, not a hot-path lookup strategy.
//
// Applications must share one Coordinator between the Maintainer and every
// application mutation that changes content membership or physical state.
// Coordinator is process-local; cross-process exclusion remains an application
// responsibility. Maintenance publishes and verifies physical data before a
// Catalog transaction grants authority, and removes old physical data only
// after authority has advanced.
//
// PrepareImport supports crash-ordered reuse of compatible immutable packs
// during restore. It copies and verifies source packs within configured Limits,
// publishes them without replacing an existing same-ID file, and returns a
// PreparedImport that still grants no authority. Applications must durably
// materialize every reported fallback before committing the prepared records
// and selected mappings through one RestoreCatalog transaction. Whole-pack
// totals describe the immutable footer; only selected, application-live hashes
// receive authority. The transaction is intentionally application-owned so its
// liveness rules remain separate from physical pack validity.
//
// Import is optional. Applications that do not supply packed restore policy can
// materialize the same content loose, and applications may mix representations
// when a pack or entry exceeds target limits or the filesystem cannot provide
// atomic no-clobber publication. A compatibility fallback is never an integrity
// fallback: declined content still requires an authenticated, hash-verified
// loose read.
//
// Physical storage operations are supported on Unix and Windows. Other Go
// targets compile but fail closed because their file APIs do not provide the
// atomic no-follow and nonblocking opens required for race-safe content access.
// On Windows, regular files are flushed and handles are closed before immutable
// packs are published or reopened; directory sync is a no-op consistent with
// Kit's wider durability policy. Publication uses hard links on Unix and
// Windows and falls back loose when a new destination cannot be created this
// way, rather than introducing a replacement-rename race.
//
// DefaultLimits retains a 64 MiB policy for ReadBounded, packed OpenStream,
// and maintenance. Store.Open is the buffered compatibility path and does not
// enforce those limits; larger authorized loose objects also remain available
// through OpenStream. Streaming removes the former object-sized heap
// requirement but does not raise packing policy automatically. Callers must
// budget scratch (about twice the largest concurrently prepared plain object),
// decoder windows, active streams, and descriptors before increasing limits.
// RetirePack can return ErrPackRetirementDeferred when a physical file remains
// in use; this is retryable cleanup and never permission to restore retired
// catalog authority.
package packstore
