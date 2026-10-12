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
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = jobs.New(t.Context(), nil); t.Cleanup(d.Jobs.Stop) })
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/estimate", nil, selection)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var estimate store.PhotoCheckoutEstimate
	require.NoError(t, json.Unmarshal([]byte(body), &estimate))
	require.Equal(t, int64(1), estimate.Files)
	require.Equal(t, size, estimate.Bytes)
	request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: t.TempDir()}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "photo_checkout", created.Operation.Kind)
	require.Equal(t, created.Operation.ID, created.Checkout.OperationID)
	require.Eventually(t, func() bool {
		op, err := catalog.StorageOperation(t.Context(), created.Operation.ID)
		return err == nil && op.FinishedAt != nil
	}, 5*time.Second, 10*time.Millisecond)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/"+created.Checkout.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Equal(t, "completed", status.Checkout.State)
	require.Len(t, status.Entries, 1)
	require.True(t, status.Entries[0].Published)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var list api.PhotoCheckoutList
	require.NoError(t, json.Unmarshal([]byte(body), &list))
	require.Len(t, list.Items, 1)
	response, body = do(t, ts, http.MethodGet, "/api/v1/jobs/"+created.Operation.ID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"copied_bytes":11`)
	response, body = do(t, ts, http.MethodGet, "/api/v1/photos/checkouts/00000000-0000-4000-8000-000000000001", nil, nil)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{All: true}, Destination: t.TempDir()})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}

func TestPhotoCheckoutRoutesRequireDaemonCredentialsAndLocalCreation(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = jobs.New(t.Context(), nil); t.Cleanup(d.Jobs.Stop) })
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/photos/checkouts", nil},
		{http.MethodGet, "/api/v1/photos/checkouts/00000000-0000-4000-8000-000000000001", nil},
		{http.MethodPost, "/api/v1/photos/checkouts/estimate", store.PhotoCheckoutSelection{All: true}},
		{http.MethodPost, "/api/v1/photos/checkouts", api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{All: true}, Destination: t.TempDir(), ByteLimit: 100}},
	} {
		response, body := do(t, ts, tc.method, tc.path, browser, tc.body)
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
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

func TestPhotoCheckoutCreateReturnsPersistedStartFailure(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, func(d *api.Deps) {
		d.Jobs = jobs.New(t.Context(), nil)
		d.Jobs.Stop()
	})
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	request := api.PhotoCheckoutCreateRequest{Selection: store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, Destination: t.TempDir()}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "failed", created.Checkout.State)
	require.Equal(t, "failed", created.Operation.State)
	require.NotEmpty(t, created.Operation.FinishedAt)
	require.Equal(t, created.Checkout.Error, created.Operation.Error)
	require.Contains(t, created.Operation.Error, "stopping")
	status, err := catalog.PhotoCheckout(t.Context(), created.Checkout.ID)
	require.NoError(t, err)
	require.Equal(t, created.Checkout, status)
}
