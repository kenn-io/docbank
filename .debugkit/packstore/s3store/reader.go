package s3store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/internal/packvalidate"
)

const rangeChunkBytes = int64(8 << 20)

func (b *Backend) requireAttached() error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.owner == nil {
		return fmt.Errorf("%w: s3store backend is not attached", packstore.ErrStoreFenced)
	}
	return nil
}

// OpenLoose opens one catalog-authorized object. Read admission intentionally
// uses the cached binding; marker round trips are reserved for destructive
// operations.
func (b *Backend) OpenLoose(
	ctx context.Context,
	expected packstore.Hash,
	location packstore.LooseLocation,
) (packstore.VerifiedReadCloser, int64, error) {
	if err := expected.Validate(); err != nil {
		return nil, 0, err
	}
	if location.Encoding != packstore.LooseEncodingRaw {
		return nil, 0, errors.Join(
			packstore.ErrStoreUnavailable,
			fmt.Errorf("s3store: unsupported loose encoding %d", location.Encoding),
		)
	}
	if err := b.requireAttached(); err != nil {
		return nil, 0, err
	}
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keys.loose(expected, location.Encoding)),
	})
	if err != nil {
		return nil, 0, classifyError("open loose object", err)
	}
	if output.ContentLength != nil && *output.ContentLength != location.StoredSize {
		_ = output.Body.Close()
		return nil, 0, errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: loose stored size %d differs from authority %d",
				*output.ContentLength, location.StoredSize),
		)
	}
	return &verifiedBody{
		ctx: ctx, body: output.Body, expected: expected, size: location.LogicalSize,
		digest: sha256.New(),
	}, location.LogicalSize, nil
}

// OpenPack downloads the immutable pack through bounded range requests, then
// opens and verifies the requested entry using the canonical pack reader.
func (b *Backend) OpenPack(
	ctx context.Context,
	expected packstore.Hash,
	entry packstore.IndexEntry,
) (stream packstore.VerifiedReadCloser, size int64, resultErr error) {
	if err := entry.Validate(); err != nil {
		return nil, 0, err
	}
	if entry.Hash != expected {
		return nil, 0, errors.New("s3store: indexed hash does not match requested hash")
	}
	if err := b.requireAttached(); err != nil {
		return nil, 0, err
	}
	staged, path, err := b.downloadPackRanges(ctx, entry.PackID)
	if err != nil {
		return nil, 0, err
	}
	reader, err := pack.NewReaderFromFileWithOptions(
		staged,
		entry.PackID,
		nil,
		b.packReaderOptions(),
	)
	if err != nil {
		_ = os.Remove(path)
		if mapped, ok := mapPackLimit(err); ok {
			return nil, 0, packstore.ClassifyRepresentationLimitError(mapped)
		}
		return nil, 0, errors.Join(packstore.ErrPhysicalCorrupt, err)
	}
	entries := reader.Entries()
	if err := packvalidate.UniqueEntries(entries); err != nil {
		return nil, 0, errors.Join(
			packstore.ErrPhysicalCorrupt,
			err, reader.Close(), os.Remove(path),
		)
	}
	canonical, ok := findPackEntry(entries, entry)
	if !ok {
		return nil, 0, errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.Join(reader.Close(), os.Remove(path)),
			errors.New("s3store: pack entry differs from catalog authority"),
		)
	}
	if err := b.checkSelectedPackEntryLimits(canonical); err != nil {
		return nil, 0, errors.Join(
			packstore.ClassifyRepresentationLimitError(err), reader.Close(), os.Remove(path),
		)
	}
	blob, err := reader.OpenBlob(ctx, canonical)
	if err != nil {
		if mapped, ok := mapPackLimit(err); ok {
			return nil, 0, errors.Join(
				packstore.ClassifyRepresentationLimitError(mapped), reader.Close(), os.Remove(path),
			)
		}
		return nil, 0, errors.Join(
			packstore.ErrPhysicalCorrupt,
			err, reader.Close(), os.Remove(path),
		)
	}
	return &packBody{blob: blob, reader: reader, path: path}, entry.RawLen, nil
}

