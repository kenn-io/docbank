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
	"go.kenn.io/docbank/sqlite"
)

func migrationFixtureRequest(t *testing.T) api.FotobankInventoryRequest {
	t.Helper()
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "source"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	fixture.OwnerMapPath = filepath.Join(t.TempDir(), "owner-map.json")
	return api.FotobankInventoryRequest{CatalogPath: fixture.CatalogPath, VaultRoot: fixture.VaultRoot, OwnerMapPath: fixture.OwnerMapPath}
}

func TestFotobankInventoryOperatorRoute(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, server := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var run api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(body), &run))
	require.NotEmpty(t, run.ID)
	require.Equal(t, request.OwnerMapPath, run.OwnerMapPath)
	require.Equal(t, int64(1), run.Report.Counts.Owners)
	require.Equal(t, int64(1), run.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), run.Report.Counts.CheckoutEntries)

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
}

func TestMigrationRunRoutes(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, _ := newTestServer(t, nil)
	created, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusCreated, created.StatusCode, body)
	var run api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(body), &run))

	listed, listBody := get(t, ts, "/api/v1/migrations/runs?limit=1", nil)
	require.Equal(t, http.StatusOK, listed.StatusCode, listBody)
	var page api.MigrationRunPage
	require.NoError(t, json.Unmarshal([]byte(listBody), &page))
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Equal(t, int64(1), page.Items[0].Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), page.Items[0].Report.Counts.CheckoutEntries)

	shown, showBody := get(t, ts, "/api/v1/migrations/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, shown.StatusCode, showBody)
	var got api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(showBody), &got))
	require.Equal(t, run.ID, got.ID)
	require.Equal(t, int64(1), got.Report.Counts.AlbumMemberships)
	require.Equal(t, int64(1), got.Report.Counts.CheckoutEntries)

	missing, missingBody := get(t, ts, "/api/v1/migrations/runs/00000000-0000-4000-8000-000000000099", nil)
	require.Equal(t, http.StatusNotFound, missing.StatusCode, missingBody)
}

func TestFotobankInventoryRemovesTemplateWhenRunSaveFails(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, server := newTestServer(t, nil)
	db, err := store.DefaultSQLiteDriver().Open(server.DBPath, sqlite.OpenOptions{
		Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred,
	})
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_migration_run BEFORE INSERT ON photo_migration_runs
		BEGIN SELECT RAISE(ABORT, 'injected run save failure'); END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.NotEqual(t, http.StatusCreated, response.StatusCode, body)
	_, err = os.Stat(request.OwnerMapPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	page, err := server.ListPhotoMigrationRuns(t.Context(), 0, 50)
	require.NoError(t, err)
	require.Zero(t, page.Total)
}

func TestFotobankInventoryCorruptArchiveLeavesSourceAndRunUntouched(t *testing.T) {
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
	ownerMapPath := filepath.Join(t.TempDir(), "owner-map.json")
	ts, server := newTestServer(t, nil)
	request := api.FotobankInventoryRequest{ArchiveRoot: archiveRoot, OwnerMapPath: ownerMapPath}
	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.NotEqual(t, http.StatusCreated, response.StatusCode, body)
	archiveAfter, err := (fotobanktest.Install{Root: archiveRoot}).Digest()
	require.NoError(t, err)
	require.Equal(t, archiveBefore, archiveAfter)
	_, err = os.Stat(ownerMapPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	page, err := server.ListPhotoMigrationRuns(t.Context(), 0, 50)
	require.NoError(t, err)
	require.Zero(t, page.Total)
}
