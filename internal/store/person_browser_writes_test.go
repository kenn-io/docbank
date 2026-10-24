package store

import (
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPersonBrowserMergeReplayRequiresHiddenUnlockForCustodian(t *testing.T) {
	t.Parallel()
	for _, scopeKind := range []string{"document", "collection"} {
		t.Run(scopeKind, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			survivor, err := s.CreatePerson(ctx, "Library Person", "operator")
			require.NoError(t, err)
			library := browsePhotoNode(t, s, "library.jpg", fakeHash("library"), "image/jpeg")
			_, err = s.MarkPersonInPhoto(ctx, library.CurrentVersionID, survivor.PersonID, "")
			require.NoError(t, err)
			absorbed, err := s.CreatePerson(ctx, "Hidden Custodian", "operator")
			require.NoError(t, err)
			run, err := s.BeginIngest(ctx, "test", "/synthetic")
			require.NoError(t, err)
			hidden, _, err := s.IngestFile(ctx, run, s.RootID(), "hidden.jpg", fakeHash("hidden"), 20, "image/jpeg", "/synthetic/hidden.jpg", "2026-01-01T00:00:00Z")
			require.NoError(t, err)
			scope := CustodianScope{Kind: "document", NodeID: hidden.ID, ContentVersionID: hidden.CurrentVersionID}
			if scopeKind == "collection" {
				scope = CustodianScope{Kind: "collection", IngestID: run.ID()}
			}
			assignment, err := s.SetCustodian(ctx, CustodianRequest{Scope: scope, PersonID: absorbed.PersonID,
				RawLabel: absorbed.DisplayName, Rank: "primary", Basis: "operator_assigned", IfMatchRevision: 1})
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
			target := requireDocumentEventTarget(t, s, hidden.CurrentVersionID)
			record := document.DocumentEventsV1{VaultUID: s.VaultID(), ContentVersionID: hidden.CurrentVersionID,
				ContractVersion: document.DocumentEventsContractV1, DocumentKind: "image"}
			_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint,
				requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
			require.NoError(t, err)
			input, err := s.PrepareDocumentPeopleInputs(ctx, hidden.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, input.Custodians, 1)
			raw, err := canonical.Marshal(input)
			require.NoError(t, err)
			_, err = s.PublishDocumentPeople(ctx, DocumentPeoplePublication{
				People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1,
					ContentVersionID: input.ContentVersionID, EventGenerationID: input.EventGenerationID,
					Edges: []document.DocumentPersonEdgeV1{{PersonID: absorbed.PersonID, Role: "custodian",
						EvidenceKind: "custodian_assignment", EvidenceID: assignment.AssignmentID,
						Confidence: "operator_asserted", Basis: "operator_assigned", RawLabel: absorbed.DisplayName, ClaimCount: 1}}},
				InputsSHA256: fakeSHA256(raw), ResolverFingerprint: document.PersonResolverFingerprint(),
				NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch,
			})
			require.NoError(t, err)
			browser := WithVisiblePeopleOnly(ctx)
			operation := uuid.NewString()
			_, err = s.MergePersons(browser, survivor.PersonID, absorbed.PersonID, operation, survivor.Revision, absorbed.Revision)
			require.ErrorIs(t, err, ErrNotFound, "published Hidden custodian evidence conceals the absorbed person")
			token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			first, err := s.MergePersons(WithPhotoHiddenToken(browser, token), survivor.PersonID, absorbed.PersonID, operation, survivor.Revision, absorbed.Revision)
			require.NoError(t, err)
			require.Equal(t, "Hidden Custodian", first.AbsorbedDisplayName)
			require.Equal(t, []string{assignment.AssignmentID}, first.Moved.AssignmentIDs)
			require.Empty(t, first.Moved.AssertionIDs)
			require.Empty(t, first.Moved.FaceCandidates)
			require.NoError(t, s.LockPhotoHidden(ctx))
			replayed, err := s.MergePersons(browser, survivor.PersonID, absorbed.PersonID, operation, survivor.Revision, absorbed.Revision)
			require.ErrorIs(t, err, ErrHiddenLocked, "locked replay returned absorbed display name %q", replayed.AbsorbedDisplayName)
			require.Empty(t, replayed.AbsorbedDisplayName)
			token, _, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			replayed, err = s.MergePersons(WithPhotoHiddenToken(browser, token), survivor.PersonID, absorbed.PersonID, operation, survivor.Revision, absorbed.Revision)
			require.NoError(t, err)
			require.Equal(t, first, replayed)
		})
	}
}

