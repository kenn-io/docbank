package store

import (
	"bytes"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestFaceCandidateRejectionAfterSourceUnavailable(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"named", "unnamed"} {
		for _, change := range []string{"excluded", "replaced", "trashed"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				t.Parallel()
				s := newTestStore(t)
				ctx := t.Context()
				var nodes []Node
				var faces []FaceSuggestionFace
				for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
					node := browsePhotoNode(t, s, name, fakeHash("a1"), "image/jpeg")
					face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
					require.NoError(t, err)
					nodes = append(nodes, node)
					faces = append(faces, FaceSuggestionFace{PhotoFace: face})
				}
				personID, name := "", ""
				if kind == "named" {
					person, err := s.CreatePerson(ctx, "Ada", "operator")
					require.NoError(t, err)
					personID, name = person.PersonID, person.DisplayName
				}
				group, err := NewFaceCandidate(faces, personID, name)
				require.NoError(t, err)
				require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
				original, _, err := s.PersonCandidates(ctx, "open", 24, 0)
				require.NoError(t, err)
				require.Len(t, original, 1)
				switch change {
				case "excluded":
					asset, err := s.PhotoAssetForNode(ctx, nodes[0].ID)
					require.NoError(t, err)
					_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
				case "replaced":
					_, _, err = s.ReplaceContent(ctx, nodes[0].ID, nodes[0].Revision, fakeHash("b2"), 8, "image/jpeg")
					require.NoError(t, err)
				case "trashed":
					_, _, err = s.Trash(ctx, nodes[0].ID, UnconditionalRev)
					require.NoError(t, err)
				}
				page, total, err := s.FaceCandidates(ctx, 24, 0)
				require.NoError(t, err)
				require.EqualValues(t, 1, total)
				require.Len(t, page, 1)
				require.Len(t, page[0].Faces, 2)
				decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, Action: "reject", FaceOnly: true})
				require.NoError(t, err)
				require.Equal(t, "rejected", decided.State)
				require.Equal(t, original[0].EvidenceSHA256, decided.EvidenceSHA256)
				var backup bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &backup))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(ctx, &backup))
				require.NoError(t, restored.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, restored), []PersonMatchCandidate{group}))
				page, total, err = restored.FaceCandidates(ctx, 24, 0)
				require.NoError(t, err)
				require.Empty(t, page)
				require.Zero(t, total)
			})
		}
	}
}

func TestFaceDismissedRegroupKeepsDecisionIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	nodes := []Node{browsePhotoNode(t, s, "a.jpg", fakeHash("a1"), "image/jpeg"), browsePhotoNode(t, s, "b.jpg", fakeHash("b2"), "image/jpeg"), browsePhotoNode(t, s, "c.jpg", fakeHash("c3"), "image/jpeg")}
	slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.CurrentVersionID, b.CurrentVersionID) })
	var faces []FaceSuggestionFace
	for i, id := range []string{"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"} {
		face, _, err := s.CreatePhotoFace(ctx, nodes[i].CurrentVersionID, id, document.FaceBox{Width: .3, Height: .4}, "")
		require.NoError(t, err)
		faces = append(faces, FaceSuggestionFace{PhotoFace: face})
	}
	slices.Reverse(faces)
	group, err := NewFaceCandidate(faces, "", "")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	decideFaceForTest(t, s, nodes[2], faces[0].FaceID, "", "dismiss")
	page, _, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, Action: "reject", FaceOnly: true})
	require.NoError(t, err)
	pair, err := NewFaceCandidate(faces[1:], "", "")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{pair}))
	page, _, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page)
}

type faceCandidatePairFixture struct {
	node          Node
	first, second PhotoFace
	group         PersonMatchCandidate
}

