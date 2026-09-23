package pdfproduction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/redactiontest"
)

func qualificationRecipe() redaction.Recipe {
	return QualifiedRecipe()
}

func TestPDFiumLocalRender(t *testing.T) {
	pdf := redactiontest.PDF(t, []string{"synthetic page"}, "synthetic qualification")
	engine, err := NewPDFium(qualificationRecipe())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	page := redaction.Page{Number: 1, FrameSHA256: digest([]byte("frame")), Width: 85000, Height: 110000}
	raster, err := engine.Render(t.Context(), Source{bytes.NewReader(pdf), int64(len(pdf)), digest(pdf)}, page, qualificationRecipe())
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, 2550, 3300), raster.Pixels.Bounds())
	require.Equal(t, 300, raster.DPI)
	require.Equal(t, page, raster.Page)
}

func TestPhysicalPointDecimalIsExactForInchTenThousandths(t *testing.T) {
	tests := map[int64]string{
		0:                       "0",
		1:                       "0.0072",
		125:                     "0.9",
		85000:                   "612",
		document.MaxPageInteger: "64851834634135.1352",
	}
	for value, expected := range tests {
		actual, err := physicalPointDecimal(value)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
		parsed, err := freshPhysicalPointDecimal(value)
		require.NoError(t, err)
		require.Equal(t, expected, parsed)
	}
}

func TestPhysicalPixelRectDoesNotOverflowAtAcceptedCoordinateLimit(t *testing.T) {
	frame := document.PageFrameV1{Width: document.MaxPageInteger, Height: document.MaxPageInteger}
	box := physicalBounds{
		x0: document.MaxPageInteger - 20_000, y0: document.MaxPageInteger - 30_000,
		x1: document.MaxPageInteger - 10_000, y1: document.MaxPageInteger - 15_000,
	}
	got := physicalPixelRect(image.Rect(0, 0, 16384, 16384), frame, box)
	require.Equal(t, image.Rect(16382, 16382, 16384, 16384), got)
}

func TestFreshWriterUsesInchTenThousandGeometry(t *testing.T) {
	artifact := unicodeArtifact(t)
	artifact.Page.Width, artifact.Page.Height = 85000, 110000
	artifact.Layout.Source, artifact.Layout.Output = artifact.Page, artifact.Page
	layout, err := canonical.Marshal(artifact.Layout)
	require.NoError(t, err)
	artifact.LayoutSHA256 = digest(layout)
	img := image.NewNRGBA(image.Rect(0, 0, 2550, 3300))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var pngData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, img))
	encoded := bytes.Clone(pngData.Bytes())
	artifact.PNGSHA256, artifact.PNGSize = digest(encoded), int64(len(encoded))
	artifact.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }

	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{artifact}}, qualificationRecipe()))
	require.Contains(t, output.String(), "/MediaBox [0 0 612 792]")
}

func TestFreshUnicodePDF(t *testing.T) {
	artifact := unicodeArtifact(t)
	var output bytes.Buffer
	require.NoError(t, writeFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{artifact}}, qualificationRecipe()))
	// PDFium independently interprets the PDF font program and ToUnicode CMap.
	// The expected Unicode is manually authored, not serialized by production code.
	pool, err := webassembly.Init(webassembly.Config{FSConfig: wazero.NewFSConfig(), MaxTotal: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	instance, err := pool.GetInstanceWithContext(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, instance.Close()) })
	data := output.Bytes()
	doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	require.NoError(t, err)
	text, err := instance.GetPageText(&requests.GetPageText{Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}}})
	require.NoError(t, err)
	require.Contains(t, text.Text, "café")
	require.Contains(t, text.Text, "給与")
	require.Contains(t, text.Text, "[REDACTED]")
}

type artifactSequence struct {
	pages []PageArtifact
	index int
}

func (s *artifactSequence) Next(context.Context) (PageArtifact, error) {
	if s.index == len(s.pages) {
		return PageArtifact{}, io.EOF
	}
	p := s.pages[s.index]
	s.index++
	return p, nil
}

func digest(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }

func unicodeArtifact(tb testing.TB) PageArtifact {
	tb.Helper()
	page := redaction.Page{Number: 1, FrameSHA256: digest([]byte("frame")), Width: 10000, Height: 10000, Span: redaction.Span{End: 21}}
	box := func(y int64) []redaction.Box {
		return []redaction.Box{{Page: 1, FrameSHA256: page.FrameSHA256, X0: 1000, Y0: y, X1: 9000, Y1: y + 1000}}
	}
	runs := []redaction.Run{
		{Kind: "text", Text: "café", Page: 1, SourceSpan: &redaction.Span{Start: 0, End: 5}, Boxes: box(500), FontSizeMilliPoints: 11000},
		{Kind: "text", Text: "給与", Page: 1, SourceSpan: &redaction.Span{Start: 5, End: 11}, Boxes: box(2000), FontSizeMilliPoints: 11000},
		{Kind: "redaction", Text: "[REDACTED]", Page: 1, Boxes: box(3500), FontSizeMilliPoints: 11000},
	}
	img := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	for y := range 300 {
		for x := range 300 {
			img.SetNRGBA(x, y, color.NRGBA{R: byte(x), G: byte(y), B: byte(x ^ y), A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(tb, png.Encode(&encoded, img))
	data := bytes.Clone(encoded.Bytes())
	layout := redaction.PageLayout{Source: page, Output: page}
	layoutJSON, err := canonical.Marshal(layout)
	require.NoError(tb, err)
	endorsements := []redaction.Endorsement{}
	endorsementsJSON, err := canonical.Marshal(endorsements)
	require.NoError(tb, err)
	return PageArtifact{Page: page, PNGSHA256: digest(data), ResolvedSHA256: digest([]byte("sanitized plan")),
		EndorsementsSHA256: digest(endorsementsJSON), LayoutSHA256: digest(layoutJSON), PNGSize: int64(len(data)),
		OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, Runs: runs, Layout: layout, Endorsements: endorsements}
}
