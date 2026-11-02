// Package s3 stores verified immutable objects through the S3 API.
// Provider contracts require validation against each intended endpoint.
// Multipart cleanup uses the caller's remaining context. Configure the bucket's
// incomplete-multipart lifecycle rule to reclaim parts after cancellation or a
// process crash. A generic HEAD 404 cannot distinguish a missing key from a
// missing bucket; callers must validate their configured namespace separately.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"go.kenn.io/kit/internal/s3io"
	"go.kenn.io/kit/objectstore"
)

// Config binds an existing bucket namespace. Credentials must be explicit;
// construction never loads ambient configuration or probes the bucket.
type Config struct {
	Bucket, Prefix, Region, Endpoint string
	Credentials                      aws.CredentialsProvider
	ForcePathStyle                   bool
	AllowInsecureTransport           bool
	PartBytes                        int64
	PageSize                         int
	// AllowReadback permits routine upload verification by complete SHA-256
	// readback when provider checksums are absent. Conflict/uncertain outcome
	// reconciliation always reads back when full-object SHA-256 is unavailable.
	AllowReadback bool
}

// Store implements immutable byte operations without catalog or retention policy.
type Store struct {
	client         *awss3.Client
	bucket, prefix string
	part           int64
	page           int
	readback       bool
}

// New validates static configuration without contacting the endpoint.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("s3: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Bucket == "" || cfg.Credentials == nil {
		return nil, errors.New("s3: bucket and explicit credentials are required")
	}
	if err := objectstore.ValidatePrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("s3: endpoint must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
		}
		if u.Scheme == "http" && !cfg.AllowInsecureTransport {
			return nil, errors.New("s3: HTTP requires AllowInsecureTransport")
		}
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PartBytes == 0 {
		cfg.PartBytes = 8 << 20
	}
	if cfg.PartBytes < 5<<20 || cfg.PartBytes > 5<<30 || uint64(cfg.PartBytes) > uint64(^uint(0)>>1) {
		return nil, errors.New("s3: part size must fit platform int and be between 5 MiB and 5 GiB")
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = 1000
	}
	if cfg.PageSize < 1 || cfg.PageSize > 1000 {
		return nil, errors.New("s3: page size must be between 1 and 1000")
	}
	client := awss3.NewFromConfig(aws.Config{Region: cfg.Region, Credentials: cfg.Credentials, HTTPClient: &dispatchClient{client: awshttp.NewBuildableClient()}}, func(o *awss3.Options) {
		o.UsePathStyle = cfg.ForcePathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	prefix := strings.TrimSuffix(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{client: client, bucket: cfg.Bucket, prefix: prefix, part: cfg.PartBytes, page: cfg.PageSize, readback: cfg.AllowReadback}, nil
}

func (s *Store) key(key string) (string, error) {
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	api, hasAPIError := errors.AsType[smithy.APIError](err)
	switch statusCode(err) {
	case 404:
		if !hasAPIError || api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound" {
			sentinel = objectstore.ErrNotFound
		}
	case 412:
		sentinel = objectstore.ErrRevisionChanged
	}
	if hasAPIError && api.ErrorCode() == "BadDigest" {
		sentinel = objectstore.ErrChecksumMismatch
	}
	return fmt.Errorf("s3: %s: %w", op, errors.Join(sentinel, err))
}

func statusCode(err error) int {
	status, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err)
	if ok {
		return status.HTTPStatusCode()
	}
	return 0
}

// OpenRange opens exactly length bytes starting at offset, guarded by revision
// when provided. The caller closes the returned body, including on read error.
func (s *Store) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	full, err := s.key(key)
	if err != nil {
		return nil, err
	}
	body, err := s3io.OpenRange(ctx, s.client, s.bucket, full, revision, offset, length)
	return body, classify("open range", err)
}
