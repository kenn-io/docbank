package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// serverKey is the fixed key every test server in this file is built with:
// production always has an effective key (see cmd/docbank/daemon.go and
// NewServer's refusal of an empty one), so the fake server must too.
const serverKey = "server-key"

// newClient builds a real server keyed with serverKey and a client keyed
// with clientKey (which may differ from serverKey, to exercise the
// mismatched-key path — see TestWrongAPIKeyIsRejected).
func newClient(t *testing.T, clientKey string) (*client.Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cfg := config.Default()
	cfg.Server.APIKey = serverKey
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	srv := api.NewServer(api.Deps{Store: s, Blobs: blobs, Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return client.New(ts.URL, clientKey), s
}

func TestRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()

	dir, err := c.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	assert.Equal(t, "/docs", dir.Path)

	got, err := c.Stat(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, dir.ID, got.ID)

	renamed, err := c.Move(ctx, dir.ID, got.Revision, nil, new("papers"))
	require.NoError(t, err)
	assert.Equal(t, "/papers", renamed.Path)

	moved, err := c.MovePath(ctx, "/papers", "/docs")
	require.NoError(t, err)
	assert.Equal(t, "/docs", moved.Path)

	kids, err := c.Children(ctx, s.RootID())
	require.NoError(t, err)
	assert.Len(t, kids, 1)

	trashed, err := c.TrashPath(ctx, "/docs")
	require.NoError(t, err)
	restored, err := c.Restore(ctx, dir.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/docs", restored.Path)
}

func TestErrorMapping(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()

	_, err := c.Stat(ctx, "/missing")
	require.ErrorIs(t, err, store.ErrNotFound)

	d, err := c.Mkdir(ctx, s.RootID(), "dup")
	require.NoError(t, err)
	_, err = c.Mkdir(ctx, s.RootID(), "dup")
	require.ErrorIs(t, err, store.ErrExists)

	_, err = c.Move(ctx, d.ID, d.Revision+99, nil, new("x"))
	require.ErrorIs(t, err, store.ErrStaleRevision)
}

func TestDeferredRetirementProblemKeepsTypedClientError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(api.Error{
			Title: http.StatusText(http.StatusServiceUnavailable), Status: http.StatusServiceUnavailable,
			Code: "pack_retirement_deferred", Detail: "replacement committed; run storage pack",
		}); err != nil {
			t.Errorf("encoding problem response: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "key")
	_, err := c.StorageRepack(t.Context(), 0, 0, 0)
	require.ErrorIs(t, err, packstore.ErrPackRetirementDeferred)
}

func TestAPIKeySent(t *testing.T) {
	c, _ := newClient(t, serverKey)
	require.NoError(t, c.Health(t.Context()))
	_, err := c.TrashList(t.Context()) // authed route succeeds only with the key
	require.NoError(t, err)
}

func TestStoragePackRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	src := filepath.Join(t.TempDir(), "pack.txt")
	require.NoError(t, os.WriteFile(src, []byte("pack me"), 0o600))
	ingested, err := c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, ingested.Added)

	report, err := c.StoragePack(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, report.BlobsPacked)
	assert.True(t, report.BudgetExhausted)
}

func TestBackupCreateStreamRoundTripAndTypedError(t *testing.T) {
	c, _ := newClient(t, serverKey)
	repoPath := filepath.Join(t.TempDir(), "repo")
	_, err := c.BackupInit(t.Context(), repoPath)
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "backup.txt")
	require.NoError(t, os.WriteFile(src, []byte("stream this backup"), 0o600))
	_, err = c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)

	var events []api.BackupProgress
	snapshot, err := c.BackupCreateStream(t.Context(), client.BackupCreateOptions{
		Repo: repoPath, Tag: "client", Jobs: 1,
	}, func(event api.BackupProgress) { events = append(events, event) })
	require.NoError(t, err)
	assert.Equal(t, "client", snapshot.Tag)
	assert.Equal(t, int64(1), snapshot.Files)
	finalStages := map[string]bool{}
	for _, event := range events {
		if event.Final {
			finalStages[event.Stage] = true
		}
	}
	for _, stage := range []string{"freeze", "metadata", "attachments", "seal"} {
		assert.True(t, finalStages[stage], "stage %q emitted a final event", stage)
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	_, err = c.BackupCreateStream(cancelCtx, client.BackupCreateOptions{Repo: repoPath},
		func(api.BackupProgress) { cancel() })
	require.Error(t, err)

	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	var lock *backup.RepoLock
	var lockErr error
	require.Eventually(t, func() bool {
		lock, lockErr = repo.AcquireExclusiveLock("test", false)
		return lockErr == nil
	}, 5*time.Second, 10*time.Millisecond,
		"server-side cancellation must release the repository lock")
	t.Cleanup(func() { _ = lock.Release() })
	snapshots, err := c.BackupList(t.Context(), repoPath)
	require.NoError(t, err)
	assert.Len(t, snapshots, 1, "a disconnected progress client must not publish a snapshot")
	_, err = c.BackupCreateStream(t.Context(), client.BackupCreateOptions{Repo: repoPath}, nil)
	require.ErrorIs(t, err, backup.ErrRepoLocked)
}

func TestContentIdentityAndVerificationRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	content := []byte("remote writer evidence")
	src := filepath.Join(t.TempDir(), "evidence.txt")
	require.NoError(t, os.WriteFile(src, content, 0o600))
	report, err := c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)

	node, err := c.Stat(t.Context(), "/inbox/evidence.txt")
	require.NoError(t, err)
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])
	assert.Equal(t, wantHash, node.BlobHash)

	stream, err := c.Content(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, wantHash, stream.BlobHash)
	assert.Equal(t, int64(len(content)), stream.Size)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, content, got)
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":", stream.ContentDigest())

	verified, err := c.VerifyNodeContent(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	assert.True(t, verified.Verified)
	assert.Equal(t, wantHash, verified.ComputedHash)

	packed, err := c.StoragePack(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	verified, err = c.VerifyNodeContent(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	assert.True(t, verified.Verified, "the same evidence contract reads packed authority")
	assert.Equal(t, wantHash, verified.ComputedHash)
}

func TestDigestCheckedUploadRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	content := "client upload"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])

	added, err := c.Upload(t.Context(), s.RootID(), "upload.txt", "text/plain",
		hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, "added", added.Status)
	assert.Equal(t, hash, added.ComputedHash)
	assert.Equal(t, hash, added.Node.BlobHash)

	skipped, err := c.Upload(t.Context(), s.RootID(), "upload.txt", "text/plain",
		hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, "skipped", skipped.Status)
	assert.Equal(t, added.Node.ID, skipped.Node.ID)

	wrong := sha256.Sum256([]byte("wrong"))
	_, err = c.Upload(t.Context(), s.RootID(), "rejected.txt", "text/plain",
		hex.EncodeToString(wrong[:]), int64(len(content)), strings.NewReader(content))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest_mismatch")
}

func TestStorageRepackRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	for name, content := range map[string]string{
		"keep.txt": "keep", "drop-a.txt": "drop a", "drop-b.txt": "drop b",
	} {
		src := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(src, []byte(content), 0o600))
		ingested, err := c.Ingest(t.Context(), []string{src}, "/inbox")
		require.NoError(t, err)
		require.Equal(t, 1, ingested.Added)
	}
	_, err := c.StoragePack(t.Context(), 0)
	require.NoError(t, err)
	for _, path := range []string{"/inbox/drop-a.txt", "/inbox/drop-b.txt"} {
		drop, statErr := c.Stat(t.Context(), path)
		require.NoError(t, statErr)
		_, err = c.Trash(t.Context(), drop.ID, drop.Revision)
		require.NoError(t, err)
	}
	_, err = c.TrashEmpty(t.Context(), "", true)
	require.NoError(t, err)
	_, err = c.GC(t.Context(), true)
	require.NoError(t, err)

	report, err := c.StorageRepack(t.Context(), 0, time.Nanosecond, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, report.PacksRewritten)
	assert.Equal(t, 1, report.PacksRemoved)
}

// TestWrongAPIKeyIsRejected is the client-side half of the keyless-loopback
// regression: a client holding the wrong key must get a clearly readable
// 401, never silent success and never an opaque envelope dump.
func TestWrongAPIKeyIsRejected(t *testing.T) {
	c, _ := newClient(t, "wrong-key")
	_, err := c.TrashList(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "unauthorized")
}
