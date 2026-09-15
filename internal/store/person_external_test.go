package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestRecordExternalUIDAliasesPreservesExistingHopBound(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Example Person", "operator")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID,
		System: "msgvault", ArchiveID: "example-archive", UID: "uid-32", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.NoError(t, err)
	for index := range 32 {
		require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "example-archive",
			fmt.Sprintf("uid-%d", index+1), []string{fmt.Sprintf("uid-%d", index)}))
	}
	_, err = s.ResolveExternalPersonUID(ctx, "msgvault", "example-archive", "uid-0")
	require.NoError(t, err)
	err = s.RecordExternalUIDAliases(ctx, "msgvault", "example-archive", "uid-33", []string{"uid-32"})
	require.ErrorContains(t, err, "hop limit")
	resolved, err := s.ResolveExternalPersonUID(ctx, "msgvault", "example-archive", "uid-0")
	require.NoError(t, err)
	require.Equal(t, person.PersonID, resolved.PersonID)
	require.Equal(t, "uid-32", resolved.ResolvedUID)
}

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
	require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "middle", []string{"old"}))
	require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "new", []string{"middle"}))
	person, _, err = s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
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

func TestRecordExternalUIDAliasesRetiresCurrentIdentity(t *testing.T) {
	for _, targetExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("target exists=%t", targetExists), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			owner, err := s.CreatePerson(ctx, "Original owner", "operator")
			require.NoError(t, err)
			identity := PersonExternalIdentity{PersonID: owner.PersonID, System: "msgvault", ArchiveID: "synthetic",
				UID: "old", UIDKind: "vcard_uid", UIDState: "current"}
			_, err = s.LinkExternalIdentity(ctx, identity, owner.Revision)
			require.NoError(t, err)
			owner, _, err = s.PersonByID(ctx, owner.PersonID)
			require.NoError(t, err)
			var target Person
			if targetExists {
				target, err = s.CreatePerson(ctx, "Surviving owner", "operator")
				require.NoError(t, err)
				_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: target.PersonID, System: "msgvault", ArchiveID: "synthetic",
					UID: "new", UIDKind: "vcard_uid", UIDState: "current"}, target.Revision)
				require.NoError(t, err)
			}
			require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "new", []string{"old"}))
			identities, err := s.PersonExternalIdentities(ctx, owner.PersonID)
			require.NoError(t, err)
			require.Len(t, identities, 1)
			require.Equal(t, "retired", identities[0].UIDState)
			keys, err := s.ActorKeysForPerson(ctx, PersonActorKeysRequest{PersonID: owner.PersonID})
			require.NoError(t, err)
			require.Empty(t, keys.Items)
			changed, _, err := s.PersonByID(ctx, owner.PersonID)
			require.NoError(t, err)
			require.Equal(t, owner.Revision+1, changed.Revision)
			_, err = s.LinkExternalIdentity(ctx, identity, owner.Revision)
			require.ErrorIs(t, err, ErrStaleRevision)
			_, err = s.LinkExternalIdentity(ctx, identity, changed.Revision)
			require.ErrorIs(t, err, ErrPersonIdentityConflict)
			resolved, err := s.ResolveExternalPersonUID(ctx, "msgvault", "synthetic", "old")
			if targetExists {
				require.NoError(t, err)
				require.Equal(t, target.PersonID, resolved.PersonID)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
			}
		})
	}
}

