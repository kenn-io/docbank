package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestCandidateEvidenceIdentity(t *testing.T) {
	values := []PersonCandidateOccurrence{{
		ContentVersionID: "version-a", Role: "author",
		EvidenceKind: "source_metadata", EvidenceID: "generation-a",
	}}
	first, err := candidateEvidenceDigest(values)
	require.NoError(t, err)
	again, err := candidateEvidenceDigest(values)
	require.NoError(t, err)
	require.Equal(t, first, again)
	values[0].EvidenceID = "generation-b"
	changed, err := candidateEvidenceDigest(values)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
}

func TestOpenPersonCandidateReplaysDecisionAndSeparatesNewEvidence(t *testing.T) {
	s := newTestStore(t)
	_, versionID := seedPeopleVersion(t, s)
	firstInput := candidateForTest(t, "name_alias:ada", "Ada", "", []PersonCandidateOccurrence{
		{ContentVersionID: versionID, Role: "sender", EvidenceKind: "email_generation", EvidenceID: "claim-b"},
		{ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim-a"},
		{ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim-a"},
	})
	first := openCandidateForTest(t, s, firstInput)
	require.EqualValues(t, 2, first.OccurrenceCount)

	decided, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
		CandidateID: first.CandidateID, Action: "reject", ExpectedRevision: first.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, "rejected", decided.State)

	replayed := openCandidateForTest(t, s, firstInput)
	require.Equal(t, decided.CandidateID, replayed.CandidateID)
	require.Equal(t, "rejected", replayed.State)
	require.EqualValues(t, 2, replayed.OccurrenceCount)

	secondInput := candidateForTest(t, "name_alias:ada", "Ada", "", []PersonCandidateOccurrence{{
		ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim-new",
	}})
	second := openCandidateForTest(t, s, secondInput)
	require.NotEqual(t, first.CandidateID, second.CandidateID)
	require.Equal(t, "open", second.State)

	open, total, err := s.PersonCandidates(t.Context(), "open", 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, []string{second.CandidateID}, []string{open[0].CandidateID})
	rejected, total, err := s.PersonCandidates(t.Context(), "rejected", 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, []string{first.CandidateID}, []string{rejected[0].CandidateID})
}

func TestDecidePersonCandidateLinksOnlyCapturedOccurrences(t *testing.T) {
	s := newTestStore(t)
	_, capturedVersion := seedPeopleVersion(t, s)
	_, unrelatedVersion := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Ada Lovelace", "operator")
	require.NoError(t, err)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:ada lovelace", "Ada Lovelace", person.PersonID, []PersonCandidateOccurrence{{
		ContentVersionID: capturedVersion, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "author-claim",
	}}))

	decided, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
		CandidateID: candidate.CandidateID, Action: "link", PersonID: person.PersonID,
		ExpectedRevision: candidate.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, "linked", decided.State)
	require.Equal(t, person.PersonID, decided.DecidedPersonID)

	assertions, err := s.DocumentPersonAssertions(t.Context(), capturedVersion)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, person.PersonID, assertions[0].PersonID)
	require.Equal(t, "author", assertions[0].Role)
	require.Equal(t, "assert", assertions[0].Action)
	unrelated, err := s.DocumentPersonAssertions(t.Context(), unrelatedVersion)
	require.NoError(t, err)
	require.Empty(t, unrelated)

	_, err = s.DecidePersonCandidate(t.Context(), CandidateDecision{
		CandidateID: candidate.CandidateID, Action: "reject", ExpectedRevision: candidate.Revision,
	})
	require.ErrorIs(t, err, ErrPersonCandidateDecided)
}

func TestDecidePersonCandidateCreatesCuratedPersonAtomically(t *testing.T) {
	s := newTestStore(t)
	_, versionID := seedPeopleVersion(t, s)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:grace hopper", "Grace Hopper", "", []PersonCandidateOccurrence{{
		ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "author-claim",
	}}))

	_, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
		CandidateID: candidate.CandidateID, Action: "new_person", ExpectedRevision: candidate.Revision + 1,
	})
	require.ErrorIs(t, err, ErrStaleRevision)

	decided, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
		CandidateID: candidate.CandidateID, Action: "new_person", ExpectedRevision: candidate.Revision,
	})
	require.NoError(t, err)
	require.NotEmpty(t, decided.DecidedPersonID)
	person, _, err := s.PersonByID(t.Context(), decided.DecidedPersonID)
	require.NoError(t, err)
	require.Equal(t, "Grace Hopper", person.DisplayName)
	require.Equal(t, "operator", person.Origin)
	require.Equal(t, "curated", person.State)
	assertions, err := s.DocumentPersonAssertions(t.Context(), versionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, person.PersonID, assertions[0].PersonID)
}

