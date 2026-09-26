package daemonconn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoMigrationClient(t *testing.T) {
	run := api.MigrationRun{ID: "123e4567-e89b-42d3-a456-426614174099", Source: api.MigrationSource{Kind: "install", Identity: "catalog"}, CreatedAt: "2026-09-25T00:00:00Z", OwnerMapPath: "C:\\owner-map.json"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "unexpected API key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/migrations/fotobank/inventories":
			var request api.FotobankInventoryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OwnerMapPath != "/tmp/owner-map.json" {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/migrations/runs":
			_ = json.NewEncoder(w).Encode(api.MigrationRunPage{Total: 1, Items: []api.MigrationRun{run}})
		case "/api/v1/migrations/runs/" + run.ID:
			_ = json.NewEncoder(w).Encode(run)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	connection := New(server.URL, "key")
	created, err := connection.CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{ArchiveRoot: "/archive", OwnerMapPath: "/tmp/owner-map.json"})
	require.NoError(t, err)
	require.Equal(t, run.ID, created.ID)
	page, err := connection.ListMigrationRuns(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	got, err := connection.MigrationRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.Equal(t, run.ID, got.ID)
}
