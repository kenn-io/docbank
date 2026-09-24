package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func handoffArchiveHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestPublishedProductionPackageDownloadUsesExactRetainedVersions(t *testing.T) {
	vault, root, job, retained := store.PublishedProductionPackageHTTPFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key"
	server := api.NewServer(api.Deps{Store: vault, Blobs: blobs, VaultRoot: root,
		Cfg: cfg, WebURL: "http://docbank-synthetic.localhost:43210/"})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	path := "/api/v1/productions/jobs/" + job.ID + "/packages/" + retained.Evidence.ID + "/download"
	post := func(path, key string) (int, []byte) {
		t.Helper()
		request, requestErr := http.NewRequest(http.MethodPost, httpServer.URL+path, bytes.NewBufferString("{}"))
		require.NoError(t, requestErr)
		request.Header.Set("Content-Type", "application/json")
		request.Header["X-Api-Key"] = []string{key}
		response, callErr := httpServer.Client().Do(request)
		require.NoError(t, callErr)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, body
	}

	unauthorized, _ := post(path, "")
	require.Equal(t, http.StatusUnauthorized, unauthorized)
	response, body := post(path, cfg.Server.APIKey)
	require.Equal(t, http.StatusOK, response, string(body))
	var ticket struct {
		URL           string `json:"url"`
		ArchiveSHA256 string `json:"archive_sha256"`
		Size          int64  `json:"size"`
		VersionID     string `json:"version_id"`
	}
	require.NoError(t, json.Unmarshal(body, &ticket))
	require.Equal(t, retained.Archive.Version.BlobHash, ticket.ArchiveSHA256)
	require.Equal(t, retained.Archive.Version.Size, ticket.Size)
	require.Equal(t, retained.Archive.Version.ID, ticket.VersionID)
	require.Contains(t, ticket.URL, "?ticket=")

	get := func() (int, http.Header, []byte) {
		t.Helper()
		response, callErr := httpServer.Client().Get(httpServer.URL + ticket.URL)
		require.NoError(t, callErr)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, response.Header.Clone(), body
	}
	download, headers, archive := get()
	require.Equal(t, http.StatusOK, download)
	require.Equal(t, "application/zip", headers.Get("Content-Type"))
	require.Equal(t, retained.Archive.Version.BlobHash, handoffArchiveHash(archive))
	require.Equal(t, retained.Archive.Version.Size, int64(len(archive)))
	reused, _, _ := get()
	require.Equal(t, http.StatusNotFound, reused)
	var viaClient bytes.Buffer
	clientTicket, err := daemonconn.New(httpServer.URL, cfg.Server.APIKey).DownloadProductionPackageTo(
		t.Context(), job.ID, retained.Evidence.ID, &viaClient)
	require.NoError(t, err)
	require.Equal(t, ticket.ArchiveSHA256, clientTicket.ArchiveSHA256)
	require.Equal(t, archive, viaClient.Bytes())
	stagingRoot := filepath.Join(root, "web-downloads")
	require.Eventually(t, func() bool {
		entries, readErr := os.ReadDir(stagingRoot)
		return readErr == nil && len(entries) == 0
	}, time.Second, 10*time.Millisecond, "consumed ticket must release private staging")
	staged, err := os.ReadDir(stagingRoot)
	require.NoError(t, err)
	require.Empty(t, staged)

	missingOperation, missingOperationBody := post("/api/v1/productions/jobs/"+job.ID+
		"/packages/88888888-8888-4888-8888-888888888888/download", cfg.Server.APIKey)
	require.Equal(t, http.StatusNotFound, missingOperation)
	require.Contains(t, string(missingOperationBody), `"code":"not_found"`)
	sessionRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/daemon/web-session", nil)
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
	browserRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+path, bytes.NewBufferString("{}"))
	require.NoError(t, err)
	browserRequest.Header.Set("Content-Type", "application/json")
	browserRequest.Header["X-Api-Key"] = []string{""}
	browserRequest.Header.Set(api.WebSessionHeader, session.Token)
	browserResponse, err := httpServer.Client().Do(browserRequest)
	require.NoError(t, err)
	browserBody, err := io.ReadAll(browserResponse.Body)
	require.NoError(t, err)
	require.NoError(t, browserResponse.Body.Close())
	require.Equal(t, http.StatusOK, browserResponse.StatusCode, string(browserBody))
	var browserTicket struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(browserBody, &browserTicket))
	require.NotEmpty(t, browserTicket.URL)
	revokeRequest, err := http.NewRequest(http.MethodDelete, httpServer.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	revokeRequest.Header["X-Api-Key"] = []string{""}
	revokeRequest.Header.Set(api.WebSessionHeader, session.Token)
	revoked, err := httpServer.Client().Do(revokeRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, revoked.StatusCode)
	require.NoError(t, revoked.Body.Close())
	revokedDownload, err := httpServer.Client().Get(httpServer.URL + browserTicket.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, revokedDownload.StatusCode)
	require.NoError(t, revokedDownload.Body.Close())
	staged, err = os.ReadDir(filepath.Join(root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, staged)

	archivePath := filepath.Join(root, "blobs", ticket.ArchiveSHA256[:2], ticket.ArchiveSHA256)
	require.NoError(t, os.Remove(archivePath))
	missingBlob, missingBlobBody := post(path, cfg.Server.APIKey)
	require.Equal(t, http.StatusInternalServerError, missingBlob)
	require.Contains(t, string(missingBlobBody), `"code":"production_download_failed"`)
	staged, err = os.ReadDir(filepath.Join(root, "web-downloads"))
	require.NoError(t, err)
	require.Empty(t, staged)
}
