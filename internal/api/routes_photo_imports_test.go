package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportRoutes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "capture.JPG"), []byte("photo"), 0o600))
	ts, catalog := newTestServer(t, nil)
	body, err := json.Marshal(api.PhotoImportStartRequest{SourceRoot: root, Destination: "/photos"})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/photos/imports", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	var started api.PhotoImportRun
	require.NoError(t, json.UnmarshalRead(response.Body, &started))
	require.NotEmpty(t, started.ID)

	var latest api.PhotoImportRun
	for range 300 {
		get, getErr := ts.Client().Get(ts.URL + "/api/v1/photos/imports/" + started.ID)
		require.NoError(t, getErr)
		var output api.PhotoImportRun
		require.NoError(t, json.UnmarshalRead(get.Body, &output))
		_ = get.Body.Close()
		latest = output
		if latest.FinishedAt != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, "completed", latest.State)
	assert.Equal(t, int64(1), latest.AddedGroups)
	assert.Equal(t, int64(1), latest.CompletedGroups)
	assert.Equal(t, root, latest.SourceRoot)

	invalid, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/photos/imports", bytes.NewReader([]byte(`{"source_root":"relative","destination":"/photos"}`)))
	require.NoError(t, err)
	invalid.Header.Set("Content-Type", "application/json")
	invalidResponse, err := ts.Client().Do(invalid)
	require.NoError(t, err)
	defer func() { _ = invalidResponse.Body.Close() }()
	assert.Equal(t, http.StatusUnprocessableEntity, invalidResponse.StatusCode)

	remote := httptest.NewRecorder()
	remoteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/photos/imports", bytes.NewReader(body))
	remoteRequest.RemoteAddr = "192.0.2.1:1234"
	remoteRequest.Header.Set("X-Api-Key", testAPIKey)
	catalog.Server.Handler().ServeHTTP(remote, remoteRequest)
	assert.Equal(t, http.StatusForbidden, remote.Code)
}

