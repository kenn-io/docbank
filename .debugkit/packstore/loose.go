package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
)

const (
	looseCopyBufferBytes = 32 << 10
	looseZstdWindowBytes = 1 << 20
)

var looseCopyBufferPool = sync.Pool{
	New: func() any { return new([looseCopyBufferBytes]byte) },
}

var looseZstdEncoderPool sync.Pool

// Loose publication closes each looseZstdWriter exactly once. Detach the
// staging file before pooling so an idle encoder retains no file handle.
type looseZstdWriter struct {
	*zstd.Encoder
}

func (w *looseZstdWriter) Close() error {
	if err := w.Encoder.Close(); err != nil {
		return err
	}
	w.Reset(nil)
	looseZstdEncoderPool.Put(w.Encoder)
	return nil
}

type looseVerificationIdentityPin interface {
	Stat() (fs.FileInfo, error)
	Close() error
}

var looseWriteStripes = func() [256]chan struct{} {
	var stripes [256]chan struct{}
	for index := range stripes {
		stripes[index] = make(chan struct{}, 1)
	}
	return stripes
}()

var (
	syncLooseFile             = func(file *os.File) error { defer DebugTime("kit.sync_staging", time.Now()); return file.Sync() }
	lstatLooseDirectory       = os.Lstat
	snapshotLoosePathIdentity = snapshotPathIdentity
	newLooseZstdWriter        = func(dst io.Writer) (io.WriteCloser, error) {
		if cached := looseZstdEncoderPool.Get(); cached != nil {
			encoder := cached.(*zstd.Encoder)
			encoder.Reset(dst)
			return &looseZstdWriter{Encoder: encoder}, nil
		}
		encoder, err := zstd.NewWriter(dst,
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(looseZstdWindowBytes),
		)
		if err != nil {
			return nil, err
		}
		return &looseZstdWriter{Encoder: encoder}, nil
	}
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		return zstd.NewReader(src,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(64<<20))
	}
	newLooseHashReader = func(ctx context.Context, src io.Reader) io.Reader {
		return &contextio.Reader{Context: ctx, Reader: src}
	}
	// Verification pins deliberately reuse the non-removal repair handle. In
	// particular, Windows deduplication must not require DELETE access merely
	// to keep a checked file identity allocated through the final path recheck.
	openLooseVerificationIdentityPin = func(path string) (looseVerificationIdentityPin, fs.FileInfo, error) {
		return openLooseRepairPin(path)
	}
	publishLooseFile = func(a, b string) error {
		defer DebugTime("kit.publish", time.Now())
		return atomicfile.PublishNoReplace(a, b)
	}
	publishLooseRepairFile           = replaceLooseRepairFile
	beforeLoosePublish               = func(Hash, LooseEncoding) {}
	afterLooseStripeAcquire          = func(Hash, LooseEncoding) {}
	afterLooseRepairVerify           = func(string) {}
	closeLooseStagingFile            = func(file *os.File) error { return file.Close() }
	removeLooseStagingFile           = os.Remove
	removeLooseAlternateFile         = os.Remove
	removeLooseCanonicalFile         = os.Remove
	claimLooseRemovalPath            = atomicfile.Replace
	createLooseRemovalAside          = func(path string) error { return os.Mkdir(path, 0o700) }
	removeLooseRemovalAside          = os.Remove
	publishLooseRemovalRestoreFile   = os.Link
	beforeLooseRemovalClaim          = func(string) {}
	beforeLooseRemovalRestorePublish = func(string, string) {}
	afterLooseRemovalRestorePublish  = func(string) {}
	syncLooseStagingDir              = func(path string) error { return pack.SyncDir(path) }
	syncLooseRepairShard             = func(path string) error { return pack.SyncDir(path) }
	chmodLooseStagingFile            = func(file *os.File, mode fs.FileMode) error { return file.Chmod(mode) }
)

var (
	// ErrInvalidPolicy reports an omitted or unknown physical-storage policy.
	ErrInvalidPolicy = errors.New("packstore: invalid loose storage policy")
	// ErrContentMismatch reports bytes, size, or an existing object that does
	// not agree with its content identity.
	ErrContentMismatch         = errors.New("packstore: loose content mismatch")
	errIdentityChanged         = errors.New("packstore: loose content changed identity")
	errLooseRemovalUnavailable = errors.New("packstore: loose removal authority unavailable")
)

// Durability selects the crash guarantee for loose publication.
type Durability uint8

const (
	// AtomicPublication publishes a complete file without requiring fsync.
	AtomicPublication Durability = iota + 1
	// DurablePublication fsyncs content and directory entries before success.
	DurablePublication
)

// DedupVerification selects how an existing canonical object is checked.
type DedupVerification uint8

