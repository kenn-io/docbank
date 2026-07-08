package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// testStore bundles the store and blob store the test server was built
// with, so fixtures can write blobs and nodes through the exact instances
// the server reads from.
type testStore struct {
	*store.Store

	Blobs *blob.Store
}

// newTestServer builds a real store and blob dir in a temp dir and serves
// the API over httptest (loopback client addr). Later route tasks reuse
// this helper.
func newTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *testStore) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs := blob.New(blobsDir)
	d := api.Deps{Store: s, Blobs: blobs, Cfg: config.Default()}
	if mutate != nil {
		mutate(&d)
	}
	ts := httptest.NewServer(api.NewServer(d).Handler())
	t.Cleanup(ts.Close)
	return ts, &testStore{Store: s, Blobs: blobs}
}

// testHash returns a sha256 hex digest of seed. The store only validates
// blob hash shape, so any distinct, correctly-shaped string works as a
// stand-in for a real blob's content hash.
func testHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// createFileWithContent writes content through the server's blob store and
// links it into the tree at the given root-level path, returning the
// resulting node. ts is accepted (rather than just s) so every fixture that
// touches the running server shares one calling convention, even though
// this one only needs the store and blob store.
//
//nolint:unparam // see comment above; ts is part of the shared fixture signature.
func createFileWithContent(t *testing.T, ts *httptest.Server, s *testStore, path, content string) store.Node {
	t.Helper()
	hash, size, err := s.Blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	name := strings.TrimPrefix(path, "/")
	n, err := s.CreateFile(t.Context(), s.RootID(), name, hash, size, "text/plain")
	require.NoError(t, err)
	return n
}

func get(t *testing.T, ts *httptest.Server, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, string(body)
}

// do issues a request with an optional JSON body, marshaling non-nil bodies
// and setting Content-Type accordingly. Mutation route tests share this.
func do(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string, body any) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, string(respBody)
}

// etagOf stats a node and returns it along with the ETag header (a
// quoted revision), the value mutation endpoints expect in If-Match. The
// node itself is part of the shared fixture signature for callers that
// need it, even though today's callers only use the ETag.
//
//nolint:unparam // see comment above.
func etagOf(t *testing.T, ts *httptest.Server, id int64) (api.Node, string) {
	t.Helper()
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", id), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	return n, resp.Header.Get("ETag")
}

func TestHealthAndPing(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/health", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `"status":"ok"`)
	resp, body = get(t, ts, "/api/ping", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `"docbank"`)
}

func TestAuthRequiredWhenKeySet(t *testing.T) {
	mutate := func(d *api.Deps) { d.Cfg.Server.APIKey = "sekrit" }
	ts, _ := newTestServer(t, mutate)

	resp, body := get(t, ts, "/api/v1/nodes/1", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, `"code":"unauthorized"`)

	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"X-Api-Key": "sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"Authorization": "Bearer sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)

	// Exempt surfaces work keyless.
	for _, p := range []string{"/health", "/api/ping", "/docs", "/openapi.json", "/"} {
		resp, _ := get(t, ts, p, nil)
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, p)
	}
}

func TestKeylessAllowsAll(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := get(t, ts, "/api/v1/nodes/1", nil)
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWebPlaceholder(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, body, "/docs")

	off := func(d *api.Deps) { d.Cfg.Web.Enabled = false }
	ts2, _ := newTestServer(t, off)
	resp, _ = get(t, ts2, "/", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestShutdownRoute(t *testing.T) {
	called := make(chan struct{}, 1)
	mutate := func(d *api.Deps) {
		d.ShutdownToken = "tok"
		d.Shutdown = func() { called <- struct{}{} }
	}
	ts, _ := newTestServer(t, mutate)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode) // no token

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Docbank-Daemon-Token", "tok")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	<-called
}

func TestValidationErrorEnvelope(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	// Bad path param type → huma validation error → our envelope.
	resp, body := get(t, ts, "/api/v1/nodes/not-a-number", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`, body)
}
