package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/color"
	"image/jpeg"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestVisualPreviewRecipes(t *testing.T) {
	t.Parallel()
	fingerprints := map[string]bool{}
	for size, edge := range map[string]int{"grid": 512, "fit": 2560, "large": 4096} {
		recipe, err := VisualPreviewRecipeForSize(size)
		require.NoError(t, err)
		require.Equal(t, edge, recipe.MaxEdgePixels)
		_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
		require.NoError(t, err)
		require.False(t, fingerprints[fingerprint])
		fingerprints[fingerprint] = true
		if size == "large" {
			require.Equal(t, "03f89b744cc013004c89b1babe6d779ee043652519c586453bcde761bd7a4b04", fingerprint)
		}
	}
	_, err := VisualPreviewRecipeForSize("unknown")
	require.Error(t, err)
}

func TestProduceVisualPreviewForRecipe(t *testing.T) {
	t.Parallel()
	source := mediatest.JPEG(4200, 8, color.White)
	digest := sha256.Sum256(source)
	for size, edge := range map[string]int{"grid": 512, "fit": 2560, "large": 4096} {
		t.Run(size, func(t *testing.T) {
			t.Parallel()
			recipe, err := VisualPreviewRecipeForSize(size)
			require.NoError(t, err)
			result, err := ProduceVisualPreviewForRecipe(t.Context(), bytes.NewReader(source), VisualPreviewTarget{SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(source)), MediaType: "image/jpeg"}, recipe)
			require.NoError(t, err)
			require.Equal(t, edge, result.Preview.Output.Width)
			decoded, err := jpeg.Decode(bytes.NewReader(result.Output))
			require.NoError(t, err)
			require.Equal(t, edge, decoded.Bounds().Dx())
			tiny := mediatest.JPEG(3, 2, color.White)
			digest := sha256.Sum256(tiny)
			small, err := ProduceVisualPreviewForRecipe(t.Context(), bytes.NewReader(tiny), VisualPreviewTarget{SourceSHA256: hex.EncodeToString(digest[:]), Size: int64(len(tiny)), MediaType: "image/jpeg"}, recipe)
			require.NoError(t, err)
			require.Equal(t, 3, small.Preview.Output.Width)
		})
	}
	recipe := CurrentVisualPreviewRecipe()
	recipe.MaxEdgePixels = 123
	_, err := ProduceVisualPreviewForRecipe(t.Context(), bytes.NewReader(source), VisualPreviewTarget{}, recipe)
	require.Error(t, err)
}

func TestVisualPreviewSupportsMediaType(t *testing.T) {
	t.Parallel()
	for _, mediaType := range []string{"IMAGE/JPEG; quality=90", "image/png", "image/gif", "image/webp", "image/x-sony-arw", "image/x-adobe-dng", "image/x-canon-cr2", "image/x-nikon-nef", "image/x-fuji-raf"} {
		require.True(t, VisualPreviewSupportsMediaType(mediaType), mediaType)
	}
	for _, mediaType := range []string{"text/plain", "video/mp4", "image/x-olympus-orf", "invalid;"} {
		require.False(t, VisualPreviewSupportsMediaType(mediaType), mediaType)
	}
}

func TestProduceVisualPreviewVerifiesUnsupportedSource(t *testing.T) {
	t.Parallel()
	_, err := ProduceVisualPreviewForRecipe(t.Context(), bytes.NewReader([]byte("bad")), VisualPreviewTarget{SourceSHA256: hex.EncodeToString(make([]byte, 32)), Size: 3, MediaType: "text/plain"}, CurrentVisualPreviewRecipe())
	require.True(t, IsSourceContentUnavailable(err))
}

func TestEnsureVisualPreviewForRecipe(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := mediatest.JPEG(3000, 4, color.White)
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	grid, _ := VisualPreviewRecipeForSize("grid")
	outputFailure := errors.New("synthetic preview output write failure")
	_, err = ensureVisualPreview(t.Context(), catalog, visualPreviewFailingWriter{Store: blobs, err: outputFailure}, node.CurrentVersionID, grid)
	require.ErrorIs(t, err, outputFailure)
	_, gridFingerprint, err := document.MarshalVisualPreviewRecipeV1(grid)
	require.NoError(t, err)
	_, err = catalog.VisualPreviewGenerationByRecipe(t.Context(), node.CurrentVersionID, gridFingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	for _, size := range []string{"grid", "fit", "large"} {
		recipe, err := VisualPreviewRecipeForSize(size)
		require.NoError(t, err)
		first, err := EnsureVisualPreview(t.Context(), catalog, blobs, node.CurrentVersionID, recipe)
		require.NoError(t, err)
		retry, err := EnsureVisualPreview(t.Context(), catalog, blobs, node.CurrentVersionID, recipe)
		require.NoError(t, err)
		require.Equal(t, first, retry)
		reader, _, err := blobs.OpenStreamContext(t.Context(), first.Generation.Preview.Output.BlobSHA256)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
	}
	recipe, _ := VisualPreviewRecipeForSize("grid")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = EnsureVisualPreview(cancelled, catalog, blobs, node.CurrentVersionID, recipe)
	require.ErrorIs(t, err, context.Canceled)
	missing, err := catalog.CreateFile(t.Context(), catalog.RootID(), "missing.jpg", hex.EncodeToString(make([]byte, 32)), 3, "image/jpeg")
	require.NoError(t, err)
	_, err = EnsureVisualPreview(t.Context(), catalog, blobs, missing.CurrentVersionID, recipe)
	require.True(t, IsSourceContentUnavailable(err))
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	_, err = catalog.VisualPreviewGenerationByRecipe(t.Context(), missing.CurrentVersionID, fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
}

type visualPreviewFailingWriter struct {
	*blob.Store

	err error
}

func (writer visualPreviewFailingWriter) WriteDetailedContext(context.Context, io.Reader) (blob.WriteReceipt, error) {
	return blob.WriteReceipt{}, writer.err
}
