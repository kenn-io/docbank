package api_test

import (
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func collectionBackupDrivers() []docsqlite.Driver {
	drivers := []docsqlite.Driver{store.DefaultSQLiteDriver(), modernc.Driver{}}
	seen := make(map[string]bool)
	unique := make([]docsqlite.Driver, 0, len(drivers))
	for _, driver := range drivers {
		if !seen[driver.Name()] {
			seen[driver.Name()] = true
			unique = append(unique, driver)
		}
	}
	return unique
}

func newCollectionBackupServer(
	t *testing.T, driver docsqlite.Driver, repoPath string,
) (*httptest.Server, *testStore) {
	t.Helper()
	vaultRoot := t.TempDir()
	dbPath := filepath.Join(vaultRoot, "docbank.db")
	s, err := store.Open(dbPath, driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobsDir := filepath.Join(vaultRoot, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = testAPIKey
	cfg.Backup.Repo = repoPath
	server := api.NewServer(api.Deps{
		Store: s, Blobs: blobs, VaultRoot: vaultRoot, Cfg: cfg,
		Logger: slog.New(slog.DiscardHandler), WebURL: testWebURL,
	})
	t.Cleanup(server.Close)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	ts.Client().Transport = &apiKeyTransport{key: testAPIKey, next: ts.Client().Transport}
	return ts, &testStore{
		Store: s, Blobs: blobs, BlobsDir: blobsDir, DBPath: dbPath, Server: server,
	}
}

func TestCollectionAuthoritySurvivesPhysicalBackupRestore(t *testing.T) {
	for _, driver := range collectionBackupDrivers() {
		t.Run(driver.Name(), func(t *testing.T) {
			repoPath := filepath.Join(t.TempDir(), "repo")
			ts, live := newCollectionBackupServer(t, driver, repoPath)
			label := "Audit archive"
			source := filepath.Join(t.TempDir(), "record.txt")
			content := []byte("synthetic archived collection bytes")
			require.NoError(t, os.WriteFile(source, content, 0o600))

			resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest", nil, map[string]any{
				"paths": []string{source}, "dest": "/inbox", "collection_label": label,
			})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var first api.IngestReport
			require.NoError(t, json.Unmarshal([]byte(body), &first))
			require.NotEmpty(t, first.IngestID)
			firstLabel, err := live.CollectionLabel(t.Context(), first.IngestID)
			require.NoError(t, err)
			require.NotNil(t, firstLabel.Label)

			c := client.New(ts.URL, testAPIKey)
			preview, err := c.PreviewAudit(t.Context(), client.AuditPreviewOptions{
				NodeID: live.RootID(), AgentLabel: "physical-backup-test",
			})
			require.NoError(t, err)
			_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
			require.NoError(t, err)

			resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil, map[string]any{
				"paths": []string{source}, "dest": "/inbox",
			})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var second api.IngestReport
			require.NoError(t, json.Unmarshal([]byte(body), &second))
			require.NotEmpty(t, second.IngestID)
			assert.NotEqual(t, first.IngestID, second.IngestID)
			assert.Zero(t, second.Added)
			assert.Equal(t, 1, second.Skipped)

			resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil,
				map[string]any{"tag": "collection-authority", "jobs": 1})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var snapshot api.BackupSnapshot
			require.NoError(t, json.Unmarshal([]byte(body), &snapshot))

			target := filepath.Join(t.TempDir(), "restored")
			resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore/stream", nil,
				map[string]any{"target": target, "snapshot_id": snapshot.ID, "jobs": 1})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			events := decodeBackupRestoreEvents(t, body)
			require.NotEmpty(t, events)
			terminal := events[len(events)-1]
			require.Equal(t, "result", terminal.Type)
			require.NotNil(t, terminal.Report)
			assert.True(t, terminal.Report.Proof.ContentVerified)

			func() {
				restoredStore, err := store.Open(filepath.Join(target, "docbank.db"), driver)
				require.NoError(t, err)
				defer func() { require.NoError(t, restoredStore.Close()) }()
				restoredBlobs, err := blob.New(
					store.NewPackCatalog(restoredStore), filepath.Join(target, "blobs"),
				)
				require.NoError(t, err)
				defer func() { require.NoError(t, restoredBlobs.Close()) }()

				firstRestored, err := restoredStore.CollectionByID(t.Context(), first.IngestID)
				require.NoError(t, err)
				require.NotNil(t, firstRestored.Label)
				assert.Equal(t, *firstLabel.Label, *firstRestored.Label)
				assert.Equal(t, firstLabel.Revision, firstRestored.LabelRevision)
				secondRestored, err := restoredStore.CollectionByID(t.Context(), second.IngestID)
				require.NoError(t, err)
				assert.Nil(t, secondRestored.Label)

				firstMembers, err := restoredStore.CollectionMembers(t.Context(), first.IngestID, 10, 0)
				require.NoError(t, err)
				secondMembers, err := restoredStore.CollectionMembers(t.Context(), second.IngestID, 10, 0)
				require.NoError(t, err)
				require.Len(t, firstMembers.Items, 1)
				require.Len(t, secondMembers.Items, 1)
				assert.Equal(t, firstMembers.Items[0].Node.ID, secondMembers.Items[0].Node.ID)

				history, err := restoredStore.AuditHistory(
					t.Context(), firstMembers.Items[0].Node.ID, 50, "",
				)
				require.NoError(t, err)
				kinds := make([]string, 0, len(history.Items))
				for _, event := range history.Items {
					kinds = append(kinds, event.Kind)
				}
				assert.Contains(t, kinds, "ingest_observe")
				verification, err := restoredStore.VerifyAudit(t.Context(), nil)
				require.NoError(t, err)
				assert.True(t, verification.Evidence.Enabled)

				reader, err := restoredBlobs.OpenContext(
					t.Context(), firstMembers.Items[0].Node.BlobHash,
				)
				require.NoError(t, err)
				restoredContent, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
				assert.Equal(t, content, restoredContent)
			}()
		})
	}
}
