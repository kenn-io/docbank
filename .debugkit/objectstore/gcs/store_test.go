package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

func publish(t *testing.T, s *Store, key string, data []byte) (objectstore.Receipt, error) {
	t.Helper()
	return s.Publish(t.Context(), key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(t.Context(), Config{Bucket: "bucket"})
	require.Error(t, err)
}

func TestPublishNewObject(t *testing.T) {
	f, s := newFixture(t, nil)
	receipt, err := publish(t, s, "object", []byte("123456789"))
	require.NoError(t, err)
	assert.Equal(t, "provider-crc32c", receipt.Verification)
	assert.Equal(t, "object", receipt.Object.Key)
	stored, err := f.server.GetObject(testBucket, "namespace/object")
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(stored.Generation, 10), receipt.Object.Revision)
	assert.Equal(t, int64(9), receipt.Object.Size)
	// The standard CRC32C check value for "123456789" is 0xe3069283.
	assert.Contains(t, receipt.Object.Checksums, objectstore.Checksum{Algorithm: "crc32c", Value: "4waSgw==", Scope: "full"})
	assert.Equal(t, "123456789", string(stored.Content))
	assert.Zero(t, f.reads)
}

func TestPublishEmptyObject(t *testing.T) {
	f, s := newFixture(t, nil)
	receipt, err := publish(t, s, "empty", nil)
	require.NoError(t, err)
	assert.Zero(t, receipt.Object.Size)
	data, ok := f.get("namespace/empty")
	assert.True(t, ok)
	assert.Empty(t, data)
}

func TestPublishRejectsInvalidSourceBeforeRequest(t *testing.T) {
	f, s := newFixture(t, nil)
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader([]byte("bad")), 3, sha256.Sum256([]byte("good")))
	require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	receipt, err = s.Publish(t.Context(), "object", nil, 0, sha256.Sum256(nil))
	require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	assert.Zero(t, f.requests)
}

func TestPublishIdenticalRetryVerifiesByReadback(t *testing.T) {
	f, s := newFixture(t, nil)
	data := []byte("immutable bytes")
	first, err := publish(t, s, "object", data)
	require.NoError(t, err)
	again, err := publish(t, s, "object", data)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", again.Verification)
	assert.Equal(t, first.Object.Revision, again.Object.Revision)
	assert.Equal(t, 1, f.reads)
}

func TestPublishConflictKeepsExistingObject(t *testing.T) {
	for name, existing := range map[string]string{"same size": "different", "other size": "longer existing"} {
		t.Run(name, func(t *testing.T) {
			f, s := newFixture(t, nil)
			f.put("namespace/object", []byte(existing))
			receipt, err := publish(t, s, "object", []byte("published"))
			require.ErrorIs(t, err, objectstore.ErrExists)
			require.NotErrorIs(t, err, objectstore.ErrRevisionChanged)
			require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
			assert.Zero(t, receipt)
			stored, _ := f.get("namespace/object")
			assert.Equal(t, existing, string(stored))
		})
	}
}

// dropFirstUploadReply forwards the first upload, then discards its reply.
func dropFirstUploadReply(dropped *bool) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := next.RoundTrip(r)
			if err == nil && !*dropped && strings.HasPrefix(r.URL.Path, "/upload/") {
				*dropped = true
				_ = response.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
			return response, err
		})
	}
}

func TestPublishLostReplyReconcilesStoredBytes(t *testing.T) {
	dropped := false
	f, s := newFixture(t, dropFirstUploadReply(&dropped))
	data := []byte("stored before the reply was lost")
	receipt, err := publish(t, s, "object", data)
	require.NoError(t, err)
	assert.True(t, dropped)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	stored, ok := f.get("namespace/object")
	assert.True(t, ok)
	assert.Equal(t, data, stored)
}

func TestPublishCanceledAfterUploadIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := next.RoundTrip(r)
			if err == nil && strings.HasPrefix(r.URL.Path, "/upload/") {
				_ = response.Body.Close()
				cancel()
				return nil, context.Canceled
			}
			return response, err
		})
	})
	data := []byte("stored after the caller gave up")
	receipt, err := s.Publish(ctx, "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, receipt)
	stored, _ := f.get("namespace/object")
	assert.Equal(t, data, stored)
}

func TestPublishFailedTransportIsUnknown(t *testing.T) {
	f, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// The client does not retry this error, and the request may
			// still have reached the service.
			if strings.HasPrefix(r.URL.Path, "/upload/") {
				return nil, errors.New("connection lost")
			}
			return next.RoundTrip(r)
		})
	})
	receipt, err := publish(t, s, "object", []byte("maybe sent"))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	assert.Zero(t, receipt)
	_, ok := f.get("namespace/object")
	assert.False(t, ok)
}

func TestPublishRejectionIsDefinite(t *testing.T) {
	f, s := newFixture(t, nil)
	f.uploadStatus = http.StatusForbidden
	receipt, err := publish(t, s, "object", []byte("denied"))
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	assert.Equal(t, 1, f.uploads)
}

