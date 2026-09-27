package store_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionReproductionHTTPReadsVerifiedExactReceipt(t *testing.T) {
	vault, root, jobID, expected := store.PublishedProductionReproductionHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	get := func(job, operation, key string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			httpServer.URL+"/api/v1/productions/jobs/"+job+"/reproductions/"+operation, nil)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", key)
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	status, _ := get(jobID, expected.ID, "")
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := get(jobID, expected.ID, cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, status, string(body))
	var receipt documentproduction.ReproductionReceipt
	require.NoError(t, json.Unmarshal(body, &receipt))
	require.Equal(t, expected, receipt)
	require.NoError(t, documentproduction.ValidateReproductionReceipt(receipt))
	require.Zero(t, receipt.NumberAllocationCount)
	status, _ = get(jobID, "88000000-0000-4000-8000-000000000019", cfg.Server.APIKey)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = get("88000000-0000-4000-8000-000000000020", expected.ID, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientReceipt, err := connection.ProductionReproduction(t.Context(), jobID, expected.ID)
	require.NoError(t, err)
	require.Equal(t, expected, clientReceipt)
}
