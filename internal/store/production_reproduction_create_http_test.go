package store_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionReproductionHTTPBuildsAndRetainsExactPackage(t *testing.T) {
	vault, root, jobID, request, policy := store.PreparedProductionReproductionHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root, Cfg: cfg})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	input := struct {
		Request            documentproduction.ReproductionRequest `json:"request"`
		DeliveryPolicy     production.PackageDeliveryPolicy       `json:"delivery_policy"`
		ProfileID          string                                 `json:"profile_id"`
		MaxVolumeBytes     int64                                  `json:"max_volume_bytes"`
		MaxVolumeDocuments int                                    `json:"max_volume_documents"`
	}{request, policy, "export-dat-opt-images-v1", 50 << 20, 10}
	post := func(key string) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(input)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+"/api/v1/productions/jobs/"+jobID+"/reproductions", bytes.NewReader(body))
		require.NoError(t, err)
		httpRequest.Header.Set("X-Api-Key", key)
		httpRequest.Header.Set("Content-Type", "application/json")
		response, err := httpServer.Client().Do(httpRequest)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := post("")
	require.Equal(t, http.StatusUnauthorized, status)
	input.DeliveryPolicy.RecipientCode = "different-recipient"
	status, _ = post(cfg.Server.APIKey)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	input.DeliveryPolicy = policy
	status, body := post(cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(body))
	var receipt documentproduction.ReproductionReceipt
	require.NoError(t, json.Unmarshal(body, &receipt))
	require.NoError(t, documentproduction.ValidateReproductionReceipt(receipt))
	require.Equal(t, request.OperationID, receipt.ID)
	require.Zero(t, receipt.NumberAllocationCount)
	status, replay := post(cfg.Server.APIKey)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	input.MaxVolumeDocuments = 9
	status, _ = post(cfg.Server.APIKey)
	require.Equal(t, http.StatusConflict, status)
	loaded, err := vault.LoadProductionReproduction(t.Context(), jobID, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, receipt, loaded)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientReceipt, err := connection.CreateProductionReproduction(t.Context(), jobID,
		api.ProductionReproductionCreateRequest{
			Request: request, DeliveryPolicy: api.ProductionReproductionDeliveryPolicy(policy),
			ProfileID: input.ProfileID, MaxVolumeBytes: input.MaxVolumeBytes,
			MaxVolumeDocuments: 10,
		})
	require.NoError(t, err)
	require.Equal(t, receipt, clientReceipt)
	staged, err := os.ReadDir(filepath.Join(root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, staged)
}
