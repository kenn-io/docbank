package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
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
	ts, catalog := newTestServer(t, func(d *api.Deps) { d.Jobs = supervisor })
	_, err := catalog.SetLaneControl(t.Context(), store.LaneControl{Lane: store.StorageOperationKindPhotoCheckout, Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	hash, size, err := catalog.Blobs.Write(strings.NewReader("photo bytes"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	_, err = catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	var duplicates []store.PhotoAsset
	for _, folder := range []string{"first", "second"} {
		dir, err := catalog.Mkdir(t.Context(), catalog.RootID(), folder)
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), dir.ID, "duplicate.jpg", hash, size, "image/jpeg")
		require.NoError(t, err)
		photo, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		duplicates = append(duplicates, photo)
	}
	_, err = catalog.DetachPhotoFile(t.Context(), duplicates[1].ID, duplicates[1].Revision, duplicates[1].Files[0].ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = catalog.AttachPhotoFile(t.Context(), duplicates[0].ID, duplicates[0].Revision, duplicates[1].Files[0].NodeID, store.PhotoRoleImage, nil)
	require.NoError(t, err)
	selection := store.PhotoCheckoutSelection{All: true}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/checkouts/estimate", nil, selection)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var estimate store.PhotoCheckoutEstimate
	require.NoError(t, json.Unmarshal([]byte(body), &estimate))
	require.Equal(t, int64(1), estimate.Assets)
	require.Equal(t, int64(1), estimate.Files)
	require.Equal(t, int64(11), estimate.Bytes)
	require.Equal(t, int64(1), estimate.SkippedPhotoCount)
	require.Len(t, estimate.SkippedPhotos, 1)
	require.Equal(t, duplicates[0].ID, estimate.SkippedPhotos[0].AssetID)
	require.Equal(t, "duplicate.jpg", estimate.SkippedPhotos[0].Name)
	request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: t.TempDir(), ByteLimit: 100}
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/checkouts", nil, request)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, int64(1), created.SkippedPhotoCount)
	require.Equal(t, estimate.SkippedPhotos, created.SkippedPhotos)
	require.Equal(t, request.ByteLimit, created.Checkout.ByteLimit)
	require.Equal(t, request.Selection, created.Checkout.Selection)
	canonicalDestination, err := filepath.EvalSymlinks(request.Destination)
	require.NoError(t, err)
	require.Equal(t, canonicalDestination, created.Checkout.Destination)
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
