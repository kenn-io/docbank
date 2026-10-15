package pdfproduction

import (
	"context"
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/redactiontest"
)

// This offline qualification justifies blankCodes' finite raster geometry.
// Re-run when changing the renderer, accepted programs or probe geometry.
func TestQualifiedFontGlyphInventory(t *testing.T) {
	if os.Getenv("DOCBANK_QUALIFY_FONT_PROGRAMS") != "1" {
		t.Skip("set DOCBANK_QUALIFY_FONT_PROGRAMS=1 for complete glyph qualification")
	}
	raw, err := os.ReadFile("testdata/font-glyphs.json")
	require.NoError(t, err)
	var inventory map[string][]struct {
		ID               int
		Name             string
		Points, Contours int
	}
	require.NoError(t, json.Unmarshal(raw, &inventory))
	require.Len(t, inventory, 12)
	total, inked := 0, 0
	for name, glyphs := range inventory {
		t.Run(name, func(t *testing.T) {
			engine, err := NewPDFium(QualifiedRecipe())
			require.NoError(t, err)
			defer func() { require.NoError(t, engine.Close()) }()
			native, ok := engine.(*pdfiumEngine)
			require.True(t, ok)
			err = native.withInstanceContext(t.Context(), func(ctx context.Context,
				p pdfium.Pdfium) error {
				for _, glyph := range glyphs {
					data := redactiontest.FontPDF("BT /F1 72 Tf 72 144 Td <20> Tj ET",
						"<< /Type /Font /Subtype /Type1 /BaseFont /"+name+
							" /Encoding << /BaseEncoding /WinAnsiEncoding /Differences [32 /"+glyph.Name+"] >> >>", "")
					doc, err := p.OpenDocument(&requests.OpenDocument{File: &data})
					require.NoError(t, err)
					page, err := p.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc.Document, Index: 0})
					require.NoError(t, err)
					object, err := p.FPDFPage_GetObject(&requests.FPDFPage_GetObject{
						Page: requests.Page{ByReference: &page.Page}, Index: 0,
					})
					require.NoError(t, err)
					font, err := p.FPDFTextObj_GetFont(&requests.FPDFTextObj_GetFont{PageObject: object.PageObject})
					require.NoError(t, err)
					proof := newNativeFontProof(p, doc.Document)
					matched, err := proof.qualifiedProgram(font.Font)
					require.NoError(t, err)
					require.True(t, matched, "unqualified program for %s", name)
					blank, err := proof.blankCodes(ctx, font.Font, []uint32{32})
					require.NoError(t, err)
					require.Equal(t, glyph.Points == 0, blank, "glyph %d %s", glyph.ID, glyph.Name)
					total++
					if !blank {
						inked++
					}
					_, err = p.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: page.Page})
					require.NoError(t, err)
					_, err = p.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
					require.NoError(t, err)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
	require.Equal(t, 2816, total)
	require.Equal(t, 2792, inked)
	t.Logf("qualified %d inked and %d empty glyphs", inked, total-inked)
}
