package production

import (
	"bytes"
	"context"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

type previewClosingEngine struct{ pdfproduction.Engine }

func (e previewClosingEngine) Render(ctx context.Context, source pdfproduction.Source,
	page redaction.Page, recipe redaction.Recipe) (pdfproduction.Raster, error) {
	raster, err := e.Engine.Render(ctx, source, page, recipe)
	if file, ok := source.Reader.(*os.File); ok {
		_ = file.Close()
	}
	return raster, err
}

func TestRenderUnnumberedProductionPreviewPageBurnsVerifiedSourceAndReturnsSanitizedText(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	page := redaction.Page{Number: 1, FrameSHA256: testHash("preview frame"),
		Width: 85_000, Height: 110_000, Span: redaction.Span{Start: 0, End: 1}}
	member := newStoredFixture(t).finalized.Authority.Prepared.Members[0]
	member.Resolved = endorsementResolved(t, recipe, []redaction.Page{page}, []endorsementDecision{{
		id: "76000000-0000-4000-8000-000000000001", label: "SEALED", page: 1,
		x0: 0, y0: 0, x1: 85_000, y1: 110_000,
	}})
	member.ResolvedSHA256 = member.Resolved.SHA256
	sourcePDF := fpdf.New("P", "pt", "Letter", "")
	sourcePDF.AddPage()
	sourcePDF.SetFillColor(0, 0, 0)
	sourcePDF.Rect(50, 50, 100, 100, "F")
	var pdf bytes.Buffer
	require.NoError(t, sourcePDF.Output(&pdf))
	member.Member.PDFSHA256 = testHash(pdf.String())
	member.Member.PDFSize = int64(pdf.Len())
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	source := PinnedProductionPDF{PDFSHA256: member.Member.PDFSHA256, Size: member.Member.PDFSize,
		Stream: &syntheticVerifiedPDF{Reader: bytes.NewReader(pdf.Bytes()), verified: true}}
	preview, err := RenderUnnumberedProductionPreviewPage(t.Context(), source, member, 1, recipe, engine)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, preview.File.Close()) })
	require.Equal(t, 1, preview.Page)
	require.Equal(t, member.Resolved.SHA256, preview.ResolvedSHA256)
	require.Equal(t, []byte("[REDACTED]"), preview.Text)
	require.Equal(t, testHash(string(preview.Text)), preview.TextSHA256)
	for _, endorsement := range preview.Endorsements {
		require.NotEqual(t, "number", endorsement.Kind)
	}
	require.Positive(t, preview.PNGSize)
	require.NotEmpty(t, preview.PNGSHA256)
	_, err = preview.File.File.Seek(0, io.SeekStart)
	require.NoError(t, err)
	image, err := png.Decode(preview.File.File)
	require.NoError(t, err)
	require.Equal(t, 2550, image.Bounds().Dx())
	require.Equal(t, 3300, image.Bounds().Dy())
	// Pixel 1200,1200 is outside the black source square yet inside the
	// resolved full-page mask. The public label occupies only the top edge.
	require.Equal(t, color.NRGBA{R: 0, G: 0, B: 0, A: 255},
		color.NRGBAModel.Convert(image.At(1200, 1200)))
	path := preview.File.File.Name()
	require.NoError(t, preview.File.Close())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRenderUnnumberedProductionPreviewPageRejectsWrongSourceBeforeRendering(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	member := newStoredFixture(t).finalized.Authority.Prepared.Members[0]
	member.Resolved = endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: testHash("preview frame"), Width: 85_000,
		Height: 110_000, Span: redaction.Span{Start: 0, End: 1},
	}}, nil)
	member.ResolvedSHA256 = member.Resolved.SHA256
	source := PinnedProductionPDF{PDFSHA256: testHash("wrong"), Size: 8,
		Stream: &syntheticVerifiedPDF{Reader: bytes.NewReader([]byte("12345678")), verified: true}}
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	_, err = RenderUnnumberedProductionPreviewPage(t.Context(), source, member, 1, recipe, engine)
	require.ErrorIs(t, err, ErrJobConflict)
}

func TestRenderUnnumberedProductionPreviewPageDropsArtifactWhenSourceCleanupFails(t *testing.T) {
	recipe := pdfproduction.QualifiedRecipe()
	member := newStoredFixture(t).finalized.Authority.Prepared.Members[0]
	member.Resolved = endorsementResolved(t, recipe, []redaction.Page{{
		Number: 1, FrameSHA256: testHash("preview frame"), Width: 85_000,
		Height: 110_000, Span: redaction.Span{Start: 0, End: 1},
	}}, nil)
	member.ResolvedSHA256 = member.Resolved.SHA256
	pdf := redactiontest.PDF(t, []string{"synthetic source"}, "synthetic production")
	member.Member.PDFSHA256 = testHash(string(pdf))
	member.Member.PDFSize = int64(len(pdf))
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	source := PinnedProductionPDF{PDFSHA256: member.Member.PDFSHA256, Size: member.Member.PDFSize,
		Stream: &syntheticVerifiedPDF{Reader: bytes.NewReader(pdf), verified: true}}
	preview, err := RenderUnnumberedProductionPreviewPage(t.Context(), source, member, 1,
		recipe, previewClosingEngine{engine})
	if preview != nil {
		t.Cleanup(func() { require.NoError(t, preview.File.Close()) })
	}
	require.ErrorIs(t, err, os.ErrClosed)
	require.Nil(t, preview, "a cleanup error must not leak a candidate preview artifact")
}
