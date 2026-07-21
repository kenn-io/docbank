// Package blob adapts Kit's mixed loose-and-packed content-addressed store to
// docbank's existing ingest, API, GC, and verification interfaces.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
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

	// MinLooseCompressionBytes avoids compression staging for tiny objects.
	MinLooseCompressionBytes int64 = 4 << 10

	// MinLooseCompressionSavingsPercent keeps zstd only when it materially
	// reduces the physical loose representation.
	MinLooseCompressionSavingsPercent = 10
)

func looseCompressionPolicy() packstore.LooseCompressionOptions {
	return packstore.LooseCompressionOptions{
		Enabled:           true,
		MinBytes:          MinLooseCompressionBytes,
		MinSavingsPercent: MinLooseCompressionSavingsPercent,
	}
}

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
}

type compressedLooseCatalog struct{ packstore.Catalog }

func (c compressedLooseCatalog) ListUnpacked(ctx context.Context) ([]packstore.Candidate, error) {
	candidates, err := c.Catalog.ListUnpacked(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing unpacked blobs for compressed storage: %w", err)
	}
	for i := range candidates {
		if err := candidates[i].Hash.Validate(); err != nil {
			return nil, fmt.Errorf("validating unpacked blob candidate: %w", err)
		}
		hash := candidates[i].Hash.String()
		compressedPath := hash[:2] + "/" + hash + ".zst"
		candidates[i].Paths = append([]string{compressedPath}, candidates[i].Paths...)
	}
	return candidates, nil
}

// New constructs the daemon-owned store over catalog membership and blobsDir.
func New(catalog packstore.Catalog, blobsDir string) (*Store, error) {
	layout, err := newLayout(blobsDir)
	if err != nil {
		return nil, err
	}
	coordinator := packstore.NewCoordinator()
	maintainer, err := packstore.NewMaintainer(
		compressedLooseCatalog{Catalog: catalog}, layout, packstore.MaintainerOptions{
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
		maintainer: maintainer, coordinator: coordinator}, nil
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

// newReaderStore constructs a mixed reader without a maintenance catalog. It
// is used by this package's focused physical-storage tests.
func newReaderStore(resolver packstore.Resolver, blobsDir string) (*Store, error) {
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
	return &Store{dir: blobsDir, layout: layout, loose: loose, reader: reader}, nil
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

func validHash(hash string) bool {
	_, err := packstore.ParseHash(hash)
	return err == nil
}

// Maintainer returns the shared physical lifecycle engine.
func (s *Store) Maintainer() *packstore.Maintainer { return s.maintainer }

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
	result, err := s.loose.Write(ctx, r, packstore.WriteOptions{
		Durability:  packstore.DurablePublication,
		Dedup:       packstore.VerifyTypeAndSize,
		MaxBytes:    MaxIngestBytes,
		Compression: looseCompressionPolicy(),
	})
	if err != nil {
		return "", 0, fmt.Errorf("writing blob: %w", err)
	}
	return result.Hash.String(), result.Size, nil
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

// List returns canonical loose objects and their physical stored bytes. Raw
// and zstd representations share one logical hash; if repair evidence leaves
// both present, their sizes are summed so status and GC account for every byte
// that Remove will reclaim. Pack files are deliberately excluded.
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
			filename := entry.Name()
			hash := strings.TrimSuffix(filename, ".zst")
			if filename != hash && filename != hash+".zst" {
				continue
			}
			if !validHash(hash) || hash[:2] != shard.Name() || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("reading blob %s: %w", filename, err)
			}
			if info.Size() > math.MaxInt64-out[hash] {
				return nil, fmt.Errorf("counting physical bytes for blob %s: size overflow", hash)
			}
			out[hash] += info.Size()
		}
	}
	return out, nil
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
