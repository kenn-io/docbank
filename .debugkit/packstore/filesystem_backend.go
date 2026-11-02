package packstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore/internal/packvalidate"
	"go.kenn.io/kit/pathresolve"
)

// FilesystemBackendOptions configures one filesystem store binding. An absent
// ExpectedOwnership permits reads and ownership attachment, but all
// publication and retirement operations fail fenced until attachment succeeds.
type FilesystemBackendOptions struct {
	ExpectedOwnership *Ownership
	Limits            Limits
	ReaderSlots       int
}

// FilesystemBackend owns filesystem byte mechanics without granting logical
// membership or changing application catalog authority.
type FilesystemBackend struct {
	layout    Layout
	loose     *filesystemLooseStore
	reader    *Store
	ownership ownershipState
	limits    Limits
	legacy    bool
}

var (
	walkFilesystemTree     = filepath.WalkDir
	syncFilesystemPackFile = (*os.File).Sync
)

// NewFilesystemBackend prepares one filesystem store.
func NewFilesystemBackend(
	layout Layout,
	opts FilesystemBackendOptions,
) (*FilesystemBackend, error) {
	if layout.Root() == "" {
		return nil, errors.New("packstore: invalid empty layout")
	}
	if opts.ExpectedOwnership != nil {
		if err := opts.ExpectedOwnership.Validate(); err != nil {
			return nil, err
		}
	}
	loose, err := newFilesystemLooseStore(layout)
	if err != nil {
		return nil, err
	}
	reader, err := newStoreOptions(opts.Limits, opts.ReaderSlots)
	if err != nil {
		return nil, err
	}
	reader.layout = layout
	backend := newFilesystemBackendWithReader(layout, loose, reader, false)
	if opts.ExpectedOwnership != nil {
		backend.ownership.set(*opts.ExpectedOwnership)
	}
	return backend, nil
}

func newFilesystemBackendWithReader(
	layout Layout,
	loose *filesystemLooseStore,
	reader *Store,
	legacy bool,
) *FilesystemBackend {
	return &FilesystemBackend{
		layout: layout,
		loose:  loose,
		reader: reader,
		limits: reader.limits,
		legacy: legacy,
	}
}

// Layout returns the canonical filesystem layout.
func (b *FilesystemBackend) Layout() Layout { return b.layout }

// StoreID returns the store identity expected by this binding, or empty while
// unattached.
func (b *FilesystemBackend) StoreID() StoreID {
	expected := b.ownership.get()
	if expected == nil {
		return ""
	}
	return expected.Store
}

// Close releases cached pack descriptors.
func (b *FilesystemBackend) Close() error { return b.reader.Close() }

// Ownership reads and strictly validates the live canonical marker.
func (b *FilesystemBackend) Ownership(ctx context.Context) (Ownership, error) {
	if ctx == nil {
		return Ownership{}, errors.New("packstore: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Ownership{}, err
	}
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		return Ownership{}, err
	}
	defer func() { _ = root.Close() }()
	return readOwnershipRoot(root)
}

// ReplaceOwnership atomically publishes next after checking the expected prior
// marker. Filesystems do not provide a portable compare-and-swap rename, so
// concurrent operator takeovers are last-writer-wins and losers fence on their
// next fresh marker check.
func (b *FilesystemBackend) ReplaceOwnership(
	ctx context.Context,
	next Ownership,
	expected *Ownership,
) error {
	if ctx == nil {
		return errors.New("packstore: nil context")
	}
	if expected == nil {
		if err := os.MkdirAll(b.layout.Root(), 0o700); err != nil {
			return fmt.Errorf("packstore: prepare ownership root: %w", err)
		}
	}
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := replaceOwnershipRoot(ctx, root, next, expected); err != nil {
		return err
	}
	b.ownership.set(next)
	return nil
}

