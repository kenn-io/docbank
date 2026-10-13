package api_test

import (
	"encoding/json/v2"
	"net/http"
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
	var failed api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(body), &failed))
	storedCheckout, storedOperation, err := catalog.PhotoCheckoutOperation(t.Context(), failed.Checkout.ID)
	require.NoError(t, err)
	require.Equal(t, storedCheckout, failed.Checkout)
	var receipt any
	require.NoError(t, json.Unmarshal([]byte(storedOperation.ReceiptJSON), &receipt))
	expectedOperation := api.StorageOperation{
		ID: storedOperation.ID, Kind: storedOperation.Kind, State: string(storedOperation.State),
		PlanDigest: storedOperation.RequestDigest, TotalObjects: storedOperation.TotalObjects,
		CompletedObjects: storedOperation.CompletedObjects, CopiedObjects: storedOperation.CopiedObjects,
		CopiedBytes: storedOperation.CopiedBytes, CancelRequested: storedOperation.CancelRequested,
		Error: storedOperation.Error, Receipt: receipt,
		CreatedAt: storedOperation.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: storedOperation.UpdatedAt.Format(time.RFC3339Nano),
		FinishedAt: storedOperation.FinishedAt.Format(time.RFC3339Nano),
	}
	expectedJSON, err := json.Marshal(expectedOperation)
	require.NoError(t, err)
	var expectedResponse api.StorageOperation
	require.NoError(t, json.Unmarshal(expectedJSON, &expectedResponse))
	require.Equal(t, expectedResponse, failed.Operation)
}