func TestLinkExternalIdentityRejectsCurrentAlias(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Example Person", "operator")
	require.NoError(t, err)
	require.NoError(t, s.RecordExternalUIDAliases(ctx, "msgvault", "synthetic", "new", []string{"old"}))
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "old", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
	identities, err := s.PersonExternalIdentities(ctx, person.PersonID)
	require.NoError(t, err)
	require.Empty(t, identities)
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
	identities, err := s.PersonExternalIdentities(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	require.Equal(t, "one", identities[0].UID)
	require.Equal(t, "current", identities[0].UIDState)
}

func TestPersonExternalIdentityCannotBeLinkedToAnotherPerson(t *testing.T) {
	s := newTestStore(t)
	owner, err := s.CreatePerson(t.Context(), "Owner", "operator")
	require.NoError(t, err)
	other, err := s.CreatePerson(t.Context(), "Other", "operator")
	require.NoError(t, err)
	identity := PersonExternalIdentity{PersonID: owner.PersonID, System: "msgvault", ArchiveID: "synthetic",
		UID: "shared", UIDKind: "vcard_uid", UIDState: "current"}
	_, err = s.LinkExternalIdentity(t.Context(), identity, owner.Revision)
	require.NoError(t, err)
	identity.PersonID = other.PersonID
	_, err = s.LinkExternalIdentity(t.Context(), identity, other.Revision)
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
	resolved, err := s.ResolveExternalPersonUID(t.Context(), "msgvault", "synthetic", "shared")
	require.NoError(t, err)
	require.Equal(t, owner.PersonID, resolved.PersonID)
	unchanged, _, err := s.PersonByID(t.Context(), other.PersonID)
	require.NoError(t, err)
	require.Equal(t, other.Revision, unchanged.Revision)
}

func TestRecordExternalUIDAliasesRetargetsAfterSplit(t *testing.T) {
	s := newTestStore(t)
	var people []Person
	for _, uid := range []string{"survivor", "split"} {
		person, err := s.CreatePerson(t.Context(), uid, "transfer")
		require.NoError(t, err)
		_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{
			PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic",
			UID: uid, UIDKind: "vcard_uid", UIDState: "current",
		}, person.Revision)
		require.NoError(t, err)
		people = append(people, person)
	}
	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "survivor", []string{"retired"}))
	resolved, err := s.ResolveExternalPersonUID(t.Context(), "msgvault", "synthetic", "retired")
	require.NoError(t, err)
	require.Equal(t, people[0].PersonID, resolved.PersonID)

	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "split", []string{"retired"}))
	resolved, err = s.ResolveExternalPersonUID(t.Context(), "msgvault", "synthetic", "retired")
	require.NoError(t, err)
	require.Equal(t, people[1].PersonID, resolved.PersonID)
	require.Equal(t, "split", resolved.ResolvedUID)
}

func TestPersonActorKeysPaginationValidatesCursor(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	for index, address := range []string{"Ada@example.test", "Grace@example.test"} {
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
	require.Equal(t, []string{"email:ada@example.test"}, first.Items)
	require.EqualValues(t, 2, first.Total)
	require.NotEmpty(t, first.NextCursor)
	second, err := s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID, Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"email:grace@example.test"}, second.Items)
	require.Empty(t, second.NextCursor)
	_, err = s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: person.PersonID, Limit: 1, Cursor: first.NextCursor + "?"})
	require.ErrorIs(t, err, ErrInvalidPerson)
	other, err := s.CreatePerson(t.Context(), "Other", "operator")
	require.NoError(t, err)
	_, err = s.ActorKeysForPerson(t.Context(), PersonActorKeysRequest{PersonID: other.PersonID, Cursor: first.NextCursor})
	require.ErrorIs(t, err, ErrInvalidPerson)
}

func TestRecordExternalUIDAliasesRejectsCycleAtomically(t *testing.T) {
	s := newTestStore(t)
	person, err := s.CreatePerson(t.Context(), "Example Person", "operator")
	require.NoError(t, err)
	_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID,
		System: "msgvault", ArchiveID: "synthetic", UID: "extra", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.NoError(t, err)
	person, _, err = s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "b", []string{"a"}))
	require.NoError(t, s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "a", []string{"c"}))
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	err = s.RecordExternalUIDAliases(t.Context(), "msgvault", "synthetic", "c", []string{"extra", "a"})
	require.ErrorContains(t, err, "cycle")
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_external_uid_aliases WHERE retired_uid='extra'`).Scan(&count))
	require.Zero(t, count)
	var target string
	require.NoError(t, s.db.QueryRow(`SELECT surviving_uid FROM person_external_uid_aliases WHERE retired_uid='a'`).Scan(&target))
	require.Equal(t, "b", target)
	var unchangedEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&unchangedEpoch))
	require.Equal(t, epoch, unchangedEpoch)
	identities, err := s.PersonExternalIdentities(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	require.Equal(t, "current", identities[0].UIDState)
	unchanged, _, err := s.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Equal(t, person.Revision, unchanged.Revision)
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