func faceCandidatePair(t *testing.T, s *Store) faceCandidatePairFixture {
	t.Helper()
	node, first := faceForDecisionTest(t, s, "")
	second, _, err := s.CreatePhotoFace(t.Context(), node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	group, err := NewFaceCandidate([]FaceSuggestionFace{{PhotoFace: first}, {PhotoFace: second}}, "", "")
	require.NoError(t, err)
	return faceCandidatePairFixture{node, first, second, group}
}

func TestFaceConfirmedGroupReturnsAfterRetraction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pair := faceCandidatePair(t, s)
	node, first, second, group := pair.node, pair.first, pair.second, pair.group
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	page, _, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, Action: "new_person", DisplayName: "Ada", FaceOnly: true})
	require.NoError(t, err)
	decideFaceForTest(t, s, node, first.FaceID, "", "retract")
	decideFaceForTest(t, s, node, second.FaceID, "", "retract")
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	page, _, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestFaceSuggestionsAtomicNameAndRejection(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"new person", "existing", "reject"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			pair := faceCandidatePair(t, s)
			node, input := pair.node, pair.group
			require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{input}))
			page, total, err := s.FaceCandidates(ctx, 24, 0)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, page, 1)
			require.Len(t, page[0].Faces, 2)
			action := "new_person"
			person := ""
			name := "Ada"
			if mode == "existing" {
				p, err := s.CreatePerson(ctx, "Ada", "operator")
				require.NoError(t, err)
				person = p.PersonID
				action = "link"
				name = ""
			}
			if mode == "reject" {
				action = "reject"
				name = ""
			}
			decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, Action: action, PersonID: person, DisplayName: name, FaceOnly: true})
			require.NoError(t, err)
			faces, assertions, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			if mode == "reject" {
				require.Empty(t, assertions)
				for _, f := range faces {
					require.False(t, f.Dismissed)
				}
			} else {
				require.Len(t, assertions, 1)
				require.Equal(t, decided.DecidedPersonID, assertions[0].PersonID)
				for _, f := range faces {
					require.Equal(t, assertions[0].AssertionID, f.AssertionID)
				}
			}
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}
func TestFaceCandidateRegroupIdentityAndRevisionFences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	group := faceCandidatePair(t, s).group
	signature := faceMatchingSignature(t, s)
	_, err := s.CreateFile(ctx, s.RootID(), "unrelated.txt", fakeHash("e1"), 12, "text/plain")
	require.NoError(t, err)
	require.Equal(t, signature, faceMatchingSignature(t, s))
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	require.Equal(t, signature, faceMatchingSignature(t, s))
	page, _, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	original := page[0]
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), nil))
	page, _, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	page, _, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, original.CandidateID, page[0].CandidateID)
	require.Greater(t, page[0].Revision, original.Revision)
	signature = faceMatchingSignature(t, s)
	decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, Action: "reject", FaceOnly: true})
	require.NoError(t, err)
	require.NotEqual(t, signature, faceMatchingSignature(t, s))
	require.ErrorIs(t, s.ReplaceFaceSuggestions(ctx, signature, []PersonMatchCandidate{group}), ErrStaleRevision)
	retained, _, err := s.PersonCandidates(ctx, "rejected", 24, 0)
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, retained[0].EvidenceSHA256, decided.EvidenceSHA256)
}

func TestFaceCandidateConfirmationUsesSelectedFaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pair := faceCandidatePair(t, s)
	node, first, second, group := pair.node, pair.first, pair.second, pair.group
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{group}))
	page, _, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	decision := CandidateDecision{CandidateID: page[0].CandidateID, ExpectedRevision: page[0].Revision, FaceOnly: true, Action: "new_person", DisplayName: "Ada", FaceIDs: []string{uuid.New().String()}}
	_, err = s.DecidePersonCandidate(ctx, decision)
	require.ErrorIs(t, err, ErrInvalidPerson)
	decision.FaceIDs = []string{second.FaceID, second.FaceID}
	_, err = s.DecidePersonCandidate(ctx, decision)
	require.ErrorIs(t, err, ErrInvalidPerson)
	decision.FaceIDs = []string{second.FaceID}
	saved, err := s.DecidePersonCandidate(ctx, decision)
	require.NoError(t, err)
	faces, assertions, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	for _, f := range faces {
		if f.FaceID == first.FaceID {
			require.Empty(t, f.AssertionID)
			require.False(t, f.Dismissed)
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, Revision: f.Revision, Action: "name", PersonID: saved.DecidedPersonID})
			require.NoError(t, err)
		} else {
			require.Equal(t, assertions[0].AssertionID, f.AssertionID)
		}
	}
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestFaceCandidatePersonVisibilityAndCurrentName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Old Name", "operator")
	require.NoError(t, err)
	hidden, _ := faceForDecisionTest(t, s, person.PersonID)
	visible := browsePhotoNode(t, s, "library.jpg", fakeHash("f2"), "image/jpeg")
	face, _, err := s.CreatePhotoFace(ctx, visible.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	candidate, err := NewFaceCandidate([]FaceSuggestionFace{{PhotoFace: face}}, person.PersonID, person.DisplayName)
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{candidate}))
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	rows, total, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.Zero(t, total)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	rows, total, err = s.FaceCandidates(unlocked, 24, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 1, total)
	renamed, err := s.UpdatePerson(unlocked, person.PersonID, person.Revision, "New Name")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{candidate}))
	rows, _, err = s.FaceCandidates(unlocked, 24, 0)
	require.NoError(t, err)
	require.Equal(t, renamed.DisplayName, rows[0].DisplayName)
	survivor, err := s.CreatePerson(unlocked, "Survivor", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(unlocked, survivor.PersonID, renamed.PersonID, uuid.New().String(), survivor.Revision, renamed.Revision)
	require.NoError(t, err)
	merged, err := NewFaceCandidate([]FaceSuggestionFace{{PhotoFace: face}}, survivor.PersonID, "Old Name")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{merged}))
	rows, _, err = s.FaceCandidates(unlocked, 24, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, survivor.PersonID, rows[0].SuggestedPersonID)
	require.Equal(t, survivor.DisplayName, rows[0].DisplayName)
	// The candidate remains visible while its selected target exists only in Hidden.
	_, err = s.MarkPersonInPhoto(ctx, visible.CurrentVersionID, survivor.PersonID, "")
	require.NoError(t, err)
	concealed, err := s.CreatePerson(unlocked, "Concealed target", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(unlocked, hidden.CurrentVersionID, concealed.PersonID, "")
	require.NoError(t, err)
	decision := CandidateDecision{CandidateID: rows[0].CandidateID, ExpectedRevision: rows[0].Revision, Action: "link", PersonID: concealed.PersonID, FaceOnly: true}
	_, err = s.DecidePersonCandidate(WithVisiblePeopleOnly(ctx), decision)
	require.ErrorIs(t, err, ErrNotFound)
	faces, _, _, err := s.PhotoFacePersonNames(ctx, visible.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].AssertionID, "the failed link must preserve the unnamed face")
	linked, err := s.DecidePersonCandidate(unlocked, decision)
	require.NoError(t, err)
	require.Equal(t, concealed.PersonID, linked.DecidedPersonID)
}

