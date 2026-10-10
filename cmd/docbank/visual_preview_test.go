package main

import (
	"bytes"
	"context"
	"image/color"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
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

func TestVisualPreviewBackfillProcessesGridAndLeavesLegacyHeadEmpty(t *testing.T) {
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := mediatest.JPEG(1024, 768, color.White)
	var versions []string
	for _, name := range []string{"one.jpg", "two.jpg"} {
		receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
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
	_, err = processing.EnsureVisualPreview(t.Context(), catalog, blobs, versions[0], recipe)
	require.NoError(t, err)
	backfill, err := newVisualPreviewBackfill(catalog, blobs, api.NewOperationGate(), slog.Default())
	require.NoError(t, err)
	backfill.DrainOnce = true
	control, err := catalog.SetLaneControl(t.Context(), store.LaneControl{Lane: store.VisualPreviewLane, Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	readControl := backfill.Control
	process := backfill.Process
	var processed atomic.Int32
	backfill.Process = func(ctx context.Context, target store.PhotoVisualPreviewTarget) error {
		processed.Add(1)
		return process(ctx, target)
	}
	pausedReads := 0
	backfill.Control = func(ctx context.Context) (store.LaneControl, error) {
		current, err := readControl(ctx)
		if err != nil || !current.Paused {
			return current, err
		}
		pausedReads++
		require.Zero(t, processed.Load())
		_, err = catalog.ContentVersionVisualPreviewByRecipe(ctx, versions[1], fingerprint)
		require.ErrorIs(t, err, store.ErrNotFound)
		if pausedReads == 2 {
			control.Paused = false
			_, err = catalog.SetLaneControl(ctx, control, control.Revision)
			require.NoError(t, err)
		}
		return current, nil
	}
	require.NoError(t, backfill.Run(t.Context()))
	require.Equal(t, 2, pausedReads)
	require.Equal(t, int32(1), processed.Load())
	view, err := catalog.ContentVersionVisualPreviewByRecipe(t.Context(), versions[1], fingerprint)
	require.NoError(t, err)
	require.Equal(t, document.VisualPreviewReady, view.Generation.Preview.State)
	require.Equal(t, 512, view.Generation.Preview.Output.Width)
	_, err = catalog.ContentVersionVisualPreview(t.Context(), versions[1])
	require.ErrorIs(t, err, store.ErrNotFound)
	remaining, err := catalog.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestVisualPreviewBackfillMovesPastPhotoWithoutMediaType(t *testing.T) {
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	var versions []string
	for index, entry := range []struct{ name, mediaType string }{{"untagged.jpg", ""}, {"one.jpg", "image/jpeg"}, {"two.jpg", "image/jpeg"}} {
		source := mediatest.JPEG(64+index, 48, color.White)
		receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), catalog.RootID(), entry.name, receipt.Hash, receipt.Size, entry.mediaType, store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created})
		require.NoError(t, err)
		_, err = catalog.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		versions = append(versions, node.CurrentVersionID)
	}
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	backfill, err := newVisualPreviewBackfill(catalog, blobs, api.NewOperationGate(), slog.Default())
	require.NoError(t, err)
	backfill.DrainOnce = true
	require.NoError(t, backfill.Run(t.Context()))
	untagged, err := catalog.ContentVersionVisualPreviewByRecipe(t.Context(), versions[0], fingerprint)
	require.NoError(t, err)
	require.Equal(t, document.VisualPreviewUnsupported, untagged.Generation.Preview.State)
	for _, version := range versions[1:] {
		view, err := catalog.ContentVersionVisualPreviewByRecipe(t.Context(), version, fingerprint)
		require.NoError(t, err)
		require.Equal(t, document.VisualPreviewReady, view.Generation.Preview.State)
	}
	remaining, err := catalog.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
