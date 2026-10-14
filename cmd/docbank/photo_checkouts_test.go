package main

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutCLICreateEstimateListStatusAndYears(t *testing.T) {
	const checkoutID = "00000000-0000-4000-8000-000000000001"
	const operationID = "00000000-0000-4000-8000-000000000002"
	destination := filepath.Join("relative", "checkout")
	absoluteDestination, err := filepath.Abs(destination)
	require.NoError(t, err)
	checkout := store.PhotoCheckout{ID: checkoutID, OperationID: operationID, State: "queued", Selection: store.PhotoCheckoutSelection{All: true, AssetIDs: []string{checkoutID}, AlbumIDs: []string{operationID}}, Destination: absoluteDestination, ByteLimit: 100000}
	created := api.PhotoCheckoutCreateResult{Checkout: checkout, Operation: api.StorageOperation{ID: operationID, Kind: "photo_checkout", State: "queued"}}
	listed := []store.PhotoCheckout{checkout}
	status := api.PhotoCheckoutStatus{Checkout: checkout, Files: 7, Bytes: 101, PublishedFiles: 3, PublishedBytes: 42}
	estimate := store.PhotoCheckoutEstimate{Assets: 2, Files: 3, Bytes: 42}
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
			assert.NoError(t, json.MarshalWrite(w, status))
		case "/api/v1/photos/checkouts/estimate":
			var input store.PhotoCheckoutSelection
			assert.NoError(t, json.UnmarshalRead(r.Body, &input))
			estimates <- input
			assert.NoError(t, json.MarshalWrite(w, estimate))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := runCLI(t, "photos", "checkout", "create", destination, "--all", "--asset", checkoutID, "--album", operationID, "--byte-limit", "100000")
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
	var listOutput []store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &listOutput))
	require.Equal(t, listed, listOutput)
	out, err = runCLI(t, "photos", "checkout", "status", checkoutID)
	require.NoError(t, err, out)
	var statusOutput api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(out), &statusOutput))
	require.Equal(t, status, statusOutput)
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

func TestPhotoCheckoutCLIImportsAndCopiesGroupedPhotosThroughDaemon(t *testing.T) {
	setupVaultHome(t)
	source, destination := t.TempDir(), t.TempDir()
	files := map[string]string{
		"IMG.ARW":     "synthetic RAW bytes",
		"IMG.JPG":     "synthetic JPEG bytes",
		"IMG.ARW.XMP": `<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`,
	}
	var total int64
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(source, name), []byte(content), 0600))
		total += int64(len(content))
	}
	out, err := runCLI(t, "photos", "import", source, "/photos", "--json")
	require.NoError(t, err, out)
	var imported api.StorageOperation
	require.NoError(t, json.Unmarshal([]byte(out), &imported))
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "jobs", "show", imported.ID, "--json")
		require.NoError(t, err, out)
		require.NoError(t, json.Unmarshal([]byte(out), &imported))
		return imported.FinishedAt != ""
	}, 15*time.Second, 25*time.Millisecond)
	require.Equal(t, "completed", imported.State, imported.Error)
	out, err = runCLI(t, "photos", "checkout", "create", destination, "--all", "--byte-limit", "1000000")
	require.NoError(t, err, out)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	var status api.PhotoCheckoutStatus
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "photos", "checkout", "status", created.Checkout.ID)
		require.NoError(t, err, out)
		require.NoError(t, json.Unmarshal([]byte(out), &status))
		return status.Checkout.State != "queued" && status.Checkout.State != "running"
	}, 15*time.Second, 25*time.Millisecond)
	require.Equal(t, "completed", status.Checkout.State, status.Checkout.Error)
	require.Equal(t, int64(3), status.Files)
	require.Equal(t, int64(3), status.PublishedFiles)
	require.Equal(t, total, status.PublishedBytes)
	var parent string
	require.NoError(t, filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		name := entry.Name()
		content, ok := files[name]
		require.True(t, ok, path)
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, content, string(actual))
		if parent == "" {
			parent = filepath.Dir(path)
		}
		require.Equal(t, parent, filepath.Dir(path))
		delete(files, name)
		return nil
	}))
	require.Empty(t, files)
}