const (
	// VerifyTypeAndSize checks structural identity without rereading content.
	VerifyTypeAndSize DedupVerification = iota + 1
	// VerifyFullHash streams the existing object through SHA-256.
	VerifyFullHash
)

// RemovalDurability selects whether a successful unlink is directory-synced.
type RemovalDurability uint8

const (
	// BestEffortRemoval unlinks without requiring the directory entry to be
	// crash-durable. It is suitable only while another authoritative copy exists.
	BestEffortRemoval RemovalDurability = iota + 1
	// DurableRemoval syncs the containing directory after unlink.
	DurableRemoval
)

// WriteOptions makes publication and dedup policy explicit. ExpectedHash is
// optional for store-directory staging and required for same-directory staging.
type WriteOptions struct {
	Durability   Durability
	Dedup        DedupVerification
	ExpectedHash Hash
	ExpectedSize int64
	SizeKnown    bool
	MaxBytes     int64
	Compression  LooseCompressionOptions
}

// LooseIdentity is the complete logical identity required for loose repair.
type LooseIdentity struct {
	Hash Hash
	Size int64
}

// RepairOptions controls verified physical replacement of one loose object.
type RepairOptions struct {
	Durability  Durability
	Compression LooseCompressionOptions
	MaxBytes    int64
}

// WriteResult describes one canonical loose object. Created remains true when
// canonical publication succeeded but a later cleanup or durability step
// returned an error.
type WriteResult struct {
	Hash       Hash
	Size       int64
	Path       string
	Created    bool
	Encoding   LooseEncoding
	StoredSize int64
}

type filesystemLooseStore struct {
	layout Layout
}

func newFilesystemLooseStore(layout Layout) (*filesystemLooseStore, error) {
	if layout.Root() == "" {
		return nil, errors.New("packstore: invalid empty layout")
	}
	return &filesystemLooseStore{layout: layout}, nil
}

// Write streams src into its canonical content-addressed path.
func (s *filesystemLooseStore) Write(ctx context.Context, src io.Reader, opts WriteOptions) (WriteResult, error) {
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}

	if opts.ExpectedHash != "" && opts.SizeKnown {
		result, exists, err := s.existing(ctx, opts.ExpectedHash, opts.ExpectedSize, opts.Dedup, opts.Durability)
		if err != nil {
			return WriteResult{}, err
		}
		if exists {
			return result, nil
		}
	}

	stagingDir, err := s.stagingDir(opts)
	if err != nil {
		return WriteResult{}, err
	}
	return s.publish(ctx, src, opts, stagingDir, nil, false)
}

// WriteBytes publishes in-memory content without redundantly hashing it while
// copying. The caller must not mutate content until the method returns.
// Because identity is known before filesystem work begins, errors after that
// point return a result populated with Hash and Size.
func (s *filesystemLooseStore) WriteBytes(ctx context.Context, content []byte, opts WriteOptions) (WriteResult, error) {
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}

	sum := sha256.Sum256(content)
	hash, err := ParseHash(hex.EncodeToString(sum[:]))
	if err != nil {
		return WriteResult{}, err
	}
	size := int64(len(content))
	identity := &WriteResult{
		Hash:       hash,
		Size:       size,
		Path:       s.layout.LoosePath(hash),
		Encoding:   LooseEncodingRaw,
		StoredSize: size,
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return *identity, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrContentMismatch, size, opts.MaxBytes)
	}
	if opts.ExpectedHash != "" && hash != opts.ExpectedHash {
		return *identity, fmt.Errorf("%w: expected hash %s, got %s", ErrContentMismatch, opts.ExpectedHash, hash)
	}
	if opts.SizeKnown && size != opts.ExpectedSize {
		return *identity, fmt.Errorf("%w: expected size %d, got %d", ErrContentMismatch, opts.ExpectedSize, size)
	}
	if result, exists, err := s.existing(ctx, hash, size, opts.Dedup, opts.Durability); err != nil {
		return *identity, err
	} else if exists {
		return result, nil
	}

	stagingDir := s.layout.LooseStagingDir(hash)
	return s.publish(ctx, bytes.NewReader(content), opts, stagingDir, identity, false)
}

