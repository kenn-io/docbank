package store_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionJobStatusHTTPScopedAndMinimal(t *testing.T) {
	vault, root, setID, jobID := store.ProductionJobHTTPFixture(t)
	stored, err := vault.ProductionJobStatus(t.Context(), setID, jobID)
	require.NoError(t, err)
	require.Equal(t, "queued", stored.State)
	require.Equal(t, jobID, stored.JobID)
	require.Empty(t, stored.ReceiptSHA256)
	_, err = vault.ProductionJobStatus(t.Context(), "89000000-0000-4000-8000-000000000099", jobID)
	require.ErrorIs(t, err, store.ErrNotFound)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	get := func(path string, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+path, nil)
		require.NoError(t, err)
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	path := "/api/v1/productions/sets/" + setID + "/jobs/" + jobID
	status, _ := get(path, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := get(path, true)
	require.Equal(t, http.StatusOK, status, string(body))
	var result api.ProductionJobStatus
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, stored, store.ProductionJobStatus(result))
	require.NotContains(t, string(body), "manifest")
	require.NotContains(t, string(body), "endorsements")
	status, _ = get("/api/v1/productions/sets/89000000-0000-4000-8000-000000000099/jobs/"+jobID, true)
	require.Equal(t, http.StatusNotFound, status)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientStatus, err := client.ProductionJobStatus(t.Context(), setID, jobID)
	require.NoError(t, err)
	require.Equal(t, result, clientStatus)
	require.NoError(t, vault.CancelProductionJob(t.Context(), jobID))
	canceled, err := client.ProductionJobStatus(t.Context(), setID, jobID)
	require.NoError(t, err)
	require.Equal(t, "canceled", canceled.State)
	require.Empty(t, canceled.ReceiptSHA256)
}
