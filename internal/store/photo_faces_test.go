package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func facePhotoFixture(t *testing.T, s *Store) Node {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), "portrait.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	return node
}

func TestPhotoFaceManualAndBoxlessAssertions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	unnamed, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, box, "")
	require.NoError(t, err)
	require.Empty(t, unnamed.Face.AssertionID)
	named, err := s.UpdatePhotoFace(ctx, unnamed.Face.FaceID, 1, nil, person.PersonID)
	require.NoError(t, err)
	require.NotEmpty(t, named.Face.AssertionID)
	assertions, err := s.DocumentPersonAssertions(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, "depicted", assertions[0].Role)
	marked, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, named.Face.AssertionID, marked.AssertionID)
	_, err = s.UpdatePhotoFace(ctx, named.Face.FaceID, 1, &box, "")
	require.ErrorIs(t, err, ErrStaleRevision)
	second, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, box, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, named.Face.AssertionID, second.Face.AssertionID)
	faces, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	_, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{}, person.PersonID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceReceiptsAndUndo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	created, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, box, "")
	require.NoError(t, err)
	named, err := s.UpdatePhotoFace(ctx, created.Face.FaceID, 1, nil, person.PersonID)
	require.NoError(t, err)
	undo, err := s.UndoPhotoFaceChange(ctx, named.ReceiptID)
	require.NoError(t, err)
	require.NotEmpty(t, undo)
	faces, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	require.Empty(t, faces[0].AssertionID)
	require.Equal(t, int64(3), faces[0].Revision)
	_, err = s.UndoPhotoFaceChange(ctx, named.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	moved := document.FaceBox{X: 0.2, Y: 0.2, Width: 0.3, Height: 0.4}
	edited, err := s.UpdatePhotoFace(ctx, created.Face.FaceID, 3, &moved, "")
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceChange(ctx, edited.ReceiptID)
	require.NoError(t, err)
	faces, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, box, faces[0].Box)
	_, err = s.UndoPhotoFaceChange(ctx, created.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	fresh, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, box, person.PersonID)
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceChange(ctx, fresh.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceUndoPreservesLaterBoxlessAssertion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	created, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.3}, person.PersonID)
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceChange(ctx, created.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	faces, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
}

func TestPhotoFaceRescanRetainsIDsAndDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	a := document.FaceBox{X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2}
	b := document.FaceBox{X: 0.6, Y: 0.6, Width: 0.2, Height: 0.2}
	faces, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{b, a})
	require.NoError(t, err)
	require.Len(t, faces, 2)
	ids := map[document.FaceBox]string{}
	for _, face := range faces {
		ids[face.Box] = face.FaceID
	}
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = s.UpdatePhotoFace(ctx, ids[a], 1, nil, person.PersonID)
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, ids[b], person.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.NoError(t, err)
	manual, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{X: 0.4, Width: 0.1, Height: 0.1}, "")
	require.NoError(t, err)
	shifted := a
	shifted.X += 0.01
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v2", []document.FaceBox{shifted, b})
	require.NoError(t, err)
	require.Len(t, faces, 3)
	for _, face := range faces {
		switch face.FaceID {
		case ids[a]:
			require.Equal(t, shifted, face.Box)
			require.NotEmpty(t, face.AssertionID)
		case ids[b]:
			require.Equal(t, b, face.Box)
		case manual.Face.FaceID:
			require.Equal(t, manual.Face, face)
		default:
			t.Fatal("face ID changed")
		}
	}
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v3", nil)
	require.NoError(t, err)
	require.Len(t, faces, 3)
	rejected, total, err := s.PersonCandidates(ctx, "rejected", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, candidate.CandidateID, rejected[0].CandidateID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func openFaceCandidate(t *testing.T, s *Store, versionID, faceID, personID string) PersonMatchCandidate {
	t.Helper()
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: versionID, Role: "depicted", EvidenceKind: "face", EvidenceID: faceID}})
	require.NoError(t, err)
	var result PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		var retained bool
		result, retained, err = s.OpenPersonCandidate(t.Context(), tx, PersonMatchCandidate{ActorKey: "face:" + faceID, DisplayName: "Ada", SuggestedPersonID: personID, Reason: "face_match", Evidence: evidence})
		require.True(t, retained)
		return err
	}))
	return result
}

