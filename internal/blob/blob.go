// Package blob adapts Kit's mixed loose-and-packed content-addressed store to
// docbank's existing ingest, API, GC, and verification interfaces.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// ErrInvalidHash reports a value that is not canonical lowercase SHA-256.
var ErrInvalidHash = packstore.ErrInvalidHash

const (
	// MaxIngestBytes is docbank's admission policy for a new loose object. It
	// matches the format-v1 raw-object ceiling so every admitted object remains
	// eligible for backup.
	MaxIngestBytes int64 = int64(pack.MaxRawLen)

	// MaxPackedBlobBytes is docbank's policy for packing, packed reads, and
	// packed restore. Larger admitted objects remain authoritative loose blobs.
	MaxPackedBlobBytes int64 = 64 << 20

	// ManagedLooseCompressionMinBytes and ManagedLooseCompressionMinSavingsPercent
	// are the standalone daemon's conservative loose-storage policy. Embedded
	// callers remain explicit: the public Config zero value keeps raw storage.
	ManagedLooseCompressionMinBytes          int64 = 4 << 10
	ManagedLooseCompressionMinSavingsPercent       = 10
)

// StorageLimits returns Kit's packed-read and maintenance limits with
// docbank's current packed-object policy.
func StorageLimits() packstore.Limits {
	limits := packstore.DefaultLimits()
	limits.BlobBytes = MaxPackedBlobBytes
	return limits
}

// Store owns docbank's durable loose writer and daemon-shared mixed reader.
// The maintainer and coordinator are exposed to the daemon integration, but
// physical maintenance remains unavailable to CLI processes directly.
type Store struct {
	dir         string
	layout      packstore.Layout
	catalog     packstore.Catalog
	loose       *packstore.LooseStore
	reader      *packstore.Store
	maintainer  *packstore.Maintainer
	coordinator *packstore.Coordinator
	compression packstore.LooseCompressionOptions
}

// LooseCompressionOptions is docbank's application-neutral loose storage
// policy. The zero value preserves the legacy raw representation.
type LooseCompressionOptions struct {
	Enabled           bool
	MinBytes          int64
	MinSavingsPercent int
}

// Options controls physical blob storage without exposing Kit policy types to
// docbank's public package.
type Options struct {
	LooseCompression LooseCompressionOptions
}

// ManagedOptions returns the standalone daemon's physical storage policy.
func ManagedOptions() Options {
	return Options{LooseCompression: LooseCompressionOptions{
		Enabled:           true,
		MinBytes:          ManagedLooseCompressionMinBytes,
		MinSavingsPercent: ManagedLooseCompressionMinSavingsPercent,
	}}
}

// ValidateOptions checks physical policy before a caller acquires vault
// ownership or creates storage state.
func ValidateOptions(opts Options) error {
	compression := opts.LooseCompression
	if compression.MinBytes < 0 {
		return errors.New("loose compression minimum bytes must not be negative")
	}
	if compression.MinSavingsPercent < 0 || compression.MinSavingsPercent > 100 {
		return errors.New("loose compression minimum savings percent must be between 0 and 100")
	}
	return nil
}

// WriteReceipt describes the physical result of one logical blob write.
type WriteReceipt struct {
	Hash         string
	Size         int64
	Encoding     packstore.LooseEncoding
	StoredSize   int64
	Created      bool
	PackEligible bool
}

// EncodingName returns the stable application-facing name for the published
// loose representation.
func (r WriteReceipt) EncodingName() (string, error) {
	switch r.Encoding {
	case packstore.LooseEncodingRaw:
		return "raw", nil
	case packstore.LooseEncodingZstd:
		return "zstd", nil
	default:
		return "", fmt.Errorf("unknown loose encoding %d", r.Encoding)
	}
}

// New constructs the daemon-owned store over catalog membership and blobsDir.
func New(catalog packstore.Catalog, blobsDir string) (*Store, error) {
	return NewWithOptions(catalog, blobsDir, Options{})
}

// NewWithOptions constructs the daemon-owned store with explicit physical
// loose-storage policy.
func NewWithOptions(catalog packstore.Catalog, blobsDir string, opts Options) (*Store, error) {
	if err := ValidateOptions(opts); err != nil {
		return nil, err
	}
	layout, err := newLayout(blobsDir)
	if err != nil {
		return nil, err
	}
	coordinator := packstore.NewCoordinator()
	maintainer, err := packstore.NewMaintainer(catalog, layout, packstore.MaintainerOptions{
		Coordinator: coordinator,
		Limits:      StorageLimits(),
	})
	if err != nil {
		return nil, fmt.Errorf("creating blob maintainer: %w", err)
	}
	loose, err := packstore.NewLooseStore(layout)
	if err != nil {
		_ = maintainer.Close()
		return nil, fmt.Errorf("creating loose blob store: %w", err)
	}
	return &Store{dir: blobsDir, layout: layout, catalog: catalog, loose: loose,
		reader:     maintainer.Store(),
		maintainer: maintainer, coordinator: coordinator,
		compression: packstore.LooseCompressionOptions{
			Enabled:           opts.LooseCompression.Enabled,
			MinBytes:          opts.LooseCompression.MinBytes,
			MinSavingsPercent: opts.LooseCompression.MinSavingsPercent,
		}}, nil
}

