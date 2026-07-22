// Package docbank provides an in-process Docbank vault for Go applications.
// Standalone CLI commands remain daemon clients; embedded applications own the
// same exclusive vault lock, metadata schema, and mixed loose/packed storage
// directly through this lifecycle.
package docbank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

var (
	// ErrClosed means an operation targeted a closed embedded vault.
	ErrClosed = errors.New("docbank vault is closed")
	// ErrContentUnavailable means catalog-authorized content could not be
	// opened or its physical size disagreed with the metadata authority.
	ErrContentUnavailable = errors.New("docbank content is unavailable")
	// ErrDigestMismatch means the durable bytes did not match the caller's
	// optional expected SHA-256 identity.
	ErrDigestMismatch = errors.New("docbank content digest mismatch")
	// ErrSizeMismatch means the durable bytes did not match the caller's
	// optional expected byte count.
	ErrSizeMismatch = errors.New("docbank content size mismatch")
	// ErrContentConflict means immutable creation targeted an existing path
	// with different bytes, size, media type, provenance, or node kind.
	ErrContentConflict = errors.New("docbank immutable content conflict")

	ErrNotFound                 = store.ErrNotFound
	ErrExists                   = store.ErrExists
	ErrNotDirectory             = store.ErrNotDir
	ErrNotFile                  = store.ErrNotFile
	ErrStaleRevision            = store.ErrStaleRevision
	ErrCycle                    = store.ErrCycle
	ErrInvalidName              = store.ErrInvalidName
	ErrInvalidBatchMove         = store.ErrInvalidBatchMove
	ErrNotTrashed               = store.ErrNotTrashed
	ErrIsRoot                   = store.ErrIsRoot
	ErrAuditMutationUnsupported = store.ErrAuditMutationUnsupported
)

const (
	// DefaultChildrenLimit is the page size used when ChildrenOptions.Limit is zero.
	DefaultChildrenLimit = 500
	// MaxChildrenLimit is the largest child page one embedded call may materialize.
	MaxChildrenLimit = 5000
	// DefaultTrashEmptyMaxRoots bounds one EmptyTrash call when MaxRoots is zero.
	DefaultTrashEmptyMaxRoots = 100
	looseEncodingRawName      = "raw"
	looseEncodingZstdName     = "zstd"
)

// LooseCompressionOptions controls whether eligible new loose content may use
// zstd physical storage. The zero value preserves the legacy raw layout.
type LooseCompressionOptions struct {
	Enabled           bool
	MinBytes          int64
	MinSavingsPercent int
}

// Config selects one private vault root, its optional SQLite implementation,
// and physical loose-storage policy. Nil SQLite uses mattn/go-sqlite3 in CGO
// builds and modernc.org/sqlite when CGO is disabled.
type Config struct {
	Root             string
	SQLite           docsqlite.Driver
	LooseCompression LooseCompressionOptions
}

// Vault is one independently locked Docbank namespace. Separate Vault values
// may be open concurrently when their roots do not overlap.
type Vault struct {
	root     *os.Root
	lock     *home.Lock
	metadata *store.Store
	blobs    *blob.Store

	lifecycle sync.RWMutex
	mutation  sync.Mutex
	closed    bool

	// testAfterRepairPublication exercises the non-cancelable authority handoff
	// after durable physical publication. Production constructors leave it nil.
	testAfterRepairPublication func()
	// testAfterWriteCommit exercises receipt completion after a Put or Create
	// metadata mutation commits. Production constructors leave it nil.
	testAfterWriteCommit func()
}

