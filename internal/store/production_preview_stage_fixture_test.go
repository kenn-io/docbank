package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

// ProductionPreviewStageFixture supplies a draft whose catalog PDF is an
// actual synthetic one-page document and whose selected atom covers the page.
func ProductionPreviewStageFixture(t *testing.T) (*Store, string, redaction.Set,
	redaction.Draft, redaction.Member, []byte) {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 72, Ht: 72}})
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(20, 36, "x")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	pdfBytes := output.Bytes()
	s, member := seedProductionGateAuthorityWithPDF(t, pdfBytes,
		redaction.Box{X0: 0, Y0: 0, X1: 10_000, Y1: 10_000})
	member.ID, member.Ordinal = "89000000-0000-4000-8000-000000000041", 1
	member.Mode = "redact_selected"
	set, draft, err := s.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
		OperationID: "89000000-0000-4000-8000-000000000042", Name: "Synthetic preview"})
	require.NoError(t, err)
	decision := redaction.Decision{ID: "89000000-0000-4000-8000-000000000043", MemberID: member.ID,
		Action: "redact", Reason: "synthetic private reason", Selector: redaction.Selector{
			Kind: "text", MapSHA256: member.MapSHA256, Span: &redaction.Span{Start: 0, End: 1},
		}}
	_, err = s.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, 1, redaction.ApplyRequest{
		OperationID: "89000000-0000-4000-8000-000000000044", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &member}, {Kind: "decision", Decision: &decision}},
	})
	require.NoError(t, err)
	draft, err = s.ProductionDraft(t.Context(), set.ID, 1)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), set, draft, member, pdfBytes
}
