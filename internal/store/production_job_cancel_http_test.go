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
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionJobCancelOperationReplayAndConflict(t *testing.T) {
	vault, _, setID, jobID := store.ProductionJobHTTPFixture(t)
	const operationID = "89000000-0000-4000-8000-000000000071"
	receipt, err := vault.CancelProductionJobOperation(t.Context(), "synthetic-operator", setID, jobID, 1, operationID)
	require.NoError(t, err)
	require.Equal(t, operationID, receipt.OperationID)
	require.Equal(t, setID, receipt.SetID)
	require.EqualValues(t, 1, receipt.Revision)
	require.EqualValues(t, 1, receipt.ETag)
	replay, err := vault.CancelProductionJobOperation(t.Context(), "synthetic-operator", setID, jobID, 1, operationID)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	status, err := vault.ProductionJobStatus(t.Context(), setID, jobID)
	require.NoError(t, err)
	require.Equal(t, "canceled", status.State)
	_, err = vault.CancelProductionJobOperation(t.Context(), "synthetic-operator", setID, jobID, 2, operationID)
	require.ErrorIs(t, err, store.ErrProductionOperationConflict)
	_, err = vault.CancelProductionJobOperation(t.Context(), "synthetic-operator", setID, jobID, 2,
		"89000000-0000-4000-8000-000000000072")
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	_, err = vault.CancelProductionJobOperation(t.Context(), "synthetic-operator",
		"89000000-0000-4000-8000-000000000099", jobID, 1,
		"89000000-0000-4000-8000-000000000073")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestProductionJobCancelHTTPAndGeneratedClient(t *testing.T) {
	vault, root, setID, jobID := store.ProductionJobHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + setID + "/jobs/" + jobID + "/cancel"
	const operationID = "89000000-0000-4000-8000-000000000074"
	post := func(path, ifMatch string, authorized bool) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewBufferString(`{"operation_id":"`+operationID+`"}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			request.Header.Set("If-Match", ifMatch)
		}
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
	status, _ := post(path, "1", false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = post(path, "", true)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, body := post(path, "1", true)
	require.Equal(t, http.StatusOK, status, string(body))
	var first redaction.Receipt
	require.NoError(t, json.Unmarshal(body, &first))
	require.Equal(t, operationID, first.OperationID)
	status, body = post(path, "1", true)
	require.Equal(t, http.StatusOK, status, string(body))
	var replay redaction.Receipt
	require.NoError(t, json.Unmarshal(body, &replay))
	require.Equal(t, first, replay)
	status, _ = post(path, "2", true)
	require.Equal(t, http.StatusConflict, status)
	status, _ = post("/api/v1/productions/sets/89000000-0000-4000-8000-000000000099/jobs/"+jobID+"/cancel", "1", true)
	require.Equal(t, http.StatusNotFound, status)
	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientReceipt, err := client.CancelProductionJob(t.Context(), setID, jobID, 1,
		api.ProductionJobCancelRequest{OperationID: operationID})
	require.NoError(t, err)
	require.Equal(t, first, clientReceipt)
}
