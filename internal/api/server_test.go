package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// newTestServer builds a real store in a temp dir and serves the API over
// httptest (loopback client addr). Later route tasks reuse this helper.
//
//nolint:unparam // the *store.Store result is consumed by later route tasks' tests.
func newTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	d := api.Deps{Store: s, Blobs: blob.New(filepath.Join(dir, "blobs")), Cfg: config.Default()}
	if mutate != nil {
		mutate(&d)
	}
	ts := httptest.NewServer(api.NewServer(d).Handler())
	t.Cleanup(ts.Close)
	return ts, s
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
