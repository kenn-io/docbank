package gcs

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

const testBucket = "bucket"

// fixture is an in-process fake-gcs-server plus counters for the requests the
// store sends through it.
type fixture struct {
	t      *testing.T
	server *fakestorage.Server

	mu                       sync.Mutex
	requests, uploads, reads int
	uploadStatus             int // when set, uploads fail with this status
}

func (f *fixture) put(name string, data []byte) int64 {
	f.server.CreateObject(fakestorage.Object{BucketName: testBucket, Name: name, Content: data})
	object, err := f.server.GetObject(testBucket, name)
	require.NoError(f.t, err)
	return object.Generation
}

func (f *fixture) get(name string) ([]byte, bool) {
	object, err := f.server.GetObject(testBucket, name)
	if err != nil {
		return nil, false
	}
	return object.Content, true
}

// forward counts a request and applies any injected upload failure.
func (f *fixture) forward(next http.RoundTripper, r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests++
	upload := strings.HasPrefix(r.URL.Path, "/upload/")
	if upload {
		f.uploads++
	}
	if r.URL.Query().Get("alt") == "media" {
		f.reads++
	}
	status := f.uploadStatus
	f.mu.Unlock()
	if upload && status != 0 {
		body := fmt.Sprintf(`{"error":{"code":%d,"message":"injected"}}`, status)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	return next.RoundTrip(r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newFixture returns a store on an in-process fake GCS. wrap, when set, sees
// every request and decides how to forward it.
func newFixture(t *testing.T, wrap func(next http.RoundTripper) http.RoundTripper) (*fixture, *Store) {
	t.Helper()
	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true, Writer: io.Discard})
	require.NoError(t, err)
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: testBucket})
	f := &fixture{t: t, server: server}
	base := server.HTTPClient().Transport
	var transport http.RoundTripper = roundTripFunc(func(r *http.Request) (*http.Response, error) { return f.forward(base, r) })
	if wrap != nil {
		transport = wrap(transport)
	}
	s, err := newStore(t.Context(), Config{Bucket: testBucket, Prefix: "namespace"}, option.WithHTTPClient(&http.Client{Transport: transport}))
	require.NoError(t, err)
	s.bucket = s.bucket.Retryer(storage.WithBackoff(gax.Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond}))
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return f, s
}
