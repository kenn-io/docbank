package api_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/checkout"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutRoutes(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), nil)
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	_, err := catalog.SetLaneControl(t.Context(), store.LaneControl{Lane: store.StorageOperationKindPhotoCheckout, Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	_, err = catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "second.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	selection := store.PhotoCheckoutSelection{All: true}
	expectedEstimate, err := catalog.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	if expectedEstimate.SkippedPhotos == nil {
		expectedEstimate.SkippedPhotos = []store.PhotoCheckoutSkip{}
	}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/estimate", nil, selection)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var estimate store.PhotoCheckoutEstimate
	require.NoError(t, json.Unmarshal([]byte(body), &estimate))
	require.Equal(t, expectedEstimate, estimate)
	request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: t.TempDir(), ByteLimit: 100}
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var raw map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	require.NotContains(t, raw, "skipped_photos")
	require.NotContains(t, raw, "skipped_photo_count")
	var checkout map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw["checkout"], &checkout))
	require.JSONEq(t, `[]`, string(checkout["skipped_photos"]))
	require.JSONEq(t, `0`, string(checkout["skipped_photo_count"]))
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.NotEmpty(t, created.Checkout.ID)
	require.Equal(t, created.Checkout.OperationID, created.Operation.ID)
	require.True(t, filepath.IsAbs(created.Checkout.Destination))
	require.Equal(t, expectedEstimate.SkippedPhotoCount, created.Checkout.SkippedPhotoCount)
	require.Equal(t, expectedEstimate.SkippedPhotos, created.Checkout.SkippedPhotos)
	require.Equal(t, request.ByteLimit, created.Checkout.ByteLimit)
	require.Equal(t, request.Selection, created.Checkout.Selection)
	requestedDestination, err := os.Stat(request.Destination)
	require.NoError(t, err)
	createdDestination, err := os.Stat(created.Checkout.Destination)
	require.NoError(t, err)
	require.True(t, os.SameFile(requestedDestination, createdDestination))
	registered := supervisor.Snapshot()
	require.Len(t, registered, 1)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	expectedStatus, err := catalog.PhotoCheckoutStatus(t.Context(), created.Checkout.ID, "", 100)
	require.NoError(t, err)
	require.NotEmpty(t, expectedStatus.Checkout.DestinationIdentity)
	require.Len(t, status.Entries, 2)
	expectedStatus.Checkout.DestinationIdentity = ""
	expectedStatus.Checkout.DestinationReserved = false
	require.Equal(t, expectedStatus, status)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID+"?limit=1", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var firstPage api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(body), &firstPage))
	require.Len(t, firstPage.Entries, 1)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID+"?limit=1&after="+firstPage.Entries[0].FileID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var lastPage api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(body), &lastPage))
	require.Len(t, lastPage.Entries, 1)
	expected, err := catalog.PhotoCheckoutStatus(t.Context(), created.Checkout.ID, firstPage.Entries[0].FileID, 1)
	require.NoError(t, err)
	require.Len(t, expected.Entries, 1)
	require.Equal(t, expected.Entries[0].FileID, lastPage.Entries[0].FileID, "the API must forward the page limit and cursor")
	require.Equal(t, expected.Entries[0].Path, lastPage.Entries[0].Path)
	for _, query := range []string{"?limit=251", "?limit=0", "?after=invalid"} {
		response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID+query, nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var list api.PhotoCheckoutList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	expectedList, err := catalog.ListPhotoCheckouts(t.Context())
	require.NoError(t, err)
	for i := range expectedList {
		expectedList[i].DestinationIdentity = ""
		expectedList[i].DestinationReserved = false
	}
	require.Equal(t, expectedList, list.Items)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+created.Checkout.ID+"/retire", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"retirement_requested":true`)
	require.NoError(t, supervisor.Shutdown(context.Background()))
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), created.Checkout.ID, created.Operation.ID, store.StorageOperationCancelled, "{}", "cancelled", time.Now().Add(time.Hour)))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+created.Checkout.ID+"/retire", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/00000000-0000-4000-8000-000000000001", nil, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoCheckoutRoutesStartFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	catalog, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	hash, size, err := blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	_, err = catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	supervisor := jobs.New(t.Context(), nil)
	require.NoError(t, supervisor.Shutdown(context.Background()))
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: dir, Cfg: cfg, Jobs: supervisor})
	t.Cleanup(server.Close)
	payload, err := json.Marshal(api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{All: true}, Destination: t.TempDir(), ByteLimit: 100})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/photos/checkouts", strings.NewReader(string(payload)))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", testAPIKey)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	var problem api.Error
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, "internal", problem.Code)
}

func TestPhotoCheckoutOperationRoutesStartFailure(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"scan", "rebind", "commit"} {
		t.Run(action, func(t *testing.T) {
			supervisor := jobs.New(t.Context(), nil)
			var service checkout.Service
			ts, catalog := newTestServer(t, func(d *api.Deps) {
				d.Jobs = supervisor
				service = api.NewPhotoCheckoutService(*d)
			})
			require.NoError(t, supervisor.Shutdown(context.Background()))
			hash, size, err := catalog.Blobs.Write(strings.NewReader("photo"))
			require.NoError(t, err)
			node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
			require.NoError(t, err)
			asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			c, _, _, root, err := service.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 100)
			require.NoError(t, err)
			require.NoError(t, service.Run(t.Context(), c, root))
			require.NoError(t, root.Close())
			var payload any
			if action == "rebind" {
				payload = api.PhotoCheckoutRebindRequest{Destination: c.Destination}
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+c.ID+"/"+action, nil, payload)
			require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
			var problem api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &problem))
			require.Equal(t, "internal", problem.Code)
			current, err := catalog.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			op, err := catalog.StorageOperation(t.Context(), current.ActiveOperationID())
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, op.State)
		})
	}
}

func TestPhotoCheckoutStartResponseKeepsPreparedOperation(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"scan", "commit"} {
		t.Run(action, func(t *testing.T) {
			supervisor := jobs.New(t.Context(), nil)
			supervisor.Stop()
			gate := &photoCheckoutStartGate{Handler: slog.DiscardHandler, entered: make(chan struct{}, 1), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(gate.release) })
			t.Cleanup(release)
			var service checkout.Service
			ts, catalog := newTestServer(t, func(d *api.Deps) {
				d.Jobs = supervisor
				d.Logger = slog.New(gate)
				service = api.NewPhotoCheckoutService(*d)
			})
			hash, size, err := catalog.Blobs.Write(strings.NewReader("photo"))
			require.NoError(t, err)
			node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
			require.NoError(t, err)
			asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
			kind := "photo_checkout_" + action
			c, _, _, root, err := service.Prepare(t.Context(), selection, t.TempDir(), 100)
			require.NoError(t, err)
			require.NoError(t, service.Run(t.Context(), c, root))
			require.NoError(t, root.Close())
			request := httptest.NewRequest(http.MethodPost, ts.URL+"/api/v1/photos/checkouts/"+c.ID+"/"+action, nil)
			request.Header.Set("X-Api-Key", testAPIKey)
			request.RemoteAddr = "127.0.0.1:12345"
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); ts.Config.Handler.ServeHTTP(response, request) }()
			t.Cleanup(func() {
				release()
				<-done
			})
			select {
			case <-gate.entered:
			case <-done:
				t.Fatal("response returned before the start-error gate")
			case <-time.After(5 * time.Second):
				t.Fatal("start-error record did not reach the gate")
			}
			items, err := catalog.ListPhotoCheckouts(t.Context())
			require.NoError(t, err)
			require.Len(t, items, 1)
			c = items[0]
			first, err := catalog.StorageOperation(t.Context(), c.ActiveOperationID())
			require.NoError(t, err)
			require.Equal(t, kind, first.Kind)
			require.Equal(t, store.StorageOperationFailed, first.State)
			// The first start has failed, but its response is held while another
			// caller queues work against the now-idle checkout.
			_, competing, err := service.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutScan, "")
			require.NoError(t, err)
			require.NotEqual(t, first.ID, competing.ID)
			release()
			<-done
			require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			var problem api.Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			require.Equal(t, "internal", problem.Code)
			current, err := catalog.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, competing.ID, current.ActiveOperationID())
			require.Equal(t, "queued", current.OperationState)
		})
	}
}

func TestPhotoCheckoutJobsProjectLaneProgressResultsAndPausedCommit(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	var service checkout.Service
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor; service = api.NewPhotoCheckoutService(*d) })
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	second, err := catalog.CreateFile(t.Context(), catalog.RootID(), "second.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	secondAsset, err := catalog.PhotoAssetForNode(t.Context(), second.ID)
	require.NoError(t, err)
	c, _, _, root, err := service.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID, secondAsset.ID}}, t.TempDir(), 100)
	require.NoError(t, err)
	require.NoError(t, service.Run(t.Context(), c, root))
	require.NoError(t, root.Close())
	t.Run("missing destination", func(t *testing.T) {
		moved := c.Destination + "-moved"
		require.NoError(t, os.Rename(c.Destination, moved))
		defer func() { require.NoError(t, os.Rename(moved, c.Destination)) }()
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+c.ID+"/scan", nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
		require.Contains(t, body, "invalid_photo_checkout")
	})
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+c.ID+"/scan", nil, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var inspected api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &inspected))
	require.Eventually(t, func() bool {
		op, err := catalog.StorageOperation(t.Context(), inspected.Operation.ID)
		return err == nil && op.State == store.StorageOperationCompleted
	}, 30*time.Second, 20*time.Millisecond)
	response, body = do(t, ts, http.MethodGet, "/api/v1/jobs", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"processed_bytes":10`)
	require.Contains(t, body, `"checkout_results":{"clean":2}`)
	var inspectedJobs api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &inspectedJobs))
	for _, job := range inspectedJobs.Items {
		require.Equal(t, "photo_checkout", job.Lane)
	}
	response, body = do(t, ts, http.MethodGet, "/api/v1/jobs/"+inspected.Operation.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"checkout_results":{"clean":2}`)
	require.NotContains(t, body, `"entries":[`)

	_, err = catalog.SetLaneControl(t.Context(), store.LaneControl{Lane: "photo_checkout", Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+c.ID+"/commit", nil, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var committed api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &committed))
	response, body = do(t, ts, http.MethodGet, "/api/v1/jobs", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var list api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	found := false
	for _, job := range list.Items {
		if job.OperationID == committed.Operation.ID {
			found = true
			require.Equal(t, "photo_checkout", job.Lane)
			require.True(t, job.Controllable)
			require.True(t, job.Paused)
			require.Equal(t, "queued", job.Status)
		}
	}
	require.True(t, found, "commit operation must appear in Jobs")
}

// photoCheckoutStartGate holds the start-error boundary before response selection.
type photoCheckoutStartGate struct {
	slog.Handler

	entered chan struct{}
	release chan struct{}
}

func (*photoCheckoutStartGate) Enabled(context.Context, slog.Level) bool { return true }

func (g *photoCheckoutStartGate) Handle(ctx context.Context, record slog.Record) error {
	if record.Level == slog.LevelError {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
