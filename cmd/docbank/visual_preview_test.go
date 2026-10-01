package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestStartProcessingJobsRegistersVisualPreview(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := jobs.New(ctx, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		require.NoError(t, supervisor.Shutdown(shutdownCtx))
		cancel()
	})
	err = startProcessingJobs(
		supervisor, catalog, blobs, layout.BlobTmpDir(),
		processing.NewRenditionRuntimeRegistry(), 1, api.NewOperationGate(), slog.Default(),
	)
	require.NoError(t, err)
	var names []string
	for _, job := range supervisor.Snapshot() {
		names = append(names, job.Name)
	}
	require.Contains(t, names, "derive:visual-previews")
	require.Equal(t, 1, strings.Count(strings.Join(names, ","), "derive:visual-previews"))
	require.NotContains(t, names, "process:renditions")
}

func TestVisualPreviewBackfillRediscoversMissingGeneration(t *testing.T) {
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	var versions []string
	for _, name := range []string{"one.txt", "two.txt"} {
		receipt, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader(name))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, receipt.Hash, receipt.Size, "image/jpeg", store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created})
		require.NoError(t, err)
		_, err = catalog.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		versions = append(versions, node.CurrentVersionID)
	}
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	_, err = processing.EnsureVisualPreview(t.Context(), catalog, blobs, versions[0], recipe, false)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	processed := 0
	backfill := processing.Backfill[store.PhotoVisualPreviewTarget]{
		Name: "preview-test", Page: 10, IdleDelay: time.Second,
		List: func(ctx context.Context, after string, limit int) ([]store.PhotoVisualPreviewTarget, error) {
			return catalog.MissingPhotoVisualPreviewTargetsAfter(ctx, fingerprint, after, limit)
		},
		Key: func(target store.PhotoVisualPreviewTarget) string { return target.VersionID },
		Process: func(ctx context.Context, target store.PhotoVisualPreviewTarget) error {
			require.Equal(t, versions[1], target.VersionID)
			_, err := processing.EnsureVisualPreview(ctx, catalog, blobs, target.VersionID, recipe, false)
			if err == nil {
				processed++
				cancel()
			}
			return err
		},
	}
	require.ErrorIs(t, backfill.Run(ctx), context.Canceled)
	require.Equal(t, 1, processed)
	remaining, err := catalog.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