// New creates or opens one embedded vault and holds its exclusive hierarchy
// lock until Close. A standalone daemon or another embedded instance cannot
// own the same or an overlapping vault concurrently.
func New(ctx context.Context, config Config) (_ *Vault, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	driver := config.SQLite
	if driver == nil {
		driver = store.DefaultSQLiteDriver()
	}
	if err := docsqlite.Validate(driver); err != nil {
		return nil, err
	}
	if config.Root == "" {
		return nil, errors.New("docbank vault root is required")
	}
	blobOptions := blob.Options{LooseCompression: blob.LooseCompressionOptions{
		Enabled:           config.LooseCompression.Enabled,
		MinBytes:          config.LooseCompression.MinBytes,
		MinSavingsPercent: config.LooseCompression.MinSavingsPercent,
	}}
	if err := blob.ValidateOptions(blobOptions); err != nil {
		return nil, fmt.Errorf("docbank %w", err)
	}
	canonical, err := home.CanonicalRoot(config.Root)
	if err != nil {
		return nil, err
	}
	layout := home.Layout{Root: canonical}
	root, lock, err := layout.OpenAndLockExclusive()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, lock.Release(), root.Close())
		}
	}()
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	metadata, err := store.Open(layout.DBPath(), driver)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, metadata.Close())
		}
	}()
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(metadata), layout.BlobsDir(), blobOptions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, blobs.Close())
		}
	}()
	if err := blobs.CleanTmp(); err != nil {
		return nil, err
	}
	return &Vault{root: root, lock: lock, metadata: metadata, blobs: blobs}, nil
}

// Close waits for active operations and readers, then releases storage and
// the vault hierarchy lock. It is safe to call more than once.
func (v *Vault) Close() error {
	if v == nil {
		return nil
	}
	v.lifecycle.Lock()
	defer v.lifecycle.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	return errors.Join(v.blobs.Close(), v.metadata.Close(), v.lock.Release(), v.root.Close())
}

// SQLiteDriver reports the adapter selected for this vault.
func (v *Vault) SQLiteDriver() string {
	if v == nil || v.metadata == nil {
		return ""
	}
	return v.metadata.SQLiteDriver().Name()
}

// ID reports the stable logical vault identity preserved by metadata export,
// backup, and restore. It is independent of the vault's filesystem root.
func (v *Vault) ID() string {
	if v == nil || v.metadata == nil {
		return ""
	}
	return v.metadata.VaultID()
}

// Stat resolves a live virtual path to its stable node projection.
func (v *Vault) Stat(ctx context.Context, virtualPath string) (Node, error) {
	if err := v.begin(); err != nil {
		return Node{}, err
	}
	defer v.lifecycle.RUnlock()
	node, err := v.metadata.NodeByPath(ctx, virtualPath)
	if err != nil {
		return Node{}, err
	}
	return fromStoreNode(node), nil
}

// Children lists one bounded page of a directory's live children, directories
// first and then files, name-sorted within each kind.
func (v *Vault) Children(
	ctx context.Context, directoryID int64, opts ChildrenOptions,
) (ChildrenPage, error) {
	if err := v.begin(); err != nil {
		return ChildrenPage{}, err
	}
	defer v.lifecycle.RUnlock()
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultChildrenLimit
	}
	children, total, err := v.metadata.ChildrenPage(ctx, directoryID, limit, opts.Offset)
	if err != nil {
		return ChildrenPage{}, err
	}
	page := ChildrenPage{
		Items: make([]Node, 0, len(children)), Total: total, Limit: limit, Offset: opts.Offset,
	}
	for _, child := range children {
		page.Items = append(page.Items, fromStoreNode(child))
	}
	return page, nil
}

func (v *Vault) Versions(
	ctx context.Context, nodeID int64, opts VersionsOptions,
) (VersionsPage, error) {
	if err := v.begin(); err != nil {
		return VersionsPage{}, err
	}
	defer v.lifecycle.RUnlock()
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultVersionsLimit
	}
	versions, total, err := v.metadata.ContentVersions(ctx, nodeID, limit, opts.Offset)
	if err != nil {
		return VersionsPage{}, err
	}
	page := VersionsPage{Items: make([]ContentVersion, 0, len(versions)), Total: total, Limit: limit, Offset: opts.Offset}
	for _, version := range versions {
		page.Items = append(page.Items, fromStoreVersion(version))
	}
	return page, nil
}

// PutOptions controls one embedded content write. Expected is optional; when
// present, no node or version authority is granted unless both fields match
// the independently computed durable bytes.
type PutOptions struct {
	MediaType string
	Expected  *ContentIdentity
}

