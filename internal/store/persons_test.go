package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersonRevisionFence(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	changed, err := s.UpdatePerson(ctx, person.PersonID, person.Revision, "Ada")
	require.NoError(t, err)
	require.Equal(t, person.Revision+1, changed.Revision)
	_, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Stale overwrite")
	require.ErrorIs(t, err, ErrStaleRevision)
	read, _, err := s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, "Ada", read.DisplayName)
}

func TestPersonIdentityAuthorityAndSharedContactPoints(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	identity := PersonIdentity{Kind: "email", ValueDisplay: "Ada@EXAMPLE.TEST", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "claim-1", Confidence: "operator_asserted"}
	added, err := s.AddPersonIdentity(ctx, ada.PersonID, ada.Revision, identity)
	require.NoError(t, err)
	require.Equal(t, "Ada@example.test", added.ValueNormalized)
	ada, _, err = s.PersonByID(ctx, ada.PersonID)
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, ada.PersonID, ada.Revision, identity)
	require.ErrorIs(t, err, ErrPersonIdentityConflict)

	grace, err := s.CreatePerson(ctx, "Grace Hopper", "derived")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(ctx, grace.PersonID, grace.Revision, identity)
	require.NoError(t, err)
	identities, err := s.PersonIdentities(ctx, ada.PersonID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	require.Equal(t, added.IdentityID, identities[0].IdentityID)
	require.ErrorIs(t, s.RemovePersonIdentity(ctx, ada.PersonID, added.IdentityID, ada.Revision-1), ErrStaleRevision)
	require.NoError(t, s.RemovePersonIdentity(ctx, ada.PersonID, added.IdentityID, ada.Revision))
	identities, err = s.PersonIdentities(ctx, ada.PersonID)
	require.NoError(t, err)
	require.Empty(t, identities)
}

func TestPersonMutationsAdvanceEpochAndDirtyExistingPopulation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	nodeID, versionID := seedPeopleVersion(t, s)
	_, err = s.db.Exec(`INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,sensitive) VALUES(?,?,?,'generation','author','email:Ada@example.test','source_metadata','claim','exact_identifier','identifier_match','Ada',1,0)`, versionID, nodeID, person.PersonID)
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&before))
	person, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Ada L.")
	require.NoError(t, err)
	var after, dirty int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&after))
	require.Equal(t, before+1, after)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM document_people_dirty WHERE content_version_id=?`, versionID).Scan(&dirty))
	require.EqualValues(t, 1, dirty)
	retired, err := s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	require.Equal(t, "retired", retired.State)
	_, err = s.UpdatePerson(ctx, person.PersonID, retired.Revision, "No")
	require.ErrorIs(t, err, ErrPersonRetired)
}

func TestPersonAuthorityRejectsInvalidInputs(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreatePerson(t.Context(), "", "operator")
	require.ErrorIs(t, err, ErrInvalidPerson)
	_, err = s.CreatePerson(t.Context(), "Ada", "filesystem")
	require.ErrorIs(t, err, ErrInvalidPerson)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	_, err = s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
		Kind: "email", ValueDisplay: "ada@example.test", Origin: "operator",
		EvidenceKind: "invented", EvidenceID: "claim", Confidence: "operator_asserted",
	})
	require.ErrorIs(t, err, ErrInvalidPerson)
}

func TestRetirePersonTombstonesResolutionAndSupersedesCandidates(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Retiring person", "transfer")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
		ArchiveID: "synthetic", UID: "retiring", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	candidateID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,state,created_at) VALUES(?,?,?,?,'identifier_conflict','{}',?,1,'open',?)`,
		candidateID, "email:retiring@example.test", "Retiring person", person.PersonID, strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)

	retired, err := s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	require.Equal(t, "retired", retired.State)
	_, _, err = s.PersonByID(ctx, person.PersonID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "retiring")
	require.ErrorIs(t, err, ErrNotFound)
	var survivor, reason, candidateState string
	var survivorValid bool
	require.NoError(t, s.db.QueryRow(`SELECT COALESCE(surviving_person_id,''),surviving_person_id IS NOT NULL,reason FROM person_aliases WHERE retired_person_id=?`, person.PersonID).Scan(&survivor, &survivorValid, &reason))
	require.Empty(t, survivor)
	require.False(t, survivorValid)
	require.Equal(t, "deleted", reason)
	require.NoError(t, s.db.QueryRow(`SELECT state FROM person_match_candidates WHERE candidate_id=?`, candidateID).Scan(&candidateState))
	require.Equal(t, "superseded", candidateState)
}

func TestRetirePersonCutsOffInboundMergeAliases(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreatePerson(t.Context(), "First", "operator")
	require.NoError(t, err)
	second, err := s.CreatePerson(t.Context(), "Second", "operator")
	require.NoError(t, err)
	second, _ = addTestPersonIdentity(t, s, second, "email", "second@example.test", "second-email")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), second.PersonID, first.PersonID, operationID, second.Revision, first.Revision)
	require.NoError(t, err)
	second, _, err = s.PersonByID(t.Context(), second.PersonID)
	require.NoError(t, err)
	_, err = s.RetirePerson(t.Context(), second.PersonID, second.Revision)
	require.NoError(t, err)

	_, _, err = s.PersonByID(t.Context(), first.PersonID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: first.PersonID})
	require.ErrorIs(t, err, ErrNotFound)
	var survivorValid bool
	var reason string
	require.NoError(t, s.db.QueryRow(`SELECT surviving_person_id IS NOT NULL,reason FROM person_aliases WHERE retired_person_id=?`, first.PersonID).Scan(&survivorValid, &reason))
	require.False(t, survivorValid)
	require.Equal(t, "deleted", reason)
}