func (b *FilesystemBackend) requireOwnershipRoot(
	ctx context.Context,
	root *os.Root,
) (Ownership, error) {
	expected := b.ownership.get()
	if expected == nil {
		return Ownership{}, &OwnershipMismatchError{
			Err: errors.New("packstore: filesystem backend is not attached"),
		}
	}
	if err := ctx.Err(); err != nil {
		return Ownership{}, err
	}
	actual, err := readOwnershipRoot(root)
	if err != nil {
		return Ownership{}, &OwnershipMismatchError{Expected: *expected, Err: err}
	}
	if actual != *expected {
		return Ownership{}, &OwnershipMismatchError{Expected: *expected, Actual: actual}
	}
	return actual, nil
}

// OpenLoose opens and verifies one loose representation. Read admission does
// not perform a marker round trip; applications cache fencing observations and
// fresh marker checks are required for destructive admission.
func (b *FilesystemBackend) OpenLoose(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
) (VerifiedReadCloser, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	var stream VerifiedReadCloser
	var size int64
	var err error
	if location.Encoding == 0 {
		stream, size, err = b.reader.openLooseStream(ctx, hash)
	} else {
		stream, size, err = b.reader.openLooseStreamAt(ctx, hash, location)
	}
	if err != nil {
		return nil, 0, classifyPhysicalError(err)
	}
	return &physicalVerifiedStream{stream: stream}, size, nil
}

// OpenPack opens and verifies one indexed pack entry.
func (b *FilesystemBackend) OpenPack(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
) (VerifiedReadCloser, int64, error) {
	stream, size, err := b.reader.openPackedStream(ctx, hash, &entry)
	if err != nil {
		return nil, 0, classifyPackPhysicalError(err)
	}
	return &physicalVerifiedStream{stream: stream}, size, nil
}

type physicalVerifiedStream struct {
	stream VerifiedReadCloser
}

func (s *physicalVerifiedStream) Read(p []byte) (int, error) {
	n, err := s.stream.Read(p)
	return n, classifyPhysicalError(err)
}

func (s *physicalVerifiedStream) Verify() error {
	return classifyPhysicalError(s.stream.Verify())
}

func (s *physicalVerifiedStream) Verified() bool {
	return s.stream.Verified()
}

func (s *physicalVerifiedStream) Close() error {
	return classifyPhysicalError(s.stream.Close())
}

func (b *FilesystemBackend) OpenSeekableLoose(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
) (io.ReadSeekCloser, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	if location.Encoding != 0 {
		stream, size, err := b.OpenLoose(ctx, hash, location)
		if err != nil {
			return nil, 0, err
		}
		return materializeSeekable(stream, size)
	}
	reader, size, err := b.reader.openSeekableLoose(ctx, hash)
	return reader, size, classifyPhysicalError(err)
}

func (b *FilesystemBackend) OpenSeekablePack(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
) (io.ReadSeekCloser, int64, error) {
	reader, size, err := b.reader.openSeekablePacked(ctx, hash, &entry)
	return reader, size, classifyPackPhysicalError(err)
}

func (b *FilesystemBackend) ReadLooseBounded(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
	maxBytes int64,
) ([]byte, int64, error) {
	if err := location.validate(); err != nil {
		return nil, 0, err
	}
	if location.Encoding != 0 {
		stream, size, err := b.OpenLoose(ctx, hash, location)
		if err != nil {
			return nil, 0, err
		}
		return consumeBounded(stream, size, maxBytes)
	}
	data, size, err := b.reader.readLooseBounded(ctx, hash, maxBytes)
	return data, size, classifyPhysicalError(ClassifyRepresentationLimitError(err))
}

func (b *FilesystemBackend) ReadPackBounded(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
	maxBytes int64,
) ([]byte, int64, error) {
	data, size, err := b.reader.readPackedBounded(ctx, hash, &entry, maxBytes)
	return data, size, classifyPackPhysicalError(err)
}

func classifyPackPhysicalError(err error) error {
	return classifyPhysicalError(ClassifyRepresentationLimitError(err))
}

