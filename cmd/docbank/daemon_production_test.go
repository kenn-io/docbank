package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestProductionWorkerRegistersOnceUnderVaultSupervisor(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})
	require.NoError(t, startProductionWorker(supervisor, catalog, blobs))
	require.ErrorIs(t, startProductionWorker(supervisor, catalog, blobs), jobs.ErrDuplicate)
	snapshots := supervisor.Snapshot()
	require.Len(t, snapshots, 1)
	require.Equal(t, "production:render", snapshots[0].Name)
}
