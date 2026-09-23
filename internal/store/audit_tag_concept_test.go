package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditedConceptVocabularyRoundTrips(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	ctx := t.Context()
	second, err := s.CreateTag(ctx, "second")
	require.NoError(t, err)
	concept, err := s.SetTagConcept(ctx, tag.ID, 1, "Synthetic topic")
	require.NoError(t, err)
	concept, err = s.AddTagAlias(ctx, tag.ID, concept.Revision, "topic alias")
	require.NoError(t, err)
	require.NoError(t, s.AddConceptEdge(ctx, tag.ID, second.ID, "broader"))
	require.Equal(t, []string{"topic alias"}, concept.Aliases)
	require.NoError(t, s.ValidateMetadata(ctx))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &roundTrip))
	require.Equal(t, exported.Bytes(), roundTrip.Bytes())
	_, err = restored.db.ExecContext(ctx, `UPDATE tag_concepts SET description='tampered' WHERE tag_id=?`, tag.ID)
	require.NoError(t, err)
	require.ErrorContains(t, restored.ValidateMetadata(ctx), "replayed audit attachments do not match current metadata")
}

func TestAuditedTagDeleteKeepsConceptAuthorityWhenUnsupported(t *testing.T) {
	s, tag, _ := newAuditedTagStore(t)
	concept, err := s.SetTagConcept(t.Context(), tag.ID, 1, "Synthetic topic")
	require.NoError(t, err)
	_, err = s.DeleteTag(t.Context(), tag.ID, tag.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	still, err := s.ConceptByTagID(t.Context(), tag.ID)
	require.NoError(t, err)
	require.Equal(t, concept.Description, still.Description)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
