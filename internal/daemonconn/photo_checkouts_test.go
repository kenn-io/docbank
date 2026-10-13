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
	selection := store.PhotoCheckoutSelection{All: true}
	checkout := store.PhotoCheckout{ID: id, Selection: selection, ByteLimit: 100, Destination: filepath.Join(t.TempDir(), "destination"), OperationID: operationID, State: "queued"}
	created := api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}
	var mu sync.Mutex
	var estimateRequest store.PhotoCheckoutSelection
	var createRequest api.PhotoCheckoutCreateRequest
	var decodeErr error
	var received bool
	response := created
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts/estimate":
			var request store.PhotoCheckoutSelection
			decodeErr = json.UnmarshalRead(r.Body, &request)
			estimateRequest = request
			out = store.PhotoCheckoutEstimate{Assets: 1, Files: 1, Bytes: 5}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts":
			var request api.PhotoCheckoutCreateRequest
			decodeErr = json.UnmarshalRead(r.Body, &request)
			createRequest = request
			received = true
			out = response
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts":
			out = api.PhotoCheckoutList{Items: []store.PhotoCheckout{checkout}}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts/"+id:
			out = api.PhotoCheckoutStatus{Checkout: checkout}
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
		response, received, decodeErr = expected, false, nil
		mu.Unlock()
		request := api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination, ByteLimit: 100}
		result, err := client.CreatePhotoCheckout(t.Context(), request)
		mu.Lock()
		wasReceived, requestErr, actualRequest := received, decodeErr, createRequest
		mu.Unlock()
		require.True(t, wasReceived, "client did not send the create request")
		require.NoError(t, requestErr)
		require.Equal(t, request, actualRequest)
		return result, err
	}
	estimate, err := client.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, int64(5), estimate.Bytes)
	mu.Lock()
	estimateErr, actualEstimateRequest := decodeErr, estimateRequest
	mu.Unlock()
	require.NoError(t, estimateErr)
	require.Equal(t, selection, actualEstimateRequest)
	result, err := create(created)
	require.NoError(t, err)
	require.Equal(t, created, result)
	list, err := client.PhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Equal(t, []store.PhotoCheckout{checkout}, list)
	status, err := client.PhotoCheckoutStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, checkout, status.Checkout)

	invalidState := created
	invalidState.Checkout.State = "running"
	unfinished := created
	unfinished.Checkout.State, unfinished.Operation.State = "completed", "completed"
	mismatchedFailure := created
	mismatchedFailure.Checkout.State, mismatchedFailure.Operation.State = "failed", "failed"
	mismatchedFailure.Checkout.Error = "checkout error"
	mismatchedFailure.Operation.Error = "different operation error"
	mismatchedFailure.Operation.FinishedAt = "2026-01-01T00:00:00Z"
	for _, invalid := range []api.PhotoCheckoutCreateResult{
		{},
		{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "repair", State: "queued"}},
		invalidState, unfinished, mismatchedFailure,
	} {
		_, err = create(invalid)
		require.Error(t, err)
	}
	_, err = client.PhotoCheckoutStatus(t.Context(), "bad")
	require.Error(t, err)
}
