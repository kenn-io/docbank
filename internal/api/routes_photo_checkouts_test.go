package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
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
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
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
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, request.ByteLimit, created.Checkout.ByteLimit)
	require.Equal(t, request.Selection, created.Checkout.Selection)
	require.Equal(t, request.Destination, created.Checkout.Destination)
	registered := supervisor.Snapshot()
	require.Len(t, registered, 1)
	require.Equal(t, "storage:"+created.Operation.ID, registered[0].Name)
	entries, err := catalog.PhotoCheckoutEntries(t.Context(), created.Checkout.ID)
	require.NoError(t, err)
	require.NoError(t, catalog.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
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
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	for _, endpoint := range []struct {
		method string
		path   string
		input  any
	}{
		{http.MethodPost, "/api/v1/photos/checkouts/estimate", selection},
		{http.MethodPost, "/api/v1/photos/checkouts", request},
		{http.MethodGet, "/api/v1/photos/checkouts", nil},
		{http.MethodGet, "/api/v1/photos/checkouts/" + created.Checkout.ID, nil},
	} {
		response, body = do(t, ts, endpoint.method, endpoint.path, browser, endpoint.input)
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
}