// CreateOptions controls one immutable content creation. Expected is required;
// an existing path is idempotent only when bytes, size, and media type match.
type CreateOptions struct {
	MediaType string
	Expected  ContentIdentity
	// Provenance optionally records where this immutable document came from.
	// The source fact commits atomically with a newly created node and version.
	Provenance *ProvenanceSource
}

// Put stores a reader at an absolute virtual file path. Missing parent
// directories are created. An unchanged retry converges on the current
// version; changed bytes create a new immutable version on the same node.
func (v *Vault) Put(
	ctx context.Context, virtualPath string, content io.Reader, opts PutOptions,
) (PutReceipt, error) {
	return v.write(ctx, virtualPath, content, opts, false, nil)
}

// Create stores content only when virtualPath is absent. An identical retry,
// including any supplied provenance, returns the existing node and version;
// any different existing authority returns ErrContentConflict without
// appending history.
func (v *Vault) Create(
	ctx context.Context, virtualPath string, content io.Reader, opts CreateOptions,
) (PutReceipt, error) {
	expected := opts.Expected
	return v.write(ctx, virtualPath, content, PutOptions{
		MediaType: opts.MediaType, Expected: &expected,
	}, true, opts.Provenance)
}

// Provenance returns one bounded page of immutable origin facts for a file.
// The node, live path, count, and page come from one metadata snapshot. Path is
// empty when nodeID is in trash.
func (v *Vault) Provenance(
	ctx context.Context, nodeID int64, opts ProvenanceOptions,
) (ProvenancePage, error) {
	if err := v.begin(); err != nil {
		return ProvenancePage{}, err
	}
	defer v.lifecycle.RUnlock()
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultProvenanceLimit
	}
	page, err := v.metadata.NodeProvenance(ctx, nodeID, limit, opts.Offset)
	if err != nil {
		return ProvenancePage{}, err
	}
	result := ProvenancePage{
		Node: fromStoreNode(page.Node), Path: page.Path,
		Items: make([]ProvenanceFact, 0, len(page.Items)),
		Total: page.Total, Limit: page.Limit, Offset: page.Offset,
	}
	for _, fact := range page.Items {
		result.Items = append(result.Items, fromStoreProvenance(fact))
	}
	return result, nil
}

// MovePath renames or reparents one live path and returns its canonical new
// path. A positive IfRevision must match the source node exactly.
func (v *Vault) MovePath(
	ctx context.Context, from, to string, opts RevisionOptions,
) (MutationReceipt, error) {
	if err := v.begin(); err != nil {
		return MutationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	ifRevision, err := storeRevision(opts)
	if err != nil {
		return MutationReceipt{}, err
	}
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return MutationReceipt{}, err
	}
	node, canonicalPath, err := v.metadata.MovePathRevision(ctx, from, to, ifRevision)
	if err != nil {
		return MutationReceipt{}, err
	}
	return MutationReceipt{Node: fromStoreNode(node), Path: canonicalPath}, nil
}

// BatchMove validates and applies one final-state reorganization as a single
// metadata transaction. Results preserve request order. Path sources resolve
// inside the transaction; stable node sources require a positive revision.
func (v *Vault) BatchMove(
	ctx context.Context, moves []BatchMoveItem,
) ([]BatchMoveReceipt, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	defer v.lifecycle.RUnlock()
	requests := make([]store.BatchMoveRequest, len(moves))
	for index, move := range moves {
		requests[index] = store.BatchMoveRequest{
			SourcePath: move.SourcePath, NodeID: move.NodeID, IfRevision: move.IfRevision,
			DestinationPath: move.DestinationPath,
		}
	}
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results, err := v.metadata.BatchMove(ctx, requests)
	if err != nil {
		return nil, err
	}
	receipts := make([]BatchMoveReceipt, len(results))
	for index, result := range results {
		receipts[index] = BatchMoveReceipt{
			Node: fromStoreNode(result.Node), FromPath: result.FromPath, Path: result.Path,
		}
	}
	return receipts, nil
}

