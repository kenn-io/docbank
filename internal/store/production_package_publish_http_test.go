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
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionPackagePublishHTTPMakesSuccessfulJobDownloadable(t *testing.T) {
	vault, root, job := store.PublishedProductionJobHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	gate := api.NewOperationGate()
	worker, err := exporter.New(vault, blobs, root, gate)
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root,
		Cfg: cfg, Gate: gate, Exports: worker,
		WebURL: "http://docbank-synthetic.localhost:43210/"})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/jobs/" + job.ID + "/packages"
	const operationID = "77777777-7777-4777-8777-777777777777"
	requestBody := map[string]any{"operation_id": operationID, "profile_id": "export-dat-opt-images-v1",
		"max_volume_bytes": 50 << 20, "max_volume_documents": 10}
	post := func(body any, authorized bool) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if authorized {
			request.Header.Set("X-Api-Key", cfg.Server.APIKey)
		}
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, data
	}
	status, _ := post(requestBody, false)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = post(map[string]any{"operation_id": operationID, "profile_id": "export-dat-opt-images-v1",
		"max_volume_bytes": 50<<30 + 1, "max_volume_documents": 10}, true)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, body := post(requestBody, true)
	require.Equal(t, http.StatusCreated, status, string(body))
	var published struct {
		JobID          string `json:"job_id"`
		OperationID    string `json:"operation_id"`
		ProfileID      string `json:"profile_id"`
		VersionID      string `json:"version_id"`
		ArchiveSHA256  string `json:"archive_sha256"`
		EvidenceSHA256 string `json:"evidence_sha256"`
		Size           int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal(body, &published))
	require.Equal(t, job.ID, published.JobID)
	require.Equal(t, operationID, published.OperationID)
	require.Equal(t, "export-dat-opt-images-v1", published.ProfileID)
	require.NotEmpty(t, published.EvidenceSHA256)
	replayStatus, replayBody := post(requestBody, true)
	require.Equal(t, http.StatusCreated, replayStatus, string(replayBody))
	require.Equal(t, body, replayBody)
	viaClient, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).PublishProductionPackage(
		t.Context(), job.ID, api.ProductionPackagePublishRequest{OperationID: operationID,
			ProfileID: "export-dat-opt-images-v1", MaxVolumeBytes: 50 << 20,
			MaxVolumeDocuments: 10})
	require.NoError(t, err)
	require.Equal(t, published.ArchiveSHA256, viaClient.ArchiveSHA256)
	require.Equal(t, published.EvidenceSHA256, viaClient.EvidenceSHA256)
	sessionRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		httpServer.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	sessionRequest.Header.Set("X-Api-Key", cfg.Server.APIKey)
	sessionResponse, err := httpServer.Client().Do(sessionRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, sessionResponse.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(sessionResponse.Body, &session))
	require.NoError(t, sessionResponse.Body.Close())
	require.NotEmpty(t, session.Token)
	browserPost := func() int {
		t.Helper()
		payload, marshalErr := json.Marshal(requestBody)
		require.NoError(t, marshalErr)
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost,
			httpServer.URL+path, bytes.NewReader(payload))
		require.NoError(t, requestErr)
		request.Header.Set("Content-Type", "application/json")
		request.Header["X-Api-Key"] = []string{""}
		request.Header.Set(api.WebSessionHeader, session.Token)
		response, callErr := httpServer.Client().Do(request)
		require.NoError(t, callErr)
		require.NoError(t, response.Body.Close())
		return response.StatusCode
	}
	require.Equal(t, http.StatusCreated, browserPost())
	revoke, err := http.NewRequestWithContext(t.Context(), http.MethodDelete,
		httpServer.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	revoke.Header["X-Api-Key"] = []string{""}
	revoke.Header.Set(api.WebSessionHeader, session.Token)
	revoked, err := httpServer.Client().Do(revoke)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, revoked.StatusCode)
	require.NoError(t, revoked.Body.Close())
	require.Equal(t, http.StatusUnauthorized, browserPost())
	changed := map[string]any{"operation_id": operationID, "profile_id": "export-dat-pdf-v1",
		"max_volume_bytes": 50 << 20, "max_volume_documents": 10}
	status, _ = post(changed, true)
	require.Equal(t, http.StatusConflict, status)
	var archive bytes.Buffer
	ticket, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).DownloadProductionPackageTo(
		t.Context(), job.ID, operationID, &archive)
	require.NoError(t, err)
	require.Equal(t, published.VersionID, ticket.VersionID)
	require.Equal(t, published.ArchiveSHA256, ticket.ArchiveSHA256)
	require.Equal(t, published.Size, int64(archive.Len()))
}
