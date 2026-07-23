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
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/store"
)

// testStore bundles the store and blob store the test server was built
// with, so fixtures can write blobs and nodes through the exact instances
// the server reads from.
type testStore struct {
	*store.Store

	Blobs    *blob.Store
	BlobsDir string
	DBPath   string
}

// testAPIKey is the default key newTestServer configures: production always
// has an effective key (configured or ephemeral; see cmd/docbank/
// daemon.go and NewServer's refusal of an empty one), so tests must supply
// one too. mutate can override it (e.g. TestAuthRequiredWhenKeySet uses its
// own key to prove the value itself is checked, not just its presence).
const testAPIKey = "test-api-key"
const testWebURL = "http://docbank-0123456789abcdef0123456789abcdef.localhost:43210/"

// newTestServer builds a real store and blob dir in a temp dir and serves
// the API over httptest (loopback client addr). Later route tasks reuse
// this helper. The returned server's http.Client transport auto-attaches
// the configured key to every request that doesn't already carry an
// explicit X-Api-Key header, so the ~30 non-auth-focused tests using get/
// do/try need no per-call header plumbing; tests exercising the missing-
// or wrong-key path set X-Api-Key explicitly (even to "") to opt out.
func newTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *testStore) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "docbank.db")
	s, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	d := api.Deps{
		Store: s, Blobs: blobs, VaultRoot: dir, Cfg: config.Default(),
		WebURL: testWebURL,
	}
	d.Cfg.Server.APIKey = testAPIKey
	if mutate != nil {
		mutate(&d)
	}
	ts := httptest.NewServer(api.NewServer(d).Handler())
	t.Cleanup(ts.Close)
	ts.Client().Transport = &apiKeyTransport{key: d.Cfg.Server.APIKey, next: ts.Client().Transport}
	return ts, &testStore{Store: s, Blobs: blobs, BlobsDir: blobsDir, DBPath: dbPath}
}

// apiKeyTransport injects key as X-Api-Key on any request that doesn't
// already carry the header explicitly (present-with-empty-value counts as
// explicit, so callers can opt out by setting it to "").
type apiKeyTransport struct {
	key  string
	next http.RoundTripper
}