// TrashPath moves one live path and its subtree to trash, returning the
// canonical pre-trash path. A positive IfRevision must match the root node.
func (v *Vault) TrashPath(
	ctx context.Context, path string, opts RevisionOptions,
) (MutationReceipt, error) {
	if err := v.begin(); err != nil {
		return MutationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	ifRevision, err := storeRevision(opts)
	if err != nil {
		return MutationReceipt{}, err
	}
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return MutationReceipt{}, err
	}
	node, canonicalPath, err := v.metadata.TrashPathRevision(ctx, path, ifRevision)
	if err != nil {
		return MutationReceipt{}, err
	}
	return MutationReceipt{Node: fromStoreNode(node), Path: canonicalPath}, nil
}

// Restore returns a trash root to its recorded origin, or to the canonical
// conflict-suffixed path selected by the store. A positive IfRevision must
// match the trashed node exactly.
func (v *Vault) Restore(
	ctx context.Context, nodeID int64, opts RevisionOptions,
) (MutationReceipt, error) {
	if err := v.begin(); err != nil {
		return MutationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	ifRevision, err := storeRevision(opts)
	if err != nil {
		return MutationReceipt{}, err
	}
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return MutationReceipt{}, err
	}
	node, canonicalPath, err := v.metadata.Restore(ctx, nodeID, ifRevision)
	if err != nil {
		return MutationReceipt{}, err
	}
	return MutationReceipt{Node: fromStoreNode(node), Path: canonicalPath}, nil
}

func storeRevision(opts RevisionOptions) (int64, error) {
	if opts.IfRevision < 0 {
		return 0, errors.New("docbank revision must not be negative")
	}
	if opts.IfRevision == 0 {
		return store.UnconditionalRev, nil
	}
	return opts.IfRevision, nil
}

// EmptyTrash previews or hard-deletes one finite batch of eligible trash
// roots. Subtrees cascade with their selected root; candidate and deletion
// counts therefore describe roots, not every descendant row.
func (v *Vault) EmptyTrash(
	ctx context.Context, opts TrashEmptyOptions,
) (TrashEmptyReport, error) {
	if err := v.begin(); err != nil {
		return TrashEmptyReport{}, err
	}
	defer v.lifecycle.RUnlock()
	if opts.OlderThan < 0 {
		return TrashEmptyReport{}, errors.New("docbank trash age must not be negative")
	}
	if opts.MaxRoots < 0 {
		return TrashEmptyReport{}, errors.New("docbank maximum trash roots must not be negative")
	}
	maxRoots := opts.MaxRoots
	if maxRoots == 0 {
		maxRoots = DefaultTrashEmptyMaxRoots
	}
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return TrashEmptyReport{}, err
	}
	result, err := v.metadata.TrashEmptyBounded(ctx, opts.OlderThan, maxRoots, !opts.DryRun)
	if err != nil {
		return TrashEmptyReport{}, err
	}
	return TrashEmptyReport{
		Candidates: result.Candidates,
		Deleted:    result.Deleted,
		More:       result.More,
		DryRun:     opts.DryRun,
	}, nil
}