// Repair completely verifies src against expected before atomically replacing
// its selected loose representation and removing the alternate canonical name.
// It changes only physical loose bytes; catalog membership and packed mappings
// remain the caller's responsibility. If replacement succeeds but alternate
// removal, repair-backup cleanup, shard durability, or deferred staging cleanup
// fails, Repair returns the published receipt with Created true together with
// the non-nil error. If recovery instead restores the old canonical or preserves
// the verified staging entry, DurablePublication syncs each changed namespace
// before returning Created false with the joined recovery and durability errors.
// Repair staging is private to this call; correctness assumes no external
// writer mutates that private inode after its final verification begins.
func (s *filesystemLooseStore) Repair(
	ctx context.Context,
	src io.Reader,
	expected LooseIdentity,
	opts RepairOptions,
) (WriteResult, error) {
	if err := expected.Hash.Validate(); err != nil {
		return WriteResult{}, err
	}
	writeOpts := WriteOptions{
		Durability:   opts.Durability,
		Dedup:        VerifyFullHash,
		ExpectedHash: expected.Hash,
		ExpectedSize: expected.Size,
		SizeKnown:    true,
		MaxBytes:     opts.MaxBytes,
		Compression:  opts.Compression,
	}
	if err := validateWriteOptions(writeOpts); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	stagingDir, err := s.stagingDir(writeOpts)
	if err != nil {
		return WriteResult{}, err
	}
	return s.publish(ctx, src, writeOpts, stagingDir, nil, true)
}

type stagedLooseFile struct {
	file   *os.File
	path   string
	closed bool
}

type looseZstdReader interface {
	io.Reader
	Close()
}