func TestCandidateDecisionsAdvanceBindingEpochEvenWithoutNewAssertions(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		s := newTestStore(t)
		_, versionID := seedPeopleVersion(t, s)
		candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:rejected", "Rejected", "", []PersonCandidateOccurrence{{
			ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "rejected-claim",
		}}))
		var epochBefore int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epochBefore))
		_, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
			CandidateID: candidate.CandidateID, Action: "reject", ExpectedRevision: candidate.Revision,
		})
		require.NoError(t, err)
		assertCandidateDecisionEpoch(t, s, epochBefore)
	})

	t.Run("existing assertion", func(t *testing.T) {
		s := newTestStore(t)
		_, versionID := seedPeopleVersion(t, s)
		person, err := s.CreatePerson(t.Context(), "Existing Person", "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
			ContentVersionID: versionID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1,
		})
		require.NoError(t, err)
		candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:existing", "Existing Person", person.PersonID, []PersonCandidateOccurrence{{
			ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "existing-claim",
		}}))
		var epochBefore int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epochBefore))
		_, err = s.DecidePersonCandidate(t.Context(), CandidateDecision{
			CandidateID: candidate.CandidateID, Action: "link", PersonID: person.PersonID,
			ExpectedRevision: candidate.Revision,
		})
		require.NoError(t, err)
		assertCandidateDecisionEpoch(t, s, epochBefore)
	})

	t.Run("pruned evidence", func(t *testing.T) {
		s := newTestStore(t)
		nodeID, versionID := seedPeopleVersion(t, s)
		candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:pruned", "Pruned", "", []PersonCandidateOccurrence{{
			ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "pruned-claim",
		}}))
		_, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, nodeID)
		require.NoError(t, err)
		var epochBefore int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epochBefore))
		decided, err := s.DecidePersonCandidate(t.Context(), CandidateDecision{
			CandidateID: candidate.CandidateID, Action: "reject", ExpectedRevision: candidate.Revision,
		})
		require.NoError(t, err)
		require.Equal(t, "rejected", decided.State)
		var epoch int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
		require.Equal(t, epochBefore+1, epoch)
	})
}

func assertCandidateDecisionEpoch(t *testing.T, s *Store, epochBefore int64) {
	t.Helper()
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.Equal(t, epochBefore+1, epoch)
}

func TestAssertDocumentPersonUsesRevisionFenceAndBindingEpoch(t *testing.T) {
	s := newTestStore(t)
	_, versionID := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Records Team", "operator")
	require.NoError(t, err)

	var epochBefore int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epochBefore))
	created, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: versionID, PersonID: person.PersonID, Role: "custodian",
		Action: "assert", Note: "Reviewed source", Revision: 1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.AssertionID)
	require.EqualValues(t, 1, created.Revision)

	updated, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		AssertionID: created.AssertionID, ContentVersionID: versionID, PersonID: person.PersonID,
		Role: "custodian", Action: "suppress", Note: "Operator correction", Revision: created.Revision,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, updated.Revision)
	require.Equal(t, "suppress", updated.Action)
	_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		AssertionID: created.AssertionID, ContentVersionID: versionID, PersonID: person.PersonID,
		Role: "custodian", Action: "assert", Revision: created.Revision,
	})
	require.ErrorIs(t, err, ErrStaleRevision)

	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	require.Equal(t, epochBefore+2, epoch)
}

func TestOpenPersonCandidateReportsQueueCapacity(t *testing.T) {
	s := newTestStore(t)
	_, versionID := seedPeopleVersion(t, s)
	_, err := s.db.Exec(`WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?
	) INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,
		occurrence_count,revision,state,decided_person_id,created_at,decided_at
	) SELECT printf('candidate-%05d',n),printf('name_alias:person-%05d',n),'Synthetic Person',NULL,
		'name_only','[]',printf('%064d',n),1,1,'open',NULL,?,NULL FROM seq`,
		maxOpenPersonCandidates, nowRFC3339())
	require.NoError(t, err)
	count, full, err := s.OpenPersonCandidateCount(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, maxOpenPersonCandidates, count)
	require.True(t, full)

	input := candidateForTest(t, "name_alias:overflow", "Overflow", "", []PersonCandidateOccurrence{{
		ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "overflow",
	}})
	var retained bool
	err = s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		candidate, kept, openErr := s.OpenPersonCandidate(t.Context(), tx, input)
		retained = kept
		require.Empty(t, candidate.CandidateID)
		return openErr
	})
	require.NoError(t, err)
	require.False(t, retained)
}

