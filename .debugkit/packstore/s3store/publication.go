package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/internal/strictio"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/internal/packvalidate"
)

const multipartAbortTimeout = 10 * time.Second

// PublishLoose conditionally publishes immutable logical bytes at their
// canonical raw key, then performs an independent full GET and SHA-256
// verification. ETags never grant content authority.
func (b *Backend) PublishLoose(
	ctx context.Context,
	hash packstore.Hash,
	src io.Reader,
	opts packstore.PublishOptions,
) (packstore.LooseReceipt, error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err := hash.Validate(); err != nil {
		return packstore.LooseReceipt{}, err
	}
	if src == nil {
		return packstore.LooseReceipt{}, errors.New("s3store: nil loose publication source")
	}
	opts, err = normalizeLoosePublishOptions(opts)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if opts.Compression.Enabled {
		return packstore.LooseReceipt{}, errors.New("s3store: compressed loose publication is not supported")
	}
	key := b.keys.loose(hash, packstore.LooseEncodingRaw)
	result, err := b.multipartPublish(ctx, key, src, multipartPublishOptions{
		maxBytes: opts.MaxBytes, exactSize: opts.ExpectedSize,
		sizeKnown: opts.SizeKnown, expectedDigest: hash.String(),
	})
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err := b.verifyRawObject(ctx, key, hash, result.size); err != nil {
		return packstore.LooseReceipt{}, err
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	return packstore.LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: hash,
		Location: packstore.LooseLocation{
			Encoding: packstore.LooseEncodingRaw, LogicalSize: result.size,
			StoredSize: result.size,
		},
		Created: result.created,
	}, nil
}

// RepairLoose deliberately replaces one damaged canonical loose object after
// validating the complete source locally, then independently reads it back.
func (b *Backend) RepairLoose(
	ctx context.Context,
	hash packstore.Hash,
	src io.Reader,
	opts packstore.PublishOptions,
) (receipt packstore.LooseReceipt, resultErr error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err := hash.Validate(); err != nil {
		return packstore.LooseReceipt{}, err
	}
	if src == nil {
		return packstore.LooseReceipt{}, packstore.ErrInvalidPolicy
	}
	opts, err = normalizeLoosePublishOptions(opts)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if !opts.SizeKnown || opts.Compression.Enabled {
		return packstore.LooseReceipt{}, packstore.ErrInvalidPolicy
	}
	staged, err := createPrivateTemp("kit-s3-repair-*")
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: create repair staging: %w", err,
		)
	}
	path := staged.Name()
	defer func() {
		resultErr = errors.Join(resultErr, staged.Close())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: protect repair staging: %w", err,
		)
	}
	hasher := sha256.New()
	reader := io.Reader(&contextio.Reader{Context: ctx, Reader: src})
	readLimit := opts.ExpectedSize
	if opts.MaxBytes > 0 {
		readLimit = min(readLimit, opts.MaxBytes)
	}
	if readLimit < math.MaxInt64 {
		reader = io.LimitReader(reader, readLimit+1)
	}
	size, err := io.CopyBuffer(
		io.MultiWriter(staged, hasher), reader, make([]byte, 64<<10),
	)
	if err := ctx.Err(); err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: stage repair source: %w", err,
		)
	}
	if (opts.MaxBytes > 0 && size > opts.MaxBytes) ||
		size != opts.ExpectedSize ||
		hex.EncodeToString(hasher.Sum(nil)) != hash.String() {
		return packstore.LooseReceipt{}, packstore.ErrContentMismatch
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: rewind repair source: %w", err,
		)
	}
	key := b.keys.loose(hash, packstore.LooseEncodingRaw)
	if _, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body: staged, ContentLength: aws.Int64(size),
	}); err != nil {
		return packstore.LooseReceipt{}, classifyError("replace loose object", err)
	}
	if err := b.verifyRawObject(ctx, key, hash, size); err != nil {
		return packstore.LooseReceipt{}, err
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	return packstore.LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: hash,
		Location: packstore.LooseLocation{
			Encoding: packstore.LooseEncodingRaw, LogicalSize: size,
			StoredSize: size,
		},
		Created: false,
	}, nil
}

