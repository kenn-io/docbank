package processing

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Real PDFium work uses the qualified whole-process RSS ceiling.
func TestNumberedRedactionRetainedSource(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	review, err := BuildRedactionReview(prep, masks, 1)
	require.NoError(t, err)
	ns, err := f.catalog.EnsureBatesNamespace(t.Context(), "Q", "", 2)
	require.NoError(t, err)
	r := NumberedReservationRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1}
	_, err = f.service.ReserveNumberedPDF(t.Context(), r, runtime)
	require.ErrorIs(t, err, ErrRedactionInvalidRequest)
	r.RetainOriginal = true
	reserved, err := f.service.ReserveNumberedPDF(t.Context(), r, runtime)
	require.NoError(t, err)
	require.Equal(t, "Q01", reserved.Pages[0].Label)
	require.Equal(t, "indefinite", reserved.Recipe.OriginalRetention)
	_, _, err = f.catalog.Trash(t.Context(), f.request.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.catalog.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	replay, err := f.service.ReserveNumberedPDF(t.Context(), r, nil)
	require.NoError(t, err)
	require.Equal(t, reserved, replay)
	render := RedactionRenderRequest{Preparation: prep, Masks: masks,
		ReviewSHA256: review.SHA256, AllocationID: reserved.AllocationID}
	var first, again bytes.Buffer
	sender := func(out *bytes.Buffer) RedactionSender {
		return func(_ context.Context, r io.Reader, _ RedactionArchive) error {
			_, err := io.Copy(out, r)
			return err
		}
	}
	limited := nativeRedactionControls{limits: defaultNativeRedactionLimits}
	limited.limits.pdf = 1
	require.Error(t, f.service.renderNativePDF(t.Context(), render, runtime, sender(&first), limited))
	allocation, err := f.catalog.BatesAllocation(t.Context(), reserved.AllocationID.String())
	require.NoError(t, err)
	require.Equal(t, "reserved", allocation.State)
	require.NoError(t, f.service.RenderNativePDF(t.Context(), render, runtime, sender(&first)))
	require.NoError(t, f.service.RenderNativePDF(t.Context(), render, nil, sender(&again)))
	require.Equal(t, first.Bytes(), again.Bytes())

	var loadfileOutput bytes.Buffer
	_, err = WriteLoadFileExport(t.Context(), f.catalog, f.blobs, LoadFileExportRequest{
		SnapshotID: reserved.SnapshotID.String(), ProfileID: "export-dat-pdf-v1", BatesAllocationID: reserved.AllocationID.String()}, &loadfileOutput)
	require.ErrorIs(t, err, loadfile.ErrUnrepresentable)
	require.ErrorContains(t, err, "numbered native PDFs cannot be included")
	require.Zero(t, loadfileOutput.Len())
	render.AllocationID = uuid.Nil()
	require.ErrorIs(t, f.service.RenderNativePDF(t.Context(), render, runtime, sender(&again)), store.ErrNotFound)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestNumberedRedactionUnfitLabelRetainsNothing(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	review, err := BuildRedactionReview(prep, masks, 1)
	require.NoError(t, err)
	ns, err := f.catalog.EnsureBatesNamespace(t.Context(), strings.Repeat("M", 128), "", 6)
	require.NoError(t, err)
	request := NumberedReservationRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true}
	_, err = f.service.ReserveNumberedPDF(t.Context(), request, nil)
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "endorsement_layout_conflict", problem.Code)
	recipe, err := BuildNumberedRecipe(uuid.MustParse(f.catalog.VaultID()), ns, 1, review)
	require.Error(t, err)
	plan, err := recipe.PlanRequest(uuid.Nil())
	require.NoError(t, err)
	_, err = f.catalog.ReplaySnapshotBatesRange(t.Context(), plan)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = f.catalog.Trash(t.Context(), f.request.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.catalog.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	_, err = f.catalog.ContentVersionViewByID(t.Context(), f.request.Selection.NodeID, f.request.Selection.Source.VersionID)
	require.ErrorIs(t, err, store.ErrNotFound, "a rejected label must not retain the original")
}

func TestPlainRedactionWithoutRuntimeDoesNotCompeteForSlot(t *testing.T) {
	t.Parallel()
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	op, err := f.service.beginNativePDFOperation(t.Context(), "resolve")
	require.NoError(t, err)
	defer func() { require.NoError(t, op.Close()) }()
	err = f.service.RenderNativePDF(t.Context(), RedactionRenderRequest{ReviewSHA256: strings.Repeat("a", 64), Masks: []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: strings.Repeat("a", 64), Pages: []int{1}}}}}, nil,
		func(context.Context, io.Reader, RedactionArchive) error { t.Fatal("unexpected delivery"); return nil })
	require.ErrorIs(t, err, ErrRedactionPreparationUnavailable)
}