func (b *Backend) downloadPackRanges(
	ctx context.Context,
	packID string,
) (result *os.File, path string, resultErr error) {
	head, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(b.keys.pack(packID)),
	})
	if err != nil {
		return nil, "", classifyError("inspect pack", err)
	}
	if head.ContentLength == nil || *head.ContentLength < 0 {
		return nil, "", errors.Join(
			packstore.ErrPhysicalCorrupt,
			errors.New("s3store: pack has invalid content length"),
		)
	}
	if *head.ContentLength > b.limits.PackBytes {
		limitErr := &packstore.LimitError{
			Dimension: packstore.LimitPackContainerBytes,
			Actual:    uint64(*head.ContentLength),
			Limit:     uint64(b.limits.PackBytes),
		}
		return nil, "", errors.Join(packstore.ErrPhysicalCorrupt, limitErr)
	}
	staged, err := createPrivateTemp("kit-s3-pack-range-*")
	if err != nil {
		return nil, "", fmt.Errorf("s3store: create pack read staging: %w", err)
	}
	path = staged.Name()
	stagedPath := path
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, staged.Close(), os.Remove(stagedPath))
		}
	}()
	for start := int64(0); start < *head.ContentLength; start += rangeChunkBytes {
		end := min(start+rangeChunkBytes, *head.ContentLength) - 1
		body, err := s3io.OpenRange(ctx, b.client, b.bucket, b.keys.pack(packID), "", start, end-start+1)
		if err != nil {
			if errors.Is(err, s3io.ErrRangeResponse) {
				return nil, "", errors.Join(packstore.ErrPhysicalCorrupt, err)
			}
			return nil, "", classifyError("read pack range", err)
		}
		written, copyErr := io.CopyN(staged, body, end-start+1)
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil || written != end-start+1 {
			rangeErr := errors.Join(copyErr, closeErr)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, "", errors.Join(ctxErr, rangeErr)
			}
			if errors.Is(rangeErr, context.Canceled) ||
				errors.Is(rangeErr, context.DeadlineExceeded) {
				return nil, "", rangeErr
			}
			return nil, "", errors.Join(
				packstore.ErrPhysicalCorrupt, rangeErr,
				fmt.Errorf("s3store: pack range returned %d bytes, expected %d",
					written, end-start+1),
			)
		}
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	return staged, path, nil
}

func (b *Backend) packReaderOptions() pack.ReaderOptions {
	return pack.ReaderOptions{Limits: pack.ReaderLimits{
		ContainerBytes: uint64(b.limits.PackBytes),
		FooterBytes:    uint64(b.limits.FooterBytes),
		Entries:        uint64(b.limits.PackEntries),
		WindowBytes:    uint64(max(b.limits.BlobBytes, int64(1<<10))),
	}}
}

func (b *Backend) checkSelectedPackEntryLimits(entry pack.Entry) error {
	limit := uint64(b.limits.BlobBytes)
	if entry.RawLen > limit {
		return &packstore.LimitError{
			Dimension: packstore.LimitBlobRawBytes,
			Actual:    entry.RawLen,
			Limit:     limit,
		}
	}
	if entry.StoredLen > limit {
		return &packstore.LimitError{
			Dimension: packstore.LimitBlobStoredBytes,
			Actual:    entry.StoredLen,
			Limit:     limit,
		}
	}
	return nil
}

func mapPackLimit(err error) (error, bool) {
	limit, ok := errors.AsType[*pack.StreamLimitError](err)
	if !ok {
		return nil, false
	}
	var dimension packstore.LimitDimension
	switch limit.Dimension {
	case pack.StreamLimitRawBytes:
		dimension = packstore.LimitBlobRawBytes
	case pack.StreamLimitStoredBytes:
		dimension = packstore.LimitBlobStoredBytes
	case pack.StreamLimitContainerBytes:
		dimension = packstore.LimitPackContainerBytes
	case pack.StreamLimitFooterBytes:
		dimension = packstore.LimitPackFooterBytes
	case pack.StreamLimitEntryCount:
		dimension = packstore.LimitPackEntryCount
	case pack.StreamLimitWindowBytes:
		dimension = packstore.LimitBlobWindowBytes
	default:
		return nil, false
	}
	return &packstore.LimitError{
		Dimension: dimension,
		Actual:    limit.Actual,
		Limit:     limit.Limit,
	}, true
}

