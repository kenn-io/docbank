package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func faceForDecisionTest(t *testing.T, s *Store, person string) (Node, PhotoFace) {
	t.Helper()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("f1"), "image/jpeg")
	face, _, err := s.CreatePhotoFace(t.Context(), node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .3, Height: .4}, person)
	require.NoError(t, err)
	return node, face
}

func decideFaceForTest(t *testing.T, s *Store, node Node, faceID, person, action string) PhotoFaceDecisionReceipt {
	t.Helper()
	faces, _, err := s.PhotoFaces(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	var rev int64
	for _, f := range faces {
		if f.FaceID == faceID {
			rev = f.Revision
		}
	}
	receipt, err := s.DecidePhotoFace(t.Context(), node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: faceID, PersonID: person, Action: action, Revision: rev})
	require.NoError(t, err)
	return receipt
}

func TestPhotoFaceDecisionNameRetractUndo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	named := decideFaceForTest(t, s, node, face.FaceID, p.PersonID, "name")
	require.NotEmpty(t, named.ReceiptID)
	faces, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, int64(2), faces[0].Revision)
	require.Equal(t, p.PersonID, assertions[0].PersonID)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "retract", Revision: 1})
	require.ErrorIs(t, err, ErrStaleRevision)
	undo, err := s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].AssertionID)
	_, err = s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.NotEmpty(t, faces[0].AssertionID)
	retracted := decideFaceForTest(t, s, node, face.FaceID, "", "retract")
	faces, assertions, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].AssertionID)
	require.Empty(t, assertions)
	_, err = s.UndoPhotoFaceDecision(ctx, retracted.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionRejectDismissRename(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	grace, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, ada.PersonID)
	rejected := decideFaceForTest(t, s, node, face.FaceID, ada.PersonID, "reject")
	faces, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].AssertionID)
	require.Empty(t, assertions)
	require.Equal(t, []string{ada.PersonID}, faces[0].RejectedPersonIDs)
	_, err = s.UndoPhotoFaceDecision(ctx, rejected.ReceiptID)
	require.NoError(t, err)
	decideFaceForTest(t, s, node, face.FaceID, grace.PersonID, "name")
	faces, assertions, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, grace.PersonID, assertions[0].PersonID)
	decideFaceForTest(t, s, node, face.FaceID, ada.PersonID, "reject")
	faces, assertions, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.NotEmpty(t, faces[0].AssertionID)
	require.Equal(t, grace.PersonID, assertions[0].PersonID)
	dismissed := decideFaceForTest(t, s, node, face.FaceID, "", "dismiss")
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	_, err = s.UndoPhotoFaceDecision(ctx, dismissed.ReceiptID)
	require.NoError(t, err)
	decideFaceForTest(t, s, node, face.FaceID, ada.PersonID, "name")
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].RejectedPersonIDs)
	require.False(t, faces[0].Dismissed)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionKeyFaceAndSplit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, p.PersonID)
	key, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, PersonID: p.PersonID, Action: "key", Revision: face.Revision, PersonRevision: p.Revision})
	require.NoError(t, err)
	detail, err := s.PersonDetail(ctx, p.PersonID)
	require.NoError(t, err)
	require.Equal(t, face.FaceID, detail.KeyFaceID)
	require.Len(t, detail.Assertions, 1)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", Revision: 2, PersonRevision: 1})
	require.ErrorIs(t, err, ErrStaleRevision)
	undone, err := s.UndoPhotoFaceDecision(ctx, key.ReceiptID)
	require.NoError(t, err)
	require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
	_, err = s.UndoPhotoFaceDecision(ctx, undone.ReceiptID)
	require.NoError(t, err)
	detail, err = s.PersonDetail(ctx, p.PersonID)
	require.NoError(t, err)
	split, err := s.SplitPerson(ctx, PersonSplitRequest{PersonID: p.PersonID, OperationID: uuid.NewString(), DisplayName: "Grace", Revision: detail.Revision, AssertionIDs: []string{detail.Assertions[0].AssertionID}})
	require.NoError(t, err)
	require.Equal(t, []string{face.AssertionID}, split.MovedAssertionIDs)
	require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
	require.Equal(t, face.FaceID, personKeyFaceForTest(t, s, split.NewPersonID))
	moved, err := s.PersonDetail(ctx, split.NewPersonID)
	require.NoError(t, err)
	require.Len(t, moved.Assertions, 1)
	require.Equal(t, split.NewPersonID, moved.Assertions[0].PersonID)
	_, err = s.UndoPhotoFaceDecision(ctx, key.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionUndoFencesEveryRecord(t *testing.T) {
	t.Parallel()
	for _, record := range []string{"face", "assertion", "candidate", "person"} {
		t.Run(record, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, "")
			decideFaceForTest(t, s, node, face.FaceID, p.PersonID, "reject")
			receipt := decideFaceForTest(t, s, node, face.FaceID, p.PersonID, "name")
			queries := map[string]string{"face": `UPDATE photo_faces SET revision=revision+1`, "assertion": `UPDATE person_document_assertions SET revision=revision+1`, "candidate": `UPDATE person_match_candidates SET revision=revision+1`, "person": `UPDATE persons SET revision=revision+1`}
			_, err = s.db.ExecContext(ctx, queries[record])
			require.NoError(t, err)
			_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
		})
	}
}

func TestPhotoFaceDecisionHiddenAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	receipt := decideFaceForTest(t, s, node, face.FaceID, p.PersonID, "name")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: 2})
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	detail, err := s.PersonDetail(ctx, p.PersonID)
	require.NoError(t, err)
	require.Empty(t, detail.Assertions)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &metadata))
	token, _, err := restored.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	_, err = restored.UndoPhotoFaceDecision(WithPhotoHiddenToken(ctx, token), receipt.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, restored.ValidateMetadata(ctx))
}

func TestPhotoFaceMergeReviewsContradictions(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			b, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, a.PersonID)
			decideFaceForTest(t, s, node, face.FaceID, b.PersonID, "reject")
			if reverse {
				a, b = b, a
			}
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
			var conflict *PersonFaceMergeConflict
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, []string{face.FaceID}, conflict.FaceIDs)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
			require.ErrorAs(t, err, &conflict)
			require.Empty(t, conflict.FaceIDs)
			require.True(t, conflict.HiddenConflict)
			require.NotContains(t, err.Error(), face.FaceID)
		})
	}
}

func TestPhotoFaceMergePreservesRejectionsAndMarks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, node, face.FaceID, a.PersonID, "reject")
	decideFaceForTest(t, s, node, face.FaceID, b.PersonID, "reject")
	moved, err := s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.Len(t, moved.Moved.RejectedFaceCandidates, 1)
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []string{a.PersonID}, faces[0].RejectedPersonIDs)
	require.NoError(t, s.ValidateMetadata(ctx))
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &metadata))
	require.NoError(t, restored.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionGroupedRejection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	other, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}, {ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID}})
	require.NoError(t, err)
	var c PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		c, _, err = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Ada", SuggestedPersonID: p.PersonID, Reason: "face_match", Evidence: raw})
		return err
	}))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "reject"})
	require.NoError(t, err)
	receipt := decideFaceForTest(t, s, node, face.FaceID, p.PersonID, "name")
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	for _, f := range faces {
		if f.FaceID == face.FaceID {
			require.Empty(t, f.RejectedPersonIDs)
		} else {
			require.Equal(t, []string{p.PersonID}, f.RejectedPersonIDs)
		}
	}
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	for _, f := range faces {
		require.Equal(t, []string{p.PersonID}, f.RejectedPersonIDs)
	}
	require.NoError(t, s.ValidateMetadata(ctx))
}

func personMatchNodeIDs(t *testing.T, s *Store, value query.Query) []int64 {
	t.Helper()
	compiled, err := compileQuery(t.Context(), value, queryResolver{q: s.db})
	require.NoError(t, err)
	predicate, args, err := compiled.Bind("")
	require.NoError(t, err)
	rows, err := s.db.QueryContext(t.Context(), `SELECT n.id FROM nodes n JOIN content_versions cv ON cv.node_id=n.id WHERE `+predicate+` ORDER BY n.id`, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestQueryPersonDocumentsPhotosAliasesAndDuplicateNames(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	node, _ := faceForDecisionTest(t, s, p.PersonID)
	doc, version := seedPeopleVersion(t, s)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version, PersonID: p.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, compilerQuery(t, `person:"ADA LOVELACE"`)))
	typed := snapshotTestQuery(t, `{}`)
	typed.Filters.PersonIDs = []string{p.PersonID}
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, typed))
	page := browsePhotoPage(t, s, `{"syntax":"advanced","text":"person:\"Ada Lovelace\""}`)
	require.Len(t, page.Items, 1)
	duplicate, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	_, err = compileQuery(ctx, compilerQuery(t, `person:"Ada Lovelace"`), queryResolver{q: s.db})
	require.ErrorContains(t, err, "use a person ID")
	_, err = s.MergePersons(ctx, p.PersonID, duplicate.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+duplicate.PersonID)))
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{"filters":{"person_ids":["`+p.PersonID+`"]}}`)
	require.Empty(t, page.Items)
}

func TestQueryPersonCurrentPublishedEdges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "author.txt", "d1", nil)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	require.NotEmpty(t, publication.People.Edges)
	id := publication.People.Edges[0].PersonID
	require.NotEmpty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+id)))
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+id)))
}

func TestPhotoFaceDecisionReceiptRejectsInvalidShape(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, face := faceForDecisionTest(t, s, "")
	receipt := decideFaceForTest(t, s, node, face.FaceID, "", "dismiss")
	receipt.After.Faces[0].ContentVersionID = uuid.NewString()
	raw, err := json.Marshal(photoFaceReceiptAfter{ReceiptID: receipt.ReceiptID, After: receipt.After})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE photo_change_receipts SET after_json=? WHERE receipt_id=?`, string(raw), receipt.ReceiptID)
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	require.Error(t, s.ValidateMetadata(ctx))
}
