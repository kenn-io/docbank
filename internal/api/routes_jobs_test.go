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

func TestListJobsWithoutSupervisorReturnsEmptyObject(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Empty(t, got.Items)
}

func TestBrowserJobListRedactsPrivateBackendErrors(t *testing.T) {
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
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs", nil)
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