func checkFaceCandidateCatalogReads(t *testing.T, postgres bool) {
	t.Helper()
	var s *Store
	var seed *sql.DB
	prefix := ""
	if postgres {
		setup, owner := pgtest.NewDatabase(t)
		s = newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
		seed, prefix = setup, "documents."
	} else {
		s = newTestStore(t)
		seed = s.db
	}
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "faces.jpg", fakeHash("f1"), 12, "image/jpeg")
	require.NoError(t, err)
	faces := make([]FaceSuggestionFace, 2)
	for i := range faces {
		faces[i].PhotoFace = PhotoFace{FaceID: uuid.New().String(), ContentVersionID: node.CurrentVersionID, Box: document.FaceBox{X: float64(i) * .5, Width: .2, Height: .2}, Revision: 1}
		_, err = s.db.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,created_at) VALUES($1,$2,$3,0,0.2,0.2,1,$4)`, faces[i].FaceID, node.CurrentVersionID, faces[i].Box.X, "2026-01-01T00:00:00Z")
		require.NoError(t, err)
	}
	candidate, err := NewFaceCandidate(faces, "", "")
	require.NoError(t, err)
	candidate, err = normalizePersonCandidate(candidate)
	require.NoError(t, err)
	id := uuid.New().String()
	_, err = seed.ExecContext(ctx, `INSERT INTO `+prefix+`person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at) VALUES($1,$2,$3,'face_match',$4,$5,2,1,'open',$6)`, id, candidate.ActorKey, candidate.DisplayName, string(candidate.Evidence), candidate.EvidenceSHA256, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	for _, face := range faces {
		_, err = seed.ExecContext(ctx, `INSERT INTO `+prefix+`person_candidate_faces(candidate_id,face_id,content_version_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, face.FaceID, face.ContentVersionID)
		require.NoError(t, err)
	}
	page, total, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, page, 1)
	require.Equal(t, id, page[0].CandidateID)
	require.Len(t, page[0].Faces, 2)
	for _, face := range page[0].Faces {
		require.Equal(t, node.CurrentVersionID, face.ContentVersionID)
	}
	_, err = seed.ExecContext(ctx, `UPDATE `+prefix+`photo_assets SET excluded_at=$1 WHERE asset_id=(SELECT asset_id FROM `+prefix+`photo_files WHERE node_id=$2)`, "2026-01-02T00:00:00Z", node.ID)
	require.NoError(t, err)
	page, total, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Zero(t, total)
	_, err = seed.ExecContext(ctx, `UPDATE `+prefix+`photo_assets SET excluded_at=NULL WHERE asset_id=(SELECT asset_id FROM `+prefix+`photo_files WHERE node_id=$1)`, node.ID)
	require.NoError(t, err)
	page, total, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.EqualValues(t, 1, total)
	_, err = seed.ExecContext(ctx, `UPDATE `+prefix+`nodes SET trashed_at=$1 WHERE id=$2`, "2026-01-03T00:00:00Z", node.ID)
	require.NoError(t, err)
	page, total, err = s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Zero(t, total)
}

func faceMatchingSignature(t *testing.T, s *Store) string {
	t.Helper()
	snapshot, err := s.FaceMatchingInputs(t.Context())
	require.NoError(t, err)
	return snapshot.Signature
}

func TestUnnamedFaceCandidateRequiresTwoReviewableFaces(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"name", "dismiss"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			pair := faceCandidatePair(t, s)
			require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s), []PersonMatchCandidate{pair.group}))
			personID := ""
			if action == "name" {
				person, err := s.CreatePerson(ctx, "Synthetic person", "operator")
				require.NoError(t, err)
				personID = person.PersonID
			}
			decideFaceForTest(t, s, pair.node, pair.first.FaceID, personID, action)
			page, total, err := s.FaceCandidates(ctx, 1, 0)
			require.NoError(t, err)
			require.Empty(t, page)
			require.Zero(t, total)
		})
	}
}