func (s *filesystemLooseStore) publish(
	ctx context.Context,
	src io.Reader,
	opts WriteOptions,
	stagingDir string,
	known *WriteResult,
	replace bool,
) (result WriteResult, resultErr error) {
	identity := WriteResult{}
	if known != nil {
		identity = *known
	}
	// A store staging child and the final shard share the root as parent.
	// Shard preparation syncs it before publication or repair recovery, so an
	// existing staging child needs no separate parent sync.
	deferStagingParentSync := s.layout.staging == StagingStoreDirectory && stagingDir != s.layout.Root()
	if err := ensureDirectory(stagingDir, opts.Durability, deferStagingParentSync); err != nil {
		return identity, fmt.Errorf("packstore: prepare loose staging: %w", err)
	}
	var staged []*stagedLooseFile
	defer func() {
		if err := cleanupLooseStaging(stagingDir, opts.Durability, staged...); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("packstore: clean loose staging: %w", err))
		}
	}()
	raw, err := createLooseStagingFile(stagingDir)
	if raw != nil {
		staged = append(staged, raw)
	}
	if err != nil {
		return identity, err
	}

	var compressed *stagedLooseFile
	var encoder io.WriteCloser
	if opts.Compression.Enabled {
		compressed, err = createLooseStagingFile(stagingDir)
		if compressed != nil {
			staged = append(staged, compressed)
		}
		if err != nil {
			return identity, err
		}
		if _, err := compressed.file.Write(make([]byte, compressedLooseHeaderSize)); err != nil {
			return identity, fmt.Errorf("packstore: write compressed loose header placeholder: %w", err)
		}
		encoder, err = newLooseZstdWriter(compressed.file)
		if err != nil {
			return identity, fmt.Errorf("packstore: create loose zstd encoder: %w", err)
		}
	}

	hasher := sha256.New()
	writers := []io.Writer{raw.file}
	if known == nil {
		writers = append(writers, hasher)
	}
	if encoder != nil {
		writers = append(writers, encoder)
	}
	reader := io.Reader(&contextio.Reader{Context: ctx, Reader: src})
	if opts.MaxBytes > 0 && opts.MaxBytes < math.MaxInt64 {
		reader = io.LimitReader(reader, opts.MaxBytes+1)
	}
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	size, copyErr := io.CopyBuffer(io.MultiWriter(writers...), reader, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if encoder != nil {
		err = encoder.Close()
	}
	if copyErr != nil || err != nil {
		return identity, fmt.Errorf("packstore: stage loose content: %w", errors.Join(copyErr, err))
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return identity, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrContentMismatch, size, opts.MaxBytes)
	}
	if known == nil {
		hash, err := ParseHash(hex.EncodeToString(hasher.Sum(nil)))
		if err != nil {
			return identity, err
		}
		identity = WriteResult{Hash: hash, Size: size}
	} else if size != identity.Size {
		return identity, fmt.Errorf("%w: content changed size from %d to %d", ErrContentMismatch, identity.Size, size)
	}
	if opts.ExpectedHash != "" && identity.Hash != opts.ExpectedHash {
		return identity, fmt.Errorf("%w: expected hash %s, got %s", ErrContentMismatch, opts.ExpectedHash, identity.Hash)
	}
	if opts.SizeKnown && identity.Size != opts.ExpectedSize {
		return identity, fmt.Errorf("%w: expected size %d, got %d", ErrContentMismatch, opts.ExpectedSize, identity.Size)
	}

	rawStoredSize := identity.Size
	if compressed != nil {
		header := encodeCompressedLooseHeader(uint64(identity.Size))
		if _, err := compressed.file.WriteAt(header[:], 0); err != nil {
			return identity, fmt.Errorf("packstore: finalize compressed loose header: %w", err)
		}
		info, err := compressed.file.Stat()
		if err != nil {
			return identity, fmt.Errorf("packstore: stat compressed loose staging: %w", err)
		}
		compressedSize := info.Size()
		if shouldCompressLoose(identity.Size, compressedSize, opts.Compression) {
			identity.Path = s.layout.CompressedLoosePath(identity.Hash)
			identity.Encoding = LooseEncodingZstd
			identity.StoredSize = compressedSize
		} else {
			identity.Path = s.layout.LoosePath(identity.Hash)
			identity.Encoding = LooseEncodingRaw
			identity.StoredSize = rawStoredSize
		}
	} else {
		identity.Path = s.layout.LoosePath(identity.Hash)
		identity.Encoding = LooseEncodingRaw
		identity.StoredSize = rawStoredSize
	}

	selected := raw
	if identity.Encoding == LooseEncodingZstd {
		selected = compressed
	}
	if selected == nil {
		return identity, errors.New("packstore: no loose staging file selected")
	}
	// Discarded duplicate staging needs no sync. Serialize the lookup with
	// repairs, but release the stripe before syncing new staging bytes.
	alreadyChecked := known != nil || (opts.ExpectedHash != "" && opts.SizeKnown)
	if !replace && !alreadyChecked && opts.Durability == DurablePublication {
		existing, exists, err := func() (WriteResult, bool, error) {
			releaseStripe, err := acquireLooseWriteStripe(ctx, identity.Hash)
			if err != nil {
				return WriteResult{}, false, err
			}
			defer releaseStripe()
			return s.existing(ctx, identity.Hash, identity.Size, opts.Dedup, opts.Durability)
		}()
		if err != nil {
			return identity, err
		}
		if err := ctx.Err(); err != nil {
			return identity, err
		}
		if exists {
			return existing, nil
		}
	}
	if opts.Durability == DurablePublication {
		if err := syncLooseFile(selected.file); err != nil {
			return identity, fmt.Errorf("packstore: sync loose staging file: %w", err)
		}
	}
	if err := selected.close(); err != nil {
		return identity, fmt.Errorf("packstore: close loose staging file: %w", err)
	}

	beforeLoosePublish(identity.Hash, identity.Encoding)
	stripeStart := time.Now()
	releaseStripe, err := acquireLooseWriteStripe(ctx, identity.Hash)
	DebugTime("kit.stripe_wait", stripeStart)
	if err != nil {
		return identity, err
	}
	defer releaseStripe()
	afterLooseStripeAcquire(identity.Hash, identity.Encoding)
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if !replace {
		existing, exists, err := s.existing(ctx, identity.Hash, identity.Size, opts.Dedup, opts.Durability)
		if err != nil {
			return identity, err
		}
		if err := ctx.Err(); err != nil {
			return identity, err
		}
		if exists {
			return existing, nil
		}
	}

	final := identity.Path
	shard := filepath.Dir(final)
	if deferStagingParentSync || filepath.Clean(shard) != filepath.Clean(stagingDir) {
		if err := ensureDirectory(shard, opts.Durability, false); err != nil {
			return identity, fmt.Errorf("packstore: prepare loose shard: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	if replace {
		pin, err := pinAndVerifyStagedLooseRepair(ctx, selected.path, identity)
		if err != nil {
			return identity, fmt.Errorf("packstore: verify staged loose repair: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, pin.file.Close()) }()
		afterLooseRepairVerify(selected.path)
		if err := ctx.Err(); err != nil {
			return identity, err
		}
		if err := verifyStagedLooseRepairPath(ctx, selected.path, identity); err != nil {
			return identity, fmt.Errorf("packstore: reverify staged loose repair: %w", err)
		}
		current, err := snapshotLoosePathIdentity(selected.path)
		if err != nil {
			return identity, fmt.Errorf("%w: recheck staged loose repair identity: %w", ErrContentMismatch, err)
		}
		if !sameLooseFileState(pin.identity, current) {
			return identity, fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged)
		}
		publication, publicationErr := publishLooseRepairFile(selected.path, final, pin.identity)
		if publication.KeepStaging {
			selected.path = ""
		}
		if !publication.Created {
			if publicationErr == nil {
				publicationErr = errors.New("repair publisher did not create canonical content")
			}
			durabilityErr := syncLooseRepairPublication(opts.Durability, publication, shard, stagingDir)
			return identity, errors.Join(
				fmt.Errorf("packstore: replace loose content: %w", publicationErr),
				durabilityErr,
			)
		}
		identity.Created = true
		alternate := s.layout.CompressedLoosePath(identity.Hash)
		if identity.Encoding == LooseEncodingZstd {
			alternate = s.layout.LoosePath(identity.Hash)
		}
		removeErr := removeLooseAlternateFile(alternate)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		if removeErr != nil {
			removeErr = fmt.Errorf("packstore: remove alternate loose representation: %w", removeErr)
		}
		publication.SyncShard = true
		syncErr := syncLooseRepairPublication(opts.Durability, publication, shard, stagingDir)
		if publicationErr != nil {
			publicationErr = fmt.Errorf("packstore: replace loose content: %w", publicationErr)
		}
		return identity, errors.Join(publicationErr, removeErr, syncErr)
	}
	if err := publishLooseFile(selected.path, final); err != nil {
		result, exists, verifyErr := s.existing(ctx, identity.Hash, identity.Size, opts.Dedup, opts.Durability)
		if verifyErr == nil && exists {
			return result, nil
		}
		return identity, errors.Join(fmt.Errorf("packstore: publish loose content: %w", err), verifyErr)
	}
	identity.Created = true
	if opts.Durability == DurablePublication {
		if err := pack.SyncDir(shard); err != nil {
			return identity, fmt.Errorf("packstore: sync loose shard: %w", err)
		}
	}
	return identity, nil
}

func syncLooseRepairPublication(
	durability Durability,
	publication looseRepairPublishResult,
	shard string,
	stagingDir string,
) error {
	if durability != DurablePublication {
		return nil
	}
	var syncErr error
	if publication.SyncShard {
		if err := syncLooseRepairShard(shard); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("packstore: sync repaired loose shard: %w", err))
		}
	}
	if publication.SyncStaging {
		if err := syncLooseStagingDir(stagingDir); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("packstore: sync preserved loose repair staging: %w", err))
		}
	}
	return syncErr
}

