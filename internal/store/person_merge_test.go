package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitRequiresExplicitMembership(t *testing.T) {
	request := PersonSplitRequest{PersonID: "00000000-0000-4000-8000-000000000001",
		OperationID: "00000000-0000-4000-8000-000000000002", Revision: 1}
	require.Error(t, validatePersonSplitRequest(request))
	request.IdentityIDs = []string{"00000000-0000-4000-8000-000000000003"}
	require.NoError(t, validatePersonSplitRequest(request))
	request.IdentityIDs = append(request.IdentityIDs, request.IdentityIDs[0])
	require.Error(t, validatePersonSplitRequest(request))
}

func addTestPersonIdentity(t *testing.T, s *Store, person Person, kind, value, evidenceID string) (Person, PersonIdentity) {
	t.Helper()
	identity, err := s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: kind, ValueDisplay: value, Origin: "operator", EvidenceKind: "operator_assertion",
		EvidenceID: evidenceID, Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	return person, identity
}

func TestMergePersonsDeduplicatesAliasesAndReplaysReceipt(t *testing.T) {
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "A. Lovelace", "operator")
	require.NoError(t, err)
	survivor, _ = addTestPersonIdentity(t, s, survivor, "name_alias", "Ada", "survivor-alias")
	absorbed, absorbedAlias := addTestPersonIdentity(t, s, absorbed, "name_alias", "ADA", "absorbed-alias")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	receipt, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Contains(t, receipt.Moved.IdentityIDs, absorbedAlias.IdentityID)
	require.Equal(t, []PersonMergeDeduplicatedIdentity{{
		IdentityID: absorbedAlias.IdentityID, Kind: "name_alias", ValueNormalized: "ada", ValueDisplay: "ADA",
		Normalization: "casefold", Origin: "operator", EvidenceKind: "operator_assertion",
		EvidenceID: "absorbed-alias", Confidence: "operator_asserted", RecordedAt: absorbedAlias.RecordedAt,
		RetainedIdentityID: survivorAliasID(t, s, survivor.PersonID),
	}}, receipt.Moved.DeduplicatedIdentities)
	resolved, through, err := s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	require.Equal(t, survivor.PersonID, resolved.PersonID)
	require.Equal(t, absorbed.PersonID, through)
	replayed, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision+1, absorbed.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergeReceiptKeepsExternalTupleComponentsDistinct(t *testing.T) {
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: absorbed.PersonID,
		System: "msgvault", ArchiveID: "archive/part", UID: "uid/part", UIDKind: "vcard_uid", UIDState: "current"}, absorbed.Revision)
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	receipt, err := s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	require.Equal(t, []PersonMergeExternalUID{{System: "msgvault", ArchiveID: "archive/part", UID: "uid/part"}}, receipt.Moved.ExternalUIDs)
}

func survivorAliasID(t *testing.T, s *Store, personID string) string {
	t.Helper()
	identities, err := s.PersonIdentities(t.Context(), personID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	return identities[0].IdentityID
}

func TestMergePersonsRejectsConflictingCurrentExternalUIDs(t *testing.T) {
	s := newTestStore(t)
	left, err := s.CreatePerson(t.Context(), "Left", "operator")
	require.NoError(t, err)
	right, err := s.CreatePerson(t.Context(), "Right", "operator")
	require.NoError(t, err)
	for person, uid := range map[Person]string{left: "left", right: "right"} {
		_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
			ArchiveID: "synthetic", UID: uid, UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
		require.NoError(t, err)
	}
	left, _, err = s.PersonByID(t.Context(), left.PersonID)
	require.NoError(t, err)
	right, _, err = s.PersonByID(t.Context(), right.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), left.PersonID, right.PersonID, operationID, left.Revision, right.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergePersonsAcceptsResolvedCurrentExternalUIDs(t *testing.T) {
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "Ada Old", "operator")
	require.NoError(t, err)
	for person, uid := range map[Person]string{survivor: "current", absorbed: "retired"} {
		_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
			ArchiveID: "synthetic", UID: uid, UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
		require.NoError(t, err)
	}
	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "current", []string{"retired"}))
	survivor, _, err = s.PersonByID(t.Context(), survivor.PersonID)
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(t.Context(), absorbed.PersonID)
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	identities, err := s.PersonExternalIdentities(t.Context(), survivor.PersonID)
	require.NoError(t, err)
	require.Len(t, identities, 2)
	states := map[string]string{}
	for _, identity := range identities {
		states[identity.UID] = identity.UIDState
	}
	require.Equal(t, "current", states["current"])
	require.Equal(t, "retired", states["retired"])
}

func TestMergePersonsRejectsConflictingAssertions(t *testing.T) {
	s := newTestStore(t)
	left, err := s.CreatePerson(t.Context(), "Left", "operator")
	require.NoError(t, err)
	right, err := s.CreatePerson(t.Context(), "Right", "operator")
	require.NoError(t, err)
	_, versionID := seedPeopleVersion(t, s)
	for person, action := range map[Person]string{left: "assert", right: "suppress"} {
		assertionID, err := newUUIDv4()
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at) VALUES(?,?,?,'author',?,'','2026-09-12T00:00:00Z')`, assertionID, versionID, person.PersonID, action)
		require.NoError(t, err)
	}
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), left.PersonID, right.PersonID, operationID, left.Revision, right.Revision)
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestMergePersonsRewritesAliasChainsToOneHop(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreatePerson(t.Context(), "First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(t.Context(), "Second", "operator")
	require.NoError(t, err)
	third, err := s.CreatePerson(t.Context(), "Third", "operator")
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), second.PersonID, first.PersonID, operationID, second.Revision, first.Revision)
	require.NoError(t, err)
	second, _, err = s.PersonByID(t.Context(), second.PersonID)
	require.NoError(t, err)
	operationID, err = newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), third.PersonID, second.PersonID, operationID, third.Revision, second.Revision)
	require.NoError(t, err)
	resolved, through, err := s.PersonByID(t.Context(), first.PersonID)
	require.NoError(t, err)
	require.Equal(t, third.PersonID, resolved.PersonID)
	require.Equal(t, first.PersonID, through)
}

func TestSplitPersonMovesOnlyExplicitIdentitiesAndReplays(t *testing.T) {
	s := newTestStore(t)
	source, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	source, first := addTestPersonIdentity(t, s, source, "email", "Ada@example.test", "first")
	source, second := addTestPersonIdentity(t, s, source, "email", "ada@example.test", "second")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	request := PersonSplitRequest{PersonID: source.PersonID, OperationID: operationID, DisplayName: "Ada Two",
		Revision: source.Revision, IdentityIDs: []string{second.IdentityID}}
	receipt, err := s.SplitPerson(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, []string{second.IdentityID}, receipt.MovedIdentityIDs)
	remaining, err := s.PersonIdentities(t.Context(), source.PersonID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, first.IdentityID, remaining[0].IdentityID)
	moved, err := s.PersonIdentities(t.Context(), receipt.NewPersonID)
	require.NoError(t, err)
	require.Len(t, moved, 1)
	replayed, err := s.SplitPerson(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
}
