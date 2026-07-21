package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
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

func TestIngestProgressStreamEndsWithResult(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	src := filepath.Join(t.TempDir(), "progress.txt")
	content := []byte("stream this import")
	require.NoError(t, os.WriteFile(src, content, 0o600))

	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest/stream", nil,
		map[string]any{"paths": []string{src}, "dest": "/inbox"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/x-ndjson")

	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.GreaterOrEqual(t, len(lines), 5)
	var events []api.IngestEvent
	for _, line := range lines {
		var event api.IngestEvent
		require.NoError(t, json.Unmarshal([]byte(line), &event), line)
		events = append(events, event)
	}
	for i, event := range events[:len(events)-1] {
		assert.Equal(t, "progress", event.Type, "event %d", i)
		require.NotNil(t, event.Progress)
	}
	terminal := events[len(events)-1]
	assert.Equal(t, "result", terminal.Type)
	require.NotNil(t, terminal.Report)
	assert.Equal(t, 1, terminal.Report.Added)
	assert.Nil(t, terminal.Progress)
	assert.Nil(t, terminal.Error)

	finalStages := map[string]api.IngestProgress{}
	for _, event := range events {
		if event.Progress != nil && event.Progress.Final {
			finalStages[event.Progress.Stage] = *event.Progress
		}
	}
	require.Contains(t, finalStages, "scan")
	require.Contains(t, finalStages, "ingest")
	assert.Equal(t, int64(1), finalStages["scan"].Total)
	assert.Equal(t, int64(len(content)), finalStages["scan"].BytesTotal)
	assert.Equal(t, int64(1), finalStages["ingest"].Done)
	assert.Equal(t, int64(len(content)), finalStages["ingest"].BytesDone)
}

func TestIngestPreflightIsReadOnlyAndSharesExclusions(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "keep.jsonl"), []byte("{\"ok\":true}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "config"), []byte("ignored"), 0o600))

	request := map[string]any{"paths": []string{src}, "exclude": []string{".git"}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest/preflight", nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, int64(1), report.Files)
	assert.Equal(t, int64(1), report.Excluded)
	assert.Equal(t, int64(len("{\"ok\":true}\n")), report.LogicalBytes)
	assert.Equal(t, int64(1), report.PackEligible.Files)
	assert.Zero(t, report.Errors)
	require.Len(t, report.FileTypes, 1)
	assert.Equal(t, ".jsonl", report.FileTypes[0].Extension)

	resp, body = get(t, ts, "/api/v1/path?path=/inbox", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{src}, "dest": "/inbox", "exclude": []string{".git"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var ingested api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(body), &ingested))
	assert.Equal(t, 1, ingested.Added)
	assert.Equal(t, 1, ingested.Excluded)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest/preflight", nil,
		map[string]any{"paths": []string{src}, "exclude": []string{"../outside"}})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
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
	srv := api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: dir, Cfg: cfg})
	for _, path := range []string{"/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"paths":["/x"],"dest":"/inbox"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "test-key")
			req.RemoteAddr = "192.0.2.1:4444"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.Contains(t, rec.Body.String(), `"code":"loopback_only"`)
		})
	}
}