type pinnedLooseRepair struct {
	file     *os.File
	identity fs.FileInfo
}

func pinAndVerifyStagedLooseRepair(ctx context.Context, path string, identity WriteResult) (*pinnedLooseRepair, error) {
	pinFile, pinIdentity, err := openLooseRepairPin(path)
	if err != nil {
		return nil, err
	}
	if err := verifyStagedLooseRepairPath(ctx, path, identity); err != nil {
		return nil, errors.Join(err, pinFile.Close())
	}
	return &pinnedLooseRepair{file: pinFile, identity: pinIdentity}, nil
}

func verifyStagedLooseRepairPath(ctx context.Context, path string, identity WriteResult) error {
	file, info, err := openLooseFile(path)
	if err != nil {
		return err
	}
	object := &looseObject{
		file:        file,
		encoding:    identity.Encoding,
		logicalSize: identity.Size,
		storedSize:  info.Size(),
	}
	if identity.Encoding == LooseEncodingZstd {
		header := make([]byte, compressedLooseHeaderSize)
		if _, err := io.ReadFull(file, header); err != nil {
			return errors.Join(
				fmt.Errorf("%w: read compressed loose header: %w", ErrContentMismatch, err),
				file.Close(),
			)
		}
		logicalSize, err := decodeCompressedLooseHeader(header)
		if err != nil {
			return errors.Join(
				fmt.Errorf("%w: decode compressed loose header: %w", ErrContentMismatch, err),
				file.Close(),
			)
		}
		if logicalSize != identity.Size {
			return errors.Join(
				fmt.Errorf("%w: compressed loose logical size is %d, want %d", ErrContentMismatch, logicalSize, identity.Size),
				file.Close(),
			)
		}
	}
	stream, err := newLooseVerifiedStream(ctx, identity.Hash, object)
	if err != nil {
		return err
	}
	return errors.Join(stream.Verify(), stream.Close())
}