func (a *apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, present := req.Header["X-Api-Key"]; !present && a.key != "" {
		req = req.Clone(req.Context())
		req.Header.Set("X-Api-Key", a.key)
	}
	next := a.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
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

// try is do's goroutine-safe sibling: transport errors come back as the
// third return value instead of failing the test via require, which panics
// if called off the main test goroutine. Concurrency tests that fire
// requests from goroutines must use this instead of do.
func try(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string, body any) (*http.Response, string, error) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		return nil, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		return nil, "", err
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	_ = resp.Body.Close()
	return resp, string(respBody), nil
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

func TestDaemonOwnershipChallengeDoesNotRequireOrRevealCredentials(t *testing.T) {
	const token = "per-run-shutdown-secret"
	ts, _ := newTestServer(t, func(d *api.Deps) { d.ShutdownToken = token })
	nonce := bytes.Repeat([]byte{0x42}, daemonauth.NonceBytes)
	path := daemonauth.ChallengePath + "?nonce=" + hex.EncodeToString(nonce)

	resp, body := get(t, ts, path, map[string]string{"X-Api-Key": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result struct {
		Proof string `json:"proof"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	assert.True(t, daemonauth.Verify(token, nonce, result.Proof))
	assert.NotContains(t, body, token)

	resp, _ = get(t, ts, daemonauth.ChallengePath+"?nonce=short",
		map[string]string{"X-Api-Key": ""})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAuthRequiredWhenKeySet(t *testing.T) {
	mutate := func(d *api.Deps) { d.Cfg.Server.APIKey = "sekrit" }
	ts, _ := newTestServer(t, mutate)

	// hdr sets X-Api-Key to "" explicitly, opting out of the transport's
	// auto-injected key, so this genuinely exercises the no-key request.
	noKey := map[string]string{"X-Api-Key": ""}
	resp, body := get(t, ts, "/api/v1/nodes/1", noKey)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, `"code":"unauthorized"`)

	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"X-Api-Key": "sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"Authorization": "Bearer sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)

	// A wrong key is still unauthorized, not silently accepted.
	resp, body = get(t, ts, "/api/v1/nodes/1", map[string]string{"X-Api-Key": "wrong"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, `"code":"unauthorized"`)

	// A mutating route requires the key too, not just reads.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/nodes", noKey,
		map[string]any{"parent_id": 1, "name": "x", "kind": "dir"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, `"code":"unauthorized"`)

	// Exempt surfaces work keyless.
	for _, p := range []string{"/health", "/api/ping", "/docs", "/openapi.json", "/"} {
		resp, _ := get(t, ts, p, noKey)
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, p)
	}
}

// TestKeylessConfigStillRequiresAuth is the regression test for the
// keyless-loopback finding: NewServer must never let an empty-configured
// key fall back to unauthenticated access. newTestServer's default already
// configures a non-empty key (mirroring production, which always computes
// one — see cmd/docbank/daemon.go); this test proves a request without
// that key is refused rather than silently allowed through.
func TestKeylessConfigStillRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := get(t, ts, "/api/v1/nodes/1", map[string]string{"X-Api-Key": ""})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestNewServerRefusesEmptyKey pins the defense-in-depth invariant behind
// TestKeylessConfigStillRequiresAuth: even if some future caller reproduced
// the old bug and built Deps with an empty key, NewServer itself refuses to
// construct rather than silently falling back to unauthenticated.
func TestNewServerRefusesEmptyKey(t *testing.T) {
	assert.Panics(t, func() {
		api.NewServer(api.Deps{Cfg: config.Default()})
	})
}

func TestWebApplication(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "connect-src 'self'")
	assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	assert.Contains(t, strings.ToLower(body), "<!doctype html>")

	off := func(d *api.Deps) { d.Cfg.Web.Enabled = false }
	ts2, _ := newTestServer(t, off)
	resp, _ = get(t, ts2, "/", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestWebSessionIsReadOnlyRevocableAndDaemonLocal(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var issued struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&issued))
	require.NoError(t, resp.Body.Close())
	require.NotEmpty(t, issued.Token)
	assert.Equal(t, testWebURL, issued.URL)
	assert.NotEqual(t, testAPIKey, issued.Token)

	webRequest := func(method, path string) *http.Response {
		t.Helper()
		request, requestErr := http.NewRequest(method, ts.URL+path, nil)
		require.NoError(t, requestErr)
		request.Header["X-Api-Key"] = []string{""}
		request.Header.Set(api.WebSessionHeader, issued.Token)
		response, requestErr := ts.Client().Do(request)
		require.NoError(t, requestErr)
		return response
	}

	resp = webRequest(http.MethodGet, "/api/v1/path?path=/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = webRequest(http.MethodGet, "/api/v1/info")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = webRequest(http.MethodPost, "/api/v1/nodes")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	other, _ := newTestServer(t, nil)
	req, err = http.NewRequest(http.MethodGet, other.URL+"/api/v1/path?path=/", nil)
	require.NoError(t, err)
	req.Header["X-Api-Key"] = []string{""}
	req.Header.Set(api.WebSessionHeader, issued.Token)
	resp, err = other.Client().Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = webRequest(http.MethodDelete, "/api/daemon/web-session")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = webRequest(http.MethodGet, "/api/v1/path?path=/")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}

func TestWebSessionRequiresEnabledCompiledApplication(t *testing.T) {
	for _, mutate := range []func(*api.Deps){
		func(d *api.Deps) { d.Cfg.Web.Enabled = false },
		func(d *api.Deps) { d.WebURL = "" },
	} {
		ts, _ := newTestServer(t, mutate)
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/web-session", nil)
		require.NoError(t, err)
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
}

// TestShutdownRoute proves the shutdown route now requires both the API
// key (it isn't in authExempt, so authMiddleware wraps it like every other
// route) and its own token: neither alone is enough.
func TestShutdownRoute(t *testing.T) {
	called := make(chan struct{}, 1)
	mutate := func(d *api.Deps) {
		d.ShutdownToken = "tok"
		d.Shutdown = func() { called <- struct{}{} }
	}
	ts, _ := newTestServer(t, mutate)

	// No key, no token.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Api-Key", "")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Correct key, but no token: authMiddleware passes it through, the
	// shutdown handler itself rejects it.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Api-Key", testAPIKey)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Correct token, but no key: authMiddleware rejects it before the
	// shutdown handler ever sees the token.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Api-Key", "")
	req.Header.Set("X-Docbank-Daemon-Token", "tok")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Both correct: succeeds.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Api-Key", testAPIKey)
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
