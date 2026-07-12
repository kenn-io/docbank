package client_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestStorageUnpackRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	src := filepath.Join(t.TempDir(), "unpack.txt")
	require.NoError(t, os.WriteFile(src, []byte("unpack me"), 0o600))
	ingested, err := c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, ingested.Added)
	_, err = c.StoragePack(t.Context(), 0)
	require.NoError(t, err)

	report, err := c.StorageUnpack(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, report.PacksUnpacked)
	assert.Equal(t, 1, report.BlobsRestored)
	assert.Equal(t, int64(len("unpack me")), report.BytesRestored)
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
