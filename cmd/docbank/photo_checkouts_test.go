package main

import (
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutCLICreateEstimateListRetireAndYears(t *testing.T) {
	const checkoutID = "00000000-0000-4000-8000-000000000001"
	const operationID = "00000000-0000-4000-8000-000000000002"
	destination := filepath.Join("relative", "checkout")
	absoluteDestination, err := filepath.Abs(destination)
	require.NoError(t, err)
	checkout := store.PhotoCheckout{ID: checkoutID, OperationID: operationID, State: "queued", Selection: store.PhotoCheckoutSelection{AssetIDs: []string{checkoutID}, AlbumIDs: []string{operationID}}, Destination: absoluteDestination, ByteLimit: 100000}
	skipped := []store.PhotoCheckoutSkip{{AssetID: operationID, FileID: checkoutID, Name: "duplicate.jpg", Reason: "duplicate filename"}}
	checkout.SkippedPhotos, checkout.SkippedPhotoCount = skipped, 1
	created := api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}
	listed := []store.PhotoCheckout{checkout}
	retired := checkout
	retired.State = "completed"
	retired.RetentionReleased = true
	retiredAt := "2026-01-01T00:00:00Z"
	retired.RetiredAt = &retiredAt
	estimate := store.PhotoCheckoutEstimate{Assets: 2, Files: 3, Bytes: 42, SkippedPhotos: skipped, SkippedPhotoCount: 1}
	inputs := make(chan api.PhotoCheckoutCreateRequest, 1)
	estimates := make(chan store.PhotoCheckoutSelection, 1)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/photos/checkouts":
			if r.Method == http.MethodPost {
				var input api.PhotoCheckoutCreateRequest
				assert.NoError(t, json.UnmarshalRead(r.Body, &input))
				inputs <- input
				w.WriteHeader(http.StatusAccepted)
				assert.NoError(t, json.MarshalWrite(w, created))
			} else {
				assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutList{Items: listed}))
			}
		case "/api/v1/photos/checkouts/" + checkoutID:
			assert.NoError(t, json.MarshalWrite(w, api.PhotoCheckoutStatus{Checkout: checkout}))
		case "/api/v1/photos/checkouts/" + checkoutID + "/retire":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.NoError(t, json.MarshalWrite(w, retired))
		case "/api/v1/photos/checkouts/estimate":
			var input store.PhotoCheckoutSelection
			assert.NoError(t, json.UnmarshalRead(r.Body, &input))
			estimates <- input
			assert.NoError(t, json.MarshalWrite(w, estimate))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := runCLI(t, "photos", "checkout", "create", destination, "--asset", checkoutID, "--album", operationID, "--byte-limit", "100000")
	require.NoError(t, err, out)
	var createOutput api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(out), &createOutput))
	require.Equal(t, created, createOutput)
	require.Len(t, inputs, 1)
	input := <-inputs
	require.Equal(t, checkout.Selection, input.Selection)
	require.Equal(t, int64(100000), input.ByteLimit)
	require.Equal(t, absoluteDestination, input.Destination)
	out, err = runCLI(t, "photos", "checkout", "list")
	require.NoError(t, err, out)
	summary, out, found := strings.Cut(out, "\n")
	require.True(t, found)
	require.Equal(t, checkoutID+": 1 photos skipped", summary)
	var listOutput []store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &listOutput))
	require.Equal(t, listed, listOutput)
	out, err = runCLI(t, "photos", "checkout", "status", checkoutID)
	require.NoError(t, err, out)
	summary, out, found = strings.Cut(out, "\n")
	require.True(t, found)
	require.Equal(t, "1 photos skipped", summary)
	var statusOutput api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(out), &statusOutput))
	require.Equal(t, checkout, statusOutput.Checkout)
	out, err = runCLI(t, "photos", "checkout", "retire", checkoutID)
	require.NoError(t, err, out)
	var retireOutput store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &retireOutput))
	require.Equal(t, retired, retireOutput)
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
			require.ErrorContains(t, err, "years must be YYYY or YYYY:YYYY", out)
		} else {
			require.NoError(t, err, out)
			var estimateOutput store.PhotoCheckoutEstimate
			require.NoError(t, json.Unmarshal([]byte(out), &estimateOutput))
			require.Equal(t, estimate, estimateOutput)
			require.Len(t, estimates, 1)
			received := <-estimates
			received.AssetIDs = append([]string(nil), received.AssetIDs...)
			received.AlbumIDs = append([]string(nil), received.AlbumIDs...)
			require.Equal(t, store.PhotoCheckoutSelection{Years: tc.expected}, received)
		}
	}
}

func TestPhotoCheckoutCLIScanCommitRebind(t *testing.T) {
	const id = "aaaaaaaa-0000-4000-8000-000000000001"
	const operationID = "00000000-0000-4000-8000-000000000002"
	destination, err := filepath.Abs("checkout-edits")
	require.NoError(t, err)
	committed := api.PhotoCheckoutCreateResult{Checkout: store.PhotoCheckout{ID: id, OperationID: operationID, LastOperationID: operationID, OperationKind: "photo_checkout_commit", State: "completed", OperationState: "queued", Selection: store.PhotoCheckoutSelection{All: true}, ByteLimit: 100, Destination: destination}, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout_commit", State: "queued"}}
	seen := make(chan string, 3)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/photos/checkouts/" + id + "/scan":
			seen <- "scan"
			out := committed
			out.Checkout.LastOperationID, out.Checkout.OperationKind = operationID, "photo_checkout_scan"
			out.Operation.Kind = "photo_checkout_scan"
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.MarshalWrite(w, out))
		case "/api/v1/photos/checkouts/" + id + "/commit":
			seen <- "commit"
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.MarshalWrite(w, committed))
		case "/api/v1/photos/checkouts/" + id + "/rebind":
			var request api.PhotoCheckoutRebindRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, destination, request.Destination)
			seen <- "rebind"
			out := committed
			out.Checkout.LastOperationID, out.Checkout.OperationKind = operationID, "photo_checkout_rebind"
			out.Operation.Kind = "photo_checkout_rebind"
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.MarshalWrite(w, out))
		default:
			http.NotFound(w, r)
		}
	})
	for _, action := range []string{"scan", "commit", "rebind"} {
		t.Run(action, func(t *testing.T) {
			args := []string{"photos", "checkout", action, strings.ToUpper(id)}
			if action == "rebind" {
				args = append(args, "checkout-edits")
			}
			out, err := runCLI(t, args...)
			require.NoError(t, err, out)
			require.Equal(t, action, <-seen)
			if action == "commit" {
				require.Contains(t, out, "photo_checkout_commit")
			} else {
				require.Contains(t, out, "photo_checkout_"+action)
				require.Contains(t, out, operationID)
			}
		})
	}
}

func TestPhotoCheckoutCLIEmptyRebind(t *testing.T) {
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected daemon request", http.StatusInternalServerError)
	})
	_, err := runCLI(t, "photos", "checkout", "rebind", "00000000-0000-4000-8000-000000000001", "")
	require.ErrorContains(t, err, "destination must not be empty")
}
