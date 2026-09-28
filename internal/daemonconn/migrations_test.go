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
	run := api.MigrationRun{ID: "123e4567-e89b-42d3-a456-426614174099", Source: api.MigrationSource{Kind: "install", Identity: "catalog"}, CreatedAt: "2026-09-25T00:00:00Z", OwnerMapPath: "/tmp/owner-map.json"}
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

func TestPhotoMigrationClientRejectsUnboundResponses(t *testing.T) {
	const requestOwnerMapPath = "/tmp/owner-map.json"
	const requestedID = "123e4567-e89b-42d3-a456-426614174099"

	t.Run("create missing ID", func(t *testing.T) {
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/migrations/fotobank/inventories" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.MigrationRun{OwnerMapPath: requestOwnerMapPath})
		})
		defer server.Close()

		_, err := New(server.URL, "key").CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{OwnerMapPath: requestOwnerMapPath})
		require.Error(t, err)
		require.True(t, IsResponseDecodeError(err))
		require.ErrorContains(t, err, "canonical UUIDv4")
	})

	t.Run("create noncanonical ID", func(t *testing.T) {
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/migrations/fotobank/inventories" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.MigrationRun{ID: "123E4567-E89B-42D3-A456-426614174099", OwnerMapPath: requestOwnerMapPath})
		})
		defer server.Close()

		_, err := New(server.URL, "key").CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{OwnerMapPath: requestOwnerMapPath})
		require.Error(t, err)
		require.True(t, IsResponseDecodeError(err))
		require.ErrorContains(t, err, "canonical UUIDv4")
	})

	t.Run("create mismatched owner map path", func(t *testing.T) {
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/migrations/fotobank/inventories" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.MigrationRun{ID: requestedID, OwnerMapPath: "/tmp/other-owner-map.json"})
		})
		defer server.Close()

		_, err := New(server.URL, "key").CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{OwnerMapPath: requestOwnerMapPath})
		require.Error(t, err)
		require.True(t, IsResponseDecodeError(err))
		require.ErrorContains(t, err, "owner-map path")
	})

	t.Run("show mismatched ID", func(t *testing.T) {
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/migrations/runs/"+requestedID {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(api.MigrationRun{ID: "123e4567-e89b-42d3-a456-426614174090"})
		})
		defer server.Close()

		_, err := New(server.URL, "key").MigrationRun(t.Context(), requestedID)
		require.Error(t, err)
		require.False(t, IsResponseDecodeError(err))
		require.ErrorContains(t, err, "does not match requested ID")
	})

	t.Run("noncanonical requested ID sends no request", func(t *testing.T) {
		var requests int
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			requests++
			http.NotFound(w, r)
		})
		defer server.Close()

		_, err := New(server.URL, "key").MigrationRun(t.Context(), "123E4567-E89B-42D3-A456-426614174099")
		require.Error(t, err)
		require.ErrorContains(t, err, "canonical UUIDv4")
		require.Zero(t, requests)
	})

	t.Run("list rejects noncanonical item ID", func(t *testing.T) {
		server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/migrations/runs" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(api.MigrationRunPage{Total: 1, Items: []api.MigrationRun{{ID: "123E4567-E89B-42D3-A456-426614174099"}}})
		})
		defer server.Close()

		_, err := New(server.URL, "key").ListMigrationRuns(t.Context(), 0, 10)
		require.Error(t, err)
		require.False(t, IsResponseDecodeError(err))
		require.ErrorContains(t, err, "canonical UUIDv4")
	})
}

func migrationResponseServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "unexpected API key", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
}
