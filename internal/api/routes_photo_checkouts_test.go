package api_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
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
	expectedStatus, err := catalog.PhotoCheckoutStatus(t.Context(), created.Checkout.ID)
	require.NoError(t, err)
	require.Equal(t, expectedStatus, status)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var list api.PhotoCheckoutList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	expectedList, err := catalog.ListPhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Equal(t, expectedList, list.Items)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/"+created.Checkout.ID+"/retire", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
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