func TestBrowserMergeRequiresHiddenUnlock(t *testing.T) {
	t.Parallel()
	t.Run("assertion", func(t *testing.T) {
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
	})
	t.Run("rejection", func(t *testing.T) {
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
	})
}

func TestPersonBrowserMergeReplayRequiresHiddenUnlockForCandidates(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"rejected", "deduplicated", "superseded"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
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
			if kind == "superseded" {
				candidate := candidateForTest(t, "face:"+face.FaceID, "Face", right.PersonID, []PersonCandidateOccurrence{{
					ContentVersionID: hidden.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID,
				}})
				candidate.Reason = personCandidateReasonFaceMatch
				openCandidateForTest(t, s, candidate)
			} else {
				decideFaceForTest(t, s, hidden, face.FaceID, right.PersonID, "reject")
				if kind == "deduplicated" {
					decideFaceForTest(t, s, hidden, face.FaceID, left.PersonID, "reject")
				}
			}
			assertions, err := s.DocumentPersonAssertions(ctx, hidden.CurrentVersionID)
			require.NoError(t, err)
			require.Empty(t, assertions)
			asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
			token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			browser := WithVisiblePeopleOnly(ctx)
			unlockedBrowser := WithPhotoHiddenToken(browser, token)
			operation := uuid.NewString()
			first, err := s.MergePersons(unlockedBrowser, left.PersonID, right.PersonID, operation, left.Revision, right.Revision)
			require.NoError(t, err)
			require.Empty(t, first.Moved.DeduplicatedAssertions)
			require.Len(t, first.Moved.FaceCandidates, 1)
			moved := first.Moved.FaceCandidates[0]
			if kind == "deduplicated" {
				require.NotEqual(t, moved.CandidateID, moved.RetainedCandidateID)
			} else {
				require.Equal(t, moved.CandidateID, moved.RetainedCandidateID)
			}
			if kind == "superseded" {
				require.Equal(t, []string{moved.CandidateID}, first.Moved.SupersededCandidates)
			}
			require.NoError(t, s.LockPhotoHidden(ctx))
			replayed, err := s.MergePersons(browser, left.PersonID, right.PersonID, operation, left.Revision, right.Revision)
			require.ErrorIs(t, err, ErrHiddenLocked)
			require.Empty(t, replayed.Moved)
			replayed, err = s.MergePersons(ctx, left.PersonID, right.PersonID, operation, left.Revision, right.Revision)
			require.NoError(t, err)
			require.Equal(t, first, replayed)
			token, _, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			replayed, err = s.MergePersons(WithPhotoHiddenToken(browser, token), left.PersonID, right.PersonID, operation, left.Revision, right.Revision)
			require.NoError(t, err)
			require.Equal(t, first, replayed)
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

func TestPersonBrowserMergeReplayAfterChainedHiddenDeduplication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Hidden Only A", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Hidden Only B", "operator")
	require.NoError(t, err)
	c, err := s.CreatePerson(ctx, "Library C", "operator")
	require.NoError(t, err)
	hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("chained-hidden"), "image/jpeg")
	for _, person := range []Person{a, c} {
		_, err = s.MarkPersonInPhoto(ctx, hidden.CurrentVersionID, person.PersonID, "")
		require.NoError(t, err)
	}
	library := browsePhotoNode(t, s, "library.jpg", fakeHash("chained-library"), "image/jpeg")
	_, err = s.MarkPersonInPhoto(ctx, library.CurrentVersionID, c.PersonID, "")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	browser := WithVisiblePeopleOnly(ctx)
	unlockedBrowser := WithPhotoHiddenToken(browser, token)
	a, _, err = s.PersonByID(ctx, a.PersonID)
	require.NoError(t, err)
	operation := uuid.NewString()
	first, err := s.MergePersons(unlockedBrowser, b.PersonID, a.PersonID, operation, b.Revision, a.Revision)
	require.NoError(t, err)
	require.Len(t, first.Moved.AssertionIDs, 1)
	require.Empty(t, first.Moved.DeduplicatedAssertions)
	b, _, err = s.PersonByID(ctx, b.PersonID)
	require.NoError(t, err)
	c, _, err = s.PersonByID(ctx, c.PersonID)
	require.NoError(t, err)
	second, err := s.MergePersons(unlockedBrowser, c.PersonID, b.PersonID, uuid.NewString(), c.Revision, b.Revision)
	require.NoError(t, err)
	require.Len(t, second.Moved.DeduplicatedAssertions, 1)
	require.Equal(t, first.Moved.AssertionIDs[0], second.Moved.DeduplicatedAssertions[0].Assertion.AssertionID)
	var remaining int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_document_assertions WHERE assertion_id=?`, first.Moved.AssertionIDs[0]).Scan(&remaining))
	require.Zero(t, remaining)
	require.NoError(t, s.LockPhotoHidden(ctx))
	replayed, err := s.MergePersons(browser, first.SurvivorPersonID, a.PersonID, operation, first.SurvivorRevisionBefore, a.Revision)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Empty(t, replayed.AbsorbedDisplayName)
}

func TestPersonBrowserMergeReplayAfterChainedHiddenCandidateDeduplication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Hidden Only A", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Hidden Only B", "operator")
	require.NoError(t, err)
	c, err := s.CreatePerson(ctx, "Library C", "operator")
	require.NoError(t, err)
	hidden, face := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, hidden, face.FaceID, a.PersonID, "reject")
	decideFaceForTest(t, s, hidden, face.FaceID, c.PersonID, "reject")
	library := browsePhotoNode(t, s, "library.jpg", fakeHash("chained-candidate-library"), "image/jpeg")
	_, err = s.MarkPersonInPhoto(ctx, library.CurrentVersionID, c.PersonID, "")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	browser := WithVisiblePeopleOnly(ctx)
	unlockedBrowser := WithPhotoHiddenToken(browser, token)
	a, _, err = s.PersonByID(ctx, a.PersonID)
	require.NoError(t, err)
	operation := uuid.NewString()
	first, err := s.MergePersons(unlockedBrowser, b.PersonID, a.PersonID, operation, b.Revision, a.Revision)
	require.NoError(t, err)
	require.Empty(t, first.Moved.AssertionIDs)
	require.Len(t, first.Moved.FaceCandidates, 1)
	b, _, err = s.PersonByID(ctx, b.PersonID)
	require.NoError(t, err)
	c, _, err = s.PersonByID(ctx, c.PersonID)
	require.NoError(t, err)
	second, err := s.MergePersons(unlockedBrowser, c.PersonID, b.PersonID, uuid.NewString(), c.Revision, b.Revision)
	require.NoError(t, err)
	require.Len(t, second.Moved.FaceCandidates, 1)
	require.Equal(t, first.Moved.FaceCandidates[0].CandidateID, second.Moved.FaceCandidates[0].CandidateID)
	require.NotEqual(t, second.Moved.FaceCandidates[0].CandidateID, second.Moved.FaceCandidates[0].RetainedCandidateID)
	require.NoError(t, s.LockPhotoHidden(ctx))
	replayed, err := s.MergePersons(browser, b.PersonID, a.PersonID, operation, first.SurvivorRevisionBefore, a.Revision)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Empty(t, replayed.AbsorbedDisplayName)
}