// PublishLoose writes a canonical immutable loose object after a fresh
// ownership check.
func (b *FilesystemBackend) PublishLoose(
	ctx context.Context,
	hash Hash,
	src io.Reader,
	opts PublishOptions,
) (receipt LooseReceipt, resultErr error) {
	if src == nil {
		return LooseReceipt{}, errors.New("packstore: nil loose publication source")
	}
	if err := hash.Validate(); err != nil {
		return LooseReceipt{}, err
	}
	writeOpts, err := normalizePublishOptions(hash, opts)
	if err != nil {
		return LooseReceipt{}, err
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return LooseReceipt{}, err
	}
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		return LooseReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	owner, err := b.requireOwnershipRoot(ctx, root)
	if err != nil {
		return LooseReceipt{}, err
	}
	result, err := b.publishLooseRoot(ctx, root, hash, src, writeOpts, loosePublish)
	if err != nil {
		return LooseReceipt{}, err
	}
	return LooseReceipt{
		StoreID:    owner.Store,
		Generation: generation,
		Hash:       result.Hash,
		Location: LooseLocation{
			Encoding:    result.Encoding,
			LogicalSize: result.Size,
			StoredSize:  result.StoredSize,
		},
		Created: result.Created,
	}, nil
}

// RepairLoose deliberately overwrites one canonical loose object with bytes
// that are independently verified against its immutable logical identity.
func (b *FilesystemBackend) RepairLoose(
	ctx context.Context,
	hash Hash,
	src io.Reader,
	opts PublishOptions,
) (receipt LooseReceipt, resultErr error) {
	if src == nil {
		return LooseReceipt{}, errors.New("packstore: nil loose repair source")
	}
	if err := hash.Validate(); err != nil {
		return LooseReceipt{}, err
	}
	if !opts.SizeKnown || opts.ExpectedSize < 0 {
		return LooseReceipt{}, ErrInvalidPolicy
	}
	if opts.Durability == 0 {
		opts.Durability = DurablePublication
	}
	if opts.Dedup == 0 {
		opts.Dedup = VerifyFullHash
	}
	writeOpts := WriteOptions{
		Durability: opts.Durability, Dedup: opts.Dedup,
		ExpectedHash: hash, ExpectedSize: opts.ExpectedSize, SizeKnown: true,
		MaxBytes: opts.MaxBytes, Compression: opts.Compression,
	}
	if err := validateWriteOptions(writeOpts); err != nil {
		return LooseReceipt{}, err
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return LooseReceipt{}, err
	}
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		return LooseReceipt{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	owner, err := b.requireOwnershipRoot(ctx, root)
	if err != nil {
		return LooseReceipt{}, err
	}
	result, err := b.publishLooseRoot(ctx, root, hash, src, writeOpts, looseRepair)
	if err != nil {
		return LooseReceipt{}, err
	}
	return LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: result.Hash,
		Location: LooseLocation{
			Encoding: result.Encoding, LogicalSize: result.Size,
			StoredSize: result.StoredSize,
		},
		Created: false,
	}, nil
}

func normalizePublishOptions(hash Hash, opts PublishOptions) (WriteOptions, error) {
	if opts.Durability == 0 {
		opts.Durability = DurablePublication
	}
	if opts.Dedup == 0 {
		opts.Dedup = VerifyFullHash
	}
	writeOpts := WriteOptions{
		Durability:   opts.Durability,
		Dedup:        opts.Dedup,
		ExpectedHash: hash,
		ExpectedSize: opts.ExpectedSize,
		SizeKnown:    opts.SizeKnown,
		MaxBytes:     opts.MaxBytes,
		Compression:  opts.Compression,
	}
	if err := validateWriteOptions(writeOpts); err != nil {
		return WriteOptions{}, err
	}
	return writeOpts, nil
}

