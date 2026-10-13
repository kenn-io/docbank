package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPhotoFaceManualAndBoxlessAssertions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	unnamed, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, "")
	require.NoError(t, err)
	require.Empty(t, unnamed.AssertionID)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, unnamed.FaceID, person.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: person.PersonID})
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	named := faces[0]
	require.NotEmpty(t, named.AssertionID)
	assertions, err := s.DocumentPersonAssertions(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, "depicted", assertions[0].Role)
	marked, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, named.AssertionID, marked.AssertionID)
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, named.AssertionID, second.AssertionID)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{}, person.PersonID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceRescanRetainsIDsAndDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
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
	named := openFaceCandidate(t, s, node.CurrentVersionID, ids[a], person.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: named.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: person.PersonID})
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, ids[b], person.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.NoError(t, err)
	manual, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.4, Width: 0.1, Height: 0.1}, "")
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
		case manual.FaceID:
			require.Equal(t, manual, face)
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
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	duplicate, err := s.CreatePerson(ctx, "Ada duplicate", "operator")
	require.NoError(t, err)
	first, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, ada.PersonID)
	require.NoError(t, err)
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, second.FaceID, duplicate.PersonID)
	linked, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: duplicate.PersonID})
	require.NoError(t, err)
	require.Equal(t, "linked", linked.State)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, ada.PersonID, duplicate.PersonID, operation, 1, 1)
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	for _, face := range faces {
		require.Equal(t, first.AssertionID, face.AssertionID)
	}
	replayed, assertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, second.FaceID, second.Box, duplicate.PersonID)
	require.NoError(t, err)
	require.Equal(t, int64(3), replayed.Revision)
	require.Equal(t, ada.PersonID, assertion.PersonID)
	_, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, ada.PersonID, assertions[0].PersonID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceCandidateGroupNamesEveryFace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	first, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: first.FaceID}, {ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: second.FaceID}})
	require.NoError(t, err)
	var candidate PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		candidate, _, err = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + first.FaceID, DisplayName: "Ada", SuggestedPersonID: ada.PersonID, Reason: "face_match", Evidence: evidence})
		return err
	}))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: ada.PersonID})
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	require.NotEmpty(t, faces[0].AssertionID)
	require.Equal(t, faces[0].AssertionID, faces[1].AssertionID)
}

func TestPhotoFaceHiddenReadsAndDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, person.PersonID)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, face.FaceID, face.Box, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.ErrorIs(t, err, ErrHiddenLocked)
	candidates, total, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, candidates)
	count, full, err := s.OpenPersonCandidateCount(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	require.False(t, full)
	coverage, err := s.DocumentPeopleCoverage(ctx)
	require.NoError(t, err)
	require.Zero(t, coverage.OpenCandidates)
	var global int64
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&global))
	require.Equal(t, int64(1), global)
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, _, err := s.OpenPersonCandidate(ctx, tx, candidate)
		return err
	})
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	faces, _, err := s.PhotoFaces(unlocked, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	candidates, total, err = s.PersonCandidates(unlocked, "open", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, candidates, 1)
	count, _, err = s.OpenPersonCandidateCount(unlocked)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, _, err = s.PhotoFaces(unlocked, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoFaceMetadataRoundTripAndInvalidReferences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE persons SET key_face_id=? WHERE person_id=?`, face.FaceID, person.PersonID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NoError(t, s.ValidateMetadata(ctx))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	read, _, err := target.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, face.FaceID, read.KeyFaceID)
	faces, _, err := target.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, faces)
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
	old := strings.ReplaceAll(exported.String(), `"key_face_id":"`+face.FaceID+`",`, "")
	legacy := newTestStore(t)
	require.NoError(t, legacy.ImportMetadata(ctx, strings.NewReader(old)))
}

func TestPhotoFaceAuditWritesFailClosed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", nil)
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
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	manual, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, "")
	require.NoError(t, err)
	shifted := box
	shifted.X = 0.01
	faces, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{shifted})
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{manual}, faces)
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
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	second, err := s.CreateFile(ctx, s.RootID(), "second.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	assertion, err := s.MarkPersonInPhoto(ctx, second.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.ErrorIs(t, validateFaceAssertion(ctx, s.db, node.CurrentVersionID, assertion.AssertionID), ErrInvalidPhotoFace)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{{ContentVersionID: second.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}})
	require.NoError(t, err)
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, _, err := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Ada", Reason: "face_match", Evidence: evidence})
		return err
	})
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, person.PersonID)
	_, err = s.db.Exec(`DELETE FROM photo_faces WHERE face_id=?`, face.FaceID)
	require.NoError(t, err)
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM person_match_candidates WHERE candidate_id=?`, candidate.CandidateID).Scan(&state))
	require.Equal(t, "superseded", state)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceWholePhotoMarkIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	first, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	require.NoError(t, s.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&epoch))
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	var unchanged int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&unchanged))
	require.Equal(t, epoch, unchanged)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceStaleCandidateRejection(t *testing.T) {
	t.Parallel()
	for _, stale := range []string{"replace", "detach", "exclude", "hidden_replace"} {
		t.Run(stale, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
			require.NoError(t, err)
			candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, "")
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			if stale == "hidden_replace" {
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
			}
			switch stale {
			case "replace", "hidden_replace":
				_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 8, "image/jpeg")
			case "detach":
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
			case "exclude":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			}
			require.NoError(t, err)
			_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "new_person"})
			require.Error(t, err)
			decision := CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"}
			if stale == "hidden_replace" {
				_, err = s.DecidePersonCandidate(ctx, decision)
				require.ErrorIs(t, err, ErrHiddenLocked)
				token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
				ctx = WithPhotoHiddenToken(ctx, token)
			}
			decided, err := s.DecidePersonCandidate(ctx, decision)
			require.NoError(t, err)
			require.Equal(t, "rejected", decided.State)
		})
	}
}

