package client_test

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func newClient(t *testing.T, key string) (*client.Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cfg := config.Default()
	cfg.Server.APIKey = key
	srv := api.NewServer(api.Deps{Store: s, Blobs: blob.New(filepath.Join(dir, "blobs")), Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return client.New(ts.URL, key), s
}

func TestRoundTrip(t *testing.T) {
	c, s := newClient(t, "")
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
	c, s := newClient(t, "")
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
	c, _ := newClient(t, "k")
	require.NoError(t, c.Health(t.Context()))
	_, err := c.TrashList(t.Context()) // authed route succeeds only with the key
	require.NoError(t, err)
}
