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
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPackageHTTPBuildsRetainsAndReadsExactArchive(t *testing.T) {
	vault, root, jobID := store.PreparedProductionPackageHTTPFixture(t)
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
		OperationID        string `json:"operation_id"`
		ProfileID          string `json:"profile_id"`
		MaxVolumeBytes     int64  `json:"max_volume_bytes"`
		MaxVolumeDocuments int    `json:"max_volume_documents"`
	}{"79000000-0000-4000-8000-000000000001", "export-dat-opt-images-v1", 50 << 20, 10}
	path := httpServer.URL + "/api/v1/productions/jobs/" + jobID + "/packages"
	call := func(method, suffix, key string, payload any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if payload != nil {
			encoded, err := json.Marshal(payload)
			require.NoError(t, err)
			reader = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(t.Context(), method, path+suffix, reader)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("Content-Type", "application/json")
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}
	status, _ := call(http.MethodPost, "", "", input)
	require.Equal(t, http.StatusUnauthorized, status)
	status, body := call(http.MethodPost, "", cfg.Server.APIKey, input)
	require.Equal(t, http.StatusCreated, status, string(body))
	var evidence production.PackageEvidenceReceipt
	require.NoError(t, json.Unmarshal(body, &evidence))
	require.NoError(t, production.ValidatePackageEvidenceReceipt(evidence))
	require.Equal(t, jobID, evidence.JobID)
	require.Equal(t, input.OperationID, evidence.ID)
	status, replay := call(http.MethodPost, "", cfg.Server.APIKey, input)
	require.Equal(t, http.StatusCreated, status, string(replay))
	require.Equal(t, body, replay)
	status, readback := call(http.MethodGet, "/"+input.OperationID, cfg.Server.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(readback))
	require.Equal(t, body, readback)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	clientEvidence, err := connection.CreateProductionPackage(t.Context(), jobID,
		api.ProductionPackageCreateRequest{OperationID: input.OperationID, ProfileID: input.ProfileID,
			MaxVolumeBytes: input.MaxVolumeBytes, MaxVolumeDocuments: input.MaxVolumeDocuments})
	require.NoError(t, err)
	require.Equal(t, evidence, clientEvidence)
	clientReadback, err := connection.ProductionPackage(t.Context(), jobID, input.OperationID)
	require.NoError(t, err)
	require.Equal(t, evidence, clientReadback)
	input.MaxVolumeDocuments = 1
	status, _ = call(http.MethodPost, "", cfg.Server.APIKey, input)
	require.Equal(t, http.StatusConflict, status)
	staged, err := processing.PrepareRetainedProductionPackageDownload(t.Context(), vault, blobs,
		t.TempDir(), jobID, input.OperationID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staged.Close()) })
	require.Equal(t, evidence, staged.Retained.Evidence)
	require.Equal(t, evidence.ArchiveSHA256, staged.Retained.Archive.Version.BlobHash)
	files, err := os.ReadDir(filepath.Join(root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, files)
}
