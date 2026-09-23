package pdfproduction

import (
	"bytes"
	"context"
	"image"
	"os"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestNativeVisibilityAmbiguityStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := nativeGlyphAmbiguity(ctx, image.Rect(0, 0, 100, 100), []image.Rectangle{image.Rect(0, 0, 100, 100)})
	require.ErrorIs(t, err, context.Canceled)
}

func TestInspectNativeTextAcrossPages(t *testing.T) {
	count := 2
	if os.Getenv("DOCBANK_PDF_QUALIFICATION") == "1" {
		count = 1000 // Exercises a document that exceeds one page's deadline.
	}
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	for range count {
		pdf.AddPage()
		pdf.Text(72, 72, "alpha")
	}
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: hashBytes(encoded.Bytes()), Size: int64(encoded.Len())}
	frames := make([]document.PageFrameV1, count)
	for index := range frames {
		var err error
		frames[index], err = document.NewPDFPageFrame(source, index+1,
			[4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
	}
	pages, err := InspectNativeText(t.Context(), Source{bytes.NewReader(encoded.Bytes()), source.Size, source.SHA256}, frames)
	require.NoError(t, err)
	require.Len(t, pages, count)
	for index, page := range pages {
		require.Equal(t, index+1, page.Number)
		var text strings.Builder
		for _, glyph := range page.Glyphs {
			text.WriteString(glyph.Text)
		}
		require.Equal(t, "alpha", text.String())
	}
}