// PublishPack conditionally publishes a sealed pack, then independently reads
// it back and verifies every entry before returning physical evidence.
func (b *Backend) PublishPack(
	ctx context.Context,
	packID string,
	src io.Reader,
	opts packstore.PublishOptions,
) (receipt packstore.PackReceipt, resultErr error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	if !pack.IsValidPackID(packID) {
		return packstore.PackReceipt{}, fmt.Errorf("s3store: invalid pack id %q", packID)
	}
	if src == nil {
		return packstore.PackReceipt{}, errors.New("s3store: nil pack publication source")
	}
	if opts.Durability == 0 {
		opts.Durability = packstore.DurablePublication
	}
	if opts.Durability != packstore.AtomicPublication &&
		opts.Durability != packstore.DurablePublication {
		return packstore.PackReceipt{}, packstore.ErrInvalidPolicy
	}
	maxBytes, err := effectivePackPublicationLimit(
		opts.MaxBytes,
		opts.ExpectedSize,
		opts.SizeKnown,
		b.limits.PackBytes,
	)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	stagedPath, stagedSize, stagedDigest, err := stagePackPublication(
		ctx,
		src,
		maxBytes,
		opts.ExpectedSize,
		opts.SizeKnown,
	)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	defer func() {
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	validationFile, err := os.Open(stagedPath)
	if err != nil {
		return packstore.PackReceipt{}, fmt.Errorf(
			"s3store: open pack publication staging for validation: %w", err,
		)
	}
	if err := b.validatePackFile(ctx, validationFile, packID); err != nil {
		return packstore.PackReceipt{}, err
	}
	uploadFile, err := os.Open(stagedPath)
	if err != nil {
		return packstore.PackReceipt{}, fmt.Errorf(
			"s3store: reopen pack publication staging: %w", err,
		)
	}
	defer func() { resultErr = errors.Join(resultErr, uploadFile.Close()) }()
	result, err := b.multipartPublish(ctx, b.keys.pack(packID), uploadFile, multipartPublishOptions{
		maxBytes: maxBytes, exactSize: stagedSize, sizeKnown: true,
		expectedDigest: hex.EncodeToString(stagedDigest[:]),
	})
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	size, digest, err := b.verifyPackObject(ctx, packID, result.size)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	if digest != stagedDigest || result.digest != stagedDigest {
		return packstore.PackReceipt{}, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: canonical pack differs from published bytes"),
		)
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	return packstore.PackReceipt{
		StoreID: owner.Store, Generation: generation, PackID: packID,
		Size: size, Created: result.created,
	}, nil
}

func stagePackPublication(
	ctx context.Context,
	src io.Reader,
	maxBytes int64,
	exactSize int64,
	sizeKnown bool,
) (path string, size int64, digest [sha256.Size]byte, resultErr error) {
	staged, err := createPrivateTemp("kit-s3-pack-publish-*")
	if err != nil {
		return "", 0, digest, fmt.Errorf("s3store: create pack publication staging: %w", err)
	}
	stagedPath := staged.Name()
	keep := false
	open := true
	defer func() {
		if open {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if !keep {
			if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return "", 0, digest, fmt.Errorf("s3store: protect pack publication staging: %w", err)
	}
	hasher := sha256.New()
	reader := io.Reader(&contextio.Reader{Context: ctx, Reader: src})
	if maxBytes < math.MaxInt64 {
		reader = io.LimitReader(reader, maxBytes+1)
	}
	size, err = io.CopyBuffer(
		io.MultiWriter(staged, hasher),
		reader,
		make([]byte, 64<<10),
	)
	if err := ctx.Err(); err != nil {
		return "", 0, digest, err
	}
	if err != nil {
		return "", 0, digest, fmt.Errorf("s3store: stage pack publication: %w", err)
	}
	if size > maxBytes {
		return "", 0, digest, &packstore.LimitError{
			Dimension: packstore.LimitPackContainerBytes,
			Actual:    uint64(size),
			Limit:     uint64(maxBytes),
		}
	}
	if sizeKnown && size != exactSize {
		return "", 0, digest, fmt.Errorf(
			"%w: publication size %d does not match expected %d",
			packstore.ErrContentMismatch, size, exactSize,
		)
	}
	copy(digest[:], hasher.Sum(nil))
	if err := staged.Sync(); err != nil {
		return "", 0, digest, fmt.Errorf("s3store: sync pack publication staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		open = false
		return "", 0, digest, fmt.Errorf("s3store: close pack publication staging: %w", err)
	}
	open = false
	keep = true
	return stagedPath, size, digest, nil
}

type publicationResult struct {
	size    int64
	digest  [sha256.Size]byte
	created bool
}

type multipartPublishOptions struct {
	maxBytes       int64
	exactSize      int64
	sizeKnown      bool
	expectedDigest string
}

func (b *Backend) multipartPublish(
	ctx context.Context,
	key string,
	src io.Reader,
	opts multipartPublishOptions,
) (result publicationResult, resultErr error) {
	created, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return publicationResult{}, classifyError("begin multipart publication", err)
	}
	uploadID := aws.ToString(created.UploadId)
	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), multipartAbortTimeout,
		)
		defer cancel()
		_, abortErr := b.client.AbortMultipartUpload(abortCtx,
			&s3.AbortMultipartUploadInput{
				Bucket: aws.String(b.bucket), Key: aws.String(key),
				UploadId: aws.String(uploadID),
			})
		if abortErr != nil {
			resultErr = errors.Join(resultErr, classifyError("abort multipart publication", abortErr))
		}
	}()

	hasher := sha256.New()
	bufferBytes := b.part
	if opts.maxBytes > 0 && opts.maxBytes < bufferBytes {
		bufferBytes = opts.maxBytes + 1
	}
	if opts.sizeKnown && opts.exactSize < bufferBytes {
		bufferBytes = opts.exactSize + 1
	}
	buffer := make([]byte, int(bufferBytes))
	reader := &contextio.Reader{Context: ctx, Reader: src}
	var parts []types.CompletedPart
	for partNumber := int32(1); ; partNumber++ {
		readBytes := int64(len(buffer))
		if opts.maxBytes > 0 {
			remaining := opts.maxBytes - result.size
			if remaining < readBytes {
				readBytes = remaining + 1
			}
		}
		if opts.sizeKnown {
			remaining := opts.exactSize - result.size
			if remaining < readBytes {
				readBytes = remaining + 1
			}
		}
		n, readErr := strictio.Fill(reader, buffer[:readBytes])
		if err := ctx.Err(); err != nil {
			return publicationResult{}, err
		}
		if err := strictio.IgnoreEOF(readErr); err != nil {
			return publicationResult{}, fmt.Errorf("s3store: read publication source: %w", err)
		}
		if n == 0 {
			if opts.sizeKnown && result.size != opts.exactSize {
				return publicationResult{}, fmt.Errorf(
					"%w: publication size %d does not match expected %d",
					packstore.ErrContentMismatch, result.size, opts.exactSize,
				)
			}
			if len(parts) == 0 {
				return b.publishEmpty(ctx, key, opts.expectedDigest)
			}
			break
		}
		result.size += int64(n)
		if opts.maxBytes > 0 && result.size > opts.maxBytes {
			return publicationResult{}, &packstore.LimitError{
				Dimension: packstore.LimitPackContainerBytes,
				Actual:    uint64(result.size),
				Limit:     uint64(opts.maxBytes),
			}
		}
		if opts.sizeKnown && result.size > opts.exactSize {
			return publicationResult{}, fmt.Errorf(
				"%w: publication size %d does not match expected %d",
				packstore.ErrContentMismatch, result.size, opts.exactSize,
			)
		}
		_, _ = hasher.Write(buffer[:n])
		uploaded, err := s3io.UploadPart(ctx, b.client, b.bucket, key, uploadID, partNumber, buffer[:n], nil)
		if err != nil {
			return publicationResult{}, classifyError("upload multipart part", err)
		}
		parts = append(parts, types.CompletedPart{
			ETag: uploaded.ETag, PartNumber: aws.Int32(partNumber),
		})
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if opts.sizeKnown && result.size != opts.exactSize {
		return publicationResult{}, fmt.Errorf(
			"%w: publication size %d does not match expected %d",
			packstore.ErrContentMismatch, result.size, opts.exactSize,
		)
	}
	copy(result.digest[:], hasher.Sum(nil))
	if opts.expectedDigest != "" && hex.EncodeToString(result.digest[:]) != opts.expectedDigest {
		return publicationResult{}, fmt.Errorf(
			"%w: publication bytes differ from %s",
			packstore.ErrContentMismatch, opts.expectedDigest,
		)
	}
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		UploadId: aws.String(uploadID), IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	switch statusCode(err) {
	case 0:
		if err != nil {
			return publicationResult{}, classifyError("complete multipart publication", err)
		}
		result.created = true
		completed = true
	case http.StatusPreconditionFailed:
		result.created = false
		return result, nil
	default:
		return publicationResult{}, classifyError("complete multipart publication", err)
	}
	return result, nil
}

func normalizeLoosePublishOptions(
	opts packstore.PublishOptions,
) (packstore.PublishOptions, error) {
	if opts.Durability == 0 {
		opts.Durability = packstore.DurablePublication
	}
	if opts.Dedup == 0 {
		opts.Dedup = packstore.VerifyFullHash
	}
	if opts.Durability != packstore.AtomicPublication &&
		opts.Durability != packstore.DurablePublication {
		return packstore.PublishOptions{}, packstore.ErrInvalidPolicy
	}
	if opts.Dedup != packstore.VerifyTypeAndSize && opts.Dedup != packstore.VerifyFullHash {
		return packstore.PublishOptions{}, packstore.ErrInvalidPolicy
	}
	if opts.ExpectedSize < 0 || opts.MaxBytes < 0 ||
		opts.Compression.MinBytes < 0 || opts.Compression.MinSavingsPercent < 0 ||
		opts.Compression.MinSavingsPercent > 100 {
		return packstore.PublishOptions{}, packstore.ErrInvalidPolicy
	}
	if opts.SizeKnown && opts.MaxBytes > 0 && opts.ExpectedSize > opts.MaxBytes {
		return packstore.PublishOptions{}, fmt.Errorf(
			"%w: expected size is %d bytes, limit is %d",
			packstore.ErrContentMismatch, opts.ExpectedSize, opts.MaxBytes,
		)
	}
	return opts, nil
}

func effectivePackPublicationLimit(
	maxBytes int64,
	expectedSize int64,
	sizeKnown bool,
	configured int64,
) (int64, error) {
	if maxBytes < 0 || expectedSize < 0 || configured <= 0 {
		return 0, packstore.ErrInvalidPolicy
	}
	effective := configured
	if maxBytes > 0 {
		effective = min(maxBytes, configured)
	}
	if sizeKnown && expectedSize > effective {
		return 0, &packstore.LimitError{
			Dimension: packstore.LimitPackContainerBytes,
			Actual:    uint64(expectedSize),
			Limit:     uint64(effective),
		}
	}
	return effective, nil
}

func (b *Backend) publishEmpty(
	ctx context.Context,
	key string,
	expectedDigest string,
) (publicationResult, error) {
	digest := sha256.Sum256(nil)
	if expectedDigest != "" && hex.EncodeToString(digest[:]) != expectedDigest {
		return publicationResult{}, fmt.Errorf(
			"%w: publication bytes differ from %s",
			packstore.ErrContentMismatch, expectedDigest,
		)
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body: bytes.NewReader(nil), IfNoneMatch: aws.String("*"),
	})
	if err != nil && statusCode(err) != http.StatusPreconditionFailed {
		return publicationResult{}, classifyError("publish empty object", err)
	}
	return publicationResult{
		digest: digest, created: err == nil,
	}, nil
}