func acquireLooseWriteStripe(ctx context.Context, hash Hash) (func(), error) {
	stripe := looseWriteStripes[looseHashLockIndex(hash)]
	select {
	case stripe <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-stripe
			return nil, err
		}
		return func() { <-stripe }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func looseHashLockIndex(hash Hash) byte {
	return hexNibble(hash[0])<<4 | hexNibble(hash[1])
}

func hexNibble(value byte) byte {
	if value >= '0' && value <= '9' {
		return value - '0'
	}
	return value - 'a' + 10
}

func createLooseStagingFile(dir string) (*stagedLooseFile, error) {
	defer DebugTime("kit.create_staging", time.Now())
	file, err := os.CreateTemp(dir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("packstore: create loose staging file: %w", err)
	}
	staged := &stagedLooseFile{file: file, path: file.Name()}
	if err := chmodLooseStagingFile(file, 0o600); err != nil {
		return staged, fmt.Errorf("packstore: chmod loose staging file: %w", err)
	}
	return staged, nil
}

func cleanupLooseStaging(dir string, durability Durability, staged ...*stagedLooseFile) error {
	defer DebugTime("kit.cleanup_staging", time.Now())
	var cleanupErr error
	var unlinksAttempted bool
	for _, file := range staged {
		attempted, err := file.cleanup()
		unlinksAttempted = unlinksAttempted || attempted
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if durability == DurablePublication && unlinksAttempted {
		cleanupErr = errors.Join(cleanupErr, syncLooseStagingDir(dir))
	}
	return cleanupErr
}

func (f *stagedLooseFile) cleanup() (bool, error) {
	if f == nil {
		return false, nil
	}
	closeErr := f.close()
	var removeErr error
	unlinkAttempted := f.path != ""
	if f.path != "" {
		removeErr = removeLooseStagingFile(f.path)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		if removeErr == nil {
			f.path = ""
		}
	}
	return unlinkAttempted, errors.Join(closeErr, removeErr)
}

func (f *stagedLooseFile) close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	return closeLooseStagingFile(f.file)
}

func shouldCompressLoose(logicalSize, storedSize int64, opts LooseCompressionOptions) bool {
	if !opts.Enabled || logicalSize < opts.MinBytes || storedSize > logicalSize {
		return false
	}
	whole := logicalSize / 100 * int64(opts.MinSavingsPercent)
	remainder := logicalSize % 100 * int64(opts.MinSavingsPercent)
	requiredSavings := whole + (remainder+99)/100
	return logicalSize-storedSize >= requiredSavings
}

// Verify checks whether the canonical loose object exists and satisfies the
// requested identity, deduplication, and durability policy.
func (s *filesystemLooseStore) Verify(hash Hash, size int64, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	if err := hash.Validate(); err != nil {
		return WriteResult{}, false, err
	}
	opts := WriteOptions{
		Durability:   durability,
		Dedup:        verification,
		ExpectedHash: hash,
		ExpectedSize: size,
		SizeKnown:    true,
	}
	if err := validateWriteOptions(opts); err != nil {
		return WriteResult{}, false, err
	}
	return s.existing(context.Background(), hash, size, verification, durability)
}

// Remove deletes both canonical physical representations of a loose object.
// Missing objects are successful; symlinks and other non-regular entries are
// preserved and reported as content mismatches.
func (s *filesystemLooseStore) Remove(hash Hash, durability RemovalDurability) error {
	if err := hash.Validate(); err != nil {
		return err
	}
	if durability != BestEffortRemoval && durability != DurableRemoval {
		return ErrInvalidPolicy
	}
	rawPath := s.layout.LoosePath(hash)
	var removeErr error
	for _, path := range []string{rawPath, s.layout.CompressedLoosePath(hash)} {
		_, err := removeLoosePath(path, openLooseIdentityPin)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("packstore: remove loose content: %w", err))
			continue
		}
	}
	if durability == DurableRemoval {
		if err := pack.SyncDir(filepath.Dir(rawPath)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			removeErr = errors.Join(removeErr, fmt.Errorf("packstore: sync loose removal: %w", err))
		}
	}
	return removeErr
}

func validateWriteOptions(opts WriteOptions) error {
	if opts.Durability != AtomicPublication && opts.Durability != DurablePublication {
		return ErrInvalidPolicy
	}
	if opts.Dedup != VerifyTypeAndSize && opts.Dedup != VerifyFullHash {
		return ErrInvalidPolicy
	}
	if opts.ExpectedHash != "" {
		if err := opts.ExpectedHash.Validate(); err != nil {
			return err
		}
	}
	if opts.ExpectedSize < 0 || opts.MaxBytes < 0 {
		return ErrInvalidPolicy
	}
	if opts.Compression.MinBytes < 0 || opts.Compression.MinSavingsPercent < 0 || opts.Compression.MinSavingsPercent > 100 {
		return ErrInvalidPolicy
	}
	if opts.SizeKnown && opts.MaxBytes > 0 && opts.ExpectedSize > opts.MaxBytes {
		return fmt.Errorf("%w: expected size is %d bytes, limit is %d", ErrContentMismatch, opts.ExpectedSize, opts.MaxBytes)
	}
	return nil
}

func (s *filesystemLooseStore) stagingDir(opts WriteOptions) (string, error) {
	if s.layout.staging == StagingSameDirectory {
		if opts.ExpectedHash == "" {
			return "", fmt.Errorf("%w: same-directory staging requires expected hash", ErrInvalidPolicy)
		}
		return s.layout.LooseStagingDir(opts.ExpectedHash), nil
	}
	return filepath.Join(s.layout.Root(), s.layout.stagingDir), nil
}

func (s *filesystemLooseStore) existing(ctx context.Context, hash Hash, size int64, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	defer DebugTime("kit.existing", time.Now())
	result, exists, err := s.existingPath(ctx, s.layout.CompressedLoosePath(hash), hash, size, LooseEncodingZstd, verification, durability)
	if err != nil || exists {
		return result, exists, err
	}
	return s.existingPath(ctx, s.layout.LoosePath(hash), hash, size, LooseEncodingRaw, verification, durability)
}