func TestPhotoFaceRescanPreservesOpenCandidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	faces, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{{Width: 0.2, Height: 0.2}})
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, faces[0].FaceID, "")
	rescanned, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v2", nil)
	require.NoError(t, err)
	require.Equal(t, faces, rescanned)
	decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "new_person"})
	require.NoError(t, err)
	require.Equal(t, "linked", decided.State)
}

func TestPhotoFaceKeyFaceClearingAndMerge(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"delete", "suppress", "inherit", "keep"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
			require.NoError(t, err)
			_, err = s.db.Exec(`UPDATE persons SET key_face_id=?,updated_at='2000-01-01T00:00:00Z' WHERE person_id=?`, face.FaceID, person.PersonID)
			require.NoError(t, err)
			expectedKey := ""
			switch change {
			case "delete":
				_, err = s.db.Exec(`DELETE FROM photo_faces WHERE face_id=?`, face.FaceID)
			case "suppress":
				_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{AssertionID: face.AssertionID, ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "depicted", Action: "suppress", Revision: 1})
				require.NoError(t, err)
				_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
				require.ErrorIs(t, err, ErrPersonIdentityConflict)
				_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), face.Box, person.PersonID)
				require.ErrorIs(t, err, ErrPersonIdentityConflict)
				unnamed, _, createErr := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), face.Box, "")
				require.NoError(t, createErr)
				candidate := openFaceCandidate(t, s, node.CurrentVersionID, unnamed.FaceID, person.PersonID)
				_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: person.PersonID})
				require.ErrorIs(t, err, ErrPersonIdentityConflict)
				err = nil
			default:
				survivor, createErr := s.CreatePerson(ctx, "Survivor", "operator")
				require.NoError(t, createErr)
				survivingFace, _, createErr := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, survivor.PersonID)
				require.NoError(t, createErr)
				expectedKey = face.FaceID
				if change == "keep" {
					_, err = s.db.Exec(`UPDATE persons SET key_face_id=? WHERE person_id=?`, survivingFace.FaceID, survivor.PersonID)
					require.NoError(t, err)
					expectedKey = survivingFace.FaceID
				}
				operation, createErr := newUUIDv4()
				require.NoError(t, createErr)
				_, err = s.MergePersons(ctx, survivor.PersonID, person.PersonID, operation, 1, 1)
				require.NoError(t, err)
				person = survivor
				var revision int64
				var assertionID string
				require.NoError(t, s.db.QueryRow(`SELECT revision,assertion_id FROM photo_faces WHERE face_id=?`, face.FaceID).Scan(&revision, &assertionID))
				require.Equal(t, int64(2), revision)
				require.Equal(t, survivingFace.AssertionID, assertionID)
			}
			require.NoError(t, err)
			updated, _, err := s.PersonByID(ctx, person.PersonID)
			require.NoError(t, err)
			require.Equal(t, expectedKey, updated.KeyFaceID)
			require.Equal(t, int64(2), updated.Revision)
			require.NotEqual(t, "2000-01-01T00:00:00Z", updated.UpdatedAt)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceRestoreBoundsAndSemanticReferences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.NoError(t, err)
	other, err := s.CreateFile(ctx, s.RootID(), "other.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	wrongVersion, err := s.MarkPersonInPhoto(ctx, other.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	wrongRole, err := s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	for _, assertion := range []PersonDocumentAssertion{wrongVersion, wrongRole} {
		// Mutate only the face row, preserving the assertion rows and immutable receipts.
		lines := strings.Split(exported.String(), "\n")
		for i, line := range lines {
			if strings.Contains(line, `"type":"photo_face"`) {
				lines[i] = strings.Replace(line, face.AssertionID, assertion.AssertionID, 1)
			}
		}
		invalid := strings.Join(lines, "\n")
		require.ErrorIs(t, newTestStore(t).ImportMetadata(ctx, strings.NewReader(invalid)), ErrInvalidPhotoFace)
	}
	var row map[string]any
	for line := range strings.SplitSeq(exported.String(), "\n") {
		if strings.Contains(line, `"type":"photo_face"`) {
			require.NoError(t, json.Unmarshal([]byte(line), &row))
		}
	}
	var bounded strings.Builder
	bounded.WriteString(exported.String())
	for range maxPhotoFaces - 1 {
		id, err := newUUIDv4()
		require.NoError(t, err)
		row["face_id"] = id
		raw, err := json.Marshal(row)
		require.NoError(t, err)
		bounded.Write(raw)
		bounded.WriteByte('\n')
	}
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, strings.NewReader(bounded.String())))
	replayed, _, err := target.CreatePhotoFace(ctx, node.CurrentVersionID, face.FaceID, face.Box, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, face, replayed)
	_, _, err = target.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), face.Box, "")
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	id, err := newUUIDv4()
	require.NoError(t, err)
	row["face_id"] = id
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	bounded.Write(raw)
	bounded.WriteByte('\n')
	require.ErrorIs(t, newTestStore(t).ImportMetadata(ctx, strings.NewReader(bounded.String())), ErrInvalidPhotoFace)
	_, err = target.db.Exec(`INSERT INTO photo_faces SELECT ?,content_version_id,detector_fingerprint,x,y,width,height,revision,assertion_id,created_at FROM photo_faces LIMIT 1`, id)
	require.NoError(t, err)
	_, _, err = target.PhotoFaces(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
}

