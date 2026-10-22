package store

import (
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBrowserMergeRequiresHiddenUnlock(t *testing.T) {
	t.Parallel()
	for _, hiddenSurvivor := range []bool{false, true} {
		t.Run(strconv.FormatBool(hiddenSurvivor), func(t *testing.T) {
			for _, client := range []string{"browser", "unlocked browser", "api key"} {
				t.Run(client, func(t *testing.T) {
					s := newTestStore(t)
					ctx := t.Context()
					left, err := s.CreatePerson(ctx, "Ada", "operator")
					require.NoError(t, err)
					right, err := s.CreatePerson(ctx, "Grace", "operator")
					require.NoError(t, err)
					for _, person := range []Person{left, right} {
						photo := browsePhotoNode(t, s, person.DisplayName+".jpg", fakeHash(person.DisplayName), "image/jpeg")
						_, err = s.MarkPersonInPhoto(ctx, photo.CurrentVersionID, person.PersonID, "")
						require.NoError(t, err)
					}
					hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("hidden"), "image/jpeg")
					owner := right
					if hiddenSurvivor {
						owner = left
					}
					_, err = s.MarkPersonInPhoto(ctx, hidden.CurrentVersionID, owner.PersonID, "")
					require.NoError(t, err)
					asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
					require.NoError(t, err)
					require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
					_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
					write := ctx
					if client != "api key" {
						write = WithVisiblePeopleOnly(ctx)
					}
					if client == "unlocked browser" {
						token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
						require.NoError(t, err)
						write = WithPhotoHiddenToken(write, token)
					}
					_, err = s.MergePersons(write, left.PersonID, right.PersonID, uuid.NewString(), left.Revision, right.Revision)
					if client == "browser" {
						require.ErrorIs(t, err, ErrHiddenLocked)
						require.ErrorContains(t, err, "unlock Hidden and retry the merge")
						assertions, err := s.DocumentPersonAssertions(ctx, hidden.CurrentVersionID)
						require.NoError(t, err)
						require.Len(t, assertions, 1)
						require.Equal(t, owner.PersonID, assertions[0].PersonID)
						person, _, err := s.PersonByID(ctx, right.PersonID)
						require.NoError(t, err)
						require.Equal(t, right.Revision, person.Revision)
					} else {
						require.NoError(t, err)
						assertions, err := s.DocumentPersonAssertions(ctx, hidden.CurrentVersionID)
						require.NoError(t, err)
						require.Equal(t, left.PersonID, assertions[0].PersonID)
					}
				})
			}
		})
	}
}