// RepairContent replaces the physical bytes for one existing content identity
// after fully verifying trusted against its required SHA-256 and size. All
// nodes and historical versions keep referencing the same immutable identity.
func (v *Vault) RepairContent(
	ctx context.Context, identity ContentIdentity, trusted io.Reader,
) (RepairReceipt, error) {
	if err := v.begin(); err != nil {
		return RepairReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	if trusted == nil {
		return RepairReceipt{}, errors.New("docbank trusted repair reader is required")
	}
	parsed, err := packstore.ParseHash(identity.SHA256)
	if err != nil || parsed.String() != identity.SHA256 {
		return RepairReceipt{}, errors.New("repair content hash must be canonical lowercase SHA-256")
	}
	if identity.Size < 0 {
		return RepairReceipt{}, errors.New("repair content size must not be negative")
	}

	v.mutation.Lock()
	defer v.mutation.Unlock()
	var receipt RepairReceipt
	err = v.blobs.WithMutation(ctx, func() error {
		membership, err := v.metadata.BlobInfo(ctx, identity.SHA256)
		if err != nil {
			return err
		}
		if membership.Size != identity.Size {
			return fmt.Errorf("blob %s: catalog size %d does not match repair size %d",
				identity.SHA256, membership.Size, identity.Size)
		}
		existing, err := v.metadata.PhysicalContent(ctx, identity.SHA256)
		if errors.Is(err, store.ErrPhysicalAuthorityMissing) {
			existing = store.PhysicalContent{LogicalBytes: membership.Size}
		} else if err != nil {
			return err
		}
		var written blob.WriteReceipt
		if existing.Kind == "loose" {
			var encoding packstore.LooseEncoding
			switch existing.Encoding {
			case looseEncodingRawName:
				encoding = packstore.LooseEncodingRaw
			case looseEncodingZstdName:
				encoding = packstore.LooseEncodingZstd
			default:
				return fmt.Errorf("blob %s has unknown loose encoding %q",
					identity.SHA256, existing.Encoding)
			}
			written, err = v.blobs.RepairContextWithEncoding(
				ctx, identity.SHA256, identity.Size, trusted, encoding,
			)
		} else {
			written, err = v.blobs.RepairContext(ctx, identity.SHA256, identity.Size, trusted)
		}
		if err != nil {
			return err
		}
		if v.testAfterRepairPublication != nil {
			v.testAfterRepairPublication()
		}
		physical, err := blobPhysical(written)
		if err != nil {
			return err
		}
		// Once verified bytes have replaced the canonical loose representation,
		// finish the authority handoff even if the request disconnects. Leaving
		// the catalog pointed at a retired representation would make valid bytes
		// unavailable until an operator repaired them again.
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		references, err := v.metadata.RepairBlobAuthority(
			commitCtx, identity.SHA256, identity.Size, physical,
		)
		if err != nil {
			return err
		}
		receipt = RepairReceipt{
			Computed: identity,
			Physical: PhysicalContent{
				Kind: "loose", Encoding: physical.Encoding,
				LogicalBytes: identity.Size, StoredBytes: physical.StoredBytes,
				PackEligible: physical.PackEligible,
			},
			ReferencesPreserved: references,
		}
		return nil
	})
	if err != nil {
		return RepairReceipt{}, err
	}
	return receipt, nil
}

func (v *Vault) write(
	ctx context.Context, virtualPath string, content io.Reader, opts PutOptions, immutable bool,
	provenance *ProvenanceSource,
) (PutReceipt, error) {
	if err := v.begin(); err != nil {
		return PutReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	if content == nil {
		return PutReceipt{}, errors.New("docbank content reader is required")
	}
	if !utf8.ValidString(opts.MediaType) {
		return PutReceipt{}, errors.New("docbank media type is not valid UTF-8")
	}
	if provenance != nil {
		snapshot := *provenance
		if provenance.ModifiedAt != nil {
			modifiedAt := *provenance.ModifiedAt
			snapshot.ModifiedAt = &modifiedAt
		}
		provenance = &snapshot
		if !immutable {
			return PutReceipt{}, errors.New("docbank provenance requires immutable creation")
		}
		if err := validateProvenanceSource(*provenance); err != nil {
			return PutReceipt{}, err
		}
	}
	parentPath, name, canonicalPath, err := normalizeVirtualFilePath(virtualPath)
	if err != nil {
		return PutReceipt{}, err
	}
	if opts.Expected != nil {
		parsed, parseErr := packstore.ParseHash(opts.Expected.SHA256)
		if parseErr != nil || parsed.String() != opts.Expected.SHA256 {
			return PutReceipt{}, errors.New("expected content hash must be canonical lowercase SHA-256")
		}
		if opts.Expected.Size < 0 {
			return PutReceipt{}, errors.New("expected content size must not be negative")
		}
	}

	v.mutation.Lock()
	defer v.mutation.Unlock()
	var receipt PutReceipt
	var physicalCreated bool
	err = v.blobs.WithMutation(ctx, func() (resultErr error) {
		written, writeErr := v.blobs.WriteDetailedContext(ctx, content)
		if writeErr != nil {
			return writeErr
		}
		hash, size := written.Hash, written.Size
		physical, physicalErr := blobPhysical(written)
		if physicalErr != nil {
			return physicalErr
		}
		physicalCreated = physical.Created
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, v.removeUnrecordedLoose(hash))
				return
			}
			if receipt.Physical.Kind == "" {
				authority, authorityErr := v.metadata.PhysicalContent(ctx, hash)
				if authorityErr != nil {
					resultErr = authorityErr
					return
				}
				receipt.Physical = fromStorePhysical(authority)
			}
			if receipt.Physical.Kind == "packed" {
				// Metadata authority has committed. Redundant loose cleanup is
				// maintenance and must not turn durable success into failure.
				_ = v.blobs.Remove(hash)
			}
		}()
		receipt.Computed = ContentIdentity{SHA256: hash, Size: size}
		if opts.Expected != nil && opts.Expected.Size != size {
			return fmt.Errorf("expected %d bytes, computed %d: %w",
				opts.Expected.Size, size, ErrSizeMismatch)
		}
		if opts.Expected != nil && opts.Expected.SHA256 != hash {
			return fmt.Errorf("expected SHA-256 %s, computed %s: %w",
				opts.Expected.SHA256, hash, ErrDigestMismatch)
		}
		parent, mkdirErr := v.metadata.MkdirAll(ctx, parentPath)
		if mkdirErr != nil {
			return mkdirErr
		}

		existing, lookupErr := v.metadata.NodeByPath(ctx, canonicalPath)
		switch {
		case errors.Is(lookupErr, store.ErrNotFound):
			var created store.ContentWriteReceipt
			var createErr error
			if provenance == nil {
				created, createErr = v.metadata.CreateFileWithReceipt(
					ctx, parent.ID, name, hash, size, opts.MediaType, physical,
				)
			} else {
				run, beginErr := v.metadata.BeginEmbeddedIngest(
					ctx, provenance.Kind, provenance.Description,
				)
				if beginErr != nil {
					return beginErr
				}
				created, createErr = v.metadata.IngestFileExactWithReceipt(
					ctx, run, parent.ID, name, hash, size, opts.MediaType,
					provenance.Reference, provenanceModifiedAt(provenance), physical,
				)
			}
			if createErr != nil {
				return createErr
			}
			receipt.Node = fromStoreNode(created.Node)
			receipt.Version = fromStoreVersion(created.Version)
			receipt.Physical = fromStorePhysical(created.Physical)
			receipt.Created = true
			if v.testAfterWriteCommit != nil {
				v.testAfterWriteCommit()
			}
			return nil
		case lookupErr != nil:
			return lookupErr
		case existing.IsDir():
			if immutable {
				return fmt.Errorf("virtual path %q names a directory: %w", canonicalPath, ErrContentConflict)
			}
			return fmt.Errorf("virtual path %q: %w", canonicalPath, store.ErrNotFile)
		case existing.BlobHash == hash && existing.Size == size && existing.MimeType == opts.MediaType:
			var confirmed store.ContentWriteReceipt
			var confirmErr error
			if provenance == nil {
				confirmed, confirmErr = v.metadata.ConfirmContentWithReceipt(
					ctx, existing.ID, existing.Revision, hash, size, opts.MediaType, physical,
				)
			} else {
				confirmed, confirmErr = v.metadata.ConfirmIngestedContentWithReceipt(
					ctx, existing.ID, existing.Revision, hash, size, opts.MediaType,
					provenance.Kind, provenance.Description, provenance.Reference,
					provenanceModifiedAt(provenance), physical,
				)
			}
			if confirmErr != nil {
				if errors.Is(confirmErr, store.ErrProvenanceMismatch) {
					return fmt.Errorf("virtual path %q has different provenance: %w",
						canonicalPath, ErrContentConflict)
				}
				return confirmErr
			}
			receipt.Node = fromStoreNode(confirmed.Node)
			receipt.Version = fromStoreVersion(confirmed.Version)
			receipt.Physical = fromStorePhysical(confirmed.Physical)
			return nil
		default:
			if immutable {
				return fmt.Errorf("virtual path %q already has different content or media type: %w",
					canonicalPath, ErrContentConflict)
			}
			updated, replaceErr := v.metadata.ReplaceContentWithReceipt(
				ctx, existing.ID, existing.Revision, hash, size, opts.MediaType, physical,
			)
			if replaceErr != nil {
				return replaceErr
			}
			receipt.Node = fromStoreNode(updated.Node)
			receipt.Version = fromStoreVersion(updated.Version)
			receipt.Physical = fromStorePhysical(updated.Physical)
			receipt.Replaced = true
			if v.testAfterWriteCommit != nil {
				v.testAfterWriteCommit()
			}
			return nil
		}
	})
	if err != nil {
		return receipt, err
	}
	receipt.PhysicalCreated = physicalCreated && receipt.Physical.Kind == "loose"
	return receipt, nil
}