// PublishPack writes a sealed plain pack and independently verifies every
// entry before returning a receipt. Existing identical canonical packs are
// adopted only after the same full read-back verification.
func (b *FilesystemBackend) PublishPack(
	ctx context.Context,
	packID string,
	src io.Reader,
	opts PublishOptions,
) (receipt PackReceipt, resultErr error) {
	if !pack.IsValidPackID(packID) {
		return PackReceipt{}, fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	if src == nil {
		return PackReceipt{}, errors.New("packstore: nil pack publication source")
	}
	if opts.Durability == 0 {
		opts.Durability = DurablePublication
	}
	if opts.Durability != AtomicPublication && opts.Durability != DurablePublication {
		return PackReceipt{}, ErrInvalidPolicy
	}
	maxBytes, err := effectivePackPublicationLimit(
		opts.MaxBytes,
		opts.ExpectedSize,
		opts.SizeKnown,
		b.limits.PackBytes,
	)
	if err != nil {
		return PackReceipt{}, err
	}
	generation, err := newLocationGeneration()
	if err != nil {
		return PackReceipt{}, err
	}
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: open pack publication root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	owner, err := b.requireOwnershipRoot(ctx, root)
	if err != nil {
		return PackReceipt{}, err
	}
	durable := opts.Durability == DurablePublication
	packsRoot, err := ensureRootDirNoSymlinks(root, "packs", durable)
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: prepare pack directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, packsRoot.Close()) }()
	stagingRoot, err := ensureRootDirNoSymlinks(packsRoot, ".staging", durable)
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: prepare pack staging: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, stagingRoot.Close()) }()
	staged, stagedName, err := createRootTemp(stagingRoot, ".pack-")
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: create pack staging: %w", err)
	}
	stagedPath := filepath.Join(b.layout.PacksDir(), ".staging", stagedName)
	stagedOpen := true
	defer func() {
		if stagedOpen {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := stagingRoot.Remove(stagedName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: protect pack staging: %w", err)
	}
	hasher := sha256.New()
	written, err := copyBoundedContext(ctx, io.MultiWriter(staged, hasher), src, maxBytes)
	if err != nil {
		return PackReceipt{}, err
	}
	if opts.SizeKnown && written != opts.ExpectedSize {
		return PackReceipt{}, fmt.Errorf(
			"%w: expected pack size %d, got %d",
			ErrContentMismatch, opts.ExpectedSize, written,
		)
	}
	var expectedDigest [sha256.Size]byte
	copy(expectedDigest[:], hasher.Sum(nil))
	if durable {
		if err := syncFilesystemPackFile(staged); err != nil {
			return PackReceipt{}, fmt.Errorf("packstore: sync pack staging: %w", err)
		}
	}
	if err := staged.Close(); err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: close pack staging: %w", err)
	}
	stagedOpen = false
	stagedValidation, err := openRootRegularFile(stagingRoot, stagedName, stagedPath)
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: open pack staging for validation: %w", err)
	}
	if err := validateFilesystemPackFile(ctx, stagedValidation, packID, b.limits); err != nil {
		return PackReceipt{}, err
	}
	final := b.layout.PackPath(packID)
	shardName := packID[:2]
	shardRoot, err := ensureRootDirNoSymlinks(packsRoot, shardName, durable)
	if err != nil {
		return PackReceipt{}, fmt.Errorf("packstore: prepare pack shard: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, shardRoot.Close()) }()
	finalName := packID + PackExt
	created := false
	if err := packsRoot.Link(filepath.Join(".staging", stagedName), filepath.Join(shardName, finalName)); err == nil {
		created = true
	} else if !errors.Is(err, fs.ErrExist) {
		return PackReceipt{}, fmt.Errorf("packstore: publish pack: %w", err)
	}
	canonical, err := openRootRegularFile(shardRoot, finalName, final)
	if err != nil {
		return PackReceipt{}, err
	}
	size, err := verifyFilesystemPack(
		ctx,
		canonical,
		packID,
		b.limits,
		written,
		expectedDigest,
	)
	if err != nil {
		return PackReceipt{}, classifyFilesystemPackVerificationError(err)
	}
	if durable {
		if err := syncFilesystemRootDir(shardRoot); err != nil {
			return PackReceipt{}, fmt.Errorf("packstore: sync pack publication: %w", err)
		}
	}
	return PackReceipt{
		StoreID: owner.Store, Generation: generation,
		PackID: packID, Size: size, Created: created,
	}, nil
}

func classifyFilesystemPackVerificationError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrContentMismatch) {
		return err
	}
	return errors.Join(ErrPhysicalCorrupt, err)
}

