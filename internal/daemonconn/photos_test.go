package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoClientAcceptsCreateTimestampsAndNoOpMutations(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 1,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 1, Role: "raw", CreatedAt: createdAt}},
	}
	settings := api.PhotoSettings{Revision: 1, UpdatedAt: createdAt}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON := func(value any) {
			body, err := json.Marshal(value)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(body)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/assets":
			w.Header().Set("ETag", `"1"`)
			w.WriteHeader(http.StatusCreated)
			writeJSON(asset)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/photos/assets/"+assetID+"/exclude":
			excludedAt := createdAt
			asset.ExcludedAt = &excludedAt
			w.Header().Set("ETag", `"1"`)
			writeJSON(asset)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/photos/assets/"+assetID+"/display":
			w.Header().Set("ETag", `"1"`)
			writeJSON(asset)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/photos/settings":
			w.Header().Set("ETag", `"1"`)
			writeJSON(settings)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := New(server.URL, "synthetic-key")
	created, err := client.CreatePhotoAsset(t.Context(), 1, "raw", "photo")
	require.NoError(t, err)
	require.Equal(t, createdAt, created.CreatedAt)
	require.Equal(t, createdAt, created.UpdatedAt)

	excluded, err := client.ExcludePhotoAsset(t.Context(), assetID, 1, true)
	require.NoError(t, err)
	require.NotNil(t, excluded.ExcludedAt)
	_, err = client.SetPhotoDisplay(t.Context(), assetID, 1, nil)
	require.NoError(t, err)
	_, err = client.SetPhotoSettings(t.Context(), 1, nil)
	require.NoError(t, err)
}

func TestPhotoNodeAddressedResponsesMustContainTheNode(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	fileID := "00000000-0000-4000-8000-000000000010"
	createdAt := "2026-09-22T00:00:00Z"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 2,
		DisplayFileID: &fileID, DisplaySource: "default",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: 1, Role: "image", CreatedAt: createdAt}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		body, err := json.Marshal(asset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")

	_, err := client.PhotoAssetForNode(t.Context(), 2)
	require.ErrorContains(t, err, "does not contain node 2")
	_, err = client.AttachPhotoFile(t.Context(), assetID, 1, 2, "image", nil)
	require.True(t, IsResponseDecodeError(err))
	_, err = client.DetachPhotoFile(t.Context(), assetID, 1, fileID, false)
	require.True(t, IsResponseDecodeError(err))
	_, err = client.AttachPhotoFile(t.Context(), assetID, 1, 1, "image", nil)
	require.NoError(t, err)
	for _, values := range []struct {
		name      string
		confirmed store.PhotoAuthoredFields
		rating    int
	}{
		{name: "unknown confirmation", confirmed: 128},
		{name: "unconfirmed value", rating: 3},
	} {
		t.Run(values.name, func(t *testing.T) {
			asset.Files[0].Confirmed = values.confirmed
			asset.Files[0].Rating = values.rating
			_, err := client.PhotoAssetForNode(t.Context(), 1)
			require.ErrorContains(t, err, "invalid authored values")
			_, err = client.AttachPhotoFile(t.Context(), assetID, 1, 1, "image", nil)
			require.True(t, IsResponseDecodeError(err))
		})
	}
}

func TestPhotoImportClients(t *testing.T) {
	operation := api.StorageOperation{ID: "00000000-0000-4000-8000-000000000021", Kind: "photo_import", State: "queued",
		CreatedAt: "2026-09-22T00:00:00Z", UpdatedAt: "2026-09-22T00:00:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/photos/imports" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		body, err := json.Marshal(operation)
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := New(server.URL, "synthetic-key")
	sourceRoot := filepath.Join(t.TempDir(), "camera")
	started, err := client.StartPhotoImport(t.Context(), sourceRoot, "/photos")
	require.NoError(t, err)
	assert.Equal(t, operation.ID, started.ID)

	for _, malformed := range []api.StorageOperation{{}, {ID: operation.ID, Kind: "repair", State: "queued"},
		{ID: operation.ID, Kind: "photo_import", State: "completed", FinishedAt: "2026-09-22T00:00:00Z"}} {
		operation = malformed
		_, err = client.StartPhotoImport(t.Context(), sourceRoot, "/photos")
		require.Error(t, err)
		assert.True(t, IsResponseDecodeError(err))
	}
}
