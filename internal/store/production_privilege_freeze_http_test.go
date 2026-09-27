package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/productiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPrivilegeFreezeHTTPRechecksStoredAuthority(t *testing.T) {
	root := t.TempDir()
	vault, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	draft := productiontest.SeedPrivilegeLogDraft(t, vault)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), vault,
		productionservice.PrivilegeLogValidationRequest{
			OperationID: "39393939-3939-4393-8393-393939393939",
			LogID:       draft.LogID, Revision: draft.Revision, ExpectedGeneration: draft.Generation,
			ValidatedAt: time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC),
		})
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	request := api.ProductionPrivilegeFreezeRequest{
		OperationID:          "40404040-4040-4404-8404-404040404040",
		ExpectedGeneration:   draft.Generation,
		ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		FrozenAt:             "2026-09-22T14:01:00Z",
	}
	path := "/api/v1/production-privilege-logs/" + draft.LogID + "/revisions/1/freeze"
	post := func(body api.ProductionPrivilegeFreezeRequest, key string) (int, []byte) {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("X-Api-Key", key)
		response, err := httpServer.Client().Do(httpRequest)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, body := post(request, "")
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var receipt documentproduction.PrivilegeLogReceipt
	require.NoError(t, json.Unmarshal(body, &receipt))
	require.NoError(t, documentproduction.ValidatePrivilegeLogReceipt(receipt))
	require.Equal(t, draft.LogID, receipt.LogID)
	require.Equal(t, draft.Revision, receipt.Revision)
	require.Equal(t, len(draft.Rows), receipt.RowCount)
	require.NotContains(t, string(body), "Synthetic private rationale.")
	status, replay := post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	clientReceipt, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).
		FreezeProductionPrivilegeLog(t.Context(), draft.LogID, draft.Revision, request)
	require.NoError(t, err)
	require.Equal(t, receipt, clientReceipt)
	request.FrozenAt = "2026-09-22T14:02:00Z"
	status, body = post(request, cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.NotContains(t, string(body), "Synthetic private rationale.")
}
