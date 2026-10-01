package docbank

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	internalprocessing "go.kenn.io/docbank/internal/processing"
	"image/color"
	"image/jpeg"
	"io"
	"testing"
)

func TestVisualPreviewSupportsMediaType(t *testing.T) {
	require.True(t, VisualPreviewSupportsMediaType("IMAGE/JPEG; q=90"))
	require.True(t, VisualPreviewSupportsMediaType("image/x-adobe-dng"))
	require.False(t, VisualPreviewSupportsMediaType("video/mp4"))
}

func TestVaultVisualPreviewSizes(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	source := mediatest.JPEG(4200, 8, color.White)
	created, err := vault.Create(t.Context(), "/photo.jpg", bytes.NewReader(source), CreateOptions{MediaType: "image/jpeg", Expected: contentIdentity(source)})
	require.NoError(t, err)
	versionID := created.Version.ID
	_, err = vault.VisualPreviewForSize(t.Context(), versionID, VisualPreviewGrid)
	require.ErrorIs(t, err, ErrNotFound)
	for size, edge := range map[VisualPreviewSize]int{VisualPreviewGrid: 512, VisualPreviewFit: 2560} {
		first, err := vault.EnsureVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		require.Equal(t, edge, first.Output.Width)
		_, err = vault.VisualPreview(t.Context(), versionID)
		require.ErrorIs(t, err, ErrNotFound)
	}
	_, err = internalprocessing.EnsureVisualPreview(t.Context(), vault.metadata, vault.blobs, versionID, internalprocessing.CurrentVisualPreviewRecipe(), false)
	require.NoError(t, err)
	_, err = vault.VisualPreview(t.Context(), versionID)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, vault.blobs.Remove(created.Version.BlobHash))
	large, err := vault.EnsureVisualPreview(t.Context(), versionID)
	require.NoError(t, err)
	require.Equal(t, 4096, large.Output.Width)
	for size, edge := range map[VisualPreviewSize]int{VisualPreviewGrid: 512, VisualPreviewFit: 2560, VisualPreviewLarge: 4096} {
		exact, err := vault.VisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		retry, err := vault.EnsureVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		require.Equal(t, exact, retry)
		opened, err := vault.OpenVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		data, err := io.ReadAll(opened.Reader)
		require.NoError(t, err)
		require.NoError(t, opened.Reader.Close())
		image, err := jpeg.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, edge, image.Bounds().Dx())
		legacy, err := vault.VisualPreview(t.Context(), versionID)
		require.NoError(t, err)
		require.Equal(t, large.GenerationID, legacy.GenerationID)
	}
	_, err = vault.EnsureVisualPreviewForSize(t.Context(), versionID, "invalid")
	require.Error(t, err)
}
