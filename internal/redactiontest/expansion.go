package redactiontest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

// Expansion returns a real resolver proposal too large for a 64 KiB error body.
// This fixture exercises transport limits, not PDF source admission.
func Expansion(tb testing.TB, recipe redaction.Recipe) *redaction.Problem {
	tb.Helper()
	const count = 600
	frame := strings.Repeat("c", 64)
	mapping := redaction.TextMap{Contract: "aligned-text/v1", PDFSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), Text: strings.Repeat("AB", count),
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frame, Width: 85000, Height: 110000, Span: redaction.Span{End: 2 * count}}}}
	for i := range count {
		x, y := int64(1000+(i%50)*1500), int64(1000+(i/50)*1500)
		mapping.Atoms = append(mapping.Atoms, redaction.Atom{Span: redaction.Span{Start: int64(2 * i), End: int64(2*i + 2)}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frame, X0: x, Y0: y, X1: x + 700, Y1: y + 700}}})
	}
	mapping = redaction.NormalizeTextMap(mapping)
	var err error
	_, mapping.SHA256, err = redaction.CanonicalTextMap(mapping)
	require.NoError(tb, err)
	decision := redaction.Decision{ID: "11111111-1111-4111-8111-111111111111", MemberID: "22222222-2222-4222-8222-222222222222", Action: "redact", Selector: redaction.Selector{Kind: "text", MapSHA256: mapping.SHA256, Span: &redaction.Span{End: 2*count - 1}}}
	_, err = redaction.Resolve(mapping, "redact_selected", []redaction.Decision{decision}, recipe)
	var problem *redaction.Problem
	require.ErrorAs(tb, err, &problem)
	require.Equal(tb, "selection_expansion_required", problem.Code)
	return problem
}
