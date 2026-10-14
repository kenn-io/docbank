package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutClients(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-4000-8000-000000000011"
	const operationID = "00000000-0000-4000-8000-000000000012"
	selection := store.PhotoCheckoutSelection{All: true, AssetIDs: []string{"00000000-0000-4000-8000-000000000021"}, AlbumIDs: []string{"00000000-0000-4000-8000-000000000022"}, Years: &store.PhotoCheckoutYears{From: 2024, To: 2025}}
	checkout := store.PhotoCheckout{ID: id, Selection: selection, ByteLimit: 100, Destination: filepath.Join(t.TempDir(), "destination"), OperationID: operationID, State: "queued"}
	expectedEstimate := store.PhotoCheckoutEstimate{Assets: 1, Files: 2, Bytes: 5}
	expectedStatus := api.PhotoCheckoutStatus{Checkout: checkout, Files: 2, Bytes: 5, PublishedFiles: 1, PublishedBytes: 3}
	created := api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}
	var mu sync.Mutex
	response := created
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts/estimate":
			out = expectedEstimate
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts":
			out = response
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts":
			out = api.PhotoCheckoutList{Items: []store.PhotoCheckout{checkout}}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts/"+id:
			out = expectedStatus
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.MarshalWrite(w, out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")
	create := func(expected api.PhotoCheckoutCreateResult) (api.PhotoCheckoutCreateResult, error) {
		mu.Lock()
		response = expected
		mu.Unlock()
		request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination, ByteLimit: 100}
		return client.CreatePhotoCheckout(t.Context(), request)
	}
	estimate, err := client.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, expectedEstimate, estimate)
	result, err := create(created)
	require.NoError(t, err)
	require.Equal(t, created, result)
	list, err := client.PhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Equal(t, []store.PhotoCheckout{checkout}, list)
	status, err := client.PhotoCheckoutStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, expectedStatus, status)

	mismatchedState := created
	mismatchedState.Checkout.State = "running"
	unfinished := created
	unfinished.Checkout.State, unfinished.Operation.State = "completed", "completed"
	failedCreation := created
	failedCreation.Checkout.State, failedCreation.Operation.State = "failed", "failed"
	failedCreation.Checkout.Error = "checkout error"
	failedCreation.Operation.Error = failedCreation.Checkout.Error
	failedCreation.Operation.FinishedAt = "2026-01-01T00:00:00Z"
	result, err = create(failedCreation)
	require.NoError(t, err)
	require.Equal(t, failedCreation, result)
	failedCreation.Operation.Error = "different operation error"
	for _, invalid := range []struct {
		response api.PhotoCheckoutCreateResult
		message  string
	}{
		{api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "repair", State: "queued"}}, "invalid photo checkout creation operation"},
		{mismatchedState, "invalid photo checkout creation operation"},
		{unfinished, "terminal photo checkout creation has no finish time"},
		{failedCreation, "failed photo checkout creation has no terminal error"},
	} {
		_, err = create(invalid.response)
		require.ErrorContains(t, err, invalid.message)
	}
}
