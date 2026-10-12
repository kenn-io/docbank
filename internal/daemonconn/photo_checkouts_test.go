package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts/estimate":
			var request store.PhotoCheckoutSelection
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			require.Equal(t, selection, request)
			out = store.PhotoCheckoutEstimate{Assets: 1, Files: 1, Bytes: 5}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/checkouts":
			var request api.PhotoCheckoutCreateRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			require.Equal(t, checkout.Destination, request.Destination)
			out = created
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts":
			out = api.PhotoCheckoutList{Items: []store.PhotoCheckout{checkout}}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/photos/checkouts/"+id:
			out = api.PhotoCheckoutStatus{Checkout: checkout, Entries: []store.PhotoCheckoutEntry{}}
		default:
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.MarshalWrite(w, out))
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")
	estimate, err := client.EstimatePhotoCheckout(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, int64(5), estimate.Bytes)
	result, err := client.CreatePhotoCheckout(t.Context(), api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination, ByteLimit: 100})
	require.NoError(t, err)
	require.Equal(t, created, result)
	list, err := client.PhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Equal(t, []store.PhotoCheckout{checkout}, list)
	status, err := client.PhotoCheckoutStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, checkout, status.Checkout)
	created.Checkout.State = "failed"
	created.Checkout.Error = "supervisor is stopping"
	created.Operation.State = "failed"
	created.Operation.Error = created.Checkout.Error
	created.Operation.FinishedAt = "2026-01-01T00:00:00Z"
	result, err = client.CreatePhotoCheckout(t.Context(), api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination, ByteLimit: 100})
	require.NoError(t, err)
	require.Equal(t, created, result)
	for _, invalid := range []api.PhotoCheckoutCreateResult{{}, {Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "repair", State: "queued"}}} {
		created = invalid
		_, err = client.CreatePhotoCheckout(t.Context(), api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination, ByteLimit: 100})
		require.Error(t, err)
	}
	_, err = client.PhotoCheckoutStatus(t.Context(), "bad")
	require.Error(t, err)
	_, err = client.CreatePhotoCheckout(t.Context(), api.PhotoCheckoutCreateRequest{Selection: selection, Destination: checkout.Destination})
	require.Error(t, err)
}
