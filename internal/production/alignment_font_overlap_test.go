package production

// Ink remains a refusal even when another glyph completely contains its bounds.
import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestAlignQualifiedFontOverlappingPeriod(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	for _, position := range [][2]int{{79, 725}} {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			var rasters [][]byte
			for _, dot := range []bool{false, true} {
				content := "BT /F2 24 Tf 72 720 Td (B) Tj ET"
				if dot {
					content += fmt.Sprintf(" BT /F1 3 Tf %d %d Td <2E> Tj ET", position[0], position[1])
				}
				pdf := attributionOverlapPDF(content)
				m := alignFontFixture(t, pdf, "B", runtime)
				_, resolveErr := redaction.Resolve(m, "redact_selected", nil, pdfproduction.QualifiedRecipe())
				if !dot {
					require.NoError(t, resolveErr)
				} else {
					require.ErrorContains(t, resolveErr, "mapping_incomplete")
				}
				engine, err := pdfproduction.NewPDFium(pdfproduction.QualifiedRecipe())
				require.NoError(t, err)
				raster, err := engine.Render(t.Context(), pdfproduction.Source{Reader: bytes.NewReader(pdf),
					Size: int64(len(pdf)), SHA256: sum(pdf)}, m.Pages[0], pdfproduction.QualifiedRecipe())
				require.NoError(t, err)
				rasters = append(rasters, bytes.Clone(raster.Pixels.Pix))
				require.NoError(t, engine.Close())
			}
			changed := 0
			for i := 0; i < len(rasters[0]); i += 4 {
				if !bytes.Equal(rasters[0][i:i+4], rasters[1][i:i+4]) {
					changed++
				}
			}
			require.Positive(t, changed, "the misleading dot must actually contribute pixels")
		})
	}
}

func TestAlignQualifiedFontSameObjectApostrophe(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	var rasters [][]byte
	for _, quote := range []bool{false, true} {
		content := "BT /F1 24 Tf 72 720 Td (B) Tj ET"
		if quote {
			content = "BT /F1 24 Tf 72 720 Td [(B) 417 (')] TJ ET"
		}
		pdf := attributionOverlapPDF(content)
		m := alignFontFixture(t, pdf, "B", runtime)
		_, resolveErr := redaction.Resolve(m, "redact_selected", nil, pdfproduction.QualifiedRecipe())
		if !quote {
			require.NoError(t, resolveErr)
		} else {
			require.ErrorContains(t, resolveErr, "mapping_incomplete")
		}
		engine, err := pdfproduction.NewPDFium(pdfproduction.QualifiedRecipe())
		require.NoError(t, err)
		raster, err := engine.Render(t.Context(), pdfproduction.Source{Reader: bytes.NewReader(pdf),
			Size: int64(len(pdf)), SHA256: sum(pdf)}, m.Pages[0], pdfproduction.QualifiedRecipe())
		require.NoError(t, err)
		rasters = append(rasters, bytes.Clone(raster.Pixels.Pix))
		require.NoError(t, engine.Close())
	}
	changed := 0
	for i := 0; i < len(rasters[0]); i += 4 {
		if !bytes.Equal(rasters[0][i:i+4], rasters[1][i:i+4]) {
			changed++
		}
	}
	require.Positive(t, changed, "the misleading quote must actually contribute pixels")
}

func attributionOverlapPDF(content string) []byte {
	cmap := "1 begincodespacerange <00> <FF> endcodespacerange 3 beginbfchar <2E> <0020> " +
		"<27> <0020> <42> <0042> endbfchar"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << " +
			"/F1 4 0 R /F2 7 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding " +
			"/WinAnsiEncoding /ToUnicode 6 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	}
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1,
		xref)
	return []byte(pdf.String())
}