// StorageStats describes the daemon's current physical storage inventory.
// Packed dead bytes remain inside immutable packs until repack retires them.
type StorageStats struct {
	LooseBlobs        int
	LooseBytes        int64
	Packs             int
	PackStoredBytes   int64
	PackedBlobs       int64
	PackedRawBytes    int64
	PackedStoredBytes int64
	DeadPackedBytes   int64
}

// Stats reports loose files and catalog-authorized pack usage without opening
// or rewriting content.
func (s *Store) Stats(ctx context.Context) (StorageStats, error) {
	loose, err := s.List()
	if err != nil {
		return StorageStats{}, err
	}
	usage, err := s.catalog.ListPackUsage(ctx)
	if err != nil {
		return StorageStats{}, fmt.Errorf("listing pack usage: %w", err)
	}
	stats := StorageStats{LooseBlobs: len(loose), Packs: len(usage)}
	for _, size := range loose {
		stats.LooseBytes += size
	}
	for _, pack := range usage {
		stats.PackStoredBytes += pack.StoredBytes
		stats.PackedBlobs += pack.LiveEntries
		stats.PackedRawBytes += pack.LiveRawBytes
		stats.PackedStoredBytes += pack.LiveStoredBytes
	}
	if stats.PackedStoredBytes > stats.PackStoredBytes {
		return StorageStats{}, errors.New("pack usage is inconsistent: live stored bytes exceed pack totals")
	}
	stats.DeadPackedBytes = stats.PackStoredBytes - stats.PackedStoredBytes
	return stats, nil
}

// newReaderStoreWithOptions constructs a mixed reader without a maintenance
// catalog. It is used by this package's focused physical-storage tests.
func newReaderStoreWithOptions(
	resolver packstore.Resolver, blobsDir string, opts Options,
) (*Store, error) {
	if err := ValidateOptions(opts); err != nil {
		return nil, err
	}
	layout, err := newLayout(blobsDir)
	if err != nil {
		return nil, err
	}
	loose, err := packstore.NewLooseStore(layout)
	if err != nil {
		return nil, fmt.Errorf("creating test loose blob store: %w", err)
	}
	reader, err := packstore.NewStore(resolver, layout, packstore.StoreOptions{Limits: StorageLimits()})
	if err != nil {
		return nil, fmt.Errorf("creating test mixed blob reader: %w", err)
	}
	return &Store{
		dir: blobsDir, layout: layout, loose: loose, reader: reader,
		compression: packstore.LooseCompressionOptions{
			Enabled:           opts.LooseCompression.Enabled,
			MinBytes:          opts.LooseCompression.MinBytes,
			MinSavingsPercent: opts.LooseCompression.MinSavingsPercent,
		},
	}, nil
}

func newLayout(blobsDir string) (packstore.Layout, error) {
	layout, err := packstore.NewLayout(blobsDir, packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	if err != nil {
		return packstore.Layout{}, fmt.Errorf("creating blob layout: %w", err)
	}
	return layout, nil
}

func (s *Store) tmpDir() string { return filepath.Join(s.dir, "tmp") }
func (s *Store) path(hash string) string {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return ""
	}
	return s.layout.LoosePath(parsed)
}

func (s *Store) compressedPath(hash string) string {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return ""
	}
	return s.layout.CompressedLoosePath(parsed)
}

func validHash(hash string) bool {
	_, err := packstore.ParseHash(hash)
	return err == nil
}

// Maintainer returns the shared physical lifecycle engine.
func (s *Store) Maintainer() *packstore.Maintainer { return s.maintainer }