func (s *filesystemLooseStore) existingPath(ctx context.Context, path string, hash Hash, size int64, encoding LooseEncoding, verification DedupVerification, durability Durability) (WriteResult, bool, error) {
	const maxIdentityAttempts = 8
	var result WriteResult
	var exists bool
	var err error
	for attempt := range maxIdentityAttempts {
		if err := ctx.Err(); err != nil {
			return WriteResult{}, false, err
		}
		result, exists, err = s.existingOnce(ctx, path, hash, size, encoding, verification, durability)
		if !errors.Is(err, errIdentityChanged) {
			return result, exists, err
		}
		if durability == DurablePublication {
			return result, exists, err
		}
		if attempt != maxIdentityAttempts-1 {
			runtime.Gosched()
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
		}
	}
	return result, exists, err
}

func (s *filesystemLooseStore) existingOnce(ctx context.Context, path string, hash Hash, size int64, encoding LooseEncoding, verification DedupVerification, durability Durability) (result WriteResult, exists bool, resultErr error) {
	info, err := snapshotLoosePathIdentity(path)
	if errors.Is(err, fs.ErrNotExist) {
		return WriteResult{}, false, nil
	}
	if err != nil {
		return WriteResult{}, false, fmt.Errorf("packstore: inspect loose content: %w", err)
	}
	if err := validateRegularNoFollow(path, info); err != nil {
		return WriteResult{}, false, err
	}
	var verified *looseVerifiedIdentity
	defer func() {
		if verified == nil {
			return
		}
		if closeErr := verified.close(); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
			exists = false
		}
	}()
	if encoding == LooseEncodingZstd {
		verified, err = s.verifyCompressedPath(ctx, path, info, hash, size, verification, durability == DurablePublication)
		if err != nil {
			return WriteResult{}, false, err
		}
	} else {
		if info.Size() != size {
			return WriteResult{}, false, fmt.Errorf("%w: existing size is %d, want %d", ErrContentMismatch, info.Size(), size)
		}
		if verification == VerifyFullHash {
			verified, err = s.verifyPathHash(ctx, path, info, hash, durability == DurablePublication)
			if err != nil {
				return WriteResult{}, false, err
			}
		} else if durability == DurablePublication {
			verified, err = syncPathIdentity(path, info)
			if err != nil {
				return WriteResult{}, false, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, false, err
	}
	if durability == DurablePublication {
		if err := pack.SyncDir(s.layout.Root()); err != nil {
			return WriteResult{}, false, fmt.Errorf("packstore: sync loose root: %w", err)
		}
		if err := pack.SyncDir(filepath.Dir(path)); err != nil {
			return WriteResult{}, false, fmt.Errorf("packstore: sync existing loose shard: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, false, err
	}
	if verified != nil {
		if err := verified.recheck(path); err != nil {
			return WriteResult{}, false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, false, err
	}
	return WriteResult{
		Hash:       hash,
		Size:       size,
		Path:       path,
		Encoding:   encoding,
		StoredSize: info.Size(),
	}, true, nil
}

func (s *filesystemLooseStore) verifyCompressedPath(ctx context.Context, path string, before fs.FileInfo, expectedHash Hash, expectedSize int64, verification DedupVerification, durable bool) (result *looseVerifiedIdentity, resultErr error) {
	f, err := openNoFollow(path, durable)
	if err != nil {
		return nil, fmt.Errorf("packstore: open compressed loose content: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil {
		return nil, errors.Join(statErr, f.Close())
	}
	if !sameLooseFileState(before, descriptorInfo) {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	verified, err := pinLooseVerificationIdentity(path, before, descriptorInfo)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, verified.close())
		}
	}()
	header := make([]byte, compressedLooseHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: read compressed loose header: %w", ErrContentMismatch, err), f.Close())
	}
	logicalSize, err := decodeCompressedLooseHeader(header)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, err), f.Close())
	}
	if logicalSize != expectedSize {
		return nil, errors.Join(fmt.Errorf("%w: existing logical size is %d, want %d", ErrContentMismatch, logicalSize, expectedSize), f.Close())
	}
	if verification == VerifyFullHash {
		object := &looseObject{
			file:        f,
			encoding:    LooseEncodingZstd,
			logicalSize: logicalSize,
			storedSize:  descriptorInfo.Size(),
		}
		stream, err := newLooseVerifiedStreamWithDurability(
			ctx, expectedHash, object, durable,
		)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
			return nil, err
		}
	} else if durable {
		if err := syncLooseFile(f); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else {
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	return verified, nil
}

func (s *filesystemLooseStore) verifyPathHash(ctx context.Context, path string, before fs.FileInfo, expected Hash, durable bool) (result *looseVerifiedIdentity, resultErr error) {
	f, err := openNoFollow(path, durable)
	if err != nil {
		return nil, fmt.Errorf("packstore: open loose content: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil {
		return nil, errors.Join(statErr, f.Close())
	}
	if !sameLooseFileState(before, descriptorInfo) {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	verified, err := pinLooseVerificationIdentity(path, before, descriptorInfo)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, verified.close())
		}
	}()

	hasher := sha256.New()
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	_, readErr := io.CopyBuffer(hasher, struct{ io.Reader }{newLooseHashReader(ctx, f)}, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if readErr == nil && hex.EncodeToString(hasher.Sum(nil)) != expected.String() {
		readErr = fmt.Errorf("%w: existing hash differs from %s", ErrContentMismatch, expected)
	}
	if readErr == nil && durable {
		readErr = syncLooseFile(f)
	}
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return verified, nil
}

func syncPathIdentity(path string, before fs.FileInfo) (result *looseVerifiedIdentity, resultErr error) {
	f, err := openNoFollow(path, true)
	if err != nil {
		return nil, fmt.Errorf("packstore: open existing loose content durably: %w", err)
	}
	descriptorInfo, statErr := f.Stat()
	if statErr != nil || !sameLooseFileState(before, descriptorInfo) {
		return nil, errors.Join(statErr, fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged), f.Close())
	}
	verified, err := pinLooseVerificationIdentity(path, before, descriptorInfo)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, verified.close())
		}
	}()
	syncErr := syncLooseFile(f)
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return nil, errors.Join(syncErr, closeErr)
	}
	return verified, nil
}