func validateProvenanceSource(source ProvenanceSource) error {
	fields := []struct {
		name  string
		value string
	}{
		{name: "kind", value: source.Kind},
		{name: "description", value: source.Description},
		{name: "reference", value: source.Reference},
	}
	for _, field := range fields {
		if field.value == "" {
			return fmt.Errorf("docbank provenance source %s is required", field.name)
		}
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("docbank provenance source %s is not valid UTF-8", field.name)
		}
	}
	if source.ModifiedAt != nil {
		encoded := source.ModifiedAt.UTC().Format(time.RFC3339Nano)
		parsed, err := time.Parse(time.RFC3339Nano, encoded)
		if err != nil || parsed.UTC().Format(time.RFC3339Nano) != encoded {
			return errors.New("docbank provenance source modified time is outside RFC3339Nano")
		}
	}
	return nil
}

func provenanceModifiedAt(source *ProvenanceSource) string {
	if source == nil || source.ModifiedAt == nil {
		return ""
	}
	return source.ModifiedAt.UTC().Format(time.RFC3339Nano)
}

func blobPhysical(receipt blob.WriteReceipt) (store.BlobPhysical, error) {
	encoding, err := receipt.EncodingName()
	if err != nil {
		return store.BlobPhysical{}, err
	}
	return store.BlobPhysical{
		Encoding: encoding, StoredBytes: receipt.StoredSize,
		PackEligible: receipt.PackEligible, Created: receipt.Created,
	}, nil
}

