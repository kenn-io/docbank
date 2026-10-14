package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
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

func TestBrowserJobRoutesRedactPrivateDetails(t *testing.T) {
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

	resp, body := get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var operator api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &operator))
	require.Len(t, operator.Items, 1)
	assert.Equal(t, privateFailure, operator.Items[0].Error)

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
	assert.Equal(t, operation.ID, got.Items[0].OperationID)
	assert.NotEmpty(t, got.Items[0].Error)
	assert.NotContains(t, got.Items[0].Error, "/Users/user-a/private/archive")
	assert.NotContains(t, got.Items[0].Error, "private.example.invalid")
	t.Run("receipt redaction", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "camera")
		request, err := json.Marshal(store.PhotoImportRequest{SourceRoot: source, Destination: "/photos"})
		require.NoError(t, err)
		receipt, err := json.Marshal(store.PhotoImportReceipt{Ambiguous: 1, Ambiguities: []store.PhotoImportAmbiguity{{
			Reason: store.PhotoImportMultipleRAW, Files: []store.PhotoImportAmbiguousFile{{
				SourcePath: filepath.Join(source, "private.ARW"), NodeID: 7, Role: store.PhotoRoleRAW,
				AssetID: "00000000-0000-4000-8000-000000000001",
			}},
		}}})
		require.NoError(t, err)
		running, err := live.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, string(request))
		require.NoError(t, err)
		_, err = live.ClaimStorageOperation(t.Context(), running.ID)
		require.NoError(t, err)
		require.NoError(t, live.AdvanceStorageOperation(t.Context(), running.ID, "", 0, 0, 0, string(receipt)))
		response, body := do(t, ts, http.MethodGet, "/api/v1/jobs/"+running.ID, nil, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var operator api.StorageOperation
		require.NoError(t, json.Unmarshal([]byte(body), &operator))
		receiptJSON, err := json.Marshal(operator.Receipt)
		require.NoError(t, err)
		var operatorReceipt store.PhotoImportReceipt
		require.NoError(t, json.Unmarshal(receiptJSON, &operatorReceipt))
		require.Len(t, operatorReceipt.Ambiguities, 1)
		assert.Equal(t, int64(7), operatorReceipt.Ambiguities[0].Files[0].NodeID)

		browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
		response, body = do(t, ts, http.MethodPost, "/api/v1/jobs/"+running.ID+"/cancel", browser, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var cancelled api.StorageOperation
		require.NoError(t, json.Unmarshal([]byte(body), &cancelled))
		assert.Nil(t, cancelled.Receipt)
		assert.NotContains(t, body, "private")
	})
}