func TestIngestRoutesRejectLossyJSONTextBeforeDecoding(t *testing.T) {
	ts, s := newTestServer(t, nil)
	replacementName := "bad\ufffd.txt"
	replacementPath := filepath.Join(t.TempDir(), replacementName)
	require.NoError(t, os.WriteFile(replacementPath, []byte("must not be imported"), 0o600))
	validBody, err := json.Marshal(map[string]any{
		"paths": []string{replacementPath}, "dest": "/inbox",
	})
	require.NoError(t, err)
	tests := []struct {
		name   string
		body   []byte
		detail string
	}{
		{name: "invalid UTF-8", body: bytes.Replace(validBody, []byte("\ufffd"), []byte{0xff}, 1),
			detail: "request body is not valid UTF-8"},
		{name: "lone high surrogate", body: bytes.Replace(validBody, []byte("\ufffd"), []byte(`\ud800`), 1),
			detail: "request body contains an unpaired UTF-16 surrogate escape"},
		{name: "lone low surrogate", body: bytes.Replace(validBody, []byte("\ufffd"), []byte(`\udc00`), 1),
			detail: "request body contains an unpaired UTF-16 surrogate escape"},
	}

	for _, tt := range tests {
		require.NotEqual(t, validBody, tt.body, "fixture must replace the path's U+FFFD bytes")
		t.Run(tt.name, func(t *testing.T) {
			for _, route := range []string{
				"/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight",
			} {
				t.Run(route, func(t *testing.T) {
					req, reqErr := http.NewRequest(http.MethodPost, ts.URL+route, bytes.NewReader(tt.body))
					require.NoError(t, reqErr)
					req.Header.Set("Content-Type", "application/json")
					resp, reqErr := ts.Client().Do(req)
					require.NoError(t, reqErr)
					body, readErr := io.ReadAll(resp.Body)
					require.NoError(t, readErr)
					require.NoError(t, resp.Body.Close())
					assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
					assert.Contains(t, string(body), `"code":"validation"`)
					assert.Contains(t, string(body), tt.detail)
					_, lookupErr := s.NodeByPath(t.Context(), "/inbox/"+replacementName)
					require.ErrorIs(t, lookupErr, store.ErrNotFound,
						"malformed text must not retarget the replacement-character source")
				})
			}
		})
	}
}

func TestIngestRoutesAcceptPairedSurrogatePath(t *testing.T) {
	for _, route := range []string{
		"/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight",
	} {
		t.Run(route, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			const sourceName = "valid-\U0001f600.txt"
			sourcePath := filepath.Join(t.TempDir(), sourceName)
			require.NoError(t, os.WriteFile(sourcePath, []byte("valid pair"), 0o600))
			request := map[string]any{"paths": []string{sourcePath}}
			if route != "/api/v1/ingest/preflight" {
				request["dest"] = "/inbox"
			}
			body, err := json.Marshal(request)
			require.NoError(t, err)
			body = bytes.Replace(body, []byte("\U0001f600"), []byte(`\ud83d\ude00`), 1)
			require.Contains(t, string(body), `\ud83d\ude00`)

			req, err := http.NewRequest(http.MethodPost, ts.URL+route, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			responseBody, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode, string(responseBody))

			if route == "/api/v1/ingest/preflight" {
				var report api.IngestPreflightReport
				require.NoError(t, json.Unmarshal(responseBody, &report))
				assert.Equal(t, int64(1), report.Files)
				_, err = s.NodeByPath(t.Context(), "/inbox/"+sourceName)
				require.ErrorIs(t, err, store.ErrNotFound)
				return
			}
			if route == "/api/v1/ingest/stream" {
				lines := strings.Split(strings.TrimSpace(string(responseBody)), "\n")
				var terminal api.IngestEvent
				require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &terminal))
				require.NotNil(t, terminal.Report)
				assert.Equal(t, 1, terminal.Report.Added)
			} else {
				var report api.IngestReport
				require.NoError(t, json.Unmarshal(responseBody, &report))
				assert.Equal(t, 1, report.Added)
			}
			_, err = s.NodeByPath(t.Context(), "/inbox/"+sourceName)
			require.NoError(t, err)
		})
	}
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
	assert.True(t, report.More)

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
	assert.False(t, report.More)

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

func TestGCEndpointAdvancesPastLiveOnlyRawPage(t *testing.T) {
	ts, s := newTestServer(t, nil)
	target := createFileWithContent(t, ts, s, "/bounded-gc.txt", "bounded gc target")
	_, etag := etagOf(t, ts, target.ID)
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/trash", target.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	for i := range internalmaintenance.DefaultMaxObjects {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("live-gc-%03d", i),
			fmt.Sprintf("!live-gc-%03d", i), 1, "application/octet-stream")
		require.NoError(t, err)
	}

	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.CandidateBlobs)
	assert.Equal(t, 1, report.RemovedBlobs)
}