// LooseBacklog reports indexed loose content eligible for explicit packing.
func (v *Vault) LooseBacklog(ctx context.Context) (LooseBacklog, error) {
	if err := v.begin(); err != nil {
		return LooseBacklog{}, err
	}
	defer v.lifecycle.RUnlock()
	backlog, err := v.metadata.LooseBacklog(ctx)
	if err != nil {
		return LooseBacklog{}, err
	}
	return fromStoreLooseBacklog(backlog), nil
}

func (v *Vault) removeUnrecordedLoose(hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorded, err := v.metadata.HasBlob(ctx, hash)
	if err != nil {
		return fmt.Errorf("checking failed put cleanup for %s: %w", hash, err)
	}
	if recorded {
		physical, err := v.metadata.PhysicalContent(ctx, hash)
		if err != nil {
			return fmt.Errorf("checking failed put authority for %s: %w", hash, err)
		}
		if physical.Kind != "packed" {
			return nil
		}
	}
	if err := v.blobs.Remove(hash); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cleaning failed put blob %s: %w", hash, err)
	}
	return nil
}

// OpenContent opens the current catalog-authorized bytes for a live file. The
// reader holds a vault lease until Close; bytes are authoritative only after
// terminal io.EOF or a successful Verify.
func (v *Vault) OpenContent(ctx context.Context, virtualPath string) (*Content, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	node, err := v.metadata.NodeByPath(ctx, virtualPath)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	if node.IsDir() {
		v.lifecycle.RUnlock()
		return nil, fmt.Errorf("virtual path %q: %w", virtualPath, store.ErrNotFile)
	}
	reader, size, err := v.blobs.OpenStreamContext(ctx, node.BlobHash)
	if err != nil {
		closeErr := closeContentReader(reader)
		v.lifecycle.RUnlock()
		return nil, errors.Join(fmt.Errorf(
			"opening content for virtual path %q: %w: %w",
			virtualPath, ErrContentUnavailable, err,
		), closeErr)
	}
	if size != node.Size {
		closeErr := reader.Close()
		v.lifecycle.RUnlock()
		return nil, errors.Join(fmt.Errorf(
			"catalog size %d does not match node size %d: %w",
			size, node.Size, ErrContentUnavailable,
		), closeErr)
	}
	return &Content{
		Node:   fromStoreNode(node),
		Reader: &leasedReader{VerifiedReadCloser: reader, release: v.lifecycle.RUnlock},
	}, nil
}