type looseVerifiedIdentity struct {
	pin        looseVerificationIdentityPin
	before     fs.FileInfo
	descriptor fs.FileInfo
}

func pinLooseVerificationIdentity(path string, before, descriptor fs.FileInfo) (*looseVerifiedIdentity, error) {
	pin, pinned, err := openLooseVerificationIdentityPin(path)
	if err != nil {
		return nil, fmt.Errorf("packstore: pin verified loose content: %w", err)
	}
	if !sameLooseFileState(descriptor, pinned) {
		return nil, errors.Join(
			fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged),
			pin.Close(),
		)
	}
	return &looseVerifiedIdentity{pin: pin, before: before, descriptor: descriptor}, nil
}

func (v *looseVerifiedIdentity) recheck(path string) error {
	pinned, pinErr := v.pin.Stat()
	after, snapshotErr := snapshotLoosePathIdentity(path)
	if pinErr != nil || snapshotErr != nil {
		var resultErr error
		if pinErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("packstore: recheck pinned loose content: %w", pinErr))
		}
		if snapshotErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("packstore: recheck loose content: %w", snapshotErr))
		}
		return resultErr
	}
	if !sameLooseFileState(v.before, v.descriptor) ||
		!sameLooseFileState(v.descriptor, pinned) ||
		!sameLooseFileState(pinned, after) {
		return fmt.Errorf("%w: %w", ErrContentMismatch, errIdentityChanged)
	}
	return nil
}

func (v *looseVerifiedIdentity) close() error {
	if v == nil || v.pin == nil {
		return nil
	}
	err := v.pin.Close()
	v.pin = nil
	return err
}

func sameLooseFileState(expected, actual fs.FileInfo) bool {
	if expected == nil || actual == nil {
		return false
	}
	return os.SameFile(expected, actual) && expected.Size() == actual.Size()
}

func validateRegularNoFollow(path string, info fs.FileInfo) error {
	if info == nil {
		return fmt.Errorf("%w: %s has no file identity", ErrContentMismatch, path)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not an independent regular file", ErrContentMismatch, path)
	}
	if err := validatePlatformFileInfo(info); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrContentMismatch, path, err)
	}
	return nil
}

// deferParentSync applies only to existing directories. Creation still syncs
// every missing ancestor; the caller must sync the existing parent before use
// that requires durability, including publication and repair recovery.
func ensureDirectory(path string, durability Durability, deferParentSync bool) error {
	defer DebugTime("kit.ensure_dir", time.Now())
	info, err := lstatLooseDirectory(path)
	if errors.Is(err, fs.ErrNotExist) {
		if durability != DurablePublication {
			return os.MkdirAll(path, 0o700)
		}
		if err := pack.MkdirAllSynced(filepath.Dir(path)); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		// Another writer may have created the entry after our first Lstat.
		// Validate it and sync its parent ourselves, even if Mkdir lost that
		// race. Creating just the leaf avoids a redundant creation sync.
		info, err = lstatLooseDirectory(path)
		deferParentSync = false
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not an independent directory", path)
	}
	if durability == DurablePublication && !deferParentSync {
		if err := pack.SyncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("packstore: sync loose directory parent: %w", err)
		}
	}
	return nil
}