func TestPhotoFaceRetryAndEpoch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	otherPerson, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&epoch))
	for _, personID := range []string{"", person.PersonID, person.PersonID} {
		id := uuid.New().String()
		face, assertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, personID)
		require.NoError(t, err)
		var before, after bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &before))
		replayed, saved, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, personID)
		require.NoError(t, err)
		require.Equal(t, face, replayed)
		require.Equal(t, assertion, saved)
		require.NoError(t, s.ExportMetadata(ctx, &after))
		require.Equal(t, before.String(), after.String())
		var actual int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&actual))
		if personID == "" {
			require.Equal(t, epoch, actual)
		} else {
			require.Equal(t, epoch+1, actual)
		}
		changed := box
		changed.X = 0.5
		_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, changed, personID)
		require.ErrorIs(t, err, ErrExists)
		conflictingPerson := otherPerson.PersonID
		if personID == "" {
			conflictingPerson = person.PersonID
		}
		_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, conflictingPerson)
		require.ErrorIs(t, err, ErrExists)
		other := browsePhotoNode(t, s, id+".jpg", fakeHash("b2"), "image/jpeg")
		_, _, err = s.CreatePhotoFace(ctx, other.CurrentVersionID, id, box, personID)
		require.ErrorIs(t, err, ErrExists)
	}
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	var unnamed PhotoFace
	for _, face := range faces {
		if face.AssertionID == "" {
			unnamed = face
		}
	}
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, unnamed.FaceID, person.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: person.PersonID})
	require.NoError(t, err)
	replayed, saved, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, unnamed.FaceID, unnamed.Box, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), replayed.Revision)
	require.Equal(t, person.PersonID, saved.PersonID)
	faces, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 3)
	require.Len(t, assertions, 1)
	require.Equal(t, person.PersonID, assertions[0].PersonID)
}

func TestPhotoFacePurgeAndPruneRestoreReferences(t *testing.T) {
	t.Parallel()
	for _, remove := range []string{"purge", "prune"} {
		t.Run(remove, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
			require.NoError(t, err)
			candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, person.PersonID)
			_, err = s.db.Exec(`UPDATE persons SET key_face_id=? WHERE person_id=?`, face.FaceID, person.PersonID)
			require.NoError(t, err)
			if remove == "purge" {
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
				require.NoError(t, err)
				_, err = s.TrashEmpty(ctx, 0, true)
			} else {
				updated, _, replaceErr := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 20, "image/jpeg")
				require.NoError(t, replaceErr)
				_, err = s.PruneContentVersions(ctx, node.ID, updated.Revision, VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, true)
			}
			require.NoError(t, err)
			var count int64
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_faces`).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_document_assertions`).Scan(&count))
			require.Zero(t, count)
			var state string
			require.NoError(t, s.db.QueryRow(`SELECT state FROM person_match_candidates WHERE candidate_id=?`, candidate.CandidateID).Scan(&state))
			require.Equal(t, "superseded", state)
			read, _, err := s.PersonByID(ctx, person.PersonID)
			require.NoError(t, err)
			require.Empty(t, read.KeyFaceID)
			require.Equal(t, int64(2), read.Revision)
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &exported))
			target := newTestStore(t)
			require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
			require.NoError(t, target.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceUnexpectedHiddenErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, "")
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	_, err = s.db.Exec(`DROP TABLE photo_hidden_credentials`)
	require.NoError(t, err)
	_, _, err = s.PersonCandidates(unlocked, "open", 10, 0)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.OpenPersonCandidateCount(unlocked)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrHiddenLocked)
	_, err = s.DecidePersonCandidate(unlocked, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoFaceMergeValidatesOnlyAffectedPeople(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Duplicate", "operator")
	require.NoError(t, err)
	unrelated, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE persons SET key_face_id=? WHERE person_id=?`, uuid.New().String(), unrelated.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.New().String(), 1, 1)
	require.NoError(t, err)
	require.ErrorIs(t, s.ValidateMetadata(ctx), ErrInvalidPhotoFace)
}
