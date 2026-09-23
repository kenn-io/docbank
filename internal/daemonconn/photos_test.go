package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
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

func TestPhotoClientValidatesSidecarTargetsAfterTraversal(t *testing.T) {
	assetID := "00000000-0000-4000-8000-000000000001"
	rawID := "00000000-0000-4000-8000-000000000010"
	sidecarID := "00000000-0000-4000-8000-000000000002"
	asset := api.PhotoAsset{
		ID: assetID, Kind: "photo", Revision: 1,
		DisplayFileID: &rawID, DisplaySource: "default",
		CreatedAt: "2026-09-22T00:00:00Z", UpdatedAt: "2026-09-22T00:00:00Z",
		Files: []api.PhotoFile{
			{ID: sidecarID, AssetID: assetID, NodeID: 2, Role: "sidecar",
				SidecarOfID: &rawID, CreatedAt: "2026-09-22T00:00:00Z"},
			{ID: rawID, AssetID: assetID, NodeID: 1, Role: "raw",
				CreatedAt: "2026-09-22T00:00:00Z"},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/photos/assets/"+assetID {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		body, err := json.Marshal(asset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	got, err := New(server.URL, "synthetic-key").PhotoAsset(t.Context(), assetID)
	require.NoError(t, err)
	require.Len(t, got.Files, 2)
}
