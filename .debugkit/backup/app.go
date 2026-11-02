package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"

	"go.kenn.io/kit/packstore"
)

// MetadataSource opens one application-owned logical metadata snapshot while
// Create holds its FreezeCoordinator. OpenSnapshot must establish a stable
// view before returning; the coordinator is released immediately afterward.
type MetadataSource interface {
	Format() string
	OpenSnapshot(context.Context) (MetadataSnapshot, error)
}

// MetadataSnapshot supplies portable metadata bytes, content membership, and
// fidelity stats from one stable application view. It may additionally
// implement AuxiliarySource to contribute artifacts from that same view.
type MetadataSnapshot interface {
	OpenMetadata(context.Context) (io.ReadCloser, int64, error)
	ContentInfo(context.Context) (*ContentInfo, error)
	Stats(context.Context) (json.RawMessage, error)
	Close() error
}

// MetadataRestorer builds the application's current runtime database at
// targetPath from one verified portable metadata stream. targetPath is inside
// Kit-owned private scratch, not the eventual restore target. The restorer must
// consume the stream through EOF, close and checkpoint the database, and leave
// no sidecars. Kit copies the closed result into its confined target root.
type MetadataRestorer interface {
	RestoreMetadata(ctx context.Context, format string, metadata io.Reader, targetPath string) error
}

// RestoreTargetCoordinator binds application-owned coordination to the exact
// directory descriptor Restore uses for every target mutation. Restore calls
// AcquireRestoreTarget after securely opening the target root and before stale
// staging cleanup or content publication. The root is borrowed: implementations
// must not close it or retain it after the lease is released.
type RestoreTargetCoordinator interface {
	AcquireRestoreTarget(context.Context, *os.Root) (RestoreTargetLease, error)
}

// RestoreTargetLease holds application-owned target coordination for one
// complete restore. Release runs before Kit closes the borrowed target root.
type RestoreTargetLease interface {
	Release() error
}

// PackedContentTarget supplies the application-owned packed-storage policy
// for an optional mixed packed-and-loose restore. It opens catalog authority
// only against Restore's unpublished staged SQLite database; Kit never opens
// an application's live catalog through this interface. Implementations must
// keep catalog replacement structurally valid and neutral to RestoredStats so
// Restore can prove the final staged database before publishing it.
type PackedContentTarget interface {
	// Limits returns the target store's configured compatibility and
	// allocation ceilings.
	Limits() packstore.Limits
	// AcquireRestoreLease acquires a mutation lease from the same Coordinator
	// used by every maintainer of the target content store. The target transfers
	// sole ownership of a successful lease to Restore. Applications must acquire
	// their own operation gates before this method is called.
	AcquireRestoreLease(context.Context) (*packstore.Lease, error)
	// OpenRestoreCatalog returns the packed-authority adapter for db. db is
	// Restore's unpublished staged database, not the currently visible target.
	// It and the returned catalog's ReplaceRestoredPacks method run while the
	// restore lease is held and must not reenter its Coordinator.
	OpenRestoreCatalog(context.Context, *sql.DB) (packstore.RestoreCatalog, error)
}

// ContentInfo is what the engine needs to know about the application's
// content-addressed files, computed inside the frozen snapshot.
type ContentInfo struct {
	Refs []ContentRef // one per unique hash, first-seen order
	Rows int64        // DB rows referencing content (manifest attachments.rows)
	// NonCanonicalPaths reports any ref recorded at a path other than the
	// canonical "<hash[:2]>/<hash>" layout; such snapshots require a
	// path-aware restore and a higher manifest reader version.
	NonCanonicalPaths bool
}

// FrozenView answers the application-schema questions Create asks against the
// pinned read transaction of a FrozenSession. It may additionally implement
// AuxiliarySource to contribute artifacts from that same view.
type FrozenView interface {
	ContentInfo(ctx context.Context) (*ContentInfo, error)
	Stats(ctx context.Context) (json.RawMessage, error)
}

// App supplies every application-specific behavior the engine needs. The
// engine treats stats payloads as opaque bytes: it records them at create
// and byte-compares them at restore.
type App interface {
	FrozenView(s *FrozenSession) FrozenView
	DBFileName() string     // e.g. "app.db"
	ContentDirName() string // e.g. "content"
	// PackFileExtension returns the file extension for pack files, including
	// the leading dot (e.g. ".kpack"). Like the other layout names it must
	// remain fixed for the life of a repository: packs are located by
	// <packID><ext>, so changing it strands previously written packs.
	PackFileExtension() string
	// RestoredContentPaths re-derives hash → relative paths from a restored
	// DB so restore can materialize and verify every referenced file. Returned
	// paths must be relative and local to the content directory (no absolute
	// paths, no ".." escapes); the engine also rejects any non-local path at
	// restore time.
	RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error)
	// RestoredStats recomputes stats from a restored DB for the fidelity proof.
	RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error)
	// CheckManifest returns app-level manifest consistency problems (verify).
	CheckManifest(m *Manifest) []string
	ExcludedPaths() []string
	Version() string // recorded as the manifest's app version
}
