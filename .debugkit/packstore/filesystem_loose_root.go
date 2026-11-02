package packstore

import (
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

	"go.kenn.io/kit/internal/contextio"
)

type rootStagedLoose struct {
	dir    *os.Root
	file   *os.File
	name   string
	closed bool
}

type loosePublicationMode uint8

const (
	loosePublish loosePublicationMode = iota
	looseRepair
	looseRestore
)

func (f *rootStagedLoose) close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	return f.file.Close()
}

func (f *rootStagedLoose) cleanup() error {
	if f == nil {
		return nil
	}
	err := f.close()
	if f.name != "" {
		removeErr := f.dir.Remove(f.name)
		if !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

func (b *FilesystemBackend) publishLooseRoot(
	ctx context.Context,
	root *os.Root,
	hash Hash,
	src io.Reader,
	opts WriteOptions,
	mode loosePublicationMode,
) (result WriteResult, resultErr error) {
	result.Hash = hash
	if err := ctx.Err(); err != nil {
		return result, err
	}
	durable := opts.Durability == DurablePublication
	stagingRel := hash.String()[:2]
	if b.layout.staging == StagingStoreDirectory {
		stagingRel = b.layout.stagingDir
	}
	stagingRoot, stagingOwned, err := mutationDirectory(root, stagingRel, durable)
	if err != nil {
		return result, fmt.Errorf("packstore: prepare loose staging: %w", err)
	}
	if stagingOwned {
		defer func() { resultErr = errors.Join(resultErr, stagingRoot.Close()) }()
	}
	var staged []*rootStagedLoose
	defer func() {
		for _, file := range staged {
			resultErr = errors.Join(resultErr, file.cleanup())
		}
		if durable && len(staged) > 0 {
			resultErr = errors.Join(resultErr, syncFilesystemRootDir(stagingRoot))
		}
	}()
	raw, err := createRootStagedLoose(stagingRoot)
	if err != nil {
		return result, err
	}
	staged = append(staged, raw)
	var compressed *rootStagedLoose
	var encoder io.WriteCloser
	if opts.Compression.Enabled {
		compressed, err = createRootStagedLoose(stagingRoot)
		if err != nil {
			return result, err
		}
		staged = append(staged, compressed)
		if _, err := compressed.file.Write(make([]byte, compressedLooseHeaderSize)); err != nil {
			return result, fmt.Errorf("packstore: write compressed loose header placeholder: %w", err)
		}
		encoder, err = newLooseZstdWriter(compressed.file)
		if err != nil {
			return result, fmt.Errorf("packstore: create loose zstd encoder: %w", err)
		}
	}
	hasher := sha256.New()
	writers := []io.Writer{raw.file, hasher}
	if encoder != nil {
		writers = append(writers, encoder)
	}
	reader := io.Reader(&contextio.Reader{Context: ctx, Reader: src})
	readLimit := opts.MaxBytes
	readLimitKnown := readLimit > 0
	if opts.SizeKnown && (!readLimitKnown || opts.ExpectedSize < readLimit) {
		readLimit = opts.ExpectedSize
		readLimitKnown = true
	}
	if readLimitKnown && readLimit < math.MaxInt64 {
		reader = io.LimitReader(reader, readLimit+1)
	}
	buffer := looseCopyBufferPool.Get().(*[looseCopyBufferBytes]byte)
	size, copyErr := io.CopyBuffer(io.MultiWriter(writers...), reader, buffer[:])
	looseCopyBufferPool.Put(buffer)
	if encoder != nil {
		err = encoder.Close()
	}
	if copyErr != nil || err != nil {
		return result, fmt.Errorf("packstore: stage loose content: %w", errors.Join(copyErr, err))
	}
	result.Size = size
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return result, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrContentMismatch, size, opts.MaxBytes)
	}
	actual, err := ParseHash(hex.EncodeToString(hasher.Sum(nil)))
	if err != nil {
		return result, err
	}
	if actual != hash {
		return result, fmt.Errorf("%w: expected hash %s, got %s", ErrContentMismatch, hash, actual)
	}
	if opts.SizeKnown && size != opts.ExpectedSize {
		return result, fmt.Errorf("%w: expected size %d, got %d", ErrContentMismatch, opts.ExpectedSize, size)
	}

	result.Encoding = LooseEncodingRaw
	result.StoredSize = size
	selected := raw
	if compressed != nil {
		header := encodeCompressedLooseHeader(uint64(size))
		if _, err := compressed.file.WriteAt(header[:], 0); err != nil {
			return result, fmt.Errorf("packstore: finalize compressed loose header: %w", err)
		}
		info, err := compressed.file.Stat()
		if err != nil {
			return result, fmt.Errorf("packstore: stat compressed loose staging: %w", err)
		}
		if shouldCompressLoose(size, info.Size(), opts.Compression) {
			selected = compressed
			result.Encoding = LooseEncodingZstd
			result.StoredSize = info.Size()
		}
	}
	result.Path = b.layout.LoosePath(hash)
	if result.Encoding == LooseEncodingZstd {
		result.Path = b.layout.CompressedLoosePath(hash)
	}
	if durable {
		if err := selected.file.Sync(); err != nil {
			return result, fmt.Errorf("packstore: sync loose staging file: %w", err)
		}
	}
	if err := selected.close(); err != nil {
		return result, fmt.Errorf("packstore: close loose staging file: %w", err)
	}

	release, err := acquireLooseWriteStripe(ctx, hash)
	if err != nil {
		return result, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	finalRoot, err := ensureRootDirNoSymlinks(root, hash.String()[:2], durable)
	if err != nil {
		return result, fmt.Errorf("packstore: prepare loose shard: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, finalRoot.Close()) }()
	replace := mode == looseRepair
	if !replace {
		if existing, exists, err := existingRootLoose(ctx, finalRoot, b.layout, hash, size, durable); err != nil {
			if mode != looseRestore || ctx.Err() != nil || !repairableLooseError(err) {
				return result, err
			}
			// Restore has verified the backup bytes and may repair a damaged
			// destination. Keep valid alternate representations: the old database
			// remains authoritative until the restored catalog is published.
			replace = true
		} else if exists {
			return existing, nil
		}
	}
	finalName := hash.String()
	if result.Encoding == LooseEncodingZstd {
		finalName += ".zst"
	}
	stageRel := rootRelativeJoin(stagingRel, selected.name)
	finalRel := filepath.Join(hash.String()[:2], finalName)
	if replace {
		if _, _, err := verifyRootLoose(
			ctx, stagingRoot, selected.name, hash, size, result.Encoding, false,
		); err != nil {
			return result, fmt.Errorf("packstore: verify staged loose repair: %w", err)
		}
		if info, err := finalRoot.Lstat(finalName); err == nil {
			if err := validateRegularNoFollow(result.Path, info); err != nil {
				return result, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return result, err
		}
		if err := root.Rename(stageRel, finalRel); err != nil {
			return result, fmt.Errorf("packstore: replace loose content: %w", err)
		}
		selected.name = ""
		result.Created = true
		alternate := hash.String() + ".zst"
		alternateEncoding := LooseEncodingZstd
		if result.Encoding == LooseEncodingZstd {
			alternate = hash.String()
			alternateEncoding = LooseEncodingRaw
		}
		removeAlternate := mode == looseRepair
		if mode == looseRestore {
			if _, _, err := verifyRootLoose(ctx, finalRoot, alternate, hash, size, alternateEncoding, false); err != nil {
				if ctx.Err() != nil || !repairableLooseError(err) {
					return result, err
				}
				// Damaged bytes cannot serve the old catalog and must not mask
				// the repaired encoding in readers that probe both paths.
				removeAlternate = true
			}
		}
		if removeAlternate {
			if info, err := finalRoot.Lstat(alternate); err == nil {
				alternatePath := filepath.Join(filepath.Dir(result.Path), alternate)
				if validationErr := validateRegularNoFollow(alternatePath, info); validationErr != nil {
					return result, validationErr
				}
				if err := finalRoot.Remove(alternate); err != nil {
					return result, fmt.Errorf("packstore: remove alternate loose representation: %w", err)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return result, err
			}
		}
	} else {
		if err := root.Link(stageRel, finalRel); err != nil {
			if errors.Is(err, fs.ErrExist) {
				if existing, exists, verifyErr := existingRootLoose(
					ctx, finalRoot, b.layout, hash, size, durable,
				); verifyErr == nil && exists {
					return existing, nil
				} else {
					return result, errors.Join(fmt.Errorf("packstore: publish loose content: %w", err), verifyErr)
				}
			}
			return result, fmt.Errorf("packstore: publish loose content: %w", err)
		}
		result.Created = true
	}
	if durable {
		if err := syncFilesystemRootDir(finalRoot); err != nil {
			return result, fmt.Errorf("packstore: sync loose shard: %w", err)
		}
	}
	return result, nil
}

func mutationDirectory(root *os.Root, rel string, durable bool) (*os.Root, bool, error) {
	if rel == "." {
		return root, false, nil
	}
	dir, err := ensureRootDirNoSymlinks(root, rel, durable)
	return dir, true, err
}

func createRootStagedLoose(dir *os.Root) (*rootStagedLoose, error) {
	file, name, err := createRootTemp(dir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("packstore: create loose staging file: %w", err)
	}
	return &rootStagedLoose{dir: dir, file: file, name: name}, nil
}

func rootRelativeJoin(dir, name string) string {
	if dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func existingRootLoose(
	ctx context.Context,
	dir *os.Root,
	layout Layout,
	hash Hash,
	size int64,
	durable bool,
) (WriteResult, bool, error) {
	for _, candidate := range []struct {
		name     string
		encoding LooseEncoding
	}{
		{name: hash.String() + ".zst", encoding: LooseEncodingZstd},
		{name: hash.String(), encoding: LooseEncodingRaw},
	} {
		stored, exists, err := verifyRootLoose(
			ctx, dir, candidate.name, hash, size, candidate.encoding, durable,
		)
		if err != nil || exists {
			path := layout.LoosePath(hash)
			if candidate.encoding == LooseEncodingZstd {
				path = layout.CompressedLoosePath(hash)
			}
			return WriteResult{
				Hash: hash, Size: size, Path: path, Encoding: candidate.encoding,
				StoredSize: stored,
			}, exists, err
		}
	}
	return WriteResult{}, false, nil
}

func verifyRootLoose(
	ctx context.Context,
	dir *os.Root,
	name string,
	hash Hash,
	expectedSize int64,
	encoding LooseEncoding,
	durable bool,
) (storedSize int64, exists bool, err error) {
	file, err := openRootRegularFileMode(dir, name, name, durable)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	info, err := file.Stat()
	if err != nil {
		return 0, false, errors.Join(err, file.Close())
	}
	logicalSize := info.Size()
	if encoding == LooseEncodingZstd {
		header := make([]byte, compressedLooseHeaderSize)
		if _, err := io.ReadFull(file, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = fmt.Errorf("%w: read compressed loose header: %w", ErrContentMismatch, err)
			}
			return 0, false, errors.Join(err, file.Close())
		}
		logicalSize, err = decodeCompressedLooseHeader(header)
		if err != nil {
			return 0, false, errors.Join(fmt.Errorf("%w: decode compressed loose header: %w", ErrContentMismatch, err), file.Close())
		}
	}
	if logicalSize != expectedSize {
		return 0, false, errors.Join(
			fmt.Errorf("%w: existing size is %d, want %d", ErrContentMismatch, logicalSize, expectedSize),
			file.Close(),
		)
	}
	object := &looseObject{
		file: file, encoding: encoding, logicalSize: logicalSize, storedSize: info.Size(),
	}
	stream, err := newLooseVerifiedStreamWithDurability(ctx, hash, object, durable)
	if err != nil {
		return 0, false, err
	}
	if err := errors.Join(stream.Verify(), stream.Close()); err != nil {
		return 0, false, err
	}
	return info.Size(), true, nil
}

// A filesystem read/close error does not establish that stored bytes are bad.
func repairableLooseError(err error) bool {
	_, filesystemError := errors.AsType[*fs.PathError](err)
	return errors.Is(err, ErrContentMismatch) && !filesystemError
}
