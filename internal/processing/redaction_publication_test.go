package processing

import (
	"io"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified renderer accounts for whole-process memory.
func TestNumberedRedactionPublication(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	review, err := BuildRedactionReview(prep, masks, 1, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	plain := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	preview, err := f.service.CreateRedactionPreview(t.Context(), "alice", plain, runtime, nil)
	require.NoError(t, err)
	ns, err := f.catalog.EnsureBatesNamespace(t.Context(), "PUBLISH", "", 6)
	require.NoError(t, err)
	reserved, err := f.service.ReserveNumberedPDF(t.Context(), NumberedReservationRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256, NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true}, runtime)
	require.NoError(t, err)
	request := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256, AllocationID: reserved.AllocationID}
	// A failed attempt must leave the reservation usable from its retained source.
	_, err = f.service.PublishNumberedPDF(t.Context(), request, nil)
	require.ErrorIs(t, err, ErrRedactionPreparationUnavailable)
	_, _, err = f.catalog.Trash(t.Context(), f.request.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.catalog.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	reader, err := f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "text", 0)
	require.NoError(t, err)
	text, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(text))
	require.NoError(t, reader.Close())
	controls := nativeRedactionControls{limits: defaultNativeRedactionLimits}
	controls.hooks.afterVerify = func(string) error {
		f.service.clock = func() time.Time { return preview.ExpiresAt.Add(time.Second) }
		_, err := f.service.RedactionPreview("alice", preview.ID)
		require.ErrorIs(t, err, ErrRedactionPreviewUnavailable)
		return nil
	}
	first, err := f.service.publishNumberedReview(t.Context(), request, runtime, controls)
	require.NoError(t, err)
	require.Equal(t, "PUBLISH000001", first.Pages[0].Label)
	require.Equal(t, "verified", first.State)
	// A lost success response is recoverable without a renderer or live source.
	again, err := f.service.PublishNumberedPDF(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, first, again)
	stream, size, err := f.blobs.OpenStreamContext(t.Context(), first.BlobSHA256)
	require.NoError(t, err)
	require.Equal(t, first.Size, size)
	_, err = io.Copy(io.Discard, stream)
	require.NoError(t, err)
	require.True(t, stream.Verified())
	require.NoError(t, stream.Close())
	request.AllocationID = uuid.Nil()
	_, err = f.service.PublishNumberedPDF(t.Context(), request, runtime)
	require.ErrorIs(t, err, ErrRedactionInvalidRequest)
}
