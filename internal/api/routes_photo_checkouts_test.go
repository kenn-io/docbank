package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/estimate", nil, selection)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: t.TempDir(), ByteLimit: 100}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, request.Selection, created.Checkout.Selection)
	require.Equal(t, request.Destination, created.Checkout.Destination)
	require.Equal(t, request.ByteLimit, created.Checkout.ByteLimit)
	require.Equal(t, "photo_checkout", created.Operation.Kind)
	require.Equal(t, created.Operation.State, created.Checkout.State)
	require.Equal(t, created.Operation.ID, created.Checkout.OperationID)
	require.Eventually(t, func() bool {
		op, err := catalog.StorageOperation(t.Context(), created.Operation.ID)
		return err == nil && op.FinishedAt != nil
	}, 5*time.Second, 10*time.Millisecond)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, `"entries"`)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var list api.PhotoCheckoutList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, created.Checkout.ID, list.Items[0].ID)
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
	require.NotEmpty(t, created.Operation.FinishedAt)
	require.Equal(t, created.Checkout.Error, created.Operation.Error)
	persisted, err := catalog.PhotoCheckout(t.Context(), created.Checkout.ID)
	require.NoError(t, err)
	require.Equal(t, created.Checkout, persisted)
}

func TestPhotoCheckoutCreationRequiresLoopback(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, nil)
	body, err := json.Marshal(api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{All: true}, Destination: filepath.Join(t.TempDir(), "destination"), ByteLimit: 100})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/photos/checkouts", bytes.NewReader(body))
	request.RemoteAddr = "192.0.2.10:10000"
	request.Header.Set("X-Api-Key", testAPIKey)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	catalog.Server.Handler().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "loopback_only")
	require.Contains(t, recorder.Body.String(), "daemon's computer")
	require.NotContains(t, recorder.Body.String(), "/uploads")
}
