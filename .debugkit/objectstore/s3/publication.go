package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.kenn.io/kit/internal/contextio"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/internal/strictio"
	"go.kenn.io/kit/objectstore"
)

type (
	dispatchKey   struct{}
	dispatchState struct {
		sent      atomic.Bool
		uncertain atomic.Bool
	}
	dispatchClient struct{ client *awshttp.BuildableClient }
)

func (c *dispatchClient) Do(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	state, _ := r.Context().Value(dispatchKey{}).(*dispatchState)
	var wroteHeaders atomic.Bool
	if state != nil {
		// Connection acquisition and TLS happen before request headers. Entering
		// Do alone does not mean publication could have reached the service.
		trace := &httptrace.ClientTrace{WroteHeaders: func() {
			wroteHeaders.Store(true)
			state.sent.Store(true)
		}}
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	}
	response, err := c.client.Do(r)
	// A 2xx header may precede a truncated completion body. Preserve that
	// possible commit even when SDK decoding or a later attempt fails.
	if state != nil {
		possibleCommitResponse := response != nil && (response.StatusCode/100 == 2 || (response.StatusCode >= 500 && response.StatusCode != http.StatusNotImplemented && response.StatusCode != http.StatusHTTPVersionNotSupported))
		if (wroteHeaders.Load() && err != nil) || possibleCommitResponse {
			state.uncertain.Store(true)
		}
	}
	return response, err
}

// Publish validates the entire rewindable source before conditional publication.
// The caller owns the source and must keep its contents stable until return.
// A receipt proves observed bytes, including a verified identical retry.
func (s *Store) Publish(ctx context.Context, key string, src io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	if src == nil || size < 0 {
		return objectstore.Receipt{}, fmt.Errorf("s3: invalid source or size: %w", objectstore.ErrChecksumMismatch)
	}
	if size/s.part > 10000 || (size/s.part == 10000 && size%s.part != 0) {
		return objectstore.Receipt{}, errors.New("s3: size exceeds 10000 parts")
	}
	crc, err := validateSource(ctx, src, size, digest)
	if err != nil {
		return objectstore.Receipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return objectstore.Receipt{}, err
	}
	multipart := size > s.part
	var state dispatchState
	var completionRevision string
	uploadCtx := context.WithValue(ctx, dispatchKey{}, &state)
	if multipart {
		completionRevision, err = s.multipart(ctx, full, src, size, digest, crc, &state)
	} else {
		sha := base64.StdEncoding.EncodeToString(digest[:])
		var put *awss3.PutObjectOutput
		put, err = s.client.PutObject(uploadCtx, &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full), Body: &seekSource{ctx: ctx, ReadSeeker: src}, ContentLength: aws.Int64(size), IfNoneMatch: aws.String("*"), ChecksumSHA256: aws.String(sha)})
		// An echoed checksum shows the provider checked our SHA-256 before
		// storing; an endpoint that ignored the header would not return it.
		if err == nil && aws.ToString(put.ChecksumSHA256) == sha {
			completionRevision = aws.ToString(put.ETag)
		}
	}
	if err != nil {
		code := statusCode(err)
		classified := classify("publish", err)
		if code == http.StatusPreconditionFailed {
			// Publication has no revision precondition; verification determines
			// whether the stored bytes conflict with this source.
			classified = fmt.Errorf("s3: publish: %w", err)
		}
		if !state.uncertain.Load() && errors.Is(classified, objectstore.ErrChecksumMismatch) {
			return objectstore.Receipt{}, classified
		}
		// A finalization dispatch may have stored bytes despite transport failure.
		// 412 also covers an SDK retry of our own successful, unanswered attempt.
		if code == 412 || state.uncertain.Load() || (state.sent.Load() && (code == 0 || code == http.StatusConflict)) {
			receipt, inspectErr := s.verify(ctx, key, size, digest, crc, true, multipart, "")
			if inspectErr != nil {
				return objectstore.Receipt{}, errors.Join(inspectErr, classified)
			}
			return receipt, nil
		}
		return objectstore.Receipt{}, classified
	}
	return s.verify(ctx, key, size, digest, crc, false, multipart, completionRevision)
}

