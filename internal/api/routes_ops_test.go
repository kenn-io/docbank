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
	"go.kenn.io/kit/packstore"

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

	// A relative dest is rejected, not silently rooted at /.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "a.txt")}, "dest": "inbox"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	// An explicit empty dest gets the same /inbox default as an absent one
	// (the schema default only covers absence) — never the vault root.
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.txt"), []byte("empty dest"), 0o600))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "b.txt")}, "dest": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Added)
	resp, body = do(t, ts, http.MethodGet, "/api/v1/path?path=/inbox/b.txt", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
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
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })

	cfg := config.Default()
	cfg.Server.APIKey = "test-key"
	srv := api.NewServer(api.Deps{Store: s, Blobs: blobs, Cfg: cfg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"paths":["/x"],"dest":"/inbox"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "test-key")
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
	var out api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, int64(1), out.CandidateRoots)
	assert.Zero(t, out.Deleted)
	assert.False(t, out.Run)

	// Dry run leaves the node restorable; run performs the deletion.
	resp, body = get(t, ts, "/api/v1/trash", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "old.txt")
	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": "", "run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, int64(1), out.Deleted)
	assert.True(t, out.Run)

	// Bad age string → 422.
	resp, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": "-3d"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestStorageStatusReportsLooseAndPackedUsage(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/packed.txt", "packed storage status")

	resp, body := get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	assert.Equal(t, 1, status.LooseBlobs)
	assert.Equal(t, int64(len("packed storage status")), status.LooseBytes)
	assert.Zero(t, status.Packs)

	packed, err := s.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	resp, body = get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	assert.Zero(t, status.LooseBlobs)
	assert.Equal(t, 1, status.Packs)
	assert.Equal(t, int64(1), status.PackedBlobs)
	assert.Equal(t, int64(len("packed storage status")), status.PackedRawBytes)
	assert.Positive(t, status.PackedStoredBytes)
	assert.Equal(t, status.PackedStoredBytes, status.PackStoredBytes)
	assert.Zero(t, status.DeadPackedBytes)
}

func TestStoragePackHonorsBudgetAndConverges(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/one.txt", "one")
	createFileWithContent(t, ts, s, "/two.txt", "two")

	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/pack", nil,
		map[string]any{"max_bytes": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.StoragePackReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.BlobsPacked)
	assert.Equal(t, 1, report.PacksSealed)
	assert.True(t, report.BudgetExhausted)

	statusResp, statusBody := get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, statusResp.StatusCode, statusBody)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(statusBody), &status))
	assert.Equal(t, 1, status.LooseBlobs)
	assert.Equal(t, int64(1), status.PackedBlobs)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/pack", nil,
		map[string]any{"max_bytes": 0})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.BlobsPacked)
	assert.False(t, report.BudgetExhausted)

	statusResp, statusBody = get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, statusResp.StatusCode, statusBody)
	require.NoError(t, json.Unmarshal([]byte(statusBody), &status))
	assert.Zero(t, status.LooseBlobs)
	assert.Equal(t, int64(2), status.PackedBlobs)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/pack", nil,
		map[string]any{"max_bytes": -1})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestStorageRepackReclaimsSparsePack(t *testing.T) {
	ts, s := newTestServer(t, nil)
	files := []store.Node{
		createFileWithContent(t, ts, s, "/one.txt", "one"),
		createFileWithContent(t, ts, s, "/two.txt", "two"),
		createFileWithContent(t, ts, s, "/three.txt", "three"),
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/storage/pack", nil,
		map[string]any{"max_bytes": 0})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	for _, file := range files[:2] {
		_, etag := etagOf(t, ts, file.ID)
		resp, body = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", file.ID),
			map[string]string{"If-Match": etag}, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	statusResp, statusBody := get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, statusResp.StatusCode, statusBody)
	var before api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(statusBody), &before))
	assert.Positive(t, before.DeadPackedBytes)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/repack", nil,
		map[string]any{"min_age": "1ns", "min_dead_bytes": 1, "max_bytes": 0})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.StorageRepackReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.PacksSelected)
	assert.Equal(t, 1, report.PacksRewritten)
	assert.Equal(t, 1, report.PacksRemoved)
	assert.Equal(t, 1, report.BlobsRepacked)
	assert.Equal(t, int64(len("three")), report.BytesRepacked)

	statusResp, statusBody = get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, statusResp.StatusCode, statusBody)
	require.NoError(t, json.Unmarshal([]byte(statusBody), &before))
	assert.Zero(t, before.DeadPackedBytes)
	assert.Equal(t, int64(1), before.PackedBlobs)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/repack", nil,
		map[string]any{"min_age": "-1h", "min_dead_bytes": 1})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/storage/repack", nil,
		map[string]any{"min_age": "1ns", "min_dead_bytes": -1})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestGCRevokesPackedBlobAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	file := createFileWithContent(t, ts, s, "/packed.txt", "packed gc content")
	packed, err := s.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)

	_, etag := etagOf(t, ts, file.ID)
	resp, body := do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", file.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil,
		map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var gcReport api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &gcReport))
	assert.Zero(t, gcReport.ReclaimableBytes)
	assert.Equal(t, 1, gcReport.PendingPackedBlobs)
	assert.Positive(t, gcReport.PendingPackedBytes)
	assert.Zero(t, gcReport.ReclaimedFiles)
	assert.Equal(t, 1, gcReport.RemovedBlobs)

	_, err = s.Blobs.Open(file.BlobHash)
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := store.NewPackCatalog(s.Store).ListIndexed(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries, "GC removes packed authority in the blob-row transaction")
	records, err := store.NewPackCatalog(s.Store).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1, "physical inventory remains until reader-safe retirement")
	resp, body = get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	assert.Zero(t, status.PackedBlobs)
	assert.Positive(t, status.DeadPackedBytes)
	assert.Equal(t, status.PackStoredBytes, status.DeadPackedBytes)

	repacked, err := s.Blobs.Maintainer().Repack(t.Context(), packstore.RepackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, repacked.PacksRemoved)
}

func TestGCDryRunAndRun(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/g.txt", "gc-me")
	_, etag := etagOf(t, ts, f.ID)
	_, _ = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	_, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil, map[string]any{"run": true})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.CandidateBlobs)
	assert.Equal(t, int64(len("gc-me")), rep.ReclaimableBytes)
	assert.Zero(t, rep.PendingPackedBytes)
	assert.False(t, rep.Run)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Removed)
	assert.Equal(t, 1, rep.RemovedBlobs)
	assert.Equal(t, 1, rep.ReclaimedFiles)
	assert.Equal(t, int64(len("gc-me")), rep.ReclaimableBytes)
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
