package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkOpenCandidatesForVersionScopesOccurrences(t *testing.T, postgres bool) {
	t.Helper()
	var s *Store
	var setup *sql.DB
	table := "person_match_candidates"
	if postgres {
		var owner string
		setup, owner = pgtest.NewDatabase(t)
		s = newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
		table = "documents.person_match_candidates"
	} else {
		s = newTestStore(t)
		setup = s.db
	}
	_, first := seedPeopleVersion(t, s)
	_, second := seedPeopleVersion(t, s)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example", "", []PersonCandidateOccurrence{
		{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "first"},
		{ContentVersionID: second, Role: "blind_copy", EvidenceKind: "email_generation", EvidenceID: "second"},
	}))
	person, err := s.CreatePerson(t.Context(), "Suggested", "operator")
	require.NoError(t, err)
	suggested := openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example", person.PersonID, []PersonCandidateOccurrence{{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "suggested"}}))
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example", "", []PersonCandidateOccurrence{{ContentVersionID: second, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "second-only"}}))
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example", person.PersonID, []PersonCandidateOccurrence{{ContentVersionID: second, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "second-suggested"}}))
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:other", "Other", "", []PersonCandidateOccurrence{{ContentVersionID: second, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "other"}}))
	third := openCandidateForTest(t, s, candidateForTest(t, "name_alias:third", "Third", "", []PersonCandidateOccurrence{{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "third"}}))
	for i, id := range []string{candidate.CandidateID, suggested.CandidateID, third.CandidateID} {
		_, err := setup.ExecContext(t.Context(), `UPDATE `+table+` SET created_at=$1 WHERE candidate_id=$2`, fmt.Sprintf("2026-01-0%dT00:00:00Z", 3-i), id)
		require.NoError(t, err)
	}
	keys := []string{"name_alias:example", "name_alias:other"}
	for i := range 400 {
		keys = append(keys, fmt.Sprintf("name_alias:unused%d", i))
	}
	keys = append(keys, "name_alias:example", "name_alias:third")
	got, err := s.openCandidatesForVersion(t.Context(), s.db, first, keys)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, third.CandidateID, got[0].CandidateID)
	require.Equal(t, suggested.CandidateID, got[1].CandidateID)
	require.Equal(t, candidate.CandidateID, got[2].CandidateID)
	require.Equal(t, []VersionCandidateOccurrence{{PersonCandidateOccurrence: PersonCandidateOccurrence{ContentVersionID: first, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "first"}}}, got[2].Occurrences)
	empty, err := s.openCandidatesForVersion(t.Context(), s.db, first, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}
