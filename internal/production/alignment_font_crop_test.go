package production

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

func TestAlignQualifiedFontInheritedCrop(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	for _, tc := range []struct {
		name, content, cmap string
		refuse              bool
	}{
		{"blank space", "L R", "", false},
		{"ink mapped to space", "LAR", "1 begincodespacerange <00> <FF> endcodespacerange " +
			"1 beginbfchar <41> <0020> endbfchar", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "BT /F1 24 Tf 72 720 Td (" + tc.content + ") Tj ET"
			objects := []string{
				"<< /Type /Catalog /Pages 2 0 R >>",
				"<< /Type /Pages /Kids [3 0 R] /Count 1 /CropBox [0 0 1 1] >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
					"/CropBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
				"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica " +
					"/Encoding /WinAnsiEncoding /ToUnicode 6 0 R >>",
				fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
				fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(tc.cmap), tc.cmap),
			}
			var pdf strings.Builder
			pdf.WriteString("%PDF-1.4\n")
			offsets := make([]int, len(objects))
			for i, object := range objects {
				offsets[i] = pdf.Len()
				fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
			}
			xref := pdf.Len()
			fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
			for _, offset := range offsets {
				fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
			}
			fmt.Fprintf(&pdf, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
				len(objects)+1, xref)
			data := []byte(pdf.String())
			mapping := alignFontFixture(t, data, "L R", runtime)
			engine, err := pdfproduction.NewPDFium(pdfproduction.QualifiedRecipe())
			require.NoError(t, err)
			defer func() { require.NoError(t, engine.Close()) }()
			raster, err := engine.Render(t.Context(), pdfproduction.Source{Reader: bytes.NewReader(data),
				Size: int64(len(data)), SHA256: sum(data)}, mapping.Pages[0], pdfproduction.QualifiedRecipe())
			require.NoError(t, err)
			// The source override keeps A visible here; an inherited crop must not hide its probe.
			ink := 0
			for y := 228; y < 300; y++ {
				for x := 360; x < 385; x++ {
					if raster.Pixels.NRGBAAt(x, y).R != 255 {
						ink++
					}
				}
			}
			_, err = redaction.Resolve(mapping, "redact_selected", nil, pdfproduction.QualifiedRecipe())
			if tc.refuse {
				require.Positive(t, ink, "the misleading glyph must paint visible source pixels")
				require.ErrorContains(t, err, "mapping_incomplete")
			} else {
				require.Zero(t, ink)
				require.NoError(t, err)
			}
		})
	}
}
