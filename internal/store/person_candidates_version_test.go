package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkOpenCandidatesForVersionScopesOccurrences(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	s := newStore(t)
	_, first := seedPeopleVersion(t, s)
	_, second := seedPeopleVersion(t, s)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example", "", []PersonCandidateOccurrence{
		{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "first"},
		{ContentVersionID: second, Role: "blind_copy", EvidenceKind: "email_generation", EvidenceID: "second"},
	}))
	other := openCandidateForTest(t, s, candidateForTest(t, "name_alias:other", "Other", "", []PersonCandidateOccurrence{{ContentVersionID: second, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "other"}}))
	_, err := s.db.ExecContext(t.Context(), `UPDATE person_match_candidates SET evidence_json=$1 WHERE candidate_id=$2`, []byte("invalid-json"), other.CandidateID)
	require.NoError(t, err)
	got, err := s.openCandidatesForVersion(t.Context(), s.db, first)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, candidate.CandidateID, got[0].CandidateID)
	require.Equal(t, []PersonCandidateOccurrence{{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "first"}}, got[0].Occurrences)
}
