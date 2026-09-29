package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPersonRevisionFence(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	identity := PersonIdentity{Kind: "email", ValueDisplay: "Ada@EXAMPLE.TEST", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "claim-1", Confidence: "operator_asserted"}
	added, err := s.AddPersonIdentity(ctx, ada.PersonID, ada.Revision, identity)
	require.NoError(t, err)
	require.Equal(t, "ada@example.test", added.ValueNormalized)
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

func TestPersonMutationsAdvanceBindingEpoch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	var before int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&before))
	person, err = s.UpdatePerson(ctx, person.PersonID, person.Revision, "Ada L.")
	require.NoError(t, err)
	var after int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&after))
	require.Equal(t, before+1, after)
	retired, err := s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	require.Equal(t, "retired", retired.State)
	_, err = s.UpdatePerson(ctx, person.PersonID, retired.Revision, "No")
	require.ErrorIs(t, err, ErrPersonRetired)
}

func TestPersonMutationReturnsCommittedRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	// Keep the read pool's only connection occupied so a post-commit read cannot
	// race the caller's next edit. The write pool remains available to observe
	// and edit the committed row while the mutation is returning.
	s.db.SetMaxOpenConns(1)
	readConn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = readConn.Close() }()

	createdCh := make(chan struct {
		person Person
		err    error
	}, 1)
	go func() {
		person, err := s.CreatePerson(ctx, "Synthetic", "operator")
		createdCh <- struct {
			person Person
			err    error
		}{person, err}
	}()
	require.Eventually(t, func() bool {
		var id string
		return s.writeDB.QueryRowContext(ctx,
			`SELECT person_id FROM persons WHERE display_name=?`, "Synthetic").Scan(&id) == nil
	}, 2*time.Second, 10*time.Millisecond)
	var createdRowID string
	require.NoError(t, s.writeDB.QueryRowContext(ctx,
		`SELECT person_id FROM persons WHERE display_name=?`, "Synthetic").Scan(&createdRowID))
	_, err = s.writeDB.ExecContext(ctx,
		`UPDATE persons SET display_name=?,display_name_folded=?,revision=revision+1 WHERE person_id=?`,
		"Later edit", "later edit", createdRowID)
	require.NoError(t, err)
	require.NoError(t, readConn.Close())
	createdResult := <-createdCh
	require.NoError(t, createdResult.err)
	require.Equal(t, "Synthetic", createdResult.person.DisplayName)
	require.Equal(t, int64(1), createdResult.person.Revision)

	initial, err := s.CreatePerson(ctx, "Initial", "operator")
	require.NoError(t, err)
	readConn, err = s.db.Conn(ctx)
	require.NoError(t, err)
	updatedCh := make(chan struct {
		person Person
		err    error
	}, 1)
	go func() {
		person, err := s.UpdatePerson(ctx, initial.PersonID, initial.Revision, "Committed rename")
		updatedCh <- struct {
			person Person
			err    error
		}{person, err}
	}()
	require.Eventually(t, func() bool {
		var revision int64
		return s.writeDB.QueryRowContext(ctx,
			`SELECT revision FROM persons WHERE person_id=?`, initial.PersonID).Scan(&revision) == nil && revision == 2
	}, 2*time.Second, 10*time.Millisecond)
	_, err = s.writeDB.ExecContext(ctx,
		`UPDATE persons SET display_name=?,display_name_folded=?,revision=revision+1 WHERE person_id=?`,
		"Later edit", "later edit", initial.PersonID)
	require.NoError(t, err)
	require.NoError(t, readConn.Close())
	updatedResult := <-updatedCh
	require.NoError(t, updatedResult.err)
	require.Equal(t, "Committed rename", updatedResult.person.DisplayName)
	require.Equal(t, int64(2), updatedResult.person.Revision)
}

func TestPersonDetailResolvesAliasAndReadsOneSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Ada old", "operator")
	require.NoError(t, err)
	absorbed, identity := addTestPersonIdentity(t, s, absorbed, "name_alias", "Ada old", "synthetic-alias")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	detail, err := s.PersonDetail(ctx, absorbed.PersonID)
	require.NoError(t, err)
	require.Equal(t, survivor.PersonID, detail.PersonID)
	require.Equal(t, absorbed.PersonID, detail.ReachedThrough)
	require.Len(t, detail.Identities, 1)
	require.Equal(t, identity.IdentityID, detail.Identities[0].IdentityID)
}

func TestPersonAuthorityRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
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

func TestRetirePersonTombstonesResolution(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Retiring person", "transfer")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault",
		ArchiveID: "synthetic", UID: "retiring", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)

	retired, err := s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	require.Equal(t, "retired", retired.State)
	_, _, err = s.PersonByID(ctx, person.PersonID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "retiring")
	require.ErrorIs(t, err, ErrNotFound)
	var survivor, reason string
	var survivorValid bool
	require.NoError(t, s.db.QueryRow(`SELECT COALESCE(surviving_person_id,''),surviving_person_id IS NOT NULL,reason FROM person_aliases WHERE retired_person_id=?`, person.PersonID).Scan(&survivor, &survivorValid, &reason))
	require.Empty(t, survivor)
	require.False(t, survivorValid)
	require.Equal(t, "deleted", reason)
}

func TestRetirePersonCutsOffInboundMergeAliases(t *testing.T) {
	t.Parallel()
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