type seekSource struct {
	ctx context.Context
	io.ReadSeeker
}

func (s *seekSource) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.ReadSeeker.Read(p)
}

func validateSource(ctx context.Context, src io.ReadSeeker, size int64, digest [32]byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("s3: rewind source: %w", err)
	}
	sha := sha256.New()
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, err := io.CopyBuffer(io.MultiWriter(sha, crc), io.LimitReader(&contextio.Reader{Context: ctx, Reader: src}, size), make([]byte, 64<<10))
	if err != nil {
		return "", fmt.Errorf("s3: validate source: %w", err)
	}
	var extra [1]byte
	more, readErr := strictio.ReadFull(&contextio.Reader{Context: ctx, Reader: src}, extra[:])
	if err := strictio.IgnoreEOF(readErr); err != nil {
		return "", err
	}
	if n != size || more != 0 || string(sha.Sum(nil)) != string(digest[:]) {
		return "", objectstore.ErrChecksumMismatch
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("s3: rewind validated source: %w", err)
	}
	return base64.StdEncoding.EncodeToString(crc.Sum(nil)), nil
}

func (s *Store) multipart(ctx context.Context, key string, src io.ReadSeeker, size int64, digest [32]byte, crc string, state *dispatchState) (revision string, resultErr error) {
	created, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c, ChecksumType: types.ChecksumTypeFullObject})
	if err != nil {
		return "", err
	}
	id := aws.ToString(created.UploadId)
	if id == "" {
		return "", errors.New("s3: missing multipart upload ID")
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		// Cleanup shares the caller's remaining budget; no detached retries.
		_, abortErr := s.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(id)})
		resultErr = errors.Join(resultErr, abortErr)
	}()
	buffer := make([]byte, int(s.part))
	hasher := sha256.New()
	parts := make([]types.CompletedPart, 0, int((size-1)/s.part+1))
	for offset := int64(0); offset < size; {
		length := min(s.part, size-offset)
		n, err := strictio.ReadFull(&contextio.Reader{Context: ctx, Reader: src}, buffer[:length])
		if err != nil {
			return "", err
		}
		_, _ = hasher.Write(buffer[:n])
		part := int32(len(parts) + 1)
		checksum := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(buffer[:n], crc32.MakeTable(crc32.Castagnoli))))
		out, err := s3io.UploadPart(ctx, s.client, s.bucket, key, id, part, buffer[:n], aws.String(checksum))
		if err != nil {
			return "", err
		}
		if aws.ToString(out.ETag) == "" {
			return "", errors.New("s3: multipart part has no ETag")
		}
		if out.ChecksumCRC32C != nil && *out.ChecksumCRC32C != checksum {
			return "", objectstore.ErrChecksumMismatch
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(part), ETag: out.ETag, ChecksumCRC32C: aws.String(checksum)})
		offset += int64(n)
	}
	if string(hasher.Sum(nil)) != string(digest[:]) {
		return "", objectstore.ErrChecksumMismatch
	}
	// Only the final conditional request can make the object visible. Earlier
	// part errors are ordinary failures even after a part dispatch.
	var extra [1]byte
	more, readErr := strictio.ReadFull(&contextio.Reader{Context: ctx, Reader: src}, extra[:])
	if err := strictio.IgnoreEOF(readErr); err != nil {
		return "", err
	}
	if more != 0 {
		return "", objectstore.ErrChecksumMismatch
	}
	finalCtx := context.WithValue(ctx, dispatchKey{}, state)
	out, err := s.client.CompleteMultipartUpload(finalCtx, &awss3.CompleteMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(id), IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, ChecksumType: types.ChecksumTypeFullObject, ChecksumCRC32C: aws.String(crc), MpuObjectSize: aws.Int64(size)})
	if err != nil {
		return "", err
	}
	completed = true
	return aws.ToString(out.ETag), nil
}

