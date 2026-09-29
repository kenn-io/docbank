package daemonconn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoMigrationClient(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "inventory")
	inventory := api.FotobankInventory{
		Report:       api.MigrationReport{Source: api.MigrationSource{Kind: "install", Identity: "catalog"}, CreatedAt: "2026-09-25T00:00:00Z"},
		ReportPath:   filepath.Join(outputDir, "report.json"),
		OwnerMapPath: filepath.Join(outputDir, "owner-map.json"),
	}
	server := migrationResponseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/migrations/fotobank/inventories" {
			http.NotFound(w, r)
			return
		}
		var request api.FotobankInventoryRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OutputDir != outputDir {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(inventory)
	})
	defer server.Close()
	created, err := New(server.URL, "key").CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{ArchiveRoot: "/archive", OutputDir: outputDir})
	require.NoError(t, err)
	require.Equal(t, inventory, created)
}

func TestPhotoMigrationClientRejectsMismatchedPaths(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "inventory")
	server := migrationResponseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.FotobankInventory{
			ReportPath:   filepath.Join(outputDir, "report.json"),
			OwnerMapPath: filepath.Join(t.TempDir(), "owner-map.json"),
		})
	})
	defer server.Close()

	_, err := New(server.URL, "key").CreateFotobankInventory(t.Context(), api.FotobankInventoryRequest{ArchiveRoot: "/archive", OutputDir: outputDir})
	require.Error(t, err)
	require.True(t, IsResponseDecodeError(err))
	require.ErrorContains(t, err, "do not match output directory")
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
