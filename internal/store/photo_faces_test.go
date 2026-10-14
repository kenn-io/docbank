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
	named, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
	require.NoError(t, err)
	require.NotEmpty(t, named.AssertionID)
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, named, faces[0])
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
	c := document.FaceBox{X: 0.1, Y: 0.6, Width: 0.1, Height: 0.1}
	faces, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{b, a, c})
	require.NoError(t, err)
	require.Len(t, faces, 3)
	unchanged, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{a, b, c})
	require.NoError(t, err)
	require.Equal(t, faces, unchanged)
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
	pending := openFaceCandidate(t, s, node.CurrentVersionID, ids[c], person.PersonID)
	manual, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.4, Width: 0.1, Height: 0.1}, "")
	require.NoError(t, err)
	shifted := a
	shifted.X += 0.01
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v2", []document.FaceBox{shifted, b})
	require.NoError(t, err)
	require.Len(t, faces, 4)
	for _, face := range faces {
		switch face.FaceID {
		case ids[a]:
			require.Equal(t, shifted, face.Box)
			require.NotEmpty(t, face.AssertionID)
		case ids[b]:
			require.Equal(t, b, face.Box)
		case ids[c]:
			require.Equal(t, c, face.Box)
			require.Equal(t, "detector-v1", face.DetectorFingerprint)
			require.Empty(t, face.AssertionID)
		case manual.FaceID:
			require.Equal(t, manual, face)
		default:
			t.Fatal("face ID changed")
		}
	}
	open, openTotal, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), openTotal)
	require.Len(t, open, 1)
	require.Equal(t, pending.CandidateID, open[0].CandidateID)
	faces, err = s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v3", nil)
	require.NoError(t, err)
	require.Len(t, faces, 4)
	decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: pending.CandidateID, ExpectedRevision: 1, Action: "reject"})
	require.NoError(t, err)
	require.Equal(t, "rejected", decided.State)
	rejected, total, err := s.PersonCandidates(ctx, "rejected", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	rejectedIDs := []string{rejected[0].CandidateID, rejected[1].CandidateID}
	require.ElementsMatch(t, []string{candidate.CandidateID, pending.CandidateID}, rejectedIDs)
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
	conflict := openFaceCandidate(t, s, node.CurrentVersionID, first.FaceID, duplicate.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: conflict.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: duplicate.PersonID})
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
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
	groupFirst, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.1, Y: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	groupSecond, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.5, Y: 0.5, Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	evidence, err := canonical.Marshal([]PersonCandidateOccurrence{
		{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: groupFirst.FaceID},
		{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: groupSecond.FaceID},
	})
	require.NoError(t, err)
	var groupCandidate PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var retained bool
		groupCandidate, retained, err = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{
			ActorKey: "face:" + groupFirst.FaceID, DisplayName: "Ada", SuggestedPersonID: ada.PersonID,
			Reason: "face_match", Evidence: evidence,
		})
		require.True(t, retained)
		return err
	}))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: groupCandidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: ada.PersonID})
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 4)
	var groupAssertionID string
	for _, face := range faces {
		if face.FaceID != groupFirst.FaceID && face.FaceID != groupSecond.FaceID {
			continue
		}
		require.NotEmpty(t, face.AssertionID)
		if groupAssertionID == "" {
			groupAssertionID = face.AssertionID
		} else {
			require.Equal(t, groupAssertionID, face.AssertionID)
		}
	}
	require.NotEmpty(t, groupAssertionID)
	require.NoError(t, s.ValidateMetadata(ctx))
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
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	reconciled, err := s.ReconcilePhotoFaces(ctx, node.CurrentVersionID, "detector-v1", []document.FaceBox{face.Box})
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, reconciled)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, person.PersonID)
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
	require.NoError(t, err)
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

func TestPhotoFaceHiddenQueueCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, visibleVersion := seedPeopleVersion(t, s)
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, face.FaceID, person.PersonID)
	_, err = s.db.Exec(`WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?
	) INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,
		occurrence_count,revision,state,decided_person_id,created_at,decided_at
	) SELECT printf('00000000-0000-4000-8000-%012d',n),c.actor_key,c.display_name,c.suggested_person_id,
		c.reason,c.evidence_json,printf('%064d',n),c.occurrence_count,1,'open',NULL,c.created_at,NULL
		FROM seq,person_match_candidates c WHERE c.candidate_id=?`, maxOpenPersonCandidates-1, candidate.CandidateID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	count, full, err := s.OpenPersonCandidateCount(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	require.True(t, full)
	coverage, err := s.DocumentPeopleCoverage(ctx)
	require.NoError(t, err)
	require.Zero(t, coverage.OpenCandidates)
	require.True(t, coverage.CandidateQueueFull)
	input := candidateForTest(t, "name_alias:overflow", "Overflow", "", []PersonCandidateOccurrence{{
		ContentVersionID: visibleVersion, Role: "author", EvidenceKind: "source_metadata", EvidenceID: "overflow",
	}})
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		result, retained, err := s.OpenPersonCandidate(ctx, tx, input)
		require.Empty(t, result.CandidateID)
		require.False(t, retained)
		return err
	}))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	count, full, err = s.OpenPersonCandidateCount(WithPhotoHiddenToken(ctx, token))
	require.NoError(t, err)
	require.EqualValues(t, maxOpenPersonCandidates, count)
	require.True(t, full)
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
			require.ErrorIs(t, invalid.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))), ErrInvalidPhotoFace)
		})
	}
	// Older person records omit the optional key-face field.
	lines := strings.Split(exported.String(), "\n")
	for i, line := range lines {
		if strings.Contains(line, `"type":"person"`) {
			var record map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &record))
			require.Contains(t, record, "key_face_id")
			delete(record, "key_face_id")
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			lines[i] = string(raw)
		}
	}
	legacy := newTestStore(t)
	require.NoError(t, legacy.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))))
	var keyFace *string
	require.NoError(t, legacy.db.QueryRow(`SELECT key_face_id FROM persons WHERE person_id=?`, person.PersonID).Scan(&keyFace))
	require.Nil(t, keyFace)
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
			if stale == "hidden_replace" {
				require.ErrorIs(t, err, ErrHiddenLocked)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
			}
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
	for range document.MaxPhotoRecordsPerVersion - 1 {
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

func TestPhotoFaceRetryAndBindingEpoch(t *testing.T) {
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
	for _, personID := range []string{"", person.PersonID} {
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
		require.Equal(t, epoch, actual)
		if personID == "" {
			named, namedAssertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, person.PersonID)
			require.NoError(t, err)
			require.Equal(t, face.Revision+1, named.Revision)
			require.NotEmpty(t, named.AssertionID)
			require.Equal(t, named.AssertionID, namedAssertion.AssertionID)
			replayedName, replayedAssertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, person.PersonID)
			require.NoError(t, err)
			require.Equal(t, named, replayedName)
			require.Equal(t, namedAssertion, replayedAssertion)
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, otherPerson.PersonID)
			require.ErrorIs(t, err, ErrExists)
		}
		changed := box
		changed.X = 0.5
		_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, changed, personID)
		require.ErrorIs(t, err, ErrExists)
		if personID != "" {
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, otherPerson.PersonID)
			require.ErrorIs(t, err, ErrExists)
		}
		other := browsePhotoNode(t, s, id+".jpg", fakeHash("b2"), "image/jpeg")
		_, _, err = s.CreatePhotoFace(ctx, other.CurrentVersionID, id, box, personID)
		require.ErrorIs(t, err, ErrExists)
	}
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "a1")
	record.DocumentKind = DocumentEventKindForMIMEType(target.MIMEType)
	raw, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)
	inputsSHA := requireDocumentEventInputsSHA256(t, s, target)
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, inputsSHA, raw)
	require.NoError(t, err)
	var beforeProjection, afterProjection int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&beforeProjection))
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, otherPerson.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&afterProjection))
	require.Equal(t, beforeProjection+1, afterProjection)
}

func TestPhotoFaceBoxStaysStoredWhenPhotoRotationChanges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, "")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	rotation := 90
	_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{
		FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Rotation: &rotation},
	}})
	require.NoError(t, err)
	asset, err = s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.Equal(t, 90, asset.Files[0].Rotation)

	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, face.Box, faces[0].Box)
}

func TestPhotoFaceRescanRecordsEmptyCompletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "empty.jpg", fakeHash("ee"), "image/jpeg")
	for range 2 {
		faces, err := s.ReconcilePhotoFaces(t.Context(), node.CurrentVersionID, "detector-v1", nil)
		require.NoError(t, err)
		require.Empty(t, faces)
	}
	var scans int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_face_scans WHERE content_version_id=? AND detector_fingerprint=?`, node.CurrentVersionID, "detector-v1").Scan(&scans))
	require.Equal(t, 1, scans)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM photo_face_scans`).Scan(&scans))
	require.Zero(t, scans)
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
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
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

