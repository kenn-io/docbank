package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportRoutes(t *testing.T) {
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
	require.NotEmpty(t, response.Header.Get("ETag"))

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

func TestPhotoImportCancelRetriesStaleRevision(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	run, err := catalog.StartPhotoImportRun(t.Context(), filepath.Join(t.TempDir(), "camera"), "/photos", 0)
	require.NoError(t, err)
	_, err = catalog.SetPhotoImportTotalGroups(t.Context(), run.ID, 1)
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/imports/"+run.ID+"/cancel",
		map[string]string{"If-Match": "\"1\""}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var cancelled api.PhotoImportRun
	require.NoError(t, json.Unmarshal([]byte(body), &cancelled))
	assert.Equal(t, run.ID, cancelled.ID)
	assert.Equal(t, "cancel-requested", cancelled.State)
	assert.True(t, cancelled.CancelRequested)
}

func TestPhotoImportBrowserRedactsRunAndAllowsOnlyReadCancel(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	ambiguousRun, err := catalog.StartPhotoImportRun(t.Context(), filepath.Join(t.TempDir(), "camera"), "/photos", 1)
	require.NoError(t, err)
	ambiguity := store.PhotoImportAmbiguity{GroupKey: "cGhvdG8vcHJpdmF0ZQ", Candidates: []store.PhotoImportCandidate{{
		AssetID: "00000000-0000-4000-8000-000000000001", FileID: "00000000-0000-4000-8000-000000000002",
		NodeID: 1, Revision: 1, SourcePath: filepath.Join(t.TempDir(), "private.ARW"), BlobHash: strings.Repeat("a", 64),
	}}}
	_, err = catalog.UpdatePhotoImportProgress(t.Context(), ambiguousRun.ID, 0, 0, 0, 1, &ambiguity)
	require.NoError(t, err)
	_, err = catalog.FinishPhotoImportRun(t.Context(), ambiguousRun.ID, store.PhotoImportStateAmbiguous, "private error")
	require.NoError(t, err)

	browserToken := issueWebSession(t, ts)
	response, body := do(t, ts, http.MethodGet, "/api/v1/photos/imports/"+ambiguousRun.ID,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: browserToken}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var browserRun api.PhotoImportRun
	require.NoError(t, json.Unmarshal([]byte(body), &browserRun))
	assert.Empty(t, browserRun.SourceRoot)
	assert.Empty(t, browserRun.Error)
	require.Len(t, browserRun.Ambiguities, 1)
	assert.Empty(t, browserRun.Ambiguities[0].GroupKey)
	assert.Empty(t, browserRun.Ambiguities[0].Candidates[0].SourcePath)

	startBody, err := json.Marshal(api.PhotoImportStartRequest{SourceRoot: filepath.Join(t.TempDir(), "camera"), Destination: "/photos"})
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

	tracker.Begin()
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- gate.MutateContext(t.Context(), func() error { return nil })
	}()
	assert.Zero(t, tracker.IdleFor())
	select {
	case err := <-mutationDone:
		t.Fatalf("mutation crossed maintenance gate: %v", err)
	default:
	}
	close(release)
	require.NoError(t, <-maintenanceDone)
	require.NoError(t, <-mutationDone)
	tracker.End()
	assert.GreaterOrEqual(t, tracker.IdleFor(), time.Duration(0))
}
