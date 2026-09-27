package store_test

import (
	"bytes"
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
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionSupplementHTTPCommitsExactContinuationAndReplays(t *testing.T) {
	vault, root, request := store.PublishedProductionSupplementHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/supplements"
	post := func(input production.SupplementRequest, key string) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(input)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", key)
		response, err := httpServer.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := post(request, "")
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var record production.SupplementRecord
	require.NoError(t, json.Unmarshal(body, &record))
	require.NoError(t, production.ValidateSupplementRecord(record))
	require.Equal(t, request.OperationID, record.OperationID)
	require.Equal(t, request.ParentJobID, record.ParentJobID)
	require.Greater(t, record.StartSequence, record.ParentEndSequence)
	status, replay := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	changed := request
	changed.ParentReceiptSHA256 = request.PreparedSHA256
	status, body = post(changed, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Contains(t, string(body), `"code":"production_supplement_conflict"`)
	get := func(operationID string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			httpServer.URL+path+"/"+operationID, nil)
		require.NoError(t, err)
		req.Header.Set("X-Api-Key", cfg.Server.APIKey)
		response, err := httpServer.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, readback := get(request.OperationID)
	require.Equal(t, http.StatusOK, status, string(readback))
	require.Equal(t, replay, readback)
	status, _ = get("78000000-0000-4000-8000-000000000020")
	require.Equal(t, http.StatusNotFound, status)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientRecord, err := connection.CreateProductionSupplement(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, record, clientRecord)
	clientReadback, err := connection.ProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, record, clientReadback)
}
