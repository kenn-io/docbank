package api_test

import (
	"bytes"
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

func TestPhotoImportRoutes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "capture.JPG"), []byte("photo"), 0o600))
	gate := api.NewOperationGate()
	tracker := api.NewActivityTracker()
	supervisor := jobs.New(t.Context(), nil)
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Gate, d.Tracker, d.Jobs = gate, tracker, supervisor })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})
	var started api.StorageOperation
	require.NoError(t, gate.MaintainContext(t.Context(), func() error {
		submitted := api.PhotoImportStartRequest{SourceRoot: root, Destination: "/photos"}
		body, err := json.Marshal(submitted)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/photos/imports", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := ts.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusAccepted, response.StatusCode)
		require.NoError(t, json.UnmarshalRead(response.Body, &started))
		require.NotEmpty(t, started.ID)
		assert.Equal(t, store.StorageOperationKindPhotoImport, started.Kind)
		assert.Equal(t, "queued", started.State)
		registered := supervisor.Snapshot()
		require.Len(t, registered, 1)
		assert.Equal(t, "storage:"+started.ID, registered[0].Name)

		stored, err := catalog.StorageOperation(t.Context(), started.ID)
		require.NoError(t, err)
		var accepted store.PhotoImportRequest
		require.NoError(t, json.Unmarshal([]byte(stored.RequestJSON), &accepted))
		assert.Equal(t, store.PhotoImportRequest{SourceRoot: submitted.SourceRoot, Destination: submitted.Destination}, accepted)

		require.Eventually(t, func() bool { return tracker.IdleFor() == 0 }, time.Second, time.Millisecond)
		assert.Nil(t, supervisor.Snapshot()[0].FinishedAt)
		return nil
	}))
	require.Eventually(t, func() bool { return supervisor.Snapshot()[0].FinishedAt != nil }, 5*time.Second, 10*time.Millisecond)
	assert.Positive(t, tracker.IdleFor())

	pollCtx, cancelPoll := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelPoll()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var latest api.StorageOperation
	for {
		pollRequest, requestErr := http.NewRequestWithContext(pollCtx, http.MethodGet, ts.URL+"/api/v1/jobs/"+started.ID, nil)
		require.NoError(t, requestErr)
		get, getErr := ts.Client().Do(pollRequest)
		require.NoError(t, getErr, "last observed operation: %+v", latest)
		var output api.StorageOperation
		decodeErr := json.UnmarshalRead(get.Body, &output)
		_ = get.Body.Close()
		require.NoError(t, decodeErr, "last observed operation: %+v", latest)
		latest = output
		if latest.FinishedAt != "" {
			break
		}
		select {
		case <-pollCtx.Done():
			require.FailNow(t, "photo import did not finish", "last observed operation: %+v", latest)
		case <-ticker.C:
		}
	}
	assert.Equal(t, "completed", latest.State)
	assert.Equal(t, int64(1), latest.CompletedObjects)
	receiptJSON, receiptErr := json.Marshal(latest.Receipt)
	require.NoError(t, receiptErr)
	var receipt store.PhotoImportReceipt
	require.NoError(t, json.Unmarshal(receiptJSON, &receipt))
	assert.Equal(t, int64(1), receipt.Added)

	for _, test := range []struct{ source, message string }{
		{"relative", "absolute daemon-host path"},
		{filepath.Join(root, "missing"), "not an existing daemon-host folder"},
		{filepath.Join(root, "capture.JPG"), "not an existing daemon-host folder"},
	} {
		response, text := do(t, ts, http.MethodPost, "/api/v1/photos/imports", nil,
			api.PhotoImportStartRequest{SourceRoot: test.source, Destination: "/photos"})
		assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, text)
		assert.Contains(t, text, test.message)
	}
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/imports", browser,
		api.PhotoImportStartRequest{SourceRoot: root, Destination: "/photos"})
	assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
}
func TestPhotoImportGate(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, nil)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG.ARW"), []byte("raw"), 0o600))
	gate := api.NewOperationGate()
	runner := api.NewPhotoImportRunner(api.Deps{Store: catalog.Store, Blobs: catalog.Blobs, Gate: gate})
	require.NotNil(t, runner.Options.Mutate)
	mutate := runner.Options.Mutate
	request, err := json.Marshal(store.PhotoImportRequest{SourceRoot: root, Destination: "/photos"})
	require.NoError(t, err)
	operation, err := catalog.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, string(request))
	require.NoError(t, err)
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
	mutationEntered := false
	runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
		close(attempted)
		return mutate(ctx, func() error {
			mutationEntered = true
			return fn()
		})
	}
	go func() {
		mutationDone <- runner.Run(ctx, operation.ID)
	}()
	<-attempted
	var before bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &before))
	assert.NotContains(t, before.String(), "IMG.ARW")
	cancel()
	var runErr error
	select {
	case runErr = <-mutationDone:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("photo import did not cancel while maintenance held the gate")
	}
	assert.False(t, mutationEntered)
	close(release)
	require.ErrorIs(t, runErr, context.Canceled)
	require.NoError(t, <-maintenanceDone)
	_, err = catalog.NodeByPath(t.Context(), "/photos/IMG.ARW")
	require.ErrorIs(t, err, store.ErrNotFound)

	groupReady := make(chan struct{})
	continueGroup := make(chan struct{})
	operation, err = catalog.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, string(request))
	require.NoError(t, err)
	completed := make(chan error, 1)
	calls := 0
	runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
		calls++
		return mutate(ctx, func() error {
			if calls == 2 {
				close(groupReady)
				<-continueGroup
			}
			return fn()
		})
	}
	go func() {
		completed <- runner.Run(t.Context(), operation.ID)
	}()
	<-groupReady
	var captured bytes.Buffer
	var finishedErr error
	require.NoError(t, gate.CaptureContext(t.Context(), func() error {
		if err := catalog.ExportMetadata(t.Context(), &captured); err != nil {
			return err
		}
		close(continueGroup)
		finishedErr = <-completed
		return nil
	}))
	assert.NotContains(t, captured.String(), "IMG.ARW")
	require.NoError(t, finishedErr)
	finished, err := catalog.StorageOperation(t.Context(), operation.ID)
	require.NoError(t, err)
	var receipt store.PhotoImportReceipt
	require.NoError(t, json.Unmarshal([]byte(finished.ReceiptJSON), &receipt))
	assert.Equal(t, int64(1), receipt.Added)
	var after bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &after))
	assert.Contains(t, after.String(), "IMG.ARW")
}