// RepackWithCatalog runs Kit's existing repack engine against a caller-scoped
// catalog view. It preserves the shared coordinator, layout, limits, and raw
// byte budget semantics while allowing a higher layer to bound source
// selection without materializing the complete pack catalog.
func (s *Store) RepackWithCatalog(
	ctx context.Context, catalog packstore.Catalog, opts packstore.RepackOptions,
) (stats packstore.RepackStats, retErr error) {
	maintainer, err := packstore.NewMaintainer(catalog, s.layout, packstore.MaintainerOptions{
		Coordinator: s.coordinator,
		Limits:      StorageLimits(),
	})
	if err != nil {
		return stats, fmt.Errorf("creating scoped blob maintainer: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, maintainer.Close()) }()
	stats, err = maintainer.Repack(ctx, opts)
	if err != nil {
		return stats, fmt.Errorf("repacking scoped blobs: %w", err)
	}
	return stats, nil
}

// Coordinator returns the process-local mutation/maintenance coordinator.
func (s *Store) Coordinator() *packstore.Coordinator { return s.coordinator }

// WithMutation holds one Kit mutation lease across fn. Callers acquire the
// application operation gate first; fn must not reenter this lease.
func (s *Store) WithMutation(ctx context.Context, fn func() error) error {
	if s.coordinator == nil {
		return fn()
	}
	lease, err := s.coordinator.AcquireMutation(ctx)
	if err != nil {
		return fmt.Errorf("acquiring blob mutation lease: %w", err)
	}
	return errors.Join(fn(), lease.Release())
}

// Write streams r into durable canonical loose storage. The caller holds a
// mutation lease across the subsequent metadata transaction.
func (s *Store) Write(r io.Reader) (string, int64, error) {
	return s.WriteContext(context.Background(), r)
}

// WriteContext is Write with cancellation.
func (s *Store) WriteContext(ctx context.Context, r io.Reader) (string, int64, error) {
	receipt, err := s.WriteDetailedContext(ctx, r)
	return receipt.Hash, receipt.Size, err
}

// WriteDetailedContext writes one blob and reports its logical and physical
// representation. The caller holds a mutation lease across the subsequent
// metadata transaction.
func (s *Store) WriteDetailedContext(ctx context.Context, r io.Reader) (WriteReceipt, error) {
	result, err := s.loose.Write(ctx, r, packstore.WriteOptions{
		Durability:  packstore.DurablePublication,
		Dedup:       packstore.VerifyTypeAndSize,
		MaxBytes:    MaxIngestBytes,
		Compression: s.compression,
	})
	if err != nil {
		return WriteReceipt{}, fmt.Errorf("writing blob: %w", err)
	}
	return writeReceipt(result), nil
}

// RepairContext verifies trusted bytes against one required logical identity
// before replacing its canonical loose representation. Catalog membership and
// packed authority remain unchanged until the caller records this receipt.
func (s *Store) RepairContext(
	ctx context.Context, hash string, size int64, trusted io.Reader,
) (WriteReceipt, error) {
	return s.repairContext(ctx, hash, size, trusted, s.compression)
}

// RepairContextWithEncoding repairs a loose object without changing its
// canonical representation. Preserving an already-authoritative loose name
// keeps the catalog truthful if the process stops after physical publication
// but before the caller's metadata transaction commits.
func (s *Store) RepairContextWithEncoding(
	ctx context.Context, hash string, size int64, trusted io.Reader,
	encoding packstore.LooseEncoding,
) (WriteReceipt, error) {
	var compression packstore.LooseCompressionOptions
	switch encoding {
	case packstore.LooseEncodingRaw:
	case packstore.LooseEncodingZstd:
		compression.Enabled = true
	default:
		return WriteReceipt{}, fmt.Errorf("repairing blob %s: unknown loose encoding %d", hash, encoding)
	}
	return s.repairContext(ctx, hash, size, trusted, compression)
}

func (s *Store) repairContext(
	ctx context.Context, hash string, size int64, trusted io.Reader,
	compression packstore.LooseCompressionOptions,
) (WriteReceipt, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return WriteReceipt{}, fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	result, err := s.loose.Repair(ctx, trusted, packstore.LooseIdentity{
		Hash: parsed, Size: size,
	}, packstore.RepairOptions{
		Durability:  packstore.DurablePublication,
		Compression: compression,
		MaxBytes:    MaxIngestBytes,
	})
	if err != nil {
		return WriteReceipt{}, fmt.Errorf("repairing blob %s: %w", hash, err)
	}
	return writeReceipt(result), nil
}

func writeReceipt(result packstore.WriteResult) WriteReceipt {
	return WriteReceipt{
		Hash: result.Hash.String(), Size: result.Size,
		Encoding: result.Encoding, StoredSize: result.StoredSize,
		Created: result.Created, PackEligible: result.Size <= MaxPackedBlobBytes,
	}
}

// Open returns catalog-authorized loose or packed content.
func (s *Store) Open(hash string) (io.ReadSeekCloser, error) {
	return s.OpenContext(context.Background(), hash)
}

// OpenContext is Open with cancellation for daemon request paths.
func (s *Store) OpenContext(ctx context.Context, hash string) (io.ReadSeekCloser, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return nil, fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	reader, _, err := s.reader.Open(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", hash, err)
	}
	return reader, nil
}

// OpenStream returns catalog-authorized loose or packed content without
// buffering the complete object. The bytes become authoritative only after
// terminal EOF or a successful Verify call; an early Close reports incomplete
// verification and does not drain the stream.
func (s *Store) OpenStream(hash string) (packstore.VerifiedReadCloser, int64, error) {
	return s.OpenStreamContext(context.Background(), hash)
}

// OpenStreamContext is OpenStream with cancellation for sequential daemon
// request and backup paths.
func (s *Store) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	reader, size, err := s.reader.OpenStream(ctx, parsed)
	if err != nil {
		return nil, 0, fmt.Errorf("opening blob stream %s: %w", hash, err)
	}
	return reader, size, nil
}

// Exists reports whether catalog-authorized content can be opened.
func (s *Store) Exists(hash string) (bool, error) {
	reader, err := s.Open(hash)
	if err == nil {
		return true, reader.Close()
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Remove deletes the canonical loose copy durably. Packed authority, if any,
// remains until the application removes its catalog mapping transactionally.
func (s *Store) Remove(hash string) error {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return fmt.Errorf("blob hash %q: %w", hash, ErrInvalidHash)
	}
	if err := s.loose.Remove(parsed, packstore.DurableRemoval); err != nil {
		return fmt.Errorf("removing blob %s: %w", hash, err)
	}
	return nil
}

// List returns canonical loose objects only. GC uses this to find interrupted
// writes that never gained a blobs row; pack files are deliberately excluded.
func (s *Store) List() (map[string]int64, error) {
	shards, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading blob dir: %w", err)
	}
	out := map[string]int64{}
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue // tmp/, packs/, and anything else that is not a shard
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, shard.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading blob shard %s: %w", shard.Name(), err)
		}
		for _, entry := range entries {
			name := entry.Name()
			isCompressed := strings.HasSuffix(name, ".zst")
			logicalName := name
			if isCompressed {
				logicalName = strings.TrimSuffix(name, ".zst")
			}
			if !validHash(logicalName) || logicalName[:2] != shard.Name() || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("reading blob %s: %w", logicalName, err)
			}
			// Interrupted replacement or repair can temporarily leave both
			// representations. GC removes both, so report their complete
			// physical footprint under the one logical hash.
			out[logicalName] += info.Size()
		}
	}
	return out, nil
}