func TestPhotoImportCancelAndJobsCard(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	operation, err := catalog.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport,
		`{"source_root":"/private/camera","destination":"/photos"}`)
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/imports/"+operation.ID+"/cancel", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var cancelled api.PhotoImportRun
	require.NoError(t, json.Unmarshal([]byte(body), &cancelled))
	assert.True(t, cancelled.CancelRequested)
	assert.Equal(t, "queued", cancelled.State)

	response, body = do(t, ts, http.MethodGet, "/api/v1/jobs", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var jobs api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &jobs))
	require.Len(t, jobs.Items, 1)
	assert.Equal(t, api.Job{Name: "storage:" + operation.ID, Status: "queued",
		StartedAt: operation.CreatedAt.Format(time.RFC3339Nano), OperationID: operation.ID,
		Kind: store.StorageOperationKindPhotoImport, CanCancel: true, CancelRequested: true,
		Destination: "/photos"}, jobs.Items[0])
	assert.NotContains(t, body, "private")

	require.NoError(t, catalog.FinishStorageOperation(t.Context(), operation.ID, store.StorageOperationCancelled, "{}", "", time.Time{}))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/imports/"+operation.ID+"/cancel", nil, nil)
	assert.Equal(t, http.StatusConflict, response.StatusCode, body)

	other, err := catalog.CreateLocalOperation(t.Context(), "repair", `{}`)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/imports/"+other.ID+"/cancel", nil, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoImportBrowserRedactsRunAndAllowsOnlyReadCancel(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "camera")
	request, err := json.Marshal(store.PhotoImportRequest{SourceRoot: source, Destination: "/photos"})
	require.NoError(t, err)
	operation, err := catalog.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, string(request))
	require.NoError(t, err)
	_, err = catalog.ClaimStorageOperation(t.Context(), operation.ID)
	require.NoError(t, err)
	receipt, err := json.Marshal(store.PhotoImportReceipt{Ambiguous: 1, Ambiguities: []store.PhotoImportAmbiguity{{
		Reason: store.PhotoImportMultipleRAW, Files: []store.PhotoImportAmbiguousFile{{
			SourcePath: filepath.Join(source, "private.ARW"), NodeID: 7, Role: store.PhotoRoleRAW,
			AssetID: "00000000-0000-4000-8000-000000000001",
		}},
	}}})
	require.NoError(t, err)
	require.NoError(t, catalog.FinishStorageOperation(t.Context(), operation.ID, store.StorageOperationFailed, string(receipt), "private error", time.Time{}))

	response, body := do(t, ts, http.MethodGet, "/api/v1/photos/imports/"+operation.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "private.ARW")
	assert.Contains(t, body, "private error")

	browserToken := issueWebSession(t, ts)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/imports/"+operation.ID,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: browserToken}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var browserRun api.PhotoImportRun
	require.NoError(t, json.Unmarshal([]byte(body), &browserRun))
	assert.Empty(t, browserRun.SourceRoot)
	assert.Empty(t, browserRun.Error)
	assert.Equal(t, int64(1), browserRun.AmbiguousGroups)
	require.Len(t, browserRun.Ambiguities, 1)
	assert.Empty(t, browserRun.Ambiguities[0].Files[0].SourcePath)
	assert.NotContains(t, body, "private")

	startBody, err := json.Marshal(api.PhotoImportStartRequest{SourceRoot: source, Destination: "/photos"})
	require.NoError(t, err)
	startRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/photos/imports", bytes.NewReader(startBody))
	require.NoError(t, err)
	startRequest.Header.Set("Content-Type", "application/json")
	startRequest.Header.Set("X-Api-Key", "")
	startRequest.Header.Set(api.WebSessionHeader, browserToken)
	startResponse, err := ts.Client().Do(startRequest)
	require.NoError(t, err)
	defer func() { _ = startResponse.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, startResponse.StatusCode)
}
func TestPhotoImportGateAndActivity(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, nil)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG.ARW"), []byte("raw"), 0o600))
	importer := &ingest.Ingester{Store: catalog.Store, Blobs: catalog.Blobs}
	gate := api.NewOperationGate()
	tracker := api.NewActivityTracker()
	entered := make(chan struct{})
	release := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	attempted := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		_, err := importer.ImportPhotoDirectory(ctx, root, "/photos", ingest.PhotoImportOptions{
			Mutate: func(ctx context.Context, fn func() error) error {
				close(attempted)
				return gate.MutateContext(ctx, fn)
			}, ActivityBegin: tracker.Begin, ActivityEnd: tracker.End,
		})
		mutationDone <- err
	}()
	<-attempted
	select {
	case err := <-mutationDone:
		t.Fatalf("import crossed maintenance gate: %v", err)
	default:
	}
	var before bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &before))
	assert.NotContains(t, before.String(), "IMG.ARW")
	cancel()
	close(release)
	require.NoError(t, <-maintenanceDone)
	require.ErrorIs(t, <-mutationDone, context.Canceled)
	_, err := catalog.NodeByPath(t.Context(), "/photos/IMG.ARW")
	require.ErrorIs(t, err, store.ErrNotFound)

	groupReady := make(chan struct{})
	continueGroup := make(chan struct{})
	type importResult struct {
		report ingest.PhotoImportReport
		err    error
	}
	completed := make(chan importResult, 1)
	go func() {
		calls := 0
		report, importErr := importer.ImportPhotoDirectory(t.Context(), root, "/photos", ingest.PhotoImportOptions{
			Mutate: func(ctx context.Context, fn func() error) error {
				calls++
				return gate.MutateContext(ctx, func() error {
					if calls == 2 {
						close(groupReady)
						<-continueGroup
					}
					return fn()
				})
			}, ActivityBegin: tracker.Begin, ActivityEnd: tracker.End,
		})
		completed <- importResult{report, importErr}
	}()
	<-groupReady
	var captured bytes.Buffer
	var finished importResult
	require.NoError(t, gate.CaptureContext(t.Context(), func() error {
		if err := catalog.ExportMetadata(t.Context(), &captured); err != nil {
			return err
		}
		close(continueGroup)
		finished = <-completed
		return nil
	}))
	assert.NotContains(t, captured.String(), "IMG.ARW")
	report, err := finished.report, finished.err
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.Receipt.Added)
	var after bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &after))
	assert.Contains(t, after.String(), "IMG.ARW")
	assert.GreaterOrEqual(t, tracker.IdleFor(), time.Duration(0))
}
