package gcs

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/storage"
	"go.kenn.io/kit/internal/strictio"
	"go.kenn.io/kit/objectstore"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Config binds an existing bucket namespace and explicit OAuth credentials.
type Config struct {
	Bucket, Prefix string
	// Credentials authorize every request. Callers choose their source; the
	// store never falls back to Application Default Credentials.
	Credentials *auth.Credentials
}

// Store provides immutable byte operations through the GCS JSON API.
type Store struct {
	client *storage.Client
	bucket *storage.BucketHandle
	prefix string
}

// New validates configuration and builds a client without contacting the
// service. Close releases the client.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("gcs: explicit credentials are required")
	}
	return newStore(ctx, cfg, option.WithAuthCredentials(cfg.Credentials))
}

func newStore(ctx context.Context, cfg Config, opts ...option.ClientOption) (*Store, error) {
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\?#") {
		return nil, errors.New("gcs: bucket is required")
	}
	if err := objectstore.ValidatePrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	client, err := storage.NewClient(ctx, append(opts, storage.WithJSONReads())...)
	if err != nil {
		return nil, fmt.Errorf("gcs: create client: %w", err)
	}
	prefix := strings.TrimSuffix(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	bucket := client.Bucket(cfg.Bucket).Retryer(storage.WithMaxAttempts(3))
	return &Store{client: client, bucket: bucket, prefix: prefix}, nil
}

// Close releases the underlying client.
func (s *Store) Close() error { return s.client.Close() }

func (s *Store) key(key string) (string, error) {
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

// guard pins an object handle to revision when one is supplied.
func guard(o *storage.ObjectHandle, revision string) (*storage.ObjectHandle, int64, error) {
	if revision == "" {
		return o, 0, nil
	}
	generation, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || generation <= 0 {
		return nil, 0, fmt.Errorf("gcs: invalid revision %q", revision)
	}
	return o.If(storage.Conditions{GenerationMatch: generation}), generation, nil
}

// classify maps provider failures onto shared errors. A missing bucket stays
// distinct from a missing object.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	apiErr, isAPI := errors.AsType[*googleapi.Error](err)
	switch {
	case errors.Is(err, storage.ErrBucketNotExist):
	case errors.Is(err, storage.ErrObjectNotExist):
		err = errors.Join(objectstore.ErrNotFound, err)
	case isAPI && apiErr.Code == http.StatusPreconditionFailed:
		err = errors.Join(objectstore.ErrRevisionChanged, err)
	}
	return fmt.Errorf("gcs %s: %w", op, err)
}

func (s *Store) metadata(a *storage.ObjectAttrs) objectstore.Metadata {
	m := objectstore.Metadata{
		Key:          strings.TrimPrefix(a.Name, s.prefix),
		Size:         a.Size,
		Revision:     strconv.FormatInt(a.Generation, 10),
		LastModified: a.Updated.UTC(),
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], a.CRC32C)
	m.Checksums = append(m.Checksums, objectstore.Checksum{Algorithm: "crc32c", Value: base64.StdEncoding.EncodeToString(crc[:]), Scope: "full"})
	if len(a.MD5) > 0 {
		m.Checksums = append(m.Checksums, objectstore.Checksum{Algorithm: "md5", Value: base64.StdEncoding.EncodeToString(a.MD5), Scope: "full"})
	}
	return m
}

// Inspect observes the current object's length, generation, and provider checksums.
func (s *Store) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Metadata{}, err
	}
	attrs, err := s.bucket.Object(full).Attrs(ctx)
	if err != nil {
		_, err = s.confirmMissing(ctx, full, err)
		return objectstore.Metadata{}, classify("inspect", err)
	}
	return s.metadata(attrs), nil
}

// OpenRange returns exactly length bytes at offset from the current generation,
// guarded by revision when supplied. The caller must close the returned body.
func (s *Store) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	full, err := s.key(key)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return nil, errors.New("gcs: invalid range")
	}
	object, generation, err := guard(s.bucket.Object(full), revision)
	if err != nil {
		return nil, err
	}
	reader, err := object.NewRangeReader(ctx, offset, length)
	if err != nil {
		_, err = s.confirmMissing(ctx, full, err)
		return nil, classify("open range", err)
	}
	if generation != 0 && reader.Attrs.Generation != generation {
		return nil, errors.Join(objectstore.ErrRevisionChanged, reader.Close())
	}
	// Decompressive transcoding serves different bytes than the stored object.
	if reader.Attrs.Decompressed || reader.Attrs.StartOffset != offset || reader.Remain() != length {
		return nil, errors.Join(errors.New("gcs: response does not match requested range"), reader.Close())
	}
	return &rangeBody{ctx: ctx, body: reader, remaining: length}, nil
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

// Delete removes the current object only if revision matches when supplied.
// An already absent object succeeds. Listing grants no authority to delete.
func (s *Store) Delete(ctx context.Context, key, revision string) error {
	full, err := s.key(key)
	if err != nil {
		return err
	}
	object, _, err := guard(s.bucket.Object(full), revision)
	if err != nil {
		return err
	}
	absent, err := s.confirmMissing(ctx, full, object.Delete(ctx))
	if absent {
		return nil
	}
	return classify("delete", err)
}

// confirmMissing checks an object-not-found error against its bucket. A JSON
// API 404 does not say whether the object or the bucket is missing, but a
// one-item listing reports a missing bucket distinctly. absent reports that
// the listing succeeded, so only the object is missing. Otherwise the listing
// failure, such as a missing bucket or cancellation, is returned without the
// unconfirmed object-not-found error.
func (s *Store) confirmMissing(ctx context.Context, full string, err error) (absent bool, _ error) {
	if !errors.Is(err, storage.ErrObjectNotExist) {
		return false, err
	}
	var page []*storage.ObjectAttrs
	_, listErr := iterator.NewPager(s.bucket.Objects(ctx, &storage.Query{Prefix: full}), 1, "").NextPage(&page)
	if listErr != nil {
		return false, fmt.Errorf("confirm missing object: %w", listErr)
	}
	return true, err
}