// LooseInfo identifies one canonical loose object and its stored byte length.
type LooseInfo struct {
	Hash string
	Size int64
}

// ListPage returns one canonical-hash keyset page of loose objects. Directory
// entries are sorted by os.ReadDir, and the fixed 256-shard layout lets the
// scan stop as soon as the extra-row probe is satisfied.
func (s *Store) ListPage(after string, limit int) ([]LooseInfo, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("loose page limit must be positive")
	}
	if after != "" && !validHash(after) {
		return nil, false, errors.New("loose page cursor must be a canonical hash")
	}
	result := make([]LooseInfo, 0, limit+1)
	for shardNumber := range 256 {
		shard := fmt.Sprintf("%02x", shardNumber)
		if after != "" && shard < after[:2] {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, shard))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading blob shard %s: %w", shard, err)
		}
		var pending *LooseInfo
		flush := func() bool {
			if pending == nil {
				return false
			}
			result = append(result, *pending)
			pending = nil
			return len(result) > limit
		}
		for _, entry := range entries {
			name := entry.Name()
			compressed := strings.HasSuffix(name, ".zst")
			hash := strings.TrimSuffix(name, ".zst")
			if !validHash(hash) || hash[:2] != shard || !entry.Type().IsRegular() || hash <= after {
				continue
			}
			if pending != nil && pending.Hash != hash && flush() {
				return result[:limit], true, nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil, false, fmt.Errorf("reading blob %s: %w", hash, err)
			}
			if pending == nil {
				pending = &LooseInfo{Hash: hash, Size: info.Size()}
			} else if compressed {
				pending.Size = info.Size()
			}
		}
		if flush() {
			return result[:limit], true, nil
		}
	}
	return result, false, nil
}

// CleanTmp removes leftover loose staging files from interrupted writes.
func (s *Store) CleanTmp() error {
	info, err := os.Lstat(s.tmpDir())
	if err != nil {
		return fmt.Errorf("checking blob tmp dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("blob tmp dir %s is a symlink; refusing to clean it", s.tmpDir())
	}
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		return fmt.Errorf("reading blob tmp dir: %w", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(s.tmpDir(), entry.Name())); err != nil {
			return fmt.Errorf("removing stale temp file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// Close releases the daemon's cached pack readers.
func (s *Store) Close() error {
	if s.maintainer != nil {
		if err := s.maintainer.Close(); err != nil {
			return fmt.Errorf("closing blob maintainer: %w", err)
		}
		return nil
	}
	if s.reader != nil {
		if err := s.reader.Close(); err != nil {
			return fmt.Errorf("closing mixed blob reader: %w", err)
		}
	}
	return nil
}
