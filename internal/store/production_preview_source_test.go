package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestOpenProductionDraftPreviewSourcePinsExactOccurrenceAndDraft(t *testing.T) {
	s, first, second, setID, revision, _ := productionDuplicateGateFixture(t)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	reader := &productionArtifactVerifyReader{store: s}
	input, err := s.OpenProductionDraftPreviewSource(t.Context(), setID, revision,
		draft.ETag, second.ID, reader)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, input.PDF.Stream.Close()) })
	require.Equal(t, second.ID, input.Member.Member.ID)
	require.Equal(t, second.Ordinal, input.Member.Member.Ordinal)
	require.Equal(t, first.SourceVersionID, input.Member.Member.SourceVersionID)
	require.Equal(t, []string{second.PDFSHA256}, reader.opened)
	require.Equal(t, second.PDFSHA256, input.PDF.PDFSHA256)
	require.Equal(t, second.PDFSize, input.PDF.Size)
	require.True(t, canonical.IsSHA256Hex(input.PreviewInputSHA256))

	_, err = s.OpenProductionDraftPreviewSource(t.Context(), setID, revision,
		draft.ETag+1, second.ID, reader)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	_, err = s.OpenProductionDraftPreviewSource(t.Context(), setID, revision,
		draft.ETag, "75000000-0000-4000-8000-000000000069", reader)
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, []string{second.PDFSHA256}, reader.opened,
		"invalid draft/member selection must not open PDF bytes")
}
