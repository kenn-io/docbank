package main

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestPhotoCheckoutCLICreateListStatus(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const checkoutID = "00000000-0000-4000-8000-000000000001"
	const operationID = "00000000-0000-4000-8000-000000000002"
	const token = "synthetic-proof-token"
	destination := filepath.Join("relative", "checkout")
	absoluteDestination, err := filepath.Abs(destination)
	require.NoError(t, err)
	checkout := store.PhotoCheckout{ID: checkoutID, OperationID: operationID, State: "queued", Selection: store.PhotoCheckoutSelection{All: true, AssetIDs: []string{checkoutID}, AlbumIDs: []string{operationID}}, Destination: absoluteDestination, ByteLimit: 100000}
	created := api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}
	listed := []store.PhotoCheckout{checkout}
	status := api.PhotoCheckoutStatus{Checkout: checkout}
	estimate := store.PhotoCheckoutEstimate{Assets: 2, Files: 3, Bytes: 42}
	statusRequests := make(chan struct{}, 1)
	listRequests := make(chan struct{}, 1)
	inputs := make(chan api.PhotoCheckoutCreateRequest, 1)
	estimates := make(chan store.PhotoCheckoutSelection, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
		case "/api/v1/photos/checkouts":
			if r.Method == http.MethodPost {
				var input api.PhotoCheckoutCreateRequest
				assert.NoError(t, json.UnmarshalRead(r.Body, &input))
				inputs <- input
				w.WriteHeader(http.StatusAccepted)
				assert.NoError(t, json.MarshalWrite(w, created))
			} else {
				listRequests <- struct{}{}
				assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutList{Items: listed}))
			}
		case "/api/v1/photos/checkouts/" + checkoutID:
			statusRequests <- struct{}{}
			assert.NoError(t, json.MarshalWrite(w, status))
		case "/api/v1/photos/checkouts/estimate":
			var input store.PhotoCheckoutSelection
			assert.NoError(t, json.UnmarshalRead(r.Body, &input))
			estimates <- input
			assert.NoError(t, json.MarshalWrite(w, estimate))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err = daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
	out, err := runCLI(t, "photos", "checkout", "create", destination, "--all", "--asset", checkoutID, "--album", operationID, "--byte-limit", "100000")
	require.NoError(t, err, out)
	var createdOutput api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(out), &createdOutput))
	require.Equal(t, created, createdOutput)
	require.Len(t, inputs, 1)
	input := <-inputs
	require.Equal(t, checkout.Selection, input.Selection)
	require.Equal(t, int64(100000), input.ByteLimit)
	require.Equal(t, absoluteDestination, input.Destination)
	out, err = runCLI(t, "photos", "checkout", "list")
	require.NoError(t, err, out)
	var listedOutput []store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &listedOutput))
	require.Equal(t, listed, listedOutput)
	require.Len(t, listRequests, 1)
	out, err = runCLI(t, "photos", "checkout", "status", checkoutID)
	require.NoError(t, err, out)
	var statusOutput api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(out), &statusOutput))
	require.Equal(t, status, statusOutput)
	require.Len(t, statusRequests, 1)
	for _, tc := range []struct {
		years     string
		expected  *store.PhotoCheckoutYears
		wantError bool
	}{
		{"2024:2025", &store.PhotoCheckoutYears{From: 2024, To: 2025}, false},
		{"2024", &store.PhotoCheckoutYears{From: 2024, To: 2024}, false},
		{"bad", nil, true},
	} {
		out, err := runCLI(t, "photos", "checkout", "estimate", "--years", tc.years)
		if tc.wantError {
			require.Error(t, err, out)
			require.Empty(t, estimates)
		} else {
			require.NoError(t, err, out)
			var estimateOutput store.PhotoCheckoutEstimate
			require.NoError(t, json.Unmarshal([]byte(out), &estimateOutput))
			require.Equal(t, estimate, estimateOutput)
			require.Len(t, estimates, 1)
			require.Equal(t, tc.expected, (<-estimates).Years)
		}
	}
}
