package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutRoutes(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), nil)
	t.Cleanup(supervisor.Stop)
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	remote := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/photos/checkouts", nil)
	remote.RemoteAddr = "192.0.2.20:1234"
	remote.Header.Set("X-Api-Key", testAPIKey)
	guarded := httptest.NewRecorder()
	catalog.Server.Handler().ServeHTTP(guarded, remote)
	require.Equal(t, http.StatusForbidden, guarded.Code)
	require.Contains(t, guarded.Body.String(), "daemon's computer")
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "other.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/estimate", nil, selection)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var estimate store.PhotoCheckoutEstimate
	require.NoError(t, json.Unmarshal([]byte(body), &estimate))
	expectedEstimate, err := catalog.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, expectedEstimate, estimate)
	request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: t.TempDir(), ByteLimit: 100}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, request.Selection, created.Checkout.Selection)
	require.Equal(t, request.ByteLimit, created.Checkout.ByteLimit)
	require.Equal(t, request.Destination, created.Checkout.Destination)
	// Wait for worker completion before TempDir cleanup.
	require.Eventually(t, func() bool {
		op, err := catalog.StorageOperation(t.Context(), created.Operation.ID)
		return err == nil && op.FinishedAt != nil
	}, 5*time.Second, 10*time.Millisecond)

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
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/00000000-0000-4000-8000-000000000001", nil, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{All: true}, Destination: t.TempDir()})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	supervisor.Stop()
	request.Destination = t.TempDir()
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "failed", created.Checkout.State)
	require.Equal(t, "failed", created.Operation.State)
}
