package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestMailboxCancelKeepsSuccessWhenMaintenanceQueues(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "docbank.db")
	catalog, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	owner := "vault:" + catalog.VaultID()
	hash := strings.Repeat("a", 64)
	_, err = catalog.BeginMailboxContainer(t.Context(), store.MailboxContainerRequest{ID: "synthetic-source", Owner: owner, SHA256: hash, Size: 1, Format: "mbox"})
	require.NoError(t, err)
	require.NoError(t, catalog.RecordBlob(t.Context(), hash, 1, store.BlobPhysical{Encoding: "raw", StoredBytes: 1, PackEligible: true, Created: true}))
	require.NoError(t, catalog.PutMailboxChunk(t.Context(), owner, "synthetic-source", store.MailboxChunk{Index: 0, SHA256: hash, Size: 1}))
	_, err = catalog.SealMailboxContainer(t.Context(), owner, "synthetic-source", hash, 1)
	require.NoError(t, err)
	_, err = catalog.BeginMailboxJob(t.Context(), owner, store.MailboxJobRequest{ID: "synthetic-job", ContainerID: "synthetic-source", ContainerSHA256: hash, Settings: store.MailboxSettings{DestinationID: catalog.RootID()}}, "web")
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	gate := NewOperationGate()
	server := NewServer(Deps{Store: catalog, Cfg: cfg, VaultRoot: root, Gate: gate})
	defer server.Close()
	db, err := store.DefaultSQLiteDriver().Open(path, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	lock, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/cancel", nil).WithContext(ctx)
	request.Host = "127.0.0.1"
	request.Header.Set("X-Api-Key", cfg.Server.APIKey)
	response := httptest.NewRecorder()
	cancelled := make(chan struct{})
	go func() { server.Handler().ServeHTTP(response, request); close(cancelled) }()
	// The route holds the mutation gate while SQLite waits on our writer.
	require.Eventually(t, func() bool {
		if gate.mu.TryAcquire(operationGateExclusiveWeight) {
			gate.mu.Release(operationGateExclusiveWeight)
			return false
		}
		return true
	}, 5*time.Second, time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	maintained := make(chan error, 1)
	go func() {
		maintained <- gate.MaintainContext(ctx, func() error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	require.Eventually(t, func() bool { gate.admission.RLock(); defer gate.admission.RUnlock(); return gate.maintenance > 0 }, 5*time.Second, time.Millisecond)
	require.NoError(t, lock.Commit())
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not return")
	}
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	job, err := catalog.MailboxJob(t.Context(), owner, "synthetic-job")
	require.NoError(t, err)
	require.Equal(t, "canceled", job.State)
	cancel()
	require.NoError(t, <-maintained)
}
