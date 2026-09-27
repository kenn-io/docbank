package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentProductionDraftPreviewInputRejectsEvidenceDrift(t *testing.T) {
	s, first, second, setID, revision, _ := productionDuplicateGateFixture(t)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	command := ProductionPreviewCommand{SetID: setID, Revision: revision, ETag: draft.ETag,
		OperationID: "76000000-0000-4000-8000-000000000063", MemberID: second.ID, Page: 1}
	admitted, err := s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", command)
	require.NoError(t, err)
	current, err := s.CurrentProductionDraftPreviewInput(t.Context(), command)
	require.NoError(t, err)
	require.Equal(t, admitted.PreviewInputSHA256, current)
	productionEvidenceMetadata(t, s, first.SourceSHA256, fakeHash("98"), "Changed synthetic title")
	_, err = s.CurrentProductionDraftPreviewInput(t.Context(), command)
	require.Error(t, err)
}