func TestGCEndpointRetainsFullPhysicalOrphanReconciliation(t *testing.T) {
	ts, s := newTestServer(t, nil)
	hash, size, err := s.Blobs.Write(strings.NewReader("untracked physical content"))
	require.NoError(t, err)
	path := filepath.Join(s.BlobsDir, hash[:2], hash)
	require.FileExists(t, path)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": false})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.UntrackedFiles)
	assert.Equal(t, size, report.ReclaimableBytes)
	assert.Zero(t, report.ReclaimedFiles)
	require.FileExists(t, path)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, 1, report.UntrackedFiles)
	assert.Equal(t, 1, report.ReclaimedFiles)
	assert.Equal(t, 1, report.Removed)
	assert.NoFileExists(t, path)
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
	assert.Empty(t, rep.MetadataProblems)
}

func TestVerifyEndpointContinuesPastMalformedHashAtPageBoundary(t *testing.T) {
	ts, s := newTestServer(t, nil)
	db, err := s.SQLiteDriver().Open(s.DBPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	for i := range internalmaintenance.DefaultMaxObjects - 1 {
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			fmt.Sprintf("%064x", i), "2026-07-21T00:00:00.000000000Z")
		require.NoError(t, err)
	}
	for _, hash := range []string{
		"1-malformed-page-boundary",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	} {
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hash, "2026-07-21T00:00:00.000000000Z")
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	resp, body := do(t, ts, http.MethodPost, "/api/v1/verify", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.MetadataProblems, 1)
	assert.Contains(t, report.MetadataProblems[0], "invalid blob hash")
	require.Len(t, report.Problems, internalmaintenance.DefaultMaxObjects+1)
	assert.Equal(t, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		report.Problems[len(report.Problems)-1].Hash)
}

func TestVerifyEndpointReportsMalformedBlobMetadata(t *testing.T) {
	ts, s := newTestServer(t, nil)
	created := createFileWithContent(t, ts, s, "/malformed.txt", "still verifiable")

	db, err := s.SQLiteDriver().Open(s.DBPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE blobs SET size='not-an-integer' WHERE hash=?`, created.BlobHash)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	resp, body := do(t, ts, http.MethodPost, "/api/v1/verify", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var rep api.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.OK)
	assert.Empty(t, rep.Problems)
	require.Len(t, rep.MetadataProblems, 1)
	assert.Contains(t, rep.MetadataProblems[0], "size")
}

func TestVerifyEndpointReportsBlobInventoryFailureAlongsideMetadataFailure(t *testing.T) {
	sentinel := errors.New("blob inventory unavailable")
	var calls int
	ts, s := newTestServer(t, func(deps *api.Deps) {
		deps.VerifyPage = func(
			_ context.Context, metadata *store.Store, blobs *blob.Store,
			opts internalmaintenance.VerifyOptions,
		) (internalmaintenance.VerifyReport, error) {
			calls++
			assert.NotNil(t, metadata)
			assert.NotNil(t, blobs)
			assert.Empty(t, opts.Budget.Cursor)
			return internalmaintenance.VerifyReport{}, sentinel
		}
	})
	created := createFileWithContent(t, ts, s, "/malformed-and-faulted.txt", "content")
	db, err := s.SQLiteDriver().Open(s.DBPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE blobs SET size='not-an-integer' WHERE hash=?`,
		created.BlobHash)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	resp, body := do(t, ts, http.MethodPost, "/api/v1/verify", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Zero(t, report.OK)
	assert.Empty(t, report.Problems)
	assert.Equal(t, 1, calls)
	require.Len(t, report.MetadataProblems, 2,
		"the blob inventory failure must append to the earlier metadata report")
	assert.Contains(t, report.MetadataProblems[0], "size")
	assert.Contains(t, report.MetadataProblems[1], sentinel.Error())
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
