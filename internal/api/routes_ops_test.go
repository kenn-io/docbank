package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestIngestEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))

	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "a.txt")}, "dest": "/inbox"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var rep api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Added)

	// Relative paths are rejected: the daemon's cwd is meaningless.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{"relative/x.txt"}, "dest": "/inbox"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	// A missing source is a per-path failure, not a request failure.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "gone.txt")}, "dest": "/inbox"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Len(t, rep.Failed, 1)
}

func TestIngestRejectsNonLoopback(t *testing.T) {
	// httptest.NewRequest-style direct handler invocation with a non-loopback
	// RemoteAddr proves the middleware fence without real remote networking.
	// Deps are built directly (not via newTestServer) to mirror its internals:
	// a real store and blob dir under a temp directory.
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs := blob.New(blobsDir)

	srv := api.NewServer(api.Deps{Store: s, Blobs: blobs, Cfg: config.Default()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"paths":["/x"],"dest":"/inbox"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:4444"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"loopback_only"`)
}

func TestTrashListAndEmpty(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/old.txt", "x")
	_, etag := etagOf(t, ts, f.ID)
	resp, _ := do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := get(t, ts, "/api/v1/trash", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "old.txt")

	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Deleted int64 `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, int64(1), out.Deleted)

	// Bad age string → 422.
	resp, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": "-3d"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestGCDryRunAndRun(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/g.txt", "gc-me")
	_, etag := etagOf(t, ts, f.ID)
	_, _ = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	_, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil, map[string]any{})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.CandidateBlobs)
	assert.False(t, rep.Run)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Removed)
}

func TestVerifyEndpoint(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/ok.txt", "fine")
	resp, body := do(t, ts, http.MethodPost, "/api/v1/verify", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.OK)
	assert.Empty(t, rep.Problems)
}

func TestMaintenanceGateQueuesMutations(t *testing.T) {
	ts, s := newTestServer(t, nil)
	// Saturate: fire a gc run and a burst of mkdirs concurrently. Every
	// request must succeed — the gate queues, it never rejects — and the
	// store must end up consistent (all dirs present).
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 10 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, body, err := try(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
			if err != nil {
				errs <- fmt.Errorf("gc: transport: %w", err)
			} else if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("gc: %d %s", resp.StatusCode, body)
			}
		}()
		go func() {
			defer wg.Done()
			resp, body, err := try(t, ts, http.MethodPost, "/api/v1/nodes", nil,
				map[string]any{"parent_id": s.RootID(), "name": fmt.Sprintf("dir-%d", i), "kind": "dir"})
			if err != nil {
				errs <- fmt.Errorf("mkdir: transport: %w", err)
			} else if resp.StatusCode != http.StatusCreated {
				errs <- fmt.Errorf("mkdir: %d %s", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	kids, err := s.Children(t.Context(), s.RootID())
	require.NoError(t, err)
	assert.Len(t, kids, 10)
}
