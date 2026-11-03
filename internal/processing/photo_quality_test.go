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
	"go.kenn.io/docbank/internal/query"
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

func TestPhotoQualityGoldenVector(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			p := color.RGBA{uint8(20 + 3*x + y), uint8(10 + x + 4*y), uint8(5 + 2*x + 2*y), 255}
			if x >= 20 && y < 10 {
				p.R += 40
				p.G += 20
				p.B += 10
			}
			img.SetRGBA(x, y, p)
		}
	}
	// Changing this output requires bumping the evaluator label in document/photo_quality.go.
	s := measurePhotoQuality(img)
	require.InDelta(t, 0.1134902069716774, s.Focus, 1e-12)
	require.InDelta(t, 0.8865097930283226, s.Blur, 1e-12)
	require.InDelta(t, 0.2953981341911764, s.Brightness, 1e-12)
	require.InDelta(t, 0.32990196078431394, s.ColorRed, 1e-12)
	require.InDelta(t, 0.2911611519607846, s.ColorGreen, 1e-12)
	require.InDelta(t, 0.23576899509803928, s.ColorBlue, 1e-12)
	require.InDelta(t, 0.5775685412207165, s.Framing, 1e-12)
	require.InDelta(t, 0.3217609409170735, s.Aesthetics, 1e-12)
}

func TestPhotoQualityPreviewPipeline(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "docbank.db"))
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

func TestPhotoQualityUndecodablePreviewIsUnavailable(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", source.Hash, source.Size, "image/jpeg", processingBlobPhysical(t, source))
	require.NoError(t, err)
	garbage, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader([]byte("verified bytes that are not a JPEG")))
	require.NoError(t, err)
	recipe, err := VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
		ContractVersion: document.VisualPreviewContractV1, Recipe: recipe, SourceSHA256: source.Hash,
		State: document.VisualPreviewReady,
		Output: &document.VisualPreviewOutputV1{
			BlobSHA256: garbage.Hash, Size: garbage.Size, MediaType: "image/jpeg", Width: 32, Height: 24,
		},
	})
	require.NoError(t, err)
	_, err = catalog.PublishVisualPreview(t.Context(), node.CurrentVersionID, canonical, new(processingBlobPhysical(t, garbage)))
	require.NoError(t, err)
	targets, err := catalog.MissingPhotoQualityTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	face, _, err := catalog.CreatePhotoFace(t.Context(), node.CurrentVersionID, "11111111-1111-4111-8111-111111111111", document.FaceBox{Width: .5, Height: .5}, "")
	require.NoError(t, err)
	require.NoError(t, EvaluatePhotoQuality(t.Context(), catalog, blobs, targets[0]))

	page, err := catalog.ListPhotoAssets(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].QualityUnavailable)
	require.NotNil(t, page.Items[0].Quality)
	require.Len(t, page.Items[0].Quality.FaceReports, 1)
	report := page.Items[0].Quality.FaceReports[0]
	require.Equal(t, face.FaceID, report.FaceID)
	require.InDelta(t, .25, report.Prominence, 1e-12)
	require.Nil(t, report.Sharpness)
	require.Nil(t, report.Light)

	targets, err = catalog.MissingPhotoQualityTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestPhotoFaceCropReports(t *testing.T) {
	t.Parallel()
	source := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := range 16 {
		for x := range 32 {
			v := uint8(0)
			if x < 16 && (x+y)%2 == 0 {
				v = 255
			}
			source.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	face := store.PhotoFace{Box: document.FaceBox{X: 0, Y: 0, Width: 0.5, Height: 1}}
	var report, dark, tiny document.PhotoFaceReport
	measurePhotoFace(source, face, &report)
	require.NotNil(t, report.Sharpness)
	require.Greater(t, *report.Sharpness, 0.9)
	require.Greater(t, *report.Light, 0.9)
	face.Box.X = 0.5
	measurePhotoFace(source, face, &dark)
	require.Zero(t, *dark.Light)
	require.Zero(t, *dark.Sharpness)
	face.Box.Width = 0.2
	measurePhotoFace(source, face, &tiny)
	require.Nil(t, tiny.Light)
	require.Nil(t, tiny.Sharpness)
}
