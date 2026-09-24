package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionFinalizeHTTPSealsExactRevisionAndRejectsChangedReplay(t *testing.T) {
	vault, root, setID, revision, etag, namespaceID := store.ProductionFinalizeHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/sets/" + setID + "/revisions/" + strconv.FormatInt(revision, 10) + "/finalize"
	request := api.ProductionFinalizeRequest{OperationID: "75000000-0000-4000-8000-000000000070",
		NamespaceID: namespaceID, SnapshotID: "75000000-0000-4000-8000-000000000071"}
	call := func(body any, version int64, authorized bool) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		if authorized {
			httpRequest.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		if version > 0 {
			httpRequest.Header.Set("If-Match", `"`+strconv.FormatInt(version, 10)+`"`)
		}
		response, err := httpServer.Client().Do(httpRequest)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := call(request, etag, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = call(request, 0, true)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, _ = call(request, etag+1, true)
	require.Equal(t, http.StatusConflict, status)
	status, data := call(request, etag, true)
	require.Equal(t, http.StatusOK, status, string(data))
	var finalized api.ProductionFinalizationResult
	require.NoError(t, json.Unmarshal(data, &finalized))
	require.Equal(t, "finalized", finalized.Draft.State)
	require.Equal(t, setID, finalized.Draft.SetID)
	require.Equal(t, request.SnapshotID, finalized.SnapshotID)
	require.NotEmpty(t, finalized.ReceiptSHA256)
	status, replay := call(request, etag, true)
	require.Equal(t, http.StatusOK, status, string(replay))
	require.Equal(t, data, replay)
	changed := request
	changed.SnapshotID = "75000000-0000-4000-8000-000000000072"
	status, body := call(changed, etag, true)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Contains(t, string(body), "changed_payload")
	viaClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).FinalizeProductionDraft(
		t.Context(), setID, revision, etag, request)
	require.NoError(t, err)
	require.Equal(t, finalized, viaClient)
}
