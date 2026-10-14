package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestListJobsReturnsStableObservableState(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	supervisor := jobs.New(ctx, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	running := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, supervisor.Start("watch:inbox", func(ctx context.Context) error {
		close(running)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	<-running
	require.NoError(t, supervisor.Start("maintenance.failed", func(context.Context) error {
		return errors.New("disk unavailable")
	}))
	require.Eventually(t, func() bool {
		items := supervisor.Snapshot()
		return len(items) == 2 && items[0].Status == jobs.StatusFailed
	}, time.Second, time.Millisecond)

	ts, _ := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Items, 2)
	assert.Equal(t, "maintenance.failed", got.Items[0].Name)
	assert.Equal(t, "failed", got.Items[0].Status)
	assert.Equal(t, "disk unavailable", got.Items[0].Error)
	assert.NotEmpty(t, got.Items[0].FinishedAt)
	assert.Equal(t, "watch:inbox", got.Items[1].Name)
	assert.Equal(t, "running", got.Items[1].Status)
	assert.Empty(t, got.Items[1].FinishedAt)
	close(release)
}

func TestListJobsOmitsFinishedStorageSnapshotsWithoutDurableRows(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	for _, name := range []string{"storage:11111111-1111-4111-8111-111111111111", "storage:pack"} {
		require.NoError(t, supervisor.Start(name, func(context.Context) error { return nil }))
	}
	require.Eventually(t, func() bool {
		items := supervisor.Snapshot()
		return len(items) == 2 && items[0].Status == jobs.StatusCompleted && items[1].Status == jobs.StatusCompleted
	}, time.Second, time.Millisecond)

	ts, _ := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Items, 1)
	assert.Equal(t, "storage:pack", got.Items[0].Name)
	assert.Equal(t, "completed", got.Items[0].Status)
	var lanes []string
	for _, lane := range got.Lanes {
		lanes = append(lanes, lane.Lane)
	}
	assert.Contains(t, lanes, "place")
}

func TestJobRoutesWithoutSupervisorListEmpty(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Empty(t, got.Items)
}

func TestJobRoutesRejectTerminalCancellation(t *testing.T) {
	t.Parallel()
	ts, live := newTestServer(t, nil)
	operation, err := live.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, `{}`)
	require.NoError(t, err)
	require.NoError(t, live.FinishStorageOperation(t.Context(), operation.ID, store.StorageOperationCancelled, "{}", "", time.Time{}))
	resp, body := do(t, ts, http.MethodPost, "/api/v1/jobs/"+operation.ID+"/cancel", nil, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
}

func TestBrowserJobListRedactsPrivateBackendErrors(t *testing.T) {
	t.Parallel()
	ts, live := newTestServer(t, nil)
	privateFailure := `opening /Users/user-a/private/archive: s3 endpoint https://private.example.invalid`
	operation, err := live.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "place", RequestDigest: testHash("private operation"),
		RequestJSON: `{}`, PlanJSON: `{}`, TotalObjects: 0,
	})
	require.NoError(t, err)
	require.NoError(t, live.FinishStorageOperation(
		t.Context(), operation.ID, store.StorageOperationFailed, "", privateFailure,
		time.Now().Add(time.Hour),
	))

	photoImport, err := live.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, `{"source_root":"/private/camera","destination":"/photos"}`)
	require.NoError(t, err)
	_, err = live.ClaimStorageOperation(t.Context(), photoImport.ID)
	require.NoError(t, err)
	require.NoError(t, live.FinishStorageOperation(t.Context(), photoImport.ID, store.StorageOperationFailed, "{}", privateFailure, time.Time{}))

	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var operator api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &operator))
	errorsByOperation := make(map[string]string)
	for _, job := range operator.Items {
		errorsByOperation[job.OperationID] = job.Error
	}
	assert.Equal(t, map[string]string{operation.ID: privateFailure, photoImport.ID: privateFailure}, errorsByOperation)

	resp, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	require.NotEmpty(t, issued.Token)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs", nil)
	require.NoError(t, err)
	req.Header["X-Api-Key"] = []string{""}
	req.Header.Set(api.WebSessionHeader, issued.Token)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	var got api.JobList
	require.NoError(t, json.UnmarshalRead(resp.Body, &got))
	require.Len(t, got.Items, 2)
	var operationIDs []string
	for _, job := range got.Items {
		operationIDs = append(operationIDs, job.OperationID)
		assert.NotEmpty(t, job.Error)
		assert.NotContains(t, job.Error, "/Users/user-a/private/archive")
		assert.NotContains(t, job.Error, "private.example.invalid")
	}
	assert.ElementsMatch(t, []string{operation.ID, photoImport.ID}, operationIDs)
}
