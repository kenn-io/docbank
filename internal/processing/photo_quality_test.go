package processing

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoQualityCalibration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		fill       color.RGBA
		brightness float64
	}{
		{"black", color.RGBA{A: 255}, 0}, {"white", color.RGBA{255, 255, 255, 255}, 1}, {"red", color.RGBA{R: 255, A: 255}, 0.2126},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, 16, 16))
			for y := range 16 {
				for x := range 16 {
					img.SetRGBA(x, y, tc.fill)
				}
			}
			s := measurePhotoQuality(img)
			require.NoError(t, document.ValidatePhotoQualitySignals(s))
			require.InDelta(t, tc.brightness, s.Brightness, 1e-12)
			require.Zero(t, s.Focus)
			require.InDelta(t, 1.0, s.Blur, 1e-12)
			require.InDelta(t, 0.5, s.Framing, 1e-12)
		})
	}
	for _, level := range []uint8{20, 255} {
		img := image.NewRGBA(image.Rect(0, 0, 16, 16))
		for y := range 16 {
			for x := range 16 {
				v := uint8(0)
				if (x+y)%2 == 0 {
					v = level
				}
				img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
			}
		}
		s := measurePhotoQuality(img)
		require.InDelta(t, min(float64(level)/255/0.15, 1), s.Focus, 1e-12)
		require.InDelta(t, 1-s.Focus, s.Blur, 1e-12)
	}
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			img.SetRGBA(x, y, color.RGBA{A: 255})
		}
	}
	img.SetRGBA(5, 5, color.RGBA{255, 255, 255, 255})
	s := measurePhotoQuality(img)
	require.Greater(t, s.Framing, 0.5)
	require.InDelta(t, s.Focus*0.35+(1-2*(0.5-s.Brightness))*0.25+s.Framing*0.2, s.Aesthetics, 1e-12)
}

func TestPhotoQualityPreviewPipeline(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := mediatest.JPEG(32, 24, color.White)
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	target := store.PhotoVisualPreviewTarget{VersionID: node.CurrentVersionID, SourceSHA256: written.Hash, Size: written.Size, MediaType: "image/jpeg"}
	require.ErrorIs(t, EvaluatePhotoQuality(t.Context(), catalog, blobs, target), store.ErrNotFound)
	recipe, _ := VisualPreviewRecipeForSize("grid")
	_, err = EnsureVisualPreview(t.Context(), catalog, blobs, node.CurrentVersionID, recipe)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, EvaluatePhotoQuality(ctx, catalog, blobs, target), context.Canceled)
	require.NoError(t, EvaluatePhotoQuality(t.Context(), catalog, blobs, target))
	targets, err := catalog.MissingPhotoQualityTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
}
