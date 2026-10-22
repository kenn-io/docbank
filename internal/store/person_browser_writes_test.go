package store

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBrowserPersonWritesRequireVisibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "sensitive.txt", "c1", []document.DocumentEventActorV1{{
		ActorKey: "email:sensitive@example.test", Address: "sensitive@example.test", DisplayName: "Sensitive Only",
		Claim: "Sensitive Only", Role: "author", Sensitive: true,
	}})
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	publication.People.Edges[0].Sensitive = true
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	sensitiveID := publication.People.Edges[0].PersonID

	hiddenPhoto := browsePhotoNode(t, s, "hidden.jpg", fakeHash("c2"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, hiddenPhoto.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	hiddenOnly, err := s.CreatePerson(ctx, "Hidden Only", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(unlocked, hiddenPhoto.CurrentVersionID, hiddenOnly.PersonID, "")
	require.NoError(t, err)

	revision := func(id string) int64 {
		person, _, err := s.PersonByID(ctx, id)
		require.NoError(t, err)
		return person.Revision
	}
	visible, err := s.CreatePerson(ctx, "Visible", "operator")
	require.NoError(t, err)
	browser := WithVisiblePeopleOnly(ctx)
	for _, concealed := range []string{sensitiveID, hiddenOnly.PersonID} {
		// A stale revision does not reveal that a concealed person exists.
		for _, stale := range []int64{0, 1} {
			_, err = s.UpdatePerson(browser, concealed, revision(concealed)+stale, "Renamed")
			require.ErrorIs(t, err, ErrNotFound)
		}
		_, err = s.MergePersons(browser, visible.PersonID, concealed, uuid.NewString(), revision(visible.PersonID), revision(concealed))
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.MergePersons(browser, concealed, visible.PersonID, uuid.NewString(), revision(concealed), revision(visible.PersonID))
		require.ErrorIs(t, err, ErrNotFound)
	}
	// API-key writes keep their full scope; unlocking restores Hidden-only people.
	_, err = s.UpdatePerson(ctx, sensitiveID, revision(sensitiveID), "Sensitive Renamed")
	require.NoError(t, err)
	_, err = s.UpdatePerson(WithVisiblePeopleOnly(unlocked), hiddenOnly.PersonID, revision(hiddenOnly.PersonID), "Hidden Renamed")
	require.NoError(t, err)

	// Replay is answered from its receipt after the absorbed person retires.
	absorbed, err := s.CreatePerson(ctx, "Absorbed", "operator")
	require.NoError(t, err)
	operation, survivorRevision := uuid.NewString(), revision(visible.PersonID)
	first, err := s.MergePersons(browser, visible.PersonID, absorbed.PersonID, operation, survivorRevision, absorbed.Revision)
	require.NoError(t, err)
	replayed, err := s.MergePersons(browser, visible.PersonID, absorbed.PersonID, operation, survivorRevision, absorbed.Revision)
	require.NoError(t, err)
	require.Equal(t, first.MergeID, replayed.MergeID)
}
