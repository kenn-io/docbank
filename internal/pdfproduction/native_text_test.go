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
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/redactiontest"
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
	pages, err := InspectNativeText(t.Context(), Source{t.TempDir(),
		bytes.NewReader(encoded.Bytes()), source.Size, source.SHA256}, frames, nil)
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
	if count == 2 {
		// Each page fits on its own, but retaining both must honor the
		// document observation budget before alignment builds the map.
		first, _, err := canonical.BoundedSize(pages[0], 1<<20)
		require.NoError(t, err)
		second, _, err := canonical.BoundedSize(pages[1], 1<<20)
		require.NoError(t, err)
		_, err = inspectNativeText(t.Context(), nativeTextInput{source: Source{t.TempDir(),
			bytes.NewReader(encoded.Bytes()), source.Size, source.SHA256}, frames: frames, maxObservationBytes: first + second - 1})
		var problem *redaction.Problem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, "render_limit", problem.Code)
	}
}

func TestInspectNativeTextFromInspection(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	const text = "of the staff of life, if it fits."
	data := redactiontest.FontPDF("BT /F1 12 Tf 72.37 720 Td ("+text+") Tj ET",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>", "")
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: hashBytes(data), Size: int64(len(data))}
	inspection, err := runtime.Inspect(t.Context(), data, source, "application/pdf")
	require.NoError(t, err)
	pages, err := InspectNativeTextFromInspection(t.Context(), NativeInspectionInput{
		Source:     Source{Reader: bytes.NewReader(data), Size: source.Size, SHA256: source.SHA256, StagingDirectory: t.TempDir()},
		Inspection: inspection, MaxObservationBytes: 8 << 20,
	})
	require.NoError(t, err)
	require.Len(t, pages, 1)
	var observed strings.Builder
	for _, glyph := range pages[0].Glyphs {
		require.Empty(t, glyph.GapReason)
		observed.WriteString(glyph.Text)
	}
	require.Equal(t, text, observed.String())
}