func TestPhotoFaceCandidatesAttachSharedAssertionAndMerge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	duplicate, err := s.CreatePerson(ctx, "Ada duplicate", "operator")
	require.NoError(t, err)
	first, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, ada.PersonID)
	require.NoError(t, err)
	second, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, second.Face.FaceID, duplicate.PersonID)
	linked, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: duplicate.PersonID})
	require.NoError(t, err)
	require.Equal(t, "linked", linked.State)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, ada.PersonID, duplicate.PersonID, operation, 1, 1)
	require.NoError(t, err)
	faces, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	for _, face := range faces {
		require.Equal(t, first.Face.AssertionID, face.AssertionID)
	}
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceCandidateGroupNamesEveryFace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	first, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	second, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: first.Face.FaceID}, {ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: second.Face.FaceID}})
	require.NoError(t, err)
	var candidate PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		candidate, _, err = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + first.Face.FaceID, DisplayName: "Ada", SuggestedPersonID: ada.PersonID, Reason: "face_match", Evidence: evidence})
		return err
	}))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: ada.PersonID})
	require.NoError(t, err)
	faces, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	require.NotEmpty(t, faces[0].AssertionID)
	require.Equal(t, faces[0].AssertionID, faces[1].AssertionID)
}

func TestPhotoFaceHiddenReadsAndDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.Face.FaceID, person.PersonID)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.ErrorIs(t, err, ErrHiddenLocked)
	candidates, total, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, candidates)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	faces, err := s.PhotoFaces(unlocked, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	candidates, total, err = s.PersonCandidates(unlocked, "open", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, candidates, 1)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.PhotoFaces(unlocked, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoFaceMetadataRoundTripAndInvalidReferences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE persons SET key_face_id=? WHERE person_id=?`, face.Face.FaceID, person.PersonID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NoError(t, s.ValidateMetadata(ctx))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	read, _, err := target.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, face.Face.FaceID, read.KeyFaceID)
	faces, err := target.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face.Face}, faces)
	var roundtrip bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &roundtrip))
	require.Equal(t, exported.String(), roundtrip.String())
	for _, field := range []string{"key_face_id", "content_version_id", "assertion_id"} {
		t.Run(field, func(t *testing.T) {
			lines := strings.Split(exported.String(), "\n")
			for i, line := range lines {
				if (field == "key_face_id" && strings.Contains(line, `"type":"person"`)) || (field != "key_face_id" && strings.Contains(line, `"type":"photo_face"`)) {
					var record map[string]any
					require.NoError(t, json.Unmarshal([]byte(line), &record))
					record[field] = "11111111-1111-4111-8111-111111111111"
					raw, err := json.Marshal(record)
					require.NoError(t, err)
					lines[i] = string(raw)
				}
			}
			invalid := newTestStore(t)
			require.Error(t, invalid.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))))
		})
	}
	// Older person records omit the optional key-face field.
	old := strings.ReplaceAll(exported.String(), `"key_face_id":"`+face.Face.FaceID+`",`, "")
	legacy := newTestStore(t)
	require.NoError(t, legacy.ImportMetadata(ctx, strings.NewReader(old)))
}

func TestPhotoFaceAuditWritesFailClosed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UpdatePhotoFace(ctx, face.Face.FaceID, 1, nil, person.PersonID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", nil)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.UndoPhotoFaceChange(ctx, face.ReceiptID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_faces`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_document_assertions`).Scan(&count))
	require.Zero(t, count)
}

func TestPhotoFaceRescanKeepsManualBoxAndBreaksTies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	manual, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, box, "")
	require.NoError(t, err)
	shifted := box
	shifted.X = 0.01
	faces, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{shifted})
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{manual.Face}, faces)
	other := document.FaceBox{X: 0.5, Y: 0.5, Width: 0.2, Height: 0.2}
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{other, other})
	require.NoError(t, err)
	require.Len(t, faces, 3)
	var detectorIDs []string
	for _, face := range faces {
		if face.DetectorFingerprint != "manual" {
			detectorIDs = append(detectorIDs, face.FaceID)
		}
	}
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v2", []document.FaceBox{other})
	require.NoError(t, err)
	require.Len(t, faces, 2)
	for _, face := range faces {
		if face.FaceID == detectorIDs[0] {
			require.Equal(t, int64(2), face.Revision)
			require.Equal(t, "detector-v2", face.DetectorFingerprint)
		} else {
			require.Equal(t, int64(1), face.Revision)
		}
	}
}

func TestPhotoFaceCrossVersionAndStaleCandidates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := facePhotoFixture(t, s)
	second, err := s.CreateFile(ctx, s.RootID(), "second.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	assertion, err := s.MarkPersonInPhoto(ctx, second.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE photo_faces SET assertion_id=? WHERE face_id=?`, assertion.AssertionID, face.Face.FaceID)
	require.Error(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: second.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.Face.FaceID}})
	require.NoError(t, err)
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, _, err := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.Face.FaceID, DisplayName: "Ada", Reason: "face_match", Evidence: evidence})
		return err
	})
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.Face.FaceID, person.PersonID)
	_, err = s.UndoPhotoFaceChange(ctx, face.ReceiptID)
	require.NoError(t, err)
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM person_match_candidates WHERE candidate_id=?`, candidate.CandidateID).Scan(&state))
	require.Equal(t, "superseded", state)
	require.NoError(t, s.ValidateMetadata(ctx))
}
