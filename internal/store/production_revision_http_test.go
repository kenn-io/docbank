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

func TestProductionRevisionHTTPMutationsFenceAndReplay(t *testing.T) {
	vault, root, _, _ := store.PublishedProductionPackageHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	created, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "88888888-8888-4888-8888-888888888881", Name: "Synthetic review"})
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	base := "/api/v1/productions/sets/" + created.ID + "/revisions/1"
	call := func(method, path, ifMatch string, body any) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), method, httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		if ifMatch != "" {
			request.Header.Set("If-Match", ifMatch)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	edit := api.ProductionInstructionsRequest{OperationID: "88888888-8888-4888-8888-888888888882", Instructions: "Review only synthetic pages"}
	status, _ := call(http.MethodPut, base+"/instructions", "", edit)
	require.Equal(t, http.StatusPreconditionRequired, status)
	status, data := call(http.MethodPut, base+"/instructions", `"1"`, edit)
	require.Equal(t, http.StatusOK, status, string(data))
	var receipt redaction.Receipt
	require.NoError(t, json.Unmarshal(data, &receipt))
	require.Equal(t, edit.OperationID, receipt.OperationID)
	require.Equal(t, draft.ETag+1, receipt.ETag)
	status, replay := call(http.MethodPut, base+"/instructions", `"1"`, edit)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, data, replay)
	changed := edit
	changed.Instructions = "Different synthetic instructions"
	status, data = call(http.MethodPut, base+"/instructions", `"1"`, changed)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(data), "production_operation_conflict")
	changed.OperationID = "88888888-8888-4888-8888-888888888883"
	status, data = call(http.MethodPut, base+"/instructions", `"1"`, changed)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(data), "production_revision_conflict")

	changes := api.ProductionChangesRequest{OperationID: "88888888-8888-4888-8888-888888888884",
		Changes: []api.ProductionChange{{Kind: "recipe", RecipeID: redaction.RecipeID600DPI}}}
	status, data = call(http.MethodPost, base+"/changes", `"2"`, changes)
	require.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &receipt))
	require.EqualValues(t, 3, receipt.ETag)
	updated, err := vault.ProductionDraft(t.Context(), created.ID, 1)
	require.NoError(t, err)
	require.Equal(t, redaction.RecipeID600DPI, updated.RecipeID)
	status, data = call(http.MethodPost, base+"/changes", `"3"`, api.ProductionChangesRequest{OperationID: "88888888-8888-4888-8888-888888888885"})
	require.Equal(t, http.StatusUnprocessableEntity, status, string(data))

	client := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	viaClient, err := client.EditProductionInstructions(t.Context(), created.ID, 1, 1, edit)
	require.NoError(t, err)
	require.EqualValues(t, 2, viaClient.ETag)
	viaChanges, err := client.ApplyProductionChanges(t.Context(), created.ID, 1, 2, changes)
	require.NoError(t, err)
	require.EqualValues(t, 3, viaChanges.ETag)
}