func (b *Backend) verifyRawObject(
	ctx context.Context,
	key string,
	hash packstore.Hash,
	expectedSize int64,
) error {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return classifyError("read back loose object", err)
	}
	defer func() { _ = output.Body.Close() }()
	if err := validateReadbackSize("loose", output.ContentLength, expectedSize); err != nil {
		return err
	}
	hasher := sha256.New()
	size, err := io.CopyBuffer(
		hasher,
		boundedReadback(output.Body, expectedSize),
		make([]byte, 64<<10),
	)
	if err != nil {
		return classifyError("read back loose object", err)
	}
	if size != expectedSize || hex.EncodeToString(hasher.Sum(nil)) != hash.String() {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: loose read-back does not match canonical identity"),
		)
	}
	return nil
}

func (b *Backend) verifyPackObject(
	ctx context.Context,
	packID string,
	expectedSize int64,
) (resultSize int64, digest [sha256.Size]byte, resultErr error) {
	if expectedSize > b.limits.PackBytes {
		return 0, digest, &packstore.LimitError{
			Dimension: packstore.LimitPackContainerBytes,
			Actual:    uint64(expectedSize),
			Limit:     uint64(b.limits.PackBytes),
		}
	}
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(b.keys.pack(packID)),
	})
	if err != nil {
		return 0, digest, classifyError("read back pack", err)
	}
	defer func() { resultErr = errors.Join(resultErr, output.Body.Close()) }()
	if err := validateReadbackSize("pack", output.ContentLength, expectedSize); err != nil {
		return 0, digest, err
	}
	staged, err := createPrivateTemp("kit-s3-pack-*")
	if err != nil {
		return 0, digest, fmt.Errorf("s3store: create pack verification staging: %w", err)
	}
	stagedPath := staged.Name()
	defer func() {
		if staged != nil {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	hasher := sha256.New()
	resultSize, err = io.CopyBuffer(
		io.MultiWriter(staged, hasher),
		boundedReadback(output.Body, expectedSize),
		make([]byte, 64<<10),
	)
	if err != nil {
		return 0, digest, classifyError("read back pack", err)
	}
	copy(digest[:], hasher.Sum(nil))
	if resultSize != expectedSize {
		return 0, digest, errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: pack read-back size %d differs from %d", resultSize, expectedSize),
		)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return 0, digest, err
	}
	validationFile := staged
	staged = nil
	if err := b.validatePackFile(ctx, validationFile, packID); err != nil {
		return 0, digest, err
	}
	return resultSize, digest, nil
}

func (b *Backend) validatePackFile(ctx context.Context, file *os.File, packID string) error {
	limits := packvalidate.BlobLimits{
		RawBytes:    uint64(b.limits.BlobBytes),
		StoredBytes: uint64(b.limits.BlobBytes),
	}
	if err := packvalidate.File(ctx, file, packID, b.packReaderOptions(), limits); err != nil {
		if mapped, ok := mapPackLimit(err); ok {
			return mapped
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.Join(packstore.ErrPhysicalCorrupt, err)
	}
	return nil
}

func validateReadbackSize(kind string, actual *int64, expected int64) error {
	if expected < 0 {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: invalid negative %s read-back size %d", kind, expected),
		)
	}
	if actual != nil && *actual != expected {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf(
				"s3store: %s read-back content length %d differs from %d",
				kind,
				*actual,
				expected,
			),
		)
	}
	return nil
}

func boundedReadback(src io.Reader, expected int64) io.Reader {
	return io.MultiReader(
		io.LimitReader(src, expected),
		io.LimitReader(src, 1),
	)
}

func newGeneration() (packstore.LocationGeneration, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("s3store: generate location identity: %w", err)
	}
	return packstore.LocationGeneration(hex.EncodeToString(value[:])), nil
}
