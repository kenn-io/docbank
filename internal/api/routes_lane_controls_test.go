package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestLaneControlRoutesAndReadonlyJobs(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	for _, name := range []string{store.VisualPreviewLane, "derive:renditions"} {
		require.NoError(t, supervisor.Start(name, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }))
	}
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	path := "/api/v1/jobs/lanes/" + store.VisualPreviewLane
	resp, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, `"1"`, resp.Header.Get("ETag"))
	request := api.SetLaneControlRequest{Paused: true, Concurrency: 3}
	resp, body = do(t, ts, http.MethodPut, path, nil, request)
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": "1"}, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, `"2"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": "1"}, request)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var listed api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	require.Len(t, listed.Items, 2)
	for _, job := range listed.Items {
		switch job.Name {
		case store.VisualPreviewLane:
			assert.True(t, job.Controllable)
			assert.True(t, job.Paused)
			assert.True(t, job.CanSetConcurrency)
			assert.Equal(t, 3, job.Concurrency)
		default:
			assert.False(t, job.Controllable)
		}
	}
	resp, body = do(t, ts, http.MethodPut, "/api/v1/jobs/lanes/unknown", map[string]string{"If-Match": "1"}, api.SetLaneControlRequest{Concurrency: 1})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	for _, limit := range []int{0, 5} {
		resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": "2"}, api.SetLaneControlRequest{Concurrency: limit})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	headers := map[string]string{api.WebSessionHeader: issueWebSession(t, ts), "X-Api-Key": "", "If-Match": "2"}
	resp, body = get(t, ts, path, headers)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path, headers, api.SetLaneControlRequest{Paused: true, Concurrency: 1})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/v1/jobs/lanes/place?x=1"},
		{http.MethodPut, "/api/v1/jobs/lanes/unknown"},
		{http.MethodPut, "/api/v1/jobs/lanes/place/x"},
		{http.MethodGet, "/api/v1/jobs/lanes/place?x=1"},
		{http.MethodGet, "/api/v1/jobs/lanes/unknown"},
		{http.MethodPost, "/api/v1/jobs/lanes/place"},
		{http.MethodDelete, "/api/v1/jobs/lanes/place"},
		{http.MethodGet, "/api/v1/jobs/lanes/place/x"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			resp, body := do(t, ts, request.method, request.path, headers, nil)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)
		})
	}
}

func TestBrowserLaneControlBackendErrorIsRedacted(t *testing.T) {
	t.Parallel()
	ts, live := newTestServer(t, nil)
	headers := map[string]string{api.WebSessionHeader: issueWebSession(t, ts), "X-Api-Key": "", "If-Match": "1"}
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(live.DBPath), "lane-controls.json"), []byte("{"), 0o600))
	path := "/api/v1/jobs/lanes/" + store.VisualPreviewLane
	resp, body := do(t, ts, http.MethodPut, path, headers, api.SetLaneControlRequest{Concurrency: 1})
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
	assert.Contains(t, body, "inspect with the Docbank CLI")
	assert.NotContains(t, body, "database")
}

func TestStorageJobControlsFollowOperationState(t *testing.T) {
	t.Parallel()
	for _, state := range []store.StorageOperationState{
		store.StorageOperationQueued, store.StorageOperationRunning,
		store.StorageOperationCompleted, store.StorageOperationFailed, store.StorageOperationCancelled,
	} {
		t.Run(string(state), func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			_, err := s.SetLaneControl(t.Context(), store.LaneControl{Lane: "place", Paused: true, Concurrency: 1}, 1)
			require.NoError(t, err)
			operation, err := s.CreateLocalOperation(t.Context(), "place", `{}`)
			require.NoError(t, err)
			if state != store.StorageOperationQueued {
				_, err = s.ClaimStorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
			}
			active := state == store.StorageOperationQueued || state == store.StorageOperationRunning
			if !active {
				require.NoError(t, s.FinishStorageOperation(t.Context(), operation.ID, state, "{}", "", time.Now().Add(time.Hour)))
			}
			resp, body := get(t, ts, "/api/v1/jobs", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var listed api.JobList
			require.NoError(t, json.Unmarshal([]byte(body), &listed))
			require.Len(t, listed.Items, 1)
			job := listed.Items[0]
			assert.Equal(t, active, job.Controllable)
			assert.Equal(t, active, job.CanCancel)
			assert.Equal(t, active, job.Paused)
			if !active {
				assert.Zero(t, job.Concurrency)
				assert.Zero(t, job.ControlRevision)
			}
		})
	}
}

func TestJobListSurvivesBrokenLaneControls(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	operation, err := s.CreateLocalOperation(
		t.Context(), store.StorageOperationKindPhotoImport, `{"source_root":"example"}`,
	)
	require.NoError(t, err)
	controls := filepath.Join(filepath.Dir(s.DBPath), "lane-controls.json")
	require.NoError(t, os.WriteFile(controls, []byte("{"), 0o600))

	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var listed api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	assert.Contains(t, listed.LaneControlsError, "decoding lane controls")
	require.Len(t, listed.Items, 1)
	assert.Equal(t, operation.ID, listed.Items[0].OperationID)
	assert.False(t, listed.Items[0].Controllable)

	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	resp, body = do(t, ts, http.MethodGet, "/api/v1/jobs", browser, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "lane controls are unavailable")
	assert.NotContains(t, body, "lane-controls.json")

	lane := "/api/v1/jobs/lanes/" + store.VisualPreviewLane
	resp, body = do(t, ts, http.MethodPut, lane, map[string]string{"If-Match": "1"},
		api.SetLaneControlRequest{Paused: true, Concurrency: 1})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
	assert.Contains(t, body, "lane_controls_unreadable")
	assert.Contains(t, body, "repair or remove lane-controls.json")

	resp, body = do(t, ts, http.MethodPost, "/api/v1/jobs/"+operation.ID+"/cancel", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}
