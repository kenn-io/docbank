package s3

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/objectstore"
)

// Inspect retrieves provider metadata, requesting stored checksum evidence.
func (s *Store) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Metadata{}, err
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return objectstore.Metadata{}, classify("inspect", err)
	}
	if out.ContentLength == nil || *out.ContentLength < 0 {
		return objectstore.Metadata{}, errors.New("s3: missing or invalid object length")
	}
	m := objectstore.Metadata{Key: key, Size: *out.ContentLength, Revision: aws.ToString(out.ETag)}
	if out.LastModified != nil {
		m.LastModified = out.LastModified.UTC()
	}
	scope := ""
	switch out.ChecksumType {
	case types.ChecksumTypeFullObject:
		scope = "full"
	case types.ChecksumTypeComposite:
		scope = "composite"
	}
	// Unknown scope is preserved conservatively as composite. It cannot prove
	// full-object identity, even when its representation resembles a digest.
	if scope == "" {
		scope = "composite"
	}
	for _, c := range []struct {
		algorithm string
		value     *string
	}{{"sha256", out.ChecksumSHA256}, {"crc32c", out.ChecksumCRC32C}} {
		if c.value != nil {
			m.Checksums = append(m.Checksums, objectstore.Checksum{Algorithm: c.algorithm, Value: *c.value, Scope: scope})
		}
	}
	return m, nil
}

// ListPage preserves all names under prefix, including names outside the key
// grammar. Such inventory entries grant no permission to delete them.
func (s *Store) ListPage(ctx context.Context, prefix, token string, size int) (objectstore.Page, error) {
	if err := objectstore.ValidatePrefix(prefix); err != nil {
		return objectstore.Page{}, err
	}
	if size == 0 {
		size = s.page
	}
	if size < 1 || size > 1000 {
		return objectstore.Page{}, errors.New("s3: page size must be between 1 and 1000")
	}
	out, err := s3io.List(ctx, s.client, s.bucket, s.prefix+prefix, token, int32(size))
	if err != nil {
		return objectstore.Page{}, classify("list", err)
	}
	page := objectstore.Page{}
	for _, o := range out.Contents {
		key := aws.ToString(o.Key)
		if !strings.HasPrefix(key, s.prefix+prefix) {
			return objectstore.Page{}, errors.New("s3: listed key lies outside requested prefix")
		}
		m := objectstore.Metadata{Key: strings.TrimPrefix(key, s.prefix), Size: aws.ToInt64(o.Size), Revision: aws.ToString(o.ETag)}
		if o.LastModified != nil {
			m.LastModified = o.LastModified.UTC()
		}
		page.Objects = append(page.Objects, m)
	}
	if aws.ToBool(out.IsTruncated) {
		page.NextToken = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

// Delete removes exactly key with an optional revision precondition. Absence
// succeeds. Endpoints must honor If-Match when a revision is supplied.
func (s *Store) Delete(ctx context.Context, key, revision string) error {
	full, err := s.key(key)
	if err != nil {
		return err
	}
	input := &awss3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full)}
	if revision != "" {
		input.IfMatch = aws.String(revision)
	}
	_, err = s.client.DeleteObject(ctx, input)
	err = classify("delete", err)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil
	}
	return err
}