func TestPhotoFaceObservationTargetEligibility(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"trashed raw", "sidecar", "replaced image", "replaced raw", "excluded"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset, nodes := photoTrashFixture(t, s)
			imageFile := fileByRole(asset.Files, PhotoRoleImage)
			asset, err := s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &imageFile.ID)
			require.NoError(t, err)
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			box := document.FaceBox{Width: 0.2, Height: 0.2}
			_, _, err = s.CreatePhotoFace(ctx, nodes[0].CurrentVersionID, uuid.New().String(), box, "")
			require.NoError(t, err)
			target := nodes[0]
			switch change {
			case "trashed raw":
				_, _, err = s.Trash(ctx, *target.ParentID, UnconditionalRev)
			case "sidecar":
				target = nodes[2]
			case "replaced image", "replaced raw":
				if change == "replaced image" {
					target = nodes[1]
				}
				target, _, err = s.ReplaceContent(ctx, target.ID, target.Revision, fakeHash("d4"), 4, "text/plain")
			case "excluded":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			}
			require.NoError(t, err)
			_, _, err = s.PhotoFaces(ctx, target.CurrentVersionID)
			require.ErrorIs(t, err, ErrNotFound)
			_, _, err = s.CreatePhotoFace(ctx, target.CurrentVersionID, uuid.New().String(), box, person.PersonID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.MarkPersonInPhoto(ctx, target.CurrentVersionID, person.PersonID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: target.CurrentVersionID, PersonID: person.PersonID, Role: "depicted", Action: "assert", Revision: 1})
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.ReconcilePhotoFaces(ctx, target.CurrentVersionID, "detector-v1", []document.FaceBox{box})
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestPhotoFaceReadIncludesSuppressedReference(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, assertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
	require.NoError(t, err)
	assertion.Action = "suppress"
	suppressed, err := s.AssertDocumentPerson(ctx, *assertion)
	require.NoError(t, err)
	faces, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, faces)
	require.Equal(t, []PersonDocumentAssertion{suppressed}, assertions)
}

func TestPhotoFaceAssertionCapacityAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	_, err := s.db.Exec(`WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?
	) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at)
	SELECT printf('00000000-0000-4000-8000-%012d',n),'Synthetic Person','synthetic person','operator','curated',1,?,? FROM seq`, document.MaxPhotoRecordsPerVersion-1, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision)
	 SELECT person_id,?,person_id,'depicted','assert','',?,1 FROM persons`, node.CurrentVersionID, nowRFC3339())
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	marked, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, marked, repeated)
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
	require.NoError(t, err)
	overflow, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, overflow.PersonID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, overflow.PersonID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	unnamed, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, "")
	require.NoError(t, err)
	candidate := openFaceCandidate(t, s, node.CurrentVersionID, unnamed.FaceID, overflow.PersonID)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: 1, Action: "link", PersonID: overflow.PersonID})
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	overflowAssertion := PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: overflow.PersonID, Role: "depicted", Action: "assert", Revision: 1}
	_, err = s.AssertDocumentPerson(ctx, overflowAssertion)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	overflowAssertion.Role = "author"
	_, err = s.AssertDocumentPerson(ctx, overflowAssertion)
	require.NoError(t, err)
	_, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, document.MaxPhotoRecordsPerVersion)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NoError(t, newTestStore(t).ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	record := metadataPersonDocumentAssertion{Type: metadataPersonAssertionType, AssertionID: uuid.New().String(), ContentVersionID: node.CurrentVersionID, PersonID: overflow.PersonID, Role: "depicted", Action: "suppress", Revision: 1, RecordedAt: nowRFC3339()}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	exported.Write(raw)
	exported.WriteByte('\n')
	require.ErrorIs(t, newTestStore(t).ImportMetadata(ctx, bytes.NewReader(exported.Bytes())), ErrInvalidPhotoFace)
}

func TestPhotoFaceGenericDepictedAuthorization(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	assertion, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	assertion.Action = "suppress"
	_, err = s.AssertDocumentPerson(ctx, assertion)
	require.ErrorIs(t, err, ErrHiddenLocked)
	other, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	create := PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: other.PersonID, Role: "depicted", Action: "assert", Revision: 1}
	_, err = s.AssertDocumentPerson(ctx, create)
	require.ErrorIs(t, err, ErrHiddenLocked)
	create.Role = "author"
	_, err = s.AssertDocumentPerson(ctx, create)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(WithPhotoHiddenToken(ctx, token), assertion)
	require.NoError(t, err)
}
