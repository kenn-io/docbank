package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDocumentEventActorClaimsReadTheActiveProjection(t *testing.T) {
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "actors.txt", "a71")
	record := documentEventRecord(t, s.VaultID(), version.ID, "a7")
	record.Events[0].Actors[0].EvidenceKind = "source_metadata"
	record.Events[0].Actors[0].EvidenceID = record.Events[0].EvidenceID
	canonical := mustMarshalDocumentEvents(t, record)

	generation, err := s.PublishDocumentEvents(t.Context(), target, fakeHash("f71"),
		requireDocumentEventInputsSHA256(t, s, target), canonical)
	require.NoError(t, err)

	generationID, claims, err := s.DocumentEventActorClaims(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, generation.GenerationID, generationID)
	require.Equal(t, []DocumentEventActorClaim{{
		GenerationID: generation.GenerationID,
		EventID:      record.Events[0].EventID,
		Role:         "author",
		ActorKey:     "email:ada@example.test",
		DisplayName:  "Ada",
		Address:      "ada@example.test",
		Ordinal:      0,
		EvidenceKind: "source_metadata",
		EvidenceID:   record.Events[0].EvidenceID,
		ClaimBasis:   "source_asserted",
		AxisKey:      record.Events[0].AxisKey,
		UTCKey:       "",
		Sensitive:    false,
	}}, claims)
}

func TestDocumentEventActorClaimsKeepUndatedEvidenceOutOfActivityBounds(t *testing.T) {
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "undated-actor.txt", "a72")
	record := documentEventRecord(t, s.VaultID(), version.ID, "b7")
	record.Events[0].Actors[0].EvidenceKind = "email_generation"
	record.Events[0].Actors[0].EvidenceID = "email/undated-author"
	record.Sources = append(record.Sources, document.DocumentEventSourceV1{
		EvidenceKind: "email_generation", EvidenceID: "email/undated-author",
		EvidenceSHA256: fakeHash("c7"),
	})
	canonical := mustMarshalDocumentEvents(t, record)

	_, err := s.PublishDocumentEvents(t.Context(), target, fakeHash("f72"),
		requireDocumentEventInputsSHA256(t, s, target), canonical)
	require.NoError(t, err)
	_, claims, err := s.DocumentEventActorClaims(t.Context(), version.ID)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Empty(t, claims[0].AxisKey)
	require.Empty(t, claims[0].UTCKey)
	require.Equal(t, "source_asserted", claims[0].ClaimBasis)
}