func TestPersonCandidateAndAssertionValidation(t *testing.T) {
	s := newTestStore(t)
	_, versionID := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	bad := candidateForTest(t, "name_alias:ada", "Ada", "", []PersonCandidateOccurrence{{
		ContentVersionID: versionID, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim",
	}})
	bad.EvidenceSHA256 = strings.Repeat("f", 64)
	err = s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		_, _, openErr := s.OpenPersonCandidate(t.Context(), tx, bad)
		return openErr
	})
	require.ErrorIs(t, err, ErrInvalidPerson)
	_, _, err = s.PersonCandidates(t.Context(), "open", 251, 0)
	require.ErrorIs(t, err, ErrInvalidPerson)
	_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: versionID, PersonID: person.PersonID, Role: "invented",
		Action: "assert", Revision: 1,
	})
	require.ErrorIs(t, err, ErrInvalidPerson)
	_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: versionID, PersonID: person.PersonID, Role: "author",
		Action: "assert", Note: strings.Repeat("x", maxPersonAssertionNoteBytes+1), Revision: 1,
	})
	require.ErrorIs(t, err, ErrInvalidPerson)
}

func candidateForTest(t *testing.T, actorKey, displayName, suggestedPersonID string, occurrences []PersonCandidateOccurrence) PersonMatchCandidate {
	t.Helper()
	raw, err := canonical.Marshal(occurrences)
	require.NoError(t, err)
	digest, err := candidateEvidenceDigest(occurrences)
	require.NoError(t, err)
	return PersonMatchCandidate{
		ActorKey: actorKey, DisplayName: displayName, SuggestedPersonID: suggestedPersonID,
		Reason: "name_only", Evidence: raw, EvidenceSHA256: digest, OccurrenceCount: int64(len(occurrences)),
	}
}

func openCandidateForTest(t *testing.T, s *Store, candidate PersonMatchCandidate) PersonMatchCandidate {
	t.Helper()
	var opened PersonMatchCandidate
	var retained bool
	err := s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		var openErr error
		opened, retained, openErr = s.OpenPersonCandidate(t.Context(), tx, candidate)
		return openErr
	})
	require.NoError(t, err)
	require.True(t, retained)
	return opened
}

func TestCandidateAuthoritySurvivesPersonMerge(t *testing.T) {
	s := newTestStore(t)
	_, version := seedPeopleVersion(t, s)
	survivor, err := s.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	assertion, err := s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
		ContentVersionID: version, PersonID: absorbed.PersonID, Role: "author", Action: "assert", Revision: 1,
	})
	require.NoError(t, err)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:absorbed", "Absorbed", absorbed.PersonID, []PersonCandidateOccurrence{{
		ContentVersionID: version, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim",
	}}))
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operation, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	assertions, err := s.DocumentPersonAssertions(t.Context(), version)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, assertion.AssertionID, assertions[0].AssertionID)
	require.Equal(t, survivor.PersonID, assertions[0].PersonID)
	require.Equal(t, assertion.Revision+1, assertions[0].Revision)
	superseded, _, err := s.PersonCandidates(t.Context(), "superseded", 10, 0)
	require.NoError(t, err)
	require.Len(t, superseded, 1)
	require.Equal(t, candidate.CandidateID, superseded[0].CandidateID)
}

func TestCandidateSuggestionIsSupersededOnRetirement(t *testing.T) {
	s := newTestStore(t)
	_, version := seedPeopleVersion(t, s)
	person, err := s.CreatePerson(t.Context(), "Example Person", "operator")
	require.NoError(t, err)
	candidate := openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example Person", person.PersonID, []PersonCandidateOccurrence{{
		ContentVersionID: version, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "claim",
	}}))
	_, err = s.RetirePerson(t.Context(), person.PersonID, person.Revision)
	require.NoError(t, err)
	superseded, _, err := s.PersonCandidates(t.Context(), "superseded", 10, 0)
	require.NoError(t, err)
	require.Len(t, superseded, 1)
	require.Equal(t, candidate.CandidateID, superseded[0].CandidateID)
}

func TestCandidateAssertionsRejectMergeCollisions(t *testing.T) {
	s := newTestStore(t)
	_, version := seedPeopleVersion(t, s)
	left, err := s.CreatePerson(t.Context(), "Left", "operator")
	require.NoError(t, err)
	right, err := s.CreatePerson(t.Context(), "Right", "operator")
	require.NoError(t, err)
	for _, person := range []Person{left, right} {
		_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{
			ContentVersionID: version, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1,
		})
		require.NoError(t, err)
	}
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), left.PersonID, right.PersonID, operation, left.Revision, right.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
	assertions, err := s.DocumentPersonAssertions(t.Context(), version)
	require.NoError(t, err)
	require.Len(t, assertions, 2)
}

func TestCandidateAuthorityPreventsMetadataImport(t *testing.T) {
	s := newTestStore(t)
	openCandidateForTest(t, s, candidateForTest(t, "name_alias:example", "Example Person", "", []PersonCandidateOccurrence{{
		ContentVersionID: "00000000-0000-4000-8000-000000000001", Role: "author", EvidenceKind: "source_metadata", EvidenceID: "pruned-claim",
	}}))
	err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	})
	require.ErrorContains(t, err, "not pristine")
}
