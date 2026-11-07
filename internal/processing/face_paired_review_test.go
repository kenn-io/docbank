package processing

import (
	"bytes"
	"image/color"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestReviewPairedFaceSources(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	f := x.f
	rawBytes := mediatest.TIFFWithEmbeddedJPEG(1, mediatest.TIFFJPEGPreview{Data: mediatest.JPEG(1024, 768, color.White)})
	written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(rawBytes))
	require.NoError(t, err)
	run, err := f.catalog.BeginIngest(ctx, "cli", "Synthetic paired source")
	require.NoError(t, err)
	raw, err := f.catalog.IngestFileExact(ctx, run, f.catalog.RootID(), "paired.nef", written.Hash, written.Size, "image/x-nikon-nef", "paired.nef", "", processingBlobPhysical(t, written))
	require.NoError(t, err)
	asset, err := f.catalog.PhotoAssetForNode(ctx, raw.ID)
	require.NoError(t, err)
	_, err = f.catalog.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = f.catalog.AttachPhotoFile(ctx, f.asset.ID, f.asset.Revision, raw.ID, store.PhotoRoleRAW, nil)
	require.NoError(t, err)
	for _, file := range asset.Files {
		if file.NodeID == f.node.ID {
			asset, err = f.catalog.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &file.ID)
			require.NoError(t, err)
		}
	}
	_, _, err = f.catalog.CreatePhotoFace(ctx, raw.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	f.grant(t, x.service, config.LocalFacesProfile)
	quality, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	targets, err := f.catalog.MissingPhotoVisualPreviewTargetsAfter(ctx, quality.GridRecipe, "", 100)
	require.NoError(t, err)
	for _, target := range targets {
		_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, target.VersionID, f.recipe)
		require.NoError(t, err)
	}
	for range 2 {
		x.worker.reconcileAt = time.Time{}
		_, err = x.worker.ScanOnce(ctx)
		require.NoError(t, err)
	}
	sets, err := f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	var rawFound bool
	for _, set := range sets.Sets {
		if set.Set.InputGeneration.SourceVersionID == raw.CurrentVersionID {
			rawFound = true
		}
	}
	require.True(t, rawFound, "paired RAW face must reach suggestions while JPEG is display")
}
