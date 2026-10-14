package production

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func TestAlignWhitespaceIgnoresExtraBorder(t *testing.T) {
	for _, font := range []string{"Courier", "Times"} {
		t.Run(font, func(t *testing.T) {
			pdf := whitespacePDF(t, font, 72, "SECRET KEEP SECRET")
			m := alignNativeFixture(t, pdf, "SECRET KEEP SECRET")
			require.Equal(t, "SECRET KEEP SECRET", m.Text)
			require.Empty(t, m.Gaps)
			decision := redaction.Decision{
				ID:       "00000000-0000-4000-8000-000000000002",
				MemberID: "00000000-0000-4000-8000-000000000003",
				Action:   "redact",
				Selector: redaction.Selector{
					Kind: "rectangle", MapSHA256: m.SHA256,
					Boxes: []redaction.Box{{
						Page: 1, FrameSHA256: m.Pages[0].FrameSHA256,
						X0: 9000, Y0: 7800, X1: 16400, Y1: 11000,
					}},
				},
			}
			plan, err := redaction.Resolve(
				m, "redact_selected", []redaction.Decision{decision}, pdfproduction.QualifiedRecipe(),
			)
			require.NoError(t, err)
			text, err := redaction.Text(plan)
			require.NoError(t, err)
			require.Equal(t, "[REDACTED] KEEP SECRET\f", string(text))
		})
	}
}

func TestAlignWhitespaceBoundaryInkStillRequiresMask(t *testing.T) {
	const text = "of the staff of life, if it fits."
	pdf := whitespacePDF(t, "Times", 72.37, text)
	m := alignNativeFixture(t, pdf, text)
	// Outward rounding can still include neighboring ink at fractional positions.
	require.True(t, slices.ContainsFunc(m.Gaps, func(gap redaction.Gap) bool {
		return !gap.Unordered && gap.Anchor < int64(len(m.Text)) && m.Text[gap.Anchor] == ' '
	}), "expected an ordered whitespace gap")
	_, err := redaction.Resolve(m, "redact_selected", nil, pdfproduction.QualifiedRecipe())
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
}

func whitespacePDF(t *testing.T, font string, x float64, text string) []byte {
	t.Helper()
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.AddPage()
	pdf.SetFont(font, "", 12)
	pdf.Text(x, 72, text)
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	return encoded.Bytes()
}