func unknown(err error) error { return errors.Join(objectstore.ErrOutcomeUnknown, err) }
func (s *Store) verify(ctx context.Context, key string, size int64, digest [32]byte, crc string, reconcile, multipart bool, completionRevision string) (objectstore.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return objectstore.Receipt{}, unknown(err)
	}
	m, err := s.Inspect(ctx, key)
	if err != nil {
		return objectstore.Receipt{}, unknown(err)
	}
	mismatch := objectstore.ErrChecksumMismatch
	if reconcile {
		mismatch = objectstore.ErrExists
	}
	if m.Size != size {
		return objectstore.Receipt{}, mismatch
	}
	method := ""
	for _, c := range m.Checksums {
		if c.Algorithm != "sha256" || c.Scope != "full" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(c.Value)
		if err != nil || len(decoded) != sha256.Size {
			continue
		}
		if string(decoded) != string(digest[:]) {
			return objectstore.Receipt{}, mismatch
		}
		method = "provider-sha256"
	}
	if method == "" && !reconcile && !multipart && completionRevision != "" && m.Revision == completionRevision {
		// Some endpoints omit the checksum type on inspection. The upload's
		// confirmed SHA-256 still identifies the object it created.
		method = "provider-sha256"
	}
	if method == "" && !reconcile && multipart {
		// CRC32C proves transfer integrity only for the object we completed.
		// It cannot identify different bytes that replaced that object.
		if completionRevision == "" || m.Revision != completionRevision {
			return objectstore.Receipt{}, unknown(errors.New("s3: inspection cannot identify the completed multipart object"))
		}
		for _, c := range m.Checksums {
			if c.Algorithm == "crc32c" && c.Scope == "full" {
				decoded, err := base64.StdEncoding.DecodeString(c.Value)
				if err != nil || len(decoded) != 4 {
					continue
				}
				if base64.StdEncoding.EncodeToString(decoded) != crc {
					return objectstore.Receipt{}, mismatch
				}
				method = "provider-crc32c"
			}
		}
	}
	if method == "" {
		if !reconcile && !s.readback {
			return objectstore.Receipt{}, unknown(errors.New("s3: no usable full-object provider checksum; enable AllowReadback for full SHA-256 verification"))
		}
		if err := s.readbackSHA(ctx, m, size, digest); err != nil {
			if errors.Is(err, objectstore.ErrChecksumMismatch) {
				return objectstore.Receipt{}, mismatch
			}
			return objectstore.Receipt{}, unknown(err)
		}
		method = "readback-sha256"
	}
	return objectstore.Receipt{Object: m, SHA256: digest, Verification: method}, nil
}

func (s *Store) readbackSHA(ctx context.Context, m objectstore.Metadata, size int64, digest [32]byte) error {
	// Without an opaque revision there is no repeatable read contract to bind
	// inspection to readback. ETags guard reads but never establish identity.
	if m.Revision == "" {
		return errors.New("s3: endpoint supplied no revision for readback")
	}
	full, err := s.key(m.Key)
	if err != nil {
		return err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full), IfMatch: aws.String(m.Revision)})
	if err != nil {
		return classify("read back", err)
	}
	if out.ContentLength == nil || *out.ContentLength != size || aws.ToString(out.ETag) != m.Revision {
		return errors.Join(errors.New("s3: readback length or revision changed"), out.Body.Close())
	}
	hasher := sha256.New()
	n, readErr := io.CopyBuffer(hasher, io.LimitReader(&contextio.Reader{Context: ctx, Reader: out.Body}, size), make([]byte, 64<<10))
	var extra [1]byte
	more, tailErr := (&contextio.Reader{Context: ctx, Reader: out.Body}).Read(extra[:])
	closeErr := out.Body.Close()
	if err := strictio.IgnoreEOF(tailErr); err != nil {
		readErr = errors.Join(readErr, err)
	}
	if readErr != nil || closeErr != nil || n != size {
		return errors.Join(readErr, closeErr, io.ErrUnexpectedEOF)
	}
	if more != 0 || string(hasher.Sum(nil)) != string(digest[:]) {
		return objectstore.ErrChecksumMismatch
	}
	return nil
}
