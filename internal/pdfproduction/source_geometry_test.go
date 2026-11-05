package pdfproduction

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestNativeSourceGeometry(t *testing.T) {
	engine, err := NewPDFium(QualifiedRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	concrete, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	for _, fixture := range redactiontest.GeometryFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			data := redactiontest.GeometryPDF(fixture.Media, fixture.Crop, []int{0, 90, 180, 270}, false)
			source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: hashBytes(data), Size: int64(len(data))}
			require.NoError(t, concrete.withInstanceContext(t.Context(), func(_ context.Context, instance pdfium.Pdfium) error {
				doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
				require.NoError(t, err)
				for i, rotation := range []int{0, 90, 180, 270} {
					page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}}
					media, err := instance.FPDFPage_GetMediaBox(&requests.FPDFPage_GetMediaBox{Page: page})
					require.NoError(t, err)
					crop, err := instance.FPDFPage_GetCropBox(&requests.FPDFPage_GetCropBox{Page: page})
					require.NoError(t, err)
					// Literal vectors are also checked against EmbedPDF's actual document pages.
					require.Equal(t, [4]float64{float64(media.Left), float64(media.Bottom), float64(media.Right), float64(media.Top)}, fixture.ObservedMedia)
					require.Equal(t, [4]float64{float64(crop.Left), float64(crop.Bottom), float64(crop.Right), float64(crop.Top)}, fixture.ObservedCrop)
					frame, err := document.NewPDFPageFrame(source, i+1, fixture.Media, fixture.Crop, rotation)
					require.NoError(t, err)
					require.NoError(t, checkNativePageFrame(instance, page, frame))
				}
				return nil
			}))
		})
	}
}

func TestNativeSourceGeometryRefusesDisagreement(t *testing.T) {
	media, crop := [4]float64{0, 0, 216, 144}, [4]float64{24, 36, 168, 108}
	for _, tc := range []struct {
		name                               string
		media, crop, frameMedia, frameCrop [4]float64
		rotation                           int
		inherited                          bool
	}{
		{"media", media, crop, [4]float64{0, 0, 217, 144}, crop, 0, false},
		{"crop", media, crop, media, [4]float64{25, 36, 168, 108}, 0, false},
		{"rotation", media, crop, media, crop, 90, false},
		{"distinct inherited crop", media, crop, media, crop, 0, true},
		// A small crop, not an over-limit 8192pt raster: only projection exceeds the bound.
		{"large offset", [4]float64{0, 0, 9000, 144}, [4]float64{8192.0004, 24, 8360.0004, 120}, [4]float64{0, 0, 9000, 144}, [4]float64{8192.0004, 24, 8360.0004, 120}, 0, false},
		// Both round within one unit of the frame, but only .5512 is its projection.
		{"adjacent outside projection", [4]float64{0, 0, 1296, 1008}, [4]float64{38.5512, 25.1234, 1190.5513, 817.1234}, [4]float64{0, 0, 1296, 1008}, [4]float64{38.5512, 25.1234, 1190.5512, 817.1234}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := redactiontest.GeometryPDF(tc.media, tc.crop, []int{0}, tc.inherited)
			source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: hashBytes(data), Size: int64(len(data))}
			frame, err := document.NewPDFPageFrame(source, 1, tc.frameMedia, tc.frameCrop, tc.rotation)
			require.NoError(t, err)
			_, err = InspectNativeText(t.Context(), Source{Reader: bytes.NewReader(data), SHA256: source.SHA256, Size: source.Size, StagingDirectory: t.TempDir()}, []document.PageFrameV1{frame}, nil)
			if tc.inherited {
				require.ErrorContains(t, err, "MediaBox or CropBox is unavailable")
			} else {
				require.ErrorContains(t, err, "MediaBox, CropBox, or rotation differs from frame")
			}
		})
	}
}

func TestNativeSourceDimensions(t *testing.T) {
	data := redactiontest.GeometryPDF([4]float64{0, 0, 216, 144}, [4]float64{24, 24, 192, 120}, []int{0}, false)
	engine, err := NewPDFium(QualifiedRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	source := Source{Reader: bytes.NewReader(data), SHA256: hashBytes(data), Size: int64(len(data)), StagingDirectory: t.TempDir()}
	frame, err := document.NewPDFPageFrame(document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: source.SHA256, Size: source.Size}, 1, [4]float64{0, 0, 216, 144}, [4]float64{24, 24, 192, 120}, 0)
	require.NoError(t, err)
	_, frameHash, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, Width: 23333, Height: 13333, FrameSHA256: frameHash}
	raster, err := engine.Render(t.Context(), source, page, QualifiedRecipe())
	require.NoError(t, err)
	require.Equal(t, 700, raster.Pixels.Bounds().Dx())
	require.Equal(t, 400, raster.Pixels.Bounds().Dy())
	otherFrame, err := document.NewPDFPageFrame(frame.Source, 1, [4]float64{0, 0, 216, 144}, [4]float64{24, 24, 192.01, 120}, 0)
	require.NoError(t, err)
	_, page.FrameSHA256, err = document.MarshalPageFrameV1(otherFrame)
	require.NoError(t, err)
	page.Width = otherFrame.Width
	_, err = engine.Render(t.Context(), source, page, QualifiedRecipe())
	require.ErrorContains(t, err, "dimensions differ")
}

func TestNativeSourceGeometryEqualInheritedBoxes(t *testing.T) {
	box := [4]float64{38.5512, 25.1234, 1190.5512, 817.1234}
	data := redactiontest.GeometryPDF(box, box, []int{0}, true)
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: hashBytes(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, box, box, 0)
	require.NoError(t, err)
	pages, err := InspectNativeText(t.Context(), Source{Reader: bytes.NewReader(data), SHA256: source.SHA256, Size: source.Size, StagingDirectory: t.TempDir()}, []document.PageFrameV1{frame}, nil)
	require.NoError(t, err)
	var text strings.Builder
	for _, glyph := range pages[0].Glyphs {
		text.WriteString(glyph.Text)
	}
	require.Equal(t, "SECRET KEEP", text.String())
}
