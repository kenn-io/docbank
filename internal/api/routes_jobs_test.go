package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestListJobsReturnsStableObservableState(t *testing.T) {
	t.Parallel()
	// The supervisor records a job's outcome on its own goroutine after the
	// runner returns. Running in a bubble lets synctest.Wait show when that
	// has happened instead of polling the snapshot on the wall clock.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		supervisor := jobs.New(ctx, slog.New(slog.DiscardHandler))
		t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })
		release := make(chan struct{})
		require.NoError(t, supervisor.Start("watch:inbox", func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}))
		require.NoError(t, supervisor.Start("maintenance.failed", func(context.Context) error {
			return errors.New("disk unavailable")
		}))
		synctest.Wait()
		items := supervisor.Snapshot()
		require.Len(t, items, 2)
		require.Equal(t, jobs.StatusFailed, items[0].Status)
		require.Equal(t, jobs.StatusRunning, items[1].Status)

		ts, _ := newTestServer(t, func(d *api.Deps) {
			d.Jobs = supervisor
		})
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
	})
}

func TestListJobsOmitsFinishedStorageSnapshotsWithoutDurableRows(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
		t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })
		for _, name := range []string{"storage:11111111-1111-4111-8111-111111111111", "storage:pack"} {
			require.NoError(t, supervisor.Start(name, func(context.Context) error { return nil }))
		}
		synctest.Wait()
		items := supervisor.Snapshot()
		require.Len(t, items, 2)
		require.Equal(t, jobs.StatusCompleted, items[0].Status)
		require.Equal(t, jobs.StatusCompleted, items[1].Status)

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
	})
}

func TestListJobsWithoutSupervisorReturnsEmptyObject(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Empty(t, got.Items)
}

func TestBrowserJobListRedactsPrivateBackendErrors(t *testing.T) {
	t.Parallel()
	ts, live := newTestServer(t, nil)
	privateFailure := `opening /Users/example/private/archive: s3 endpoint https://private.example.invalid`
	operation, err := live.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "place", RequestDigest: testHash("private operation"),
		RequestJSON: `{}`, PlanJSON: `{}`, TotalObjects: 0,
	})
	require.NoError(t, err)
	require.NoError(t, live.FinishStorageOperation(
		t.Context(), operation.ID, store.StorageOperationFailed, "", privateFailure,
		time.Now().Add(time.Hour),
	))

	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, privateFailure)

	resp, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	require.NotEmpty(t, issued.Token)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/jobs", nil)
	require.NoError(t, err)
	req.Header["X-Api-Key"] = []string{""}
	req.Header.Set(api.WebSessionHeader, issued.Token)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	var got api.JobList
	require.NoError(t, json.UnmarshalRead(resp.Body, &got))
	require.Len(t, got.Items, 1)
	assert.NotEmpty(t, got.Items[0].Error)
	assert.NotContains(t, got.Items[0].Error, "/Users/example")
	assert.NotContains(t, got.Items[0].Error, "private.example.invalid")
}
