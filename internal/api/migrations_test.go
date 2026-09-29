package api_test

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/fotobanktest"
	"go.kenn.io/docbank/internal/store"
)

func migrationFixtureRequest(t *testing.T) api.FotobankInventoryRequest {
	t.Helper()
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "source"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	return api.FotobankInventoryRequest{CatalogPath: fixture.CatalogPath, VaultRoot: fixture.VaultRoot, OutputDir: filepath.Join(t.TempDir(), "inventory")}
}

func TestFotobankInventoryOperatorRoute(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, server := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var inventory api.FotobankInventory
	require.NoError(t, json.Unmarshal([]byte(body), &inventory))
	require.Equal(t, filepath.Join(request.OutputDir, "report.json"), inventory.ReportPath)
	require.Equal(t, filepath.Join(request.OutputDir, "owner-map.json"), inventory.OwnerMapPath)
	require.FileExists(t, inventory.ReportPath)
	require.FileExists(t, inventory.OwnerMapPath)
	require.Equal(t, int64(1), inventory.Report.Counts.Owners)
	require.Equal(t, int64(1), inventory.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), inventory.Report.Counts.CheckoutEntries)
	require.NoError(t, os.RemoveAll(request.OutputDir))

	unauthorized, unauthorizedBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", map[string]string{"X-Api-Key": ""}, request)
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode, unauthorizedBody)

	browser, browserBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", map[string]string{
		"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts),
	}, request)
	require.Equal(t, http.StatusForbidden, browser.StatusCode, browserBody)
	require.Contains(t, browserBody, `"code":"web_session_read_only"`)

	raw, err := json.Marshal(request)
	require.NoError(t, err)
	remoteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/migrations/fotobank/inventories", bytes.NewReader(raw))
	remoteRequest.Header.Set("Content-Type", "application/json")
	remoteRequest.Header.Set("X-Api-Key", testAPIKey)
	remoteRequest.RemoteAddr = "192.0.2.1:4444"
	remote := httptest.NewRecorder()
	server.Server.Handler().ServeHTTP(remote, remoteRequest)
	require.Equal(t, http.StatusForbidden, remote.Code)
	require.Contains(t, remote.Body.String(), `"code":"loopback_only"`)

	lock := flock.New(request.CatalogPath + ".server.lock")
	ok, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = lock.Unlock() })
	locked, lockedBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, locked.StatusCode, lockedBody)
	require.Contains(t, lockedBody, `"code":"fotobank_running"`)
	_, err = os.Stat(request.OutputDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestFotobankInventoryCorruptArchiveLeavesSourceUntouched(t *testing.T) {
	testdata, err := filepath.Abs(filepath.Join("..", "photomigration", "fotobank", "testdata", "fotobank-kit-v0.24.1"))
	require.NoError(t, err)
	archiveRoot := filepath.Join(t.TempDir(), "damaged-archive")
	err = filepath.WalkDir(testdata, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(testdata, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(archiveRoot, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect archive fixture metadata: %w", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("archive fixture contains a non-regular file")
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, contents, info.Mode().Perm())
	})
	require.NoError(t, err)
	var packPath string
	err = filepath.WalkDir(archiveRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(path) == ".mvpack" {
			packPath = path
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, packPath)
	packed, err := os.ReadFile(packPath)
	require.NoError(t, err)
	require.NotEmpty(t, packed)
	packed[len(packed)/2] ^= 0xff
	require.NoError(t, os.WriteFile(packPath, packed, 0o600))
	archiveBefore, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	require.NoError(t, err)
	outputDir := filepath.Join(t.TempDir(), "inventory")
	ts, _ := newTestServer(t, nil)
	request := api.FotobankInventoryRequest{ArchiveRoot: archiveRoot, OutputDir: outputDir}
	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.NotEqual(t, http.StatusCreated, response.StatusCode, body)
	archiveAfter, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	require.NoError(t, err)
	require.Equal(t, archiveBefore, archiveAfter)
	_, err = os.Stat(outputDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}