func copyBoundedContext(
	ctx context.Context,
	dst io.Writer,
	src io.Reader,
	maxBytes int64,
) (int64, error) {
	reader := io.Reader(&contextio.Reader{Context: ctx, Reader: src})
	if maxBytes == 0 || maxBytes == math.MaxInt64 {
		return io.CopyBuffer(dst, reader, make([]byte, 64<<10))
	}
	limited := &io.LimitedReader{R: reader, N: maxBytes + 1}
	written, err := io.CopyBuffer(dst, limited, make([]byte, 64<<10))
	if err != nil {
		return written, fmt.Errorf("packstore: copy publication: %w", err)
	}
	if written > maxBytes {
		return written, newLimitError(
			LimitPackContainerBytes,
			uint64(written),
			uint64(maxBytes),
		)
	}
	return written, nil
}

func effectivePackPublicationLimit(
	maxBytes int64,
	expectedSize int64,
	sizeKnown bool,
	configured int64,
) (int64, error) {
	if maxBytes < 0 || expectedSize < 0 || configured <= 0 {
		return 0, ErrInvalidPolicy
	}
	effective := configured
	if maxBytes > 0 {
		effective = min(maxBytes, configured)
	}
	if sizeKnown && expectedSize > effective {
		return 0, newLimitError(
			LimitPackContainerBytes,
			uint64(expectedSize),
			uint64(effective),
		)
	}
	return effective, nil
}

