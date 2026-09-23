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

func TestPhotoMutationClientsRejectWrongMembership(t *testing.T) {
	const (
		assetID = "00000000-0000-4000-8000-000000000001"
		fileID  = "00000000-0000-4000-8000-000000000010"
		created = "2026-09-22T00:00:00Z"
	)
	asset := func(revision, nodeID int64, role string) api.PhotoAsset {
		return api.PhotoAsset{
			ID: assetID, Kind: "photo", Revision: revision,
			DisplayFileID: new(fileID), DisplaySource: "default",
			CreatedAt: created, UpdatedAt: created,
			Files: []api.PhotoFile{{ID: fileID, AssetID: assetID, NodeID: nodeID, Role: role, CreatedAt: created}},
		}
	}
	run := func(t *testing.T, status int, response api.PhotoAsset, wantError bool, call func(*Connection) error) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", photoIfMatch(response.Revision))
			body, err := json.Marshal(response)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}))
		t.Cleanup(server.Close)
		err := call(New(server.URL, "synthetic-key"))
		if wantError {
			require.Error(t, err)
			require.True(t, IsResponseDecodeError(err))
		} else {
			require.NoError(t, err)
		}
	}

	t.Run("create accepts requested member", func(t *testing.T) {
		run(t, http.StatusCreated, asset(1, 1, "raw"), false, func(c *Connection) error {
			_, err := c.CreatePhotoAsset(t.Context(), 1, "raw", "photo")
			return err
		})
	})
	t.Run("create returns another node", func(t *testing.T) {
		run(t, http.StatusCreated, asset(1, 2, "raw"), true, func(c *Connection) error {
			_, err := c.CreatePhotoAsset(t.Context(), 1, "raw", "photo")
			return err
		})
	})
	t.Run("create returns another kind", func(t *testing.T) {
		response := asset(1, 1, "image")
		response.Kind = "video"
		run(t, http.StatusCreated, response, true, func(c *Connection) error {
			_, err := c.CreatePhotoAsset(t.Context(), 1, "image", "photo")
			return err
		})
	})
	t.Run("attach returns another role", func(t *testing.T) {
		run(t, http.StatusOK, asset(2, 2, "image"), true, func(c *Connection) error {
			_, err := c.AttachPhotoFile(t.Context(), assetID, 1, 2, "raw", nil)
			return err
		})
	})
	t.Run("attach returns another sidecar target", func(t *testing.T) {
		rawID := "00000000-0000-4000-8000-000000000011"
		sidecarID := "00000000-0000-4000-8000-000000000012"
		response := api.PhotoAsset{
			ID: assetID, Kind: "photo", Revision: 2,
			DisplayFileID: &rawID, DisplaySource: "default",
			CreatedAt: created, UpdatedAt: created,
			Files: []api.PhotoFile{
				{ID: rawID, AssetID: assetID, NodeID: 1, Role: "raw", CreatedAt: created},
				{ID: sidecarID, AssetID: assetID, NodeID: 2, Role: "sidecar", CreatedAt: created},
			},
		}
		run(t, http.StatusOK, response, true, func(c *Connection) error {
			_, err := c.AttachPhotoFile(t.Context(), assetID, 1, 2, "sidecar", &rawID)
			return err
		})
	})
	t.Run("detach retains the file", func(t *testing.T) {
		run(t, http.StatusOK, asset(2, 1, "image"), true, func(c *Connection) error {
			_, err := c.DetachPhotoFile(t.Context(), assetID, 1, fileID, false)
			return err
		})
	})
	t.Run("promote returns another role", func(t *testing.T) {
		run(t, http.StatusOK, asset(1, 1, "image"), true, func(c *Connection) error {
			_, err := c.PromotePhotoNode(t.Context(), 1, nil, "raw", "photo")
			return err
		})
	})
	t.Run("promote preserves sidecar target", func(t *testing.T) {
		rawID := "00000000-0000-4000-8000-000000000011"
		sidecarID := "00000000-0000-4000-8000-000000000012"
		response := api.PhotoAsset{
			ID: assetID, Kind: "photo", Revision: 1,
			DisplayFileID: &rawID, DisplaySource: "default",
			CreatedAt: created, UpdatedAt: created,
			Files: []api.PhotoFile{
				{ID: rawID, AssetID: assetID, NodeID: 1, Role: "raw", CreatedAt: created},
				{ID: sidecarID, AssetID: assetID, NodeID: 2, Role: "sidecar", SidecarOfID: &rawID, CreatedAt: created},
			},
		}
		revision := int64(1)
		run(t, http.StatusOK, response, false, func(c *Connection) error {
			_, err := c.PromotePhotoNode(t.Context(), 2, &revision, "sidecar", "photo")
			return err
		})
	})
}