func TestBrowserMergeRequiresHiddenRejectionUnlock(t *testing.T) {
	t.Parallel()
	for _, rejectSurvivor := range []bool{false, true} {
		t.Run(strconv.FormatBool(rejectSurvivor), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			left, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			right, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			for _, person := range []Person{left, right} {
				photo := browsePhotoNode(t, s, person.DisplayName+".jpg", fakeHash(person.DisplayName), "image/jpeg")
				_, err = s.MarkPersonInPhoto(ctx, photo.CurrentVersionID, person.PersonID, "")
				require.NoError(t, err)
			}
			hidden, face := faceForDecisionTest(t, s, "")
			rejected := right
			if rejectSurvivor {
				rejected = left
			}
			decideFaceForTest(t, s, hidden, face.FaceID, rejected.PersonID, "reject")
			before, _, _, err := s.PhotoFacePersonNames(ctx, hidden.CurrentVersionID)
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
			_, err = s.MergePersons(WithVisiblePeopleOnly(ctx), left.PersonID, right.PersonID, uuid.NewString(), left.Revision, right.Revision)
			require.ErrorIs(t, err, ErrHiddenLocked)
			token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			unlocked := WithPhotoHiddenToken(ctx, token)
			after, _, _, err := s.PhotoFacePersonNames(unlocked, hidden.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, before, after, "a refused merge preserves the rejection and face revision")
			_, err = s.MergePersons(WithVisiblePeopleOnly(unlocked), left.PersonID, right.PersonID, uuid.NewString(), left.Revision, right.Revision)
			require.NoError(t, err)
			after, _, _, err = s.PhotoFacePersonNames(unlocked, hidden.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, []string{left.PersonID}, after[0].RejectedPersonIDs)
			require.Greater(t, after[0].Revision, before[0].Revision)
		})
	}
}

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

	t.Run("replays the receipt after the absorbed person retires", func(t *testing.T) {
		absorbed, err := s.CreatePerson(ctx, "Absorbed", "operator")
		require.NoError(t, err)
		operation, survivorRevision := uuid.NewString(), revision(visible.PersonID)
		first, err := s.MergePersons(browser, visible.PersonID, absorbed.PersonID, operation, survivorRevision, absorbed.Revision)
		require.NoError(t, err)
		replayed, err := s.MergePersons(browser, visible.PersonID, absorbed.PersonID, operation, survivorRevision, absorbed.Revision)
		require.NoError(t, err)
		require.Equal(t, first.MergeID, replayed.MergeID)
	})
	t.Run("replay conceals the Hidden-only absorbed name after lock", func(t *testing.T) {
		photo := browsePhotoNode(t, s, "visible.jpg", fakeHash("visible-replay"), "image/jpeg")
		_, err := s.MarkPersonInPhoto(ctx, photo.CurrentVersionID, visible.PersonID, "")
		require.NoError(t, err)
		operation, survivorRevision := uuid.NewString(), revision(visible.PersonID)
		absorbedRevision := revision(hiddenOnly.PersonID)
		first, err := s.MergePersons(WithVisiblePeopleOnly(unlocked), visible.PersonID, hiddenOnly.PersonID, operation, survivorRevision, absorbedRevision)
		require.NoError(t, err)
		require.Equal(t, "Hidden Renamed", first.AbsorbedDisplayName)
		require.NoError(t, s.LockPhotoHidden(ctx))
		replayed, err := s.MergePersons(browser, visible.PersonID, hiddenOnly.PersonID, operation, survivorRevision, absorbedRevision)
		require.ErrorIs(t, err, ErrHiddenLocked)
		require.Empty(t, replayed.AbsorbedDisplayName)
		replayed, err = s.MergePersons(ctx, visible.PersonID, hiddenOnly.PersonID, operation, survivorRevision, absorbedRevision)
		require.NoError(t, err)
		require.Equal(t, first, replayed)
	})
	t.Run("replay conceals a deduplicated Hidden assertion after lock", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		browser := WithVisiblePeopleOnly(ctx)
		visible, err := s.CreatePerson(ctx, "Visible", "operator")
		require.NoError(t, err)
		hiddenPhoto := browsePhotoNode(t, s, "hidden.jpg", fakeHash("hidden-duplicate"), "image/jpeg")
		_, err = s.MarkPersonInPhoto(ctx, hiddenPhoto.CurrentVersionID, visible.PersonID, "")
		require.NoError(t, err)
		hiddenAsset, err := s.PhotoAssetForNode(ctx, hiddenPhoto.ID)
		require.NoError(t, err)
		require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
		_, err = s.SetPhotoAssetHidden(ctx, hiddenAsset.ID, hiddenAsset.Revision, true)
		require.NoError(t, err)
		photo := browsePhotoNode(t, s, "visible-duplicate.jpg", fakeHash("visible-duplicate"), "image/jpeg")
		_, err = s.MarkPersonInPhoto(ctx, photo.CurrentVersionID, visible.PersonID, "")
		require.NoError(t, err)
		// A duplicate assertion is deleted by merge; its receipt keeps the version.
		duplicate, err := s.CreatePerson(ctx, "Hidden Duplicate", "operator")
		require.NoError(t, err)
		token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
		require.NoError(t, err)
		unlockedBrowser := WithPhotoHiddenToken(browser, token)
		_, err = s.MarkPersonInPhoto(unlockedBrowser, hiddenPhoto.CurrentVersionID, duplicate.PersonID, "")
		require.NoError(t, err)
		visible, _, err = s.PersonByID(ctx, visible.PersonID)
		require.NoError(t, err)
		operation, survivorRevision := uuid.NewString(), visible.Revision
		first, err := s.MergePersons(unlockedBrowser, visible.PersonID, duplicate.PersonID, operation, survivorRevision, duplicate.Revision)
		require.NoError(t, err)
		require.Len(t, first.Moved.DeduplicatedAssertions, 1)
		require.NoError(t, s.LockPhotoHidden(ctx))
		replayed, err := s.MergePersons(browser, visible.PersonID, duplicate.PersonID, operation, survivorRevision, duplicate.Revision)
		require.ErrorIs(t, err, ErrHiddenLocked)
		require.Empty(t, replayed.AbsorbedDisplayName)
		asset, err := s.PhotoAssetForNode(ctx, photo.ID)
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
		require.NoError(t, err)
		replayed, err = s.MergePersons(browser, visible.PersonID, duplicate.PersonID, operation, survivorRevision, duplicate.Revision)
		require.ErrorIs(t, err, ErrNotFound)
		require.Empty(t, replayed.AbsorbedDisplayName)
	})
}