func verifyFilesystemPack(
	ctx context.Context,
	file *os.File,
	packID string,
	limits Limits,
	expectedSize int64,
	expectedDigest [sha256.Size]byte,
) (resultSize int64, resultErr error) {
	info, err := file.Stat()
	if err != nil {
		return 0, errors.Join(err, file.Close())
	}
	if info.Size() != expectedSize {
		return 0, errors.Join(
			fmt.Errorf(
				"%w: canonical pack size %d does not match published size %d",
				ErrContentMismatch,
				info.Size(),
				expectedSize,
			),
			file.Close(),
		)
	}
	hasher := sha256.New()
	if _, err := io.CopyBuffer(
		hasher,
		&contextio.Reader{Context: ctx, Reader: file},
		make([]byte, 64<<10),
	); err != nil {
		return 0, errors.Join(err, file.Close())
	}
	var actualDigest [sha256.Size]byte
	copy(actualDigest[:], hasher.Sum(nil))
	if actualDigest != expectedDigest {
		return 0, errors.Join(
			fmt.Errorf("%w: canonical pack bytes differ", ErrContentMismatch),
			file.Close(),
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, errors.Join(err, file.Close())
	}
	if err := packvalidate.File(
		ctx, file, packID, filesystemPackReaderOptions(limits), filesystemPackBlobLimits(limits),
	); err != nil {
		return 0, mapPackStreamLimit(err)
	}
	return info.Size(), nil
}

func validateFilesystemPackFile(
	ctx context.Context,
	file *os.File,
	packID string,
	limits Limits,
) error {
	if err := packvalidate.File(
		ctx, file, packID, filesystemPackReaderOptions(limits), filesystemPackBlobLimits(limits),
	); err != nil {
		err = mapPackStreamLimit(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrBlobTooLarge) {
			return err
		}
		return errors.Join(ErrPhysicalCorrupt, err)
	}
	return nil
}

func filesystemPackReaderOptions(limits Limits) pack.ReaderOptions {
	return pack.ReaderOptions{Limits: pack.ReaderLimits{
		ContainerBytes: uint64(limits.PackBytes),
		FooterBytes:    uint64(limits.FooterBytes),
		Entries:        uint64(limits.PackEntries),
		RawBytes:       uint64(limits.BlobBytes),
		StoredBytes:    uint64(limits.BlobBytes),
		WindowBytes:    uint64(max(limits.BlobBytes, int64(1<<10))),
	}}
}

func filesystemPackBlobLimits(limits Limits) packvalidate.BlobLimits {
	return packvalidate.BlobLimits{
		RawBytes:    uint64(limits.BlobBytes),
		StoredBytes: uint64(limits.BlobBytes),
	}
}

// Retire removes one canonical object after a fresh ownership check. Missing
// objects are successful and active readers retain their pinned descriptors.
func (b *FilesystemBackend) Retire(
	ctx context.Context,
	ref ObjectRef,
) (resultErr error) {
	root, err := openFilesystemRoot(b.layout)
	if err != nil {
		if b.legacy && errors.Is(err, fs.ErrNotExist) && ref.PackID != "" {
			b.reader.mu.Lock()
			defer b.reader.mu.Unlock()
			return b.reader.retirePackSlotLocked(ref.PackID)
		}
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	if !b.legacy {
		if _, err := b.requireOwnershipRoot(ctx, root); err != nil {
			return err
		}
	}
	switch {
	case ref.LooseHash != "" && ref.PackID == "":
		return b.retireLooseRoot(ctx, root, ref.LooseHash, ref.LooseEncoding)
	case ref.LooseHash == "" && ref.LooseEncoding == 0 && ref.PackID != "":
		return b.retirePackRoot(root, ref.PackID)
	default:
		return errors.New("packstore: object reference must select exactly one representation")
	}
}

func (b *FilesystemBackend) retirePack(packID string) (resultErr error) {
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	root, err := openFilesystemRoot(b.layout)
	if missingFilesystemRoot(err) {
		b.reader.mu.Lock()
		defer b.reader.mu.Unlock()
		return b.reader.retirePackSlotLocked(packID)
	}
	if err != nil {
		return &PackRetirementError{
			PackID: packID, Err: fmt.Errorf("packstore: open pack retirement root: %w", err),
		}
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	return b.retirePackRoot(root, packID)
}

func (b *FilesystemBackend) retirePackRoot(root *os.Root, packID string) (resultErr error) {
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("packstore: invalid pack id %q", packID)
	}
	b.reader.mu.Lock()
	defer b.reader.mu.Unlock()
	closeErr := b.reader.retirePackSlotLocked(packID)
	shardRel := filepath.Join("packs", packID[:2])
	shard, err := openRootDirNoSymlinks(root, shardRel)
	if missingFilesystemRoot(err) {
		return closeErr
	}
	if err != nil {
		return errors.Join(closeErr, &PackRetirementError{
			PackID: packID, Err: fmt.Errorf("packstore: open pack retirement shard: %w", err),
		})
	}
	defer func() { resultErr = errors.Join(resultErr, shard.Close()) }()
	removeErr := removeRootRegularFile(shard, packID+PackExt, b.layout.PackPath(packID))
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	} else if removeErr != nil {
		removeErr = &PackRetirementError{PackID: packID, Err: removeErr}
	}
	return errors.Join(closeErr, removeErr)
}

func (b *FilesystemBackend) retireLooseRoot(
	ctx context.Context,
	root *os.Root,
	hash Hash,
	encoding LooseEncoding,
) (resultErr error) {
	if err := hash.Validate(); err != nil {
		return err
	}
	var name string
	switch encoding {
	case LooseEncodingRaw:
		name = hash.String()
	case LooseEncodingZstd:
		name = hash.String() + ".zst"
	default:
		return fmt.Errorf("packstore: invalid loose retirement encoding %d", encoding)
	}
	release, err := acquireLooseWriteStripe(ctx, hash)
	if err != nil {
		return err
	}
	defer release()
	shard, err := openRootDirNoSymlinks(root, hash.String()[:2])
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("packstore: open loose retirement shard: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, shard.Close()) }()
	path := filepath.Join(filepath.Dir(b.layout.LoosePath(hash)), name)
	if err := removeRootRegularFile(shard, name, path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("packstore: retire loose content: %w", err)
	}
	if err := syncFilesystemRootDir(shard); err != nil {
		return fmt.Errorf("packstore: sync loose retirement: %w", err)
	}
	return nil
}

// Inventory returns a stable complete inventory of recognized canonical
// objects. Unknown names are reported and preserved.
func (b *FilesystemBackend) Inventory(
	ctx context.Context,
	cursor InventoryCursor,
) (InventoryPage, error) {
	if cursor != "" {
		return InventoryPage{}, errors.New("packstore: invalid filesystem inventory cursor")
	}
	var page InventoryPage
	walkRoot, err := pathresolve.EvalSymlinks(b.layout.Root())
	if errors.Is(err, fs.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	err = walkFilesystemTree(walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(walkRoot, path)
		if err != nil {
			return err
		}
		if relative == ownershipMarkerName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if ref, ok := b.canonicalObjectRef(relative); ok && validateRegularNoFollow(path, info) == nil {
			page.Objects = append(page.Objects, InventoryObject{Ref: ref, StoredSize: info.Size()})
		} else {
			page.Unknown = append(page.Unknown, filepath.ToSlash(relative))
		}
		return nil
	})
	sort.Slice(page.Objects, func(i, j int) bool {
		return objectRefKey(page.Objects[i].Ref) < objectRefKey(page.Objects[j].Ref)
	})
	sort.Strings(page.Unknown)
	return page, err
}

// NamespaceEmpty reports whether the configured root contains any file or
// non-directory entry. Empty directory scaffolding is not physical authority.
func (b *FilesystemBackend) NamespaceEmpty(ctx context.Context) (bool, error) {
	walkRoot, err := pathresolve.EvalSymlinks(b.layout.Root())
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("packstore: inspect filesystem namespace: %w", err)
	}
	empty := true
	err = walkFilesystemTree(walkRoot, func(
		_ string, entry fs.DirEntry, walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("packstore: inspect filesystem namespace: %w", err)
	}
	return empty, nil
}

func (b *FilesystemBackend) canonicalObjectRef(relative string) (ObjectRef, bool) {
	slash := filepath.ToSlash(relative)
	if strings.HasPrefix(slash, "packs/") {
		base := filepath.Base(relative)
		packID := strings.TrimSuffix(base, PackExt)
		if strings.HasSuffix(base, PackExt) &&
			b.layout.PackPath(packID) == filepath.Join(b.layout.Root(), relative) {
			return ObjectRef{PackID: packID}, true
		}
		return ObjectRef{}, false
	}
	base := filepath.Base(relative)
	hashText := strings.TrimSuffix(base, ".zst")
	hash, err := ParseHash(hashText)
	if err != nil {
		return ObjectRef{}, false
	}
	canonical := b.layout.LoosePath(hash)
	if strings.HasSuffix(base, ".zst") {
		canonical = b.layout.CompressedLoosePath(hash)
	}
	if canonical != filepath.Join(b.layout.Root(), relative) {
		return ObjectRef{}, false
	}
	encoding := LooseEncodingRaw
	if strings.HasSuffix(base, ".zst") {
		encoding = LooseEncodingZstd
	}
	return ObjectRef{LooseHash: hash, LooseEncoding: encoding}, true
}

func objectRefKey(ref ObjectRef) string {
	if ref.LooseHash != "" {
		return fmt.Sprintf("loose/%d/%s", ref.LooseEncoding, ref.LooseHash)
	}
	return "pack/" + ref.PackID
}

func newLocationGeneration() (LocationGeneration, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("packstore: generate location generation: %w", err)
	}
	return LocationGeneration(hex.EncodeToString(value[:])), nil
}