func findPackEntry(entries []pack.Entry, indexed packstore.IndexEntry) (pack.Entry, bool) {
	for _, entry := range entries {
		if entry.ID.String() == indexed.Hash.String() &&
			entry.Offset == uint64(indexed.Offset) &&
			entry.StoredLen == uint64(indexed.StoredLen) &&
			entry.RawLen == uint64(indexed.RawLen) &&
			uint8(entry.Flags) == indexed.Flags &&
			entry.CRC32C == indexed.CRC32C {
			return entry, true
		}
	}
	return pack.Entry{}, false
}

type verifiedBody struct {
	ctx      context.Context
	body     io.ReadCloser
	expected packstore.Hash
	size     int64
	read     int64
	digest   hash.Hash
	terminal error
	verified bool
	closed   bool
}

func (r *verifiedBody) Read(p []byte) (int, error) {
	if r.closed {
		if r.verified {
			return 0, io.EOF
		}
		return 0, r.terminal
	}
	if err := r.ctx.Err(); err != nil {
		return 0, r.fail(err)
	}
	remaining := r.size - r.read
	if remaining < 0 {
		return 0, r.fail(fmt.Errorf("%w: negative remaining loose size", packstore.ErrPhysicalCorrupt))
	}
	if remaining == 0 {
		return 0, r.finish()
	}
	n, err := r.body.Read(p[:min(int64(len(p)), remaining)])
	if n > 0 {
		_, _ = r.digest.Write(p[:n])
		r.read += int64(n)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, r.fail(classifyError("read loose object", err))
	}
	if errors.Is(err, io.EOF) && r.read != r.size {
		return n, r.fail(errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: loose object ended at %d bytes, expected %d", r.read, r.size),
		))
	}
	if r.read == r.size {
		return n, r.finish()
	}
	if n == 0 {
		return 0, r.fail(io.ErrNoProgress)
	}
	return n, nil
}

func (r *verifiedBody) finish() error {
	var extra [1]byte
	n, err := r.body.Read(extra[:])
	if n != 0 || err == nil {
		return r.fail(errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: loose object exceeds %d bytes", r.size),
		))
	}
	if !errors.Is(err, io.EOF) {
		return r.fail(classifyError("finish loose object", err))
	}
	if hex.EncodeToString(r.digest.Sum(nil)) != r.expected.String() {
		return r.fail(errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: loose object hash differs from %s", r.expected),
		))
	}
	closeErr := r.body.Close()
	r.closed = true
	r.verified = closeErr == nil
	r.terminal = closeErr
	if closeErr != nil {
		return closeErr
	}
	return io.EOF
}

func (r *verifiedBody) fail(err error) error {
	if r.terminal == nil {
		r.terminal = errors.Join(err, r.body.Close())
	}
	r.closed = true
	return r.terminal
}

func (r *verifiedBody) Verify() error {
	if r.verified {
		return nil
	}
	if r.terminal != nil {
		return r.terminal
	}
	_, err := io.Copy(io.Discard, r)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (r *verifiedBody) Verified() bool { return r.verified }

func (r *verifiedBody) Close() error {
	if r.closed {
		return r.terminal
	}
	r.closed = true
	r.terminal = errors.Join(pack.ErrVerificationIncomplete, r.body.Close())
	return r.terminal
}

type packBody struct {
	blob     *pack.BlobReader
	reader   *pack.Reader
	path     string
	once     bool
	closeErr error
}

func (r *packBody) Read(p []byte) (int, error) {
	n, err := r.blob.Read(p)
	if err != nil {
		r.finish()
	}
	if r.closeErr == nil {
		return n, err
	}
	return n, packstore.ClassifyIntegrityError(errors.Join(err, r.closeErr))
}

func (r *packBody) Verify() error {
	err := r.blob.Verify()
	r.finish()
	return packstore.ClassifyIntegrityError(errors.Join(err, r.closeErr))
}

func (r *packBody) Verified() bool { return r.blob.Verified() }

func (r *packBody) Close() error {
	r.finish()
	return packstore.ClassifyIntegrityError(r.closeErr)
}

func (r *packBody) finish() {
	if r.once {
		return
	}
	r.once = true
	r.closeErr = packstore.ClassifyIntegrityError(errors.Join(
		r.blob.Close(),
		r.reader.Close(),
		os.Remove(r.path),
	))
}