func TestSourceChangedDuringUploadPublishesNothing(t *testing.T) {
	data := []byte("validated bytes")
	for name, changed := range map[string][]byte{
		"shrinks": data[:4],
		"grows":   append(bytes.Clone(data), "extra"...),
	} {
		t.Run(name, func(t *testing.T) {
			f, s := newFixture(t, nil)
			src := &changeAfterValidation{Reader: bytes.NewReader(data), changed: changed}
			receipt, err := s.Publish(t.Context(), "object", src, int64(len(data)), sha256.Sum256(data))
			require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
			require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
			assert.Zero(t, receipt)
			_, ok := f.get("namespace/object")
			assert.False(t, ok)
		})
	}
}

// changeAfterValidation serves its original bytes until validation has
// rewound twice, then serves changed instead.
type changeAfterValidation struct {
	*bytes.Reader
	changed []byte
	rewinds int
}

func (s *changeAfterValidation) Seek(offset int64, whence int) (int64, error) {
	s.rewinds++
	if s.rewinds == 2 {
		s.Reader = bytes.NewReader(s.changed)
	}
	return s.Reader.Seek(offset, whence)
}

func TestOpenRange(t *testing.T) {
	f, s := newFixture(t, nil)
	generation := f.put("namespace/object", []byte("0123456789"))

	body, err := s.OpenRange(t.Context(), "object", strconv.FormatInt(generation, 10), 2, 3)
	require.NoError(t, err)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Equal(t, "234", string(got))

	_, err = s.OpenRange(t.Context(), "object", strconv.FormatInt(generation+1, 10), 0, 1)
	require.ErrorIs(t, err, objectstore.ErrRevisionChanged)
	_, err = s.OpenRange(t.Context(), "object", "", 8, 5)
	require.Error(t, err, "a range past the end must not return a short body")
	_, err = s.OpenRange(t.Context(), "object", "not-a-generation", 0, 1)
	require.Error(t, err)
	_, err = s.OpenRange(t.Context(), "missing", "", 0, 1)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	_, err = s.OpenRange(t.Context(), "object", "", 0, 0)
	require.Error(t, err)
}

func TestInspect(t *testing.T) {
	f, s := newFixture(t, nil)
	generation := f.put("namespace/object", []byte("123456789"))
	m, err := s.Inspect(t.Context(), "object")
	require.NoError(t, err)
	assert.Equal(t, "object", m.Key)
	assert.Equal(t, int64(9), m.Size)
	assert.Equal(t, strconv.FormatInt(generation, 10), m.Revision)
	assert.Equal(t, time.UTC, m.LastModified.Location())
	assert.Contains(t, m.Checksums, objectstore.Checksum{Algorithm: "crc32c", Value: "4waSgw==", Scope: "full"})
	_, err = s.Inspect(t.Context(), "missing")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
}

func TestDelete(t *testing.T) {
	var f *fixture
	var preconditions []string
	// fake-gcs-server ignores ifGenerationMatch on deletes; apply it here as
	// GCS does, and record what the store sent.
	f, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if want := r.URL.Query().Get("ifGenerationMatch"); r.Method == http.MethodDelete && want != "" {
				preconditions = append(preconditions, want)
				stored, err := f.server.GetObject(testBucket, "namespace/object")
				if err == nil && strconv.FormatInt(stored.Generation, 10) != want {
					return &http.Response{StatusCode: http.StatusPreconditionFailed, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":412,"message":"precondition"}}`)), Request: r}, nil
				}
			}
			return next.RoundTrip(r)
		})
	})
	generation := f.put("namespace/object", []byte("x"))
	require.ErrorIs(t, s.Delete(t.Context(), "object", strconv.FormatInt(generation+1, 10)), objectstore.ErrRevisionChanged)
	_, ok := f.get("namespace/object")
	assert.True(t, ok)
	require.NoError(t, s.Delete(t.Context(), "object", strconv.FormatInt(generation, 10)))
	_, ok = f.get("namespace/object")
	assert.False(t, ok)
	assert.Equal(t, []string{strconv.FormatInt(generation+1, 10), strconv.FormatInt(generation, 10)}, preconditions)
	require.NoError(t, s.Delete(t.Context(), "object", ""))
}

func TestMissingBucketIsNotMissingObject(t *testing.T) {
	_, s := newFixture(t, nil)
	s.bucket = s.client.Bucket("missing").Retryer(storage.WithMaxAttempts(1))
	err := s.Delete(t.Context(), "object", "")
	require.ErrorIs(t, err, storage.ErrBucketNotExist)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	_, err = s.ListPage(t.Context(), "", "", 1)
	require.ErrorIs(t, err, storage.ErrBucketNotExist)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	_, err = s.Inspect(t.Context(), "object")
	require.ErrorIs(t, err, storage.ErrBucketNotExist)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	_, err = s.OpenRange(t.Context(), "object", "", 0, 1)
	require.ErrorIs(t, err, storage.ErrBucketNotExist)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	receipt, err := publish(t, s, "object", []byte("no bucket"))
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
}

