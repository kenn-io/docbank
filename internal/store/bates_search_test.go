package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func seedBatesSearchArtifact(t *testing.T, s *Store) (BatesArtifact, CollectionSnapshotMember) {
	t.Helper()
	snapshot, inputs := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "HIST", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	request := batesRequest(t, namespace, snapshot, inputs)
	request.RecipeSHA256 = digestCatalogJSON(recipe)
	allocation, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	pages := make([]BatesArtifactPage, len(inputs))
	for index, input := range inputs {
		pages[index] = BatesArtifactPage{Ordinal: index + 1, OccurrenceID: input.OccurrenceID,
			SourceBlobSHA256: input.UnstampedSHA256, SourcePage: input.SourcePage,
			OutputPage: index + 1, Label: allocation.Labels[index].Label}
	}
	artifact, err := s.PublishBatesArtifact(t.Context(), BatesArtifactPublication{ArtifactID: allocation.AllocationID,
		AllocationID: allocation.AllocationID, BlobSHA256: strings.Repeat("f", 64), Size: 10,
		PageCount: len(pages), RecipeJSON: recipe, Pages: pages}, BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	require.NoError(t, err)
	members, err := s.SnapshotMembers(t.Context(), snapshot.SnapshotID, 0, 10)
	require.NoError(t, err)
	return artifact, members[0]
}

func TestFindBatesArtifactsReturnsCandidatesForExactlyOneSelector(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	artifact, member := seedBatesSearchArtifact(t, s)
	person, err := s.CreatePerson(t.Context(), "Synthetic Keeper", "operator")
	require.NoError(t, err)
	assignment, err := s.SetCustodian(t.Context(), CustodianRequest{Scope: CustodianScope{Kind: "document",
		NodeID: member.NodeID, ContentVersionID: member.ContentVersionID}, PersonID: person.PersonID,
		RawLabel: "Synthetic Keeper", Rank: "primary", Basis: "operator_assigned", SourceRef: "test", IfMatchRevision: 1})
	require.NoError(t, err)

	for name, selector := range map[string]BatesArtifactSelector{
		"label":     {BatesLabel: artifact.Pages[0].Label},
		"custodian": {CustodianLabel: "SYNTHETIC KEEPER"},
		"person":    {PersonID: person.PersonID},
	} {
		t.Run(name, func(t *testing.T) {
			page, err := s.FindBatesArtifacts(t.Context(), selector, BatesArtifactPosition{}, 10)
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			require.Equal(t, artifact.ArtifactID, page.Items[0].ArtifactID)
			require.NotEmpty(t, page.Items[0].Evidence)
			if name != "label" {
				require.Equal(t, assignment.AssignmentID, page.Items[0].Evidence[0].AssignmentID)
				require.Equal(t, member.OccurrenceID, page.Items[0].Evidence[0].OccurrenceID)
			}
		})
	}

	_, err = s.FindBatesArtifacts(t.Context(), BatesArtifactSelector{BatesLabel: artifact.Pages[0].Label,
		PersonID: person.PersonID}, BatesArtifactPosition{}, 10)
	require.ErrorIs(t, err, ErrInvalidBatesSelector)
}

func TestFindBatesArtifactEvidenceContinuesWithoutChangingCandidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	artifact, member := seedBatesSearchArtifact(t, s)
	for index := range maxBatesCandidateEvidence + 2 {
		_, err := s.SetCustodian(t.Context(), CustodianRequest{Scope: CustodianScope{Kind: "document",
			NodeID: member.NodeID, ContentVersionID: member.ContentVersionID}, RawLabel: "Evidence Keeper",
			Rank: "additional", Basis: "operator_assigned", SourceRef: fmt.Sprintf("evidence-%02d", index),
			IfMatchRevision: 1})
		require.NoError(t, err)
	}
	selector := BatesArtifactSelector{CustodianLabel: "evidence keeper"}
	first, err := s.FindBatesArtifacts(t.Context(), selector, BatesArtifactPosition{}, 10)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.Len(t, first.Items[0].Evidence, maxBatesCandidateEvidence)
	require.True(t, first.Items[0].EvidenceTruncated)
	require.Equal(t, maxBatesCandidateEvidence, first.Items[0].EvidenceNext)

	continued, err := s.FindBatesArtifactEvidence(t.Context(), selector, artifact.ArtifactID,
		first.Items[0].EvidenceNext)
	require.NoError(t, err)
	require.Len(t, continued.Items, 1)
	require.Equal(t, artifact.ArtifactID, continued.Items[0].ArtifactID)
	require.Len(t, continued.Items[0].Evidence, 2)
	require.False(t, continued.Items[0].EvidenceTruncated)
	require.Zero(t, continued.Items[0].EvidenceNext)
}
