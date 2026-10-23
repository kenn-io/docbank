package processing

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreparationDeterministic(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "initial")
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 144, Ht: 144}})
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetFont("Helvetica", "", 10)
	for _, text := range []string{"FIRST PAGE", "SECOND PAGE"} {
		pdf.AddPage()
		pdf.Text(12, 24, text)
		pdf.Text(12, 44, "ANOTHER OBJECT")
		pdf.SetFillColor(0, 0, 0)
		pdf.Rect(12, 60, 20, 10, "F")
	}
	var buffer bytes.Buffer
	require.NoError(t, pdf.Output(&buffer))
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), &buffer)
	require.NoError(t, err)
	node, version, err := f.catalog.ReplaceContent(t.Context(), f.request.Selection.NodeID, store.UnconditionalRev, receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	f.request.Selection = store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: version.ID, SHA256: receipt.Hash, Size: receipt.Size}}
	first, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	second, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Len(t, first.Frames, 2)
	require.NotEmpty(t, first.Map.Gaps)
	unordered := 0
	for _, gap := range first.Map.Gaps {
		if gap.Unordered {
			unordered++
		}
	}
	require.GreaterOrEqual(t, unordered, 2, "each page's vector object must remain an unordered gap")
	require.Equal(t, first.Frames, second.Frames)
	require.Equal(t, first.EvidenceSHA256, second.EvidenceSHA256)
	require.Equal(t, first.Map.SHA256, second.Map.SHA256)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: first.Map.SHA256, Pages: []int{1, 2}}}}
	a, err := BuildRedactionReview(first, masks, node.Revision)
	require.NoError(t, err)
	b, err := BuildRedactionReview(second, masks, node.Revision)
	require.NoError(t, err)
	require.Equal(t, a.SHA256, b.SHA256)
	masks[0].Selector.Pages = []int{1}
	_, err = BuildRedactionReview(first, masks, node.Revision)
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
}
