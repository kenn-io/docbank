package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPersonExternalAliasCycle(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO person_external_uid_aliases(system,archive_id,retired_uid,surviving_uid,observed_at)
		VALUES('msgvault','synthetic','a','b','2026-09-12T00:00:00Z'),
		      ('msgvault','synthetic','b','a','2026-09-12T00:00:00Z')`)
	require.NoError(t, err)
	_, err = s.ResolveExternalPersonUID(t.Context(), "msgvault", "synthetic", "a")
	require.ErrorContains(t, err, "cycle")
}

func TestPersonExternalIdentityLimitRejectsNewAndAllowsExistingUpdate(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	for index := range document.MaxPersonExternalIdentities {
		uid := fmt.Sprintf("uid-%02d", index)
		_, err = s.db.Exec(`INSERT INTO person_external_identities(person_id,system,archive_id,uid,uid_kind,uid_state,display_name_snapshot,linked_at,updated_at) VALUES(?,'msgvault','synthetic',?,'vcard_uid','retired','','2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`, person.PersonID, uid)
		require.NoError(t, err)
	}
	identity := PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "overflow", UIDKind: "vcard_uid", UIDState: "retired"}
	_, err = s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
	identity.UID = "uid-00"
	identity.DisplayNameSnapshot = "Ada updated"
	updated, err := s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.NoError(t, err)
	require.Equal(t, "Ada updated", updated.DisplayNameSnapshot)
}

func TestPersonExternalIdentityForwardsAndHonorsUnlinkedTombstone(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "transfer")
	require.NoError(t, err)
	oldIdentity := PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "old", UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: "Ada"}
	_, err = s.LinkExternalIdentity(ctx, oldIdentity, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "new", []string{"old"}))
	newIdentity := oldIdentity
	newIdentity.UID = "new"
	_, err = s.LinkExternalIdentity(ctx, newIdentity, person.Revision)
	require.NoError(t, err)
	resolved, err := s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
	require.NoError(t, err)
	require.Equal(t, person.PersonID, resolved.PersonID)
	require.Equal(t, "new", resolved.ResolvedUID)
	require.True(t, resolved.Forwarded)

	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.UnlinkExternalIdentity(ctx, "msgvault", "synthetic", "new", person.Revision))
	_, err = s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPersonExternalCurrentIdentityRequiresAliasTransition(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	identity := PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "one", UIDKind: "vcard_uid", UIDState: "current"}
	_, err = s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	identity.UID = "two"
	_, err = s.LinkExternalIdentity(t.Context(), identity, person.Revision)
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
}

func TestPersonActorKeysPaginationAuthenticatesCursor(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	for index, address := range []string{"Ada@example.test", "ada@example.test"} {
		_, err = s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{
			Kind: "email", ValueDisplay: address, Origin: "operator", EvidenceKind: "operator_assertion",
			EvidenceID: "claim-" + string(rune('a'+index)), Confidence: "operator_asserted",
		})
		require.NoError(t, err)
		person, _, err = s.PersonByID(t.Context(), person.PersonID)
		require.NoError(t, err)
	}
	first, err := s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.EqualValues(t, 2, first.Total)
	require.NotEmpty(t, first.NextCursor)
	second, err := s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID, Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	require.Empty(t, second.NextCursor)
	_, err = s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID, Limit: 1, Cursor: first.NextCursor + "x"})
	require.ErrorIs(t, err, ErrInvalidPerson)
}

func TestRecordExternalUIDAliasesRejectsCycleAtomically(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "b", []string{"a"}))
	err := s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "a", []string{"b"})
	require.ErrorContains(t, err, "cycle")
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_external_uid_aliases WHERE retired_uid='b'`).Scan(&count))
	require.Zero(t, count)
}

func TestActorKeysForPersonForwardsMergedPersonID(t *testing.T) {
	s := newTestStore(t)
	survivor, err := s.CreatePerson(t.Context(), "Survivor", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(t.Context(), "Absorbed", "operator")
	require.NoError(t, err)
	absorbed, _ = addTestPersonIdentity(t, s, absorbed, "email", "absorbed@example.test", "absorbed-email")
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(t.Context(), survivor.PersonID, absorbed.PersonID, operationID, survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	page, err := s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: absorbed.PersonID, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{"email:absorbed@example.test"}, page.Items)
}
