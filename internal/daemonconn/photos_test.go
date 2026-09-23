package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

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