// OpenVersionContent opens the catalog-authorized bytes for one immutable
// content version. The reader holds a vault lease and uses the same verified
// read contract as OpenContent.
func (v *Vault) OpenVersionContent(ctx context.Context, versionID string) (*VersionContent, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	version, err := v.metadata.ContentVersionByID(ctx, versionID)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	reader, size, err := v.blobs.OpenStreamContext(ctx, version.BlobHash)
	if err != nil {
		closeErr := closeContentReader(reader)
		v.lifecycle.RUnlock()
		return nil, errors.Join(fmt.Errorf(
			"opening content version %q: %w: %w",
			versionID, ErrContentUnavailable, err,
		), closeErr)
	}
	if size != version.Size {
		closeErr := reader.Close()
		v.lifecycle.RUnlock()
		return nil, errors.Join(fmt.Errorf(
			"catalog size %d does not match version size %d: %w",
			size, version.Size, ErrContentUnavailable,
		), closeErr)
	}
	return &VersionContent{
		Version: fromStoreVersion(version),
		Reader:  &leasedReader{VerifiedReadCloser: reader, release: v.lifecycle.RUnlock},
	}, nil
}

func closeContentReader(reader packstore.VerifiedReadCloser) error {
	if reader == nil {
		return nil
	}
	return reader.Close()
}

// Pack explicitly moves authorized loose content into managed immutable packs.
// It also performs the same reconciliation and repair pass as the standalone
// storage pack operation. Ordinary Put calls remain loose until Pack is called.
func (v *Vault) Pack(ctx context.Context, opts PackOptions) (PackReport, error) {
	if err := v.begin(); err != nil {
		return PackReport{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	report, err := internalmaintenance.Pack(ctx, v.metadata, v.blobs, opts.MaxBytes)
	out := fromPackStats(report.Stats)
	out.More = report.More
	return out, err
}

func (v *Vault) begin() error {
	if v == nil {
		return ErrClosed
	}
	v.lifecycle.RLock()
	if v.closed {
		v.lifecycle.RUnlock()
		return ErrClosed
	}
	return nil
}

func normalizeVirtualFilePath(value string) (parent, name, canonical string, err error) {
	if !strings.HasPrefix(value, "/") || value == "/" || strings.HasSuffix(value, "/") {
		return "", "", "", errors.New("docbank file path must be absolute and name a file")
	}
	parts := strings.Split(value[1:], "/")
	for i, part := range parts {
		if part == "" {
			return "", "", "", errors.New("docbank file path contains an empty segment")
		}
		parts[i], err = store.NormalizeName(part)
		if err != nil {
			return "", "", "", fmt.Errorf("docbank file path %q: %w", value, err)
		}
	}
	name = parts[len(parts)-1]
	canonical = "/" + strings.Join(parts, "/")
	parent = "/"
	if len(parts) > 1 {
		parent = "/" + strings.Join(parts[:len(parts)-1], "/")
	}
	return parent, name, canonical, nil
}

type leasedReader struct {
	VerifiedReadCloser

	once     sync.Once
	release  func()
	closeErr error
}

func (r *leasedReader) Close() error {
	r.once.Do(func() {
		r.closeErr = r.VerifiedReadCloser.Close()
		r.release()
	})
	return r.closeErr
}