func TestRejectedUploadWithVanishingObjectStaysUnknown(t *testing.T) {
	var f *fixture
	// The object exists when inspected after the rejection, then disappears
	// before readback, so the store cannot tell whether this upload stored it.
	f, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Get("alt") == "media" {
				require.NoError(t, f.server.Backend().DeleteObject(testBucket, "namespace/object"))
			}
			return next.RoundTrip(r)
		})
	})
	f.put("namespace/object", []byte("same bytes"))
	f.uploadStatus = http.StatusForbidden
	receipt, err := publish(t, s, "object", []byte("same bytes"))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
}

func TestListPagePreservesForeignEntriesAcrossPages(t *testing.T) {
	f, s := newFixture(t, nil)
	for _, name := range []string{"namespace/a", "namespace/b//foreign", "namespace/c +%", "other/a"} {
		f.put(name, []byte(name))
	}
	var keys []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5, "listing must terminate")
		page, err := s.ListPage(t.Context(), "", token, 2)
		require.NoError(t, err)
		for _, m := range page.Objects {
			keys = append(keys, m.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	assert.Equal(t, []string{"a", "b//foreign", "c +%"}, keys)
	_, err := s.ListPage(t.Context(), "", "", 1001)
	require.Error(t, err)
}

// resumableSize exceeds the client's 16 MiB chunk, so the upload uses a
// resumable session with more than one chunk.
const resumableSize = (16 << 20) + 19

func TestPublishResumableLostFinalReply(t *testing.T) {
	dropped := false
	f, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := next.RoundTrip(r)
			final := strings.HasSuffix(r.Header.Get("Content-Range"), "/"+strconv.Itoa(resumableSize))
			if err == nil && !dropped && final && response.StatusCode/100 == 2 {
				dropped = true
				_ = response.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
			return response, err
		})
	})
	data := bytes.Repeat([]byte("l"), resumableSize)
	receipt, err := publish(t, s, "large", data)
	require.NoError(t, err)
	assert.True(t, dropped)
	assert.Equal(t, sha256.Sum256(data), receipt.SHA256)
	stored, ok := f.get("namespace/large")
	require.True(t, ok)
	assert.True(t, bytes.Equal(data, stored))
}

func TestUnconfirmedMissingObjectKeepsListingFailure(t *testing.T) {
	errListing := errors.New("listing failed")
	_, s := newFixture(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/b/"+testBucket+"/o") {
				return nil, errListing
			}
			return next.RoundTrip(r)
		})
	})
	require.ErrorIs(t, s.Delete(t.Context(), "absent", ""), errListing)
	_, err := s.Inspect(t.Context(), "absent")
	require.ErrorIs(t, err, errListing)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	_, err = s.OpenRange(t.Context(), "absent", "", 0, 1)
	require.ErrorIs(t, err, errListing)
}

// failingEnd reports err during one pass: joined to io.EOF with the final
// bytes or on the read after them, or with extra set, together with one more
// byte after them. Validation rewinds the source twice, so the upload pass
// reads after the second rewind.
type failingEnd struct {
	*bytes.Reader
	err      error
	upload   bool
	withData bool
	extra    bool
	rewinds  int
}

func (s *failingEnd) Seek(offset int64, whence int) (int64, error) {
	s.rewinds++
	return s.Reader.Seek(offset, whence)
}

func (s *failingEnd) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	if s.upload != (s.rewinds >= 2) {
		return n, err
	}
	switch {
	case s.withData && n > 0 && s.Len() == 0:
		return n, errors.Join(io.EOF, s.err)
	case !s.withData && s.extra && n == 0 && err != nil && len(p) > 0:
		p[0] = 'x'
		return 1, s.err
	case !s.withData && n == 0 && err != nil:
		return 0, errors.Join(io.EOF, s.err)
	}
	return n, err
}

func TestPublishRejectsSourceErrorAtEnd(t *testing.T) {
	for name, tt := range map[string]struct{ upload, withData, extra bool }{
		"validation, with final bytes":  {withData: true},
		"validation, after final bytes": {},
		"validation, with extra byte":   {extra: true},
		"upload, with final bytes":      {upload: true, withData: true},
		"upload, after final bytes":     {upload: true},
		"upload, with extra byte":       {upload: true, extra: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, s := newFixture(t, nil)
			errSource := errors.New("source failed")
			data := []byte("complete bytes")
			src := &failingEnd{Reader: bytes.NewReader(data), err: errSource, upload: tt.upload, withData: tt.withData, extra: tt.extra}
			receipt, err := s.Publish(t.Context(), "object", src, int64(len(data)), sha256.Sum256(data))
			require.ErrorIs(t, err, errSource)
			require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
			assert.Zero(t, receipt)
			_, ok := f.get("namespace/object")
			assert.False(t, ok)
		})
	}
}
