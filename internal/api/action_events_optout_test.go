package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestDisabledTelemetryDoesNotPersistImportClaims(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "0")
	t.Setenv("TELEMETRY_ENABLED", "1")
	for _, workflow := range []string{"mailbox", "package", "photos", "push"} {
		t.Run(workflow, func(t *testing.T) {
			capture := newActionCollector(t)
			require.False(t, capture.reporter.Enabled())
			supervisor := jobs.New(t.Context(), nil)
			t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
			server, catalog := newTestServer(t, func(d *api.Deps) {
				d.TelemetryCapture = capture.capture
				d.Jobs = supervisor
			})
			headers := map[string]string{api.ActionSurfaceHeader: "cli"}
			switch workflow {
			case "mailbox":
				owner := "vault:" + catalog.VaultID()
				raw := []byte("From sender@example.com Sat Sep 12 10:00:00 2026\nSubject: Synthetic archive\nContent-Type: text/plain\n\nSynthetic body.\n")
				digest := sha256.Sum256(raw)
				container := store.MailboxContainerRequest{ID: "synthetic-source", Owner: owner, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw)), Format: "mbox"}
				_, err := catalog.BeginMailboxContainer(t.Context(), container)
				require.NoError(t, err)
				service := mailbox.Service{Store: catalog.Store, Blobs: catalog.Blobs, Spool: t.TempDir(), Telemetry: capture.capture}
				require.NoError(t, service.UploadChunk(t.Context(), owner, container.ID, 0, container.SHA256, container.Size, bytes.NewReader(raw)))
				_, err = service.Seal(t.Context(), owner, container.ID)
				require.NoError(t, err)
				response, body := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", headers, api.MailboxJobInput{ID: "synthetic-job", ContainerID: container.ID, ContainerSHA256: container.SHA256, Settings: api.MailboxSettingsInput{DestinationID: catalog.RootID()}})
				require.Equal(t, http.StatusCreated, response.StatusCode, body)
			case "package":
				response, body := do(t, server, http.MethodPost, "/api/v1/packages/preflights", nil, api.PackagePreflightRequest{
					Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: syntheticPackageRoot(t),
				})
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				var preview api.PackagePreflight
				require.NoError(t, json.Unmarshal([]byte(body), &preview))
				response, body = do(t, server, http.MethodPost, "/api/v1/packages/imports", headers, api.PackageImportRequest{
					PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-package", OperationID: uuid.NewV4().String(),
				})
				require.Equal(t, http.StatusAccepted, response.StatusCode, body)
			case "photos":
				root := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(root, "first.JPG"), []byte("synthetic jpeg"), 0o600))
				response, body := do(t, server, http.MethodPost, "/api/v1/photos/imports", headers, api.PhotoImportStartRequest{SourceRoot: root, Destination: "/photos"})
				require.Equal(t, http.StatusAccepted, response.StatusCode, body)
				var started api.StorageOperation
				require.NoError(t, json.Unmarshal([]byte(body), &started))
				require.Eventually(t, func() bool {
					current, err := catalog.StorageOperation(t.Context(), started.ID)
					return err == nil && current.State == store.StorageOperationCompleted
				}, 10*time.Second, 10*time.Millisecond)
			case "push":
				data := []byte("synthetic content")
				response, body := sendUpload(t, server.URL, server.Client(), catalog.RootID(), uploadRequest{
					name: "first.txt", sourceRef: "first.txt", pushName: "synthetic", runID: uuid.NewV4().String(),
					content: data, expectedHash: uploadIdentity(data), expectedSize: int64(len(data)), surface: "cli",
				})
				require.Equal(t, http.StatusCreated, response.StatusCode, body)
			}
			db, err := catalog.SQLiteDriver().Open(catalog.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			var claims int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM import_action_claims`).Scan(&claims))
			require.Zero(t, claims, "opted-out imports must not persist action claims")
		})
	}
}
