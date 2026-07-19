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

	ErrNotFound      = store.ErrNotFound
	ErrExists        = store.ErrExists
	ErrNotDirectory  = store.ErrNotDir
	ErrNotFile       = store.ErrNotFile
	ErrStaleRevision = store.ErrStaleRevision
)

const (
	// DefaultChildrenLimit is the page size used when ChildrenOptions.Limit is zero.
	DefaultChildrenLimit = 500
	// MaxChildrenLimit is the largest child page one embedded call may materialize.
	MaxChildrenLimit = 5000
)

// Config selects one private vault root and, optionally, its SQLite
// implementation. Nil SQLite uses mattn/go-sqlite3 in CGO builds and
// modernc.org/sqlite when CGO is disabled.
type Config struct {
	Root   string
	SQLite docsqlite.Driver
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
	blobs, err := blob.New(store.NewPackCatalog(metadata), layout.BlobsDir())
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

// Put stores a reader at an absolute virtual file path. Missing parent
// directories are created. An unchanged retry converges on the current
// version; changed bytes create a new immutable version on the same node.
func (v *Vault) Put(
	ctx context.Context, virtualPath string, content io.Reader, opts PutOptions,
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
	err = v.blobs.WithMutation(ctx, func() (resultErr error) {
		hash, size, writeErr := v.blobs.WriteContext(ctx, content)
		if writeErr != nil {
			return writeErr
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, v.removeUnrecordedLoose(hash))
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
			created, createErr := v.metadata.CreateFile(
				ctx, parent.ID, name, hash, size, opts.MediaType,
			)
			if createErr != nil {
				return createErr
			}
			version, versionErr := v.metadata.ContentVersionByID(ctx, created.CurrentVersionID)
			if versionErr != nil {
				return versionErr
			}
			receipt.Node = fromStoreNode(created)
			receipt.Version = fromStoreVersion(version)
			receipt.Created = true
			return nil
		case lookupErr != nil:
			return lookupErr
		case existing.IsDir():
			return fmt.Errorf("virtual path %q: %w", canonicalPath, store.ErrNotFile)
		case existing.BlobHash == hash && existing.Size == size && existing.MimeType == opts.MediaType:
			version, versionErr := v.metadata.ContentVersionByID(ctx, existing.CurrentVersionID)
			if versionErr != nil {
				return versionErr
			}
			receipt.Node = fromStoreNode(existing)
			receipt.Version = fromStoreVersion(version)
			return nil
		default:
			updated, version, replaceErr := v.metadata.ReplaceContent(
				ctx, existing.ID, existing.Revision, hash, size, opts.MediaType,
			)
			if replaceErr != nil {
				return replaceErr
			}
			receipt.Node = fromStoreNode(updated)
			receipt.Version = fromStoreVersion(version)
			receipt.Replaced = true
			return nil
		}
	})
	if err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (v *Vault) removeUnrecordedLoose(hash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorded, err := v.metadata.HasBlob(ctx, hash)
	if err != nil {
		return fmt.Errorf("checking failed put cleanup for %s: %w", hash, err)
	}
	if recorded {
		return nil
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
	stats, err := v.blobs.Maintainer().Pack(ctx, packstore.PackOptions{MaxBytes: opts.MaxBytes})
	return fromPackStats(stats), err
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
