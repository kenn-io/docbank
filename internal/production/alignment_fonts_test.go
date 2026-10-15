package production

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestAlignQualifiedFontWhitespace(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	const text = "of the staff of life, if it fits."
	for _, font := range []string{
		"Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique",
		"Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique",
		"Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic",
	} {
		t.Run(font, func(t *testing.T) {
			data := redactiontest.FontPDF("BT /F1 12 Tf 72.37 720 Td ("+text+") Tj ET",
				"<< /Type /Font /Subtype /Type1 /BaseFont /"+font+" >>", "")
			mapping := alignFontFixture(t, data, text, runtime)
			require.Equal(t, text, mapping.Text)
			require.Empty(t, mapping.Gaps)
			plan, err := redaction.Resolve(mapping, "redact_selected", nil, pdfproduction.QualifiedRecipe())
			require.NoError(t, err)
			sidecar, err := redaction.Text(plan)
			require.NoError(t, err)
			require.Equal(t, text+"\f", string(sidecar))
		})
	}
}

func alignFontFixture(t *testing.T, data []byte, text string,
	runtime *pagerender.Runtime) redaction.TextMap {
	t.Helper()
	evidence := evidenceFor(t, text)
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: sum(data), Size: int64(len(data))}
	inspection, err := pagerender.InspectSource(data, source, "application/pdf")
	require.NoError(t, err)
	mapping, err := Align(t.Context(), AlignmentInput{
		PDF: pdfproduction.Source{Reader: bytes.NewReader(data), Size: source.Size,
			SHA256: source.SHA256},
		Pages: inspection.Frames, Evidence: evidence, EvidenceSHA256: evidence.Checksum,
		PageRuntime: runtime,
	})
	require.NoError(t, err)
	return mapping
}

func TestAlignQualifiedFontsRefuseMisleadingGlyphs(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	font := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding " +
		"/WinAnsiEncoding /ToUnicode 6 0 R >>"
	for _, tc := range []struct {
		code byte
		size int
	}{
		{0x27, 3}, {0x2e, 3}, {0x2d, 1}, {0x22, 1}, {0x2a, 1}, {0xb7, 1}, {0x41, 24},
	} {
		t.Run(fmt.Sprintf("%02X/%d", tc.code, tc.size), func(t *testing.T) {
			content := fmt.Sprintf("BT /F1 %d Tf 72 720 Td <%02X> Tj ET BT /F1 12 Tf 72 680 Td (visible) Tj ET", tc.size, tc.code)
			cmap := fmt.Sprintf("1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <%02X> "+
				"<0020> endbfchar", tc.code)
			mapping := alignFontFixture(t, redactiontest.FontPDF(content, font, cmap), "visible", runtime)
			_, err := redaction.Resolve(mapping, "redact_selected", nil, pdfproduction.QualifiedRecipe())
			require.ErrorContains(t, err, "mapping_incomplete")
		})
	}
	for _, tc := range []struct{ name, codes, cmap string }{
		{"space before A", "4c4152", "2 beginbfchar <20> <0020> <41> <0020> endbfchar"},
		{"A before space", "4c4152", "2 beginbfchar <41> <0020> <20> <0020> endbfchar"},
		{"unused A", "4c2052", "1 beginbfchar <41> <0020> endbfchar"},
		{"multiple scalars", "4c4152", "1 beginbfchar <41> <00200020> endbfchar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := redactiontest.FontPDF("BT /F1 24 Tf 72 720 Td <"+tc.codes+"> Tj ET", font,
				"1 begincodespacerange <00> <FF> endcodespacerange "+tc.cmap)
			mapping := alignFontFixture(t, data, "L R", runtime)
			_, err := redaction.Resolve(mapping, "redact_selected", nil, pdfproduction.QualifiedRecipe())
			require.ErrorContains(t, err, "mapping_incomplete")
		})
	}
}
