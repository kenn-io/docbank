// Package s3io shares bounded S3 byte transfers without object naming or policy.
package s3io

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.kenn.io/kit/internal/strictio"
)

// ErrRangeResponse identifies a provider response outside the requested interval.
var ErrRangeResponse = errors.New("s3: response does not match requested range")

// List retrieves one bounded page and rejects unusable continuation metadata.
func List(ctx context.Context, client *s3.Client, bucket, prefix, token string, size int32) (*s3.ListObjectsV2Output, error) {
	if size < 1 || size > 1000 {
		return nil, errors.New("s3: page size must be between 1 and 1000")
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(size), EncodingType: types.EncodingTypeUrl}
	if token != "" {
		input.ContinuationToken = aws.String(token)
	}
	output, err := client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, err
	}
	if len(output.Contents) > int(size) {
		return nil, errors.New("s3: inventory exceeds page size")
	}
	if aws.ToBool(output.IsTruncated) && (aws.ToString(output.NextContinuationToken) == "" || aws.ToString(output.NextContinuationToken) == token) {
		return nil, errors.New("s3: truncated inventory has no advancing token")
	}
	// The SDK retains URL escapes in names. Decode once, leaving tokens opaque.
	if output.EncodingType != types.EncodingTypeUrl {
		return output, nil
	}
	for i := range output.Contents {
		key, err := url.QueryUnescape(aws.ToString(output.Contents[i].Key))
		if err != nil {
			return nil, fmt.Errorf("s3: decode inventory key: %w", err)
		}
		output.Contents[i].Key = aws.String(key)
	}
	return output, nil
}

// UploadPart sends one bounded, rewindable part; callers choose checksum policy.
func UploadPart(ctx context.Context, client *s3.Client, bucket, key, id string, number int32, data []byte, crc *string) (*s3.UploadPartOutput, error) {
	return client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id), PartNumber: aws.Int32(number), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ChecksumCRC32C: crc})
}

// OpenRange enforces the requested interval and rejects truncated bodies.
func OpenRange(ctx context.Context, client *s3.Client, bucket, key, revision string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return nil, errors.New("s3: invalid range")
	}
	input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))}
	if revision != "" {
		input.IfMatch = aws.String(revision)
	}
	output, err := client.GetObject(ctx, input)
	if err != nil {
		return nil, err
	}
	var start, end, total int64
	_, parseErr := fmt.Sscanf(aws.ToString(output.ContentRange), "bytes %d-%d/%d", &start, &end, &total)
	if parseErr != nil || start != offset || end != offset+length-1 || total <= end || output.ContentLength == nil || *output.ContentLength != length {
		return nil, errors.Join(ErrRangeResponse, output.Body.Close())
	}
	if revision != "" && aws.ToString(output.ETag) != revision {
		return nil, errors.Join(errors.New("s3: range revision differs"), output.Body.Close())
	}
	return &rangeBody{ctx: ctx, body: output.Body, remaining: length}, nil
}

type rangeBody struct {
	ctx       context.Context
	body      io.ReadCloser
	remaining int64
}

func (r *rangeBody) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	return n, strictio.RangeEnd(err, r.remaining)
}
func (r *rangeBody) Close() error { return r.body.Close() }
