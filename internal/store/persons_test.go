package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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

func TestPersonDetailResolvesAlias(t *testing.T) {
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
	detail, err := s.PersonDetailPage(ctx, absorbed.PersonID, PersonAssertionPage{})
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

func TestPeopleRecentFirstFollowsVisibleEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	people := map[string]Person{}
	for _, name := range []string{"Ada", "Bob", "Cy", "Zoe", "zoe"} {
		person, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
		people[name] = person
	}
	nodes := map[string]Node{}
	for name, recorded := range map[string]string{"Ada": "2026-01-01T00:00:00Z", "Bob": "2026-02-01T00:00:00Z", "Cy": "2026-01-15T00:00:00Z", "Zoe": "2026-03-01T00:00:00Z"} {
		node := browsePhotoNode(t, s, name+".jpg", fakeHash("recent-"+name), "image/jpeg")
		nodes[name] = node
		_, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, people[name].PersonID, "")
		require.NoError(t, err)
		_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at=? WHERE person_id=?`, recorded, people[name].PersonID)
		require.NoError(t, err)
	}
	// A suppressed assertion is not depicted evidence.
	_, err := s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at='2026-09-01T00:00:00Z',action='suppress' WHERE person_id=?`, people["Cy"].PersonID)
	require.NoError(t, err)
	page := func(ctx context.Context, options PeopleSearchOptions) []string {
		var ids []string
		afterName, afterID := "", ""
		for {
			rows, err := s.PeopleByDisplayName(ctx, "", afterName, afterID, 1, true, options)
			require.NoError(t, err)
			if len(rows) == 0 {
				return ids
			}
			ids = append(ids, rows[0].PersonID)
			afterName, afterID, options.AfterRecent = rows[0].DisplayNameFolded, rows[0].PersonID, rows[0].RecentRank
		}
	}
	zoes := []string{people["Zoe"].PersonID, people["zoe"].PersonID}
	slices.Sort(zoes)
	// Namesakes share their group's newest evidence and stay adjacent across pages.
	recent := PeopleSearchOptions{RecentFirst: true}
	require.Equal(t, append(slices.Clone(zoes), people["Bob"].PersonID, people["Ada"].PersonID, people["Cy"].PersonID), page(ctx, recent))
	byName := page(ctx, PeopleSearchOptions{})
	require.Equal(t, []string{people["Ada"].PersonID, people["Bob"].PersonID, people["Cy"].PersonID}, byName[:3])

	// Merging moves evidence, so the survivor takes the absorbed person's rank.
	cy, ada := people["Cy"], people["Ada"]
	_, err = s.MergePersons(ctx, cy.PersonID, ada.PersonID, uuid.NewString(), cy.Revision, ada.Revision)
	require.NoError(t, err)
	require.Equal(t, append(slices.Clone(zoes), people["Bob"].PersonID, cy.PersonID), page(ctx, recent))

	// Locked Hidden evidence neither ranks nor reveals its person.
	asset, err := s.PhotoAssetForNode(ctx, nodes["Zoe"].ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Equal(t, []string{people["Bob"].PersonID, cy.PersonID, people["zoe"].PersonID}, page(ctx, recent))
	full, err := s.PeopleByDisplayName(ctx, "", "", "", 100, false, recent)
	require.NoError(t, err)
	require.Equal(t, []string{full[0].PersonID, full[1].PersonID}, zoes)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	require.Equal(t, zoes, page(WithPhotoHiddenToken(ctx, token), recent)[:2])
}

func TestPeopleRecentIgnoresUnavailablePhotoEvidence(t *testing.T) {
	t.Parallel()
	for _, condition := range []string{"excluded", "trashed", "stale", "memberless"} {
		t.Run(condition, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			ada, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			bob, err := s.CreatePerson(ctx, "Bob", "operator")
			require.NoError(t, err)
			for index, person := range []Person{ada, bob} {
				node := browsePhotoNode(t, s, person.DisplayName+".jpg", fakeHash(person.DisplayName), "image/jpeg")
				_, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
				require.NoError(t, err)
				_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at=? WHERE person_id=?`, fmt.Sprintf("2026-0%d-01T00:00:00Z", index+1), person.PersonID)
				require.NoError(t, err)
				if index == 1 {
					switch condition {
					case "excluded":
						asset, assetErr := s.PhotoAssetForNode(ctx, node.ID)
						require.NoError(t, assetErr)
						_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					case "trashed":
						_, _, err = s.Trash(ctx, node.ID, node.Revision)
					case "stale":
						_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("d4"), 4, "image/jpeg")
					case "memberless":
						// Keep the depicted assertion on a version without photo membership.
						_, err = s.db.ExecContext(ctx, `UPDATE photo_assets SET display_file_id=NULL WHERE display_file_id IN (SELECT file_id FROM photo_files WHERE node_id=?)`, node.ID)
						require.NoError(t, err)
						_, err = s.db.ExecContext(ctx, `DELETE FROM photo_files WHERE node_id=?`, node.ID)
					}
					require.NoError(t, err)
				}
			}
			rows, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true, PhotoSummary: true, GridRecipe: "grid"})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, ada.PersonID, rows[0].PersonID)
			// Older readable evidence ranks Bob when his newest photo is unavailable.
			older := browsePhotoNode(t, s, "Bob-older.jpg", fakeHash("Bob-older"), "image/jpeg")
			_, err = s.MarkPersonInPhoto(ctx, older.CurrentVersionID, bob.PersonID, "")
			require.NoError(t, err)
			_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at='2025-12-01T00:00:00Z' WHERE content_version_id=?`, older.CurrentVersionID)
			require.NoError(t, err)
			rows, err = s.PeopleByDisplayName(ctx, "Bo", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
			require.NoError(t, err)
			require.Equal(t, bob.PersonID, rows[0].PersonID)
			require.Equal(t, "2025-12-01T00:00:00Z", rows[0].RecentRank)
		})
	}
}

func TestPeopleRecentFirstAfterNameClearedAndGivenAgain(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	bob, err := s.CreatePerson(ctx, "Bob", "operator")
	require.NoError(t, err)
	node := browsePhotoNode(t, s, "rename.jpg", fakeHash("rename-recency"), "image/jpeg")
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, ada.PersonID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at='2001-01-01T00:00:00Z' WHERE person_id=?`, ada.PersonID)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{Action: "retract", FaceID: face.FaceID, Revision: face.Revision})
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, bob.PersonID, "")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET recorded_at='2002-01-01T00:00:00Z' WHERE person_id=?`, bob.PersonID)
	require.NoError(t, err)
	rows, err := s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(t, err)
	require.Equal(t, bob.PersonID, rows[0].PersonID)
	named, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{Action: "name", FaceID: face.FaceID, Revision: face.Revision + 1, PersonID: ada.PersonID})
	require.NoError(t, err)
	rows, err = s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(t, err)
	require.Equal(t, ada.PersonID, rows[0].PersonID)
	require.Greater(t, rows[0].RecentRank, "2002-01-01T00:00:00Z")
	_, err = s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
	require.NoError(t, err)
	rows, err = s.PeopleByDisplayName(ctx, "", "", "", 10, true, PeopleSearchOptions{RecentFirst: true})
	require.NoError(t, err)
	require.Equal(t, bob.PersonID, rows[0].PersonID)
}
