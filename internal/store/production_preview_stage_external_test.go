package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPrepareProductionDraftPreviewStagesVerifiedImageAndSanitizedText(t *testing.T) {
	vault, root, set, draft, member, sourcePDF := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(sourcePDF))
	require.NoError(t, err)
	require.Equal(t, member.PDFSHA256, written.Hash)
	require.NoError(t, vault.RecordBlob(t.Context(), written.Hash, written.Size, store.BlobPhysical{
		Encoding: "raw", StoredBytes: written.StoredSize, Created: written.Created,
	}))
	stageDir := filepath.Join(root, "web-downloads")
	preview, err := processing.PrepareProductionDraftPreview(t.Context(), vault, blobs,
		stageDir, set.ID, draft.Revision, draft.ETag, member.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, preview.Close()) })
	require.True(t, canonical.IsSHA256Hex(preview.PreviewInputSHA256))
	require.Equal(t, member.ID, preview.MemberID)
	require.Equal(t, 1, preview.Page)
	require.NotEmpty(t, preview.ImageSHA256)
	require.Positive(t, preview.ImageSize)
	require.True(t, filepath.IsLocal(filepath.Base(preview.Image.File.Name())))
	require.Equal(t, stageDir, filepath.Dir(preview.Image.File.Name()))
	require.Equal(t, stageDir, filepath.Dir(preview.Text.File.Name()))
	for _, path := range []string{stageDir, preview.Image.File.Name(), preview.Text.File.Name()} {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.Zero(t, info.Mode().Perm()&0o077, "preview staging must be owner-private")
	}
	imageBytes, err := io.ReadAll(preview.Image.File)
	require.NoError(t, err)
	require.Equal(t, preview.ImageSize, int64(len(imageBytes)))
	imageSum := sha256.Sum256(imageBytes)
	require.Equal(t, hex.EncodeToString(imageSum[:]), preview.ImageSHA256)
	image, err := png.Decode(bytes.NewReader(imageBytes))
	require.NoError(t, err)
	require.Equal(t, 300, image.Bounds().Dx())
	require.Equal(t, 300, image.Bounds().Dy())
	require.Equal(t, color.NRGBA{A: 255}, color.NRGBAModel.Convert(image.At(150, 150)))
	textBytes, err := io.ReadAll(preview.Text.File)
	require.NoError(t, err)
	require.Equal(t, []byte("[REDACTED]"), textBytes)
	require.NotContains(t, string(textBytes), "synthetic private reason")
	textSum := sha256.Sum256(textBytes)
	require.Equal(t, hex.EncodeToString(textSum[:]), preview.TextSHA256)
	require.Equal(t, int64(len(textBytes)), preview.TextSize)
	imagePath, textPath := preview.Image.File.Name(), preview.Text.File.Name()
	require.NoError(t, preview.Close())
	_, err = os.Stat(imagePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(textPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPrepareProductionDraftPreviewRejectsStaleDraftBeforeOpeningSource(t *testing.T) {
	vault, root, set, draft, member, _ := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	stageDir := filepath.Join(root, "web-downloads")
	preview, err := processing.PrepareProductionDraftPreview(t.Context(), vault, blobs,
		stageDir, set.ID, draft.Revision, draft.ETag+1, member.ID, 1)
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	require.Nil(t, preview)
	entries, err := os.ReadDir(stageDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPrepareProductionDraftPreviewMissingPDFLeavesNoCandidateFiles(t *testing.T) {
	vault, root, set, draft, member, _ := store.ProductionPreviewStageFixture(t)
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	stageDir := filepath.Join(root, "web-downloads")
	preview, err := processing.PrepareProductionDraftPreview(t.Context(), vault, blobs,
		stageDir, set.ID, draft.Revision, draft.ETag, member.ID, 1)
	require.Error(t, err)
	require.Nil(t, preview)
	entries, err := os.ReadDir(stageDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
