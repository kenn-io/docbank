package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"strconv"
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
	_, assertions, err = s.PhotoFaces(ctx, node.CurrentVersionID)
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
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", PersonRevision: 1})
	require.ErrorIs(t, err, ErrStaleRevision)
	cleared, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", PersonRevision: 2})
	require.NoError(t, err)
	require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
	_, err = s.UndoPhotoFaceDecision(ctx, cleared.ReceiptID)
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
			if record == "person" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrStaleRevision)
			}
		})
	}
	t.Run("unchanged rows", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		a, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		n, f := faceForDecisionTest(t, s, "")
		other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
		require.NoError(t, err)
		r := decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "name")
		require.Len(t, r.Before.Faces, 1)
		require.Empty(t, r.Before.People)
		decideFaceForTest(t, s, n, other.FaceID, "", "dismiss")
		_, err = s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
		require.NoError(t, err)
	})
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
	for _, mode := range []string{"forward", "reverse", "hidden-only", "excluded", "trashed", "retained", "bounded", "rejection union"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			b, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			person := a.PersonID
			if mode == "rejection union" {
				person = ""
			}
			n, f := faceForDecisionTest(t, s, person)
			decideFaceForTest(t, s, n, f.FaceID, b.PersonID, "reject")
			if mode == "rejection union" {
				decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "reject")
				moved, err := s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
				require.NoError(t, err)
				require.Len(t, moved.Moved.FaceCandidates, 1)
				faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.Equal(t, []string{a.PersonID}, faces[0].RejectedPersonIDs)
				require.NoError(t, s.ValidateMetadata(ctx))
				return
			}
			var hiddenFaceID string
			if mode == "bounded" {
				for range 20 {
					extra, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, a.PersonID)
					require.NoError(t, err)
					decideFaceForTest(t, s, n, extra.FaceID, b.PersonID, "reject")
				}
				hidden := browsePhotoNode(t, s, "hidden-portrait.jpg", fakeHash("b2"), "image/jpeg")
				face, _, err := s.CreatePhotoFace(ctx, hidden.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, a.PersonID)
				require.NoError(t, err)
				decideFaceForTest(t, s, hidden, face.FaceID, b.PersonID, "reject")
				asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
				require.NoError(t, err)
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				hiddenFaceID = face.FaceID
			}
			asset, err := s.PhotoAssetForNode(ctx, n.ID)
			require.NoError(t, err)
			switch mode {
			case "reverse":
				a, b = b, a
			case "hidden-only":
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				hiddenFaceID = f.FaceID
			case "excluded":
				asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			case "trashed":
				asset, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
			case "retained":
				_, _, err = s.ReplaceContent(ctx, n.ID, n.Revision, fakeHash("b2"), 20, "image/jpeg")
			}
			require.NoError(t, err)
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
			var conflict *PersonFaceMergeConflictError
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, hiddenFaceID != "", conflict.HiddenConflict)
			if hiddenFaceID != "" {
				require.NotContains(t, conflict.FaceIDs, hiddenFaceID)
				require.NotContains(t, err.Error(), hiddenFaceID)
			}
			switch mode {
			case "hidden-only":
				require.Empty(t, conflict.FaceIDs)
			case "bounded":
				require.Len(t, conflict.FaceIDs, 20)
				require.True(t, conflict.More)
			case "excluded", "trashed":
				require.True(t, conflict.InaccessibleConflict)
				require.Empty(t, conflict.FaceIDs)
				require.NotContains(t, err.Error(), f.FaceID)
				if mode == "excluded" {
					_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false)
				} else {
					node, e := s.NodeByID(ctx, n.ID)
					require.NoError(t, e)
					_, _, err = s.Restore(ctx, node.ID, node.Revision)
				}
				require.NoError(t, err)
				_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
				require.ErrorAs(t, err, &conflict)
				require.Equal(t, []string{f.FaceID}, conflict.FaceIDs)
			default:
				require.Equal(t, []string{f.FaceID}, conflict.FaceIDs)
			}
			if mode == "excluded" || mode == "trashed" || mode == "retained" {
				if mode == "retained" {
					detail, err := s.PersonDetail(ctx, a.PersonID)
					require.NoError(t, err)
					require.Len(t, detail.Assertions, 1)
				}
				receipt := decideFaceForTest(t, s, n, f.FaceID, b.PersonID, "withdraw_rejection")
				faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.NotEmpty(t, faces[0].AssertionID)
				require.Empty(t, faces[0].RejectedPersonIDs)
				_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
				require.NoError(t, err)
				if mode == "retained" {
					_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
					require.ErrorIs(t, err, ErrStaleRevision)
				}
			}
		})
	}
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
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		original, e := personCandidateByIDTx(ctx, tx, c.CandidateID)
		require.NoError(t, e)
		require.Equal(t, c.Evidence, original.Evidence)
		require.Equal(t, c.EvidenceSHA256, original.EvidenceSHA256)
		require.Equal(t, "superseded", original.State)
		replay, kept, e := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: c.ActorKey, DisplayName: c.DisplayName, SuggestedPersonID: c.SuggestedPersonID, Reason: c.Reason, Evidence: c.Evidence})
		require.NoError(t, e)
		require.True(t, kept)
		require.Equal(t, c.CandidateID, replay.CandidateID)
		require.Equal(t, "superseded", replay.State)
		return nil
	}))
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &backup))
	var memberships int
	require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_candidate_faces`).Scan(&memberships))
	require.Equal(t, 3, memberships)
	restoredFaces, _, e := restored.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, e)
	for _, f := range restoredFaces {
		if f.FaceID == other.FaceID {
			require.Equal(t, []string{p.PersonID}, f.RejectedPersonIDs)
		}
	}
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	for _, f := range faces {
		if f.FaceID == face.FaceID {
			require.Empty(t, f.RejectedPersonIDs)
		} else {
			require.Equal(t, []string{p.PersonID}, f.RejectedPersonIDs)
		}
	}
	duplicate, e := restored.CreatePerson(ctx, "Duplicate", "operator")
	require.NoError(t, e)
	_, e = restored.MergePersons(ctx, duplicate.PersonID, p.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, e)
	require.NoError(t, restored.ValidateMetadata(ctx))
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
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, typed))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, typed))
	page, err = s.ListPhotoAssets(WithPhotoHiddenToken(ctx, token), PhotoBrowseRequest{Query: typed, Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
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

func personKeyFaceForTest(t *testing.T, s *Store, id string) string {
	t.Helper()
	var key string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COALESCE(key_face_id,'') FROM persons WHERE person_id=?`, id).Scan(&key))
	return key
}

func TestPhotoFaceDecisionReplayAfterDismiss(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
	_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, f.FaceID, f.Box, p.PersonID)
	require.ErrorIs(t, err, ErrExists)
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Equal(t, int64(2), faces[0].Revision)
}
func TestPhotoFaceDecisionWithdrawalMerge(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"undo", "suppress"} {
		t.Run(action, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			b, err := s.CreatePerson(ctx, "Duplicate", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, "")
			r := decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "name")
			if action == "undo" {
				_, err = s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
				require.NoError(t, err)
			} else {
				as, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				as[0].Action = "suppress"
				_, err = s.AssertDocumentPerson(ctx, as[0])
				require.NoError(t, err)
			}
			if action != "suppress" {
				as, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.Equal(t, "withdrawn", as[0].Action)
				require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+a.PersonID)))
			}
			_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, b.PersonID)
			require.NoError(t, err)
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
			if action == "suppress" {
				require.ErrorIs(t, err, ErrPersonMergeConflict)
			} else {
				require.NoError(t, err)
				require.NoError(t, s.ValidateMetadata(ctx))
				decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "name")
				require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+a.PersonID)))
			}
		})
	}
}
func TestPhotoFaceDecisionQueueFullRejection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "author", EvidenceKind: "content_version", EvidenceID: n.CurrentVersionID}})
	require.NoError(t, err)
	candidate, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "name_alias:queued", DisplayName: "Queued", Reason: "name_only", Evidence: raw})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<10000) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at) SELECT printf('00000000-0000-4000-8000-%012d',i),printf('name_alias:queued-%d',i),'Queued','name_only',?,?,1,1,'open',? FROM slots`, candidate.Evidence, candidate.EvidenceSHA256, nowRFC3339())
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Empty(t, faces[0].RejectedPersonIDs)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&count))
	require.Equal(t, 10000, count)
}

func TestPhotoFaceDecisionUndoSharedAssertion(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"first with key", "second with key", "first then second", "second then first", "earlier retraction", "explicit withdrawal"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, first := faceForDecisionTest(t, s, "")
			second, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
			require.NoError(t, err)
			a := decideFaceForTest(t, s, n, first.FaceID, p.PersonID, "name")
			b := decideFaceForTest(t, s, n, second.FaceID, p.PersonID, "name")
			remaining := second
			if mode == "second with key" || mode == "second then first" {
				remaining, a, b = first, b, a
			}
			switch mode {
			case "first with key", "second with key":
				_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: remaining.FaceID, PersonID: p.PersonID, Action: "key", Revision: 2, PersonRevision: p.Revision})
				require.NoError(t, err)
				_, err = s.UndoPhotoFaceDecision(ctx, a.ReceiptID)
				require.NoError(t, err)
				require.Equal(t, remaining.FaceID, personKeyFaceForTest(t, s, p.PersonID))
			case "first then second", "second then first":
				_, err = s.UndoPhotoFaceDecision(ctx, a.ReceiptID)
				require.NoError(t, err)
				lastUndo, err := s.UndoPhotoFaceDecision(ctx, b.ReceiptID)
				require.NoError(t, err)
				faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				for _, face := range faces {
					require.Empty(t, face.AssertionID)
				}
				require.Empty(t, assertions)
				require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
				require.NoError(t, s.ValidateMetadata(ctx))
				_, err = s.UndoPhotoFaceDecision(ctx, lastUndo.ReceiptID)
				require.NoError(t, err)
			case "explicit withdrawal":
				r := decideFaceForTest(t, s, n, first.FaceID, "", "retract")
				assertions, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				assertion := assertions[0]
				assertion.Action = "withdrawn"
				_, err = s.AssertDocumentPerson(ctx, assertion)
				require.NoError(t, err)
				var before, after bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &before))
				_, err = s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
				require.ErrorIs(t, err, ErrStaleRevision)
				require.NoError(t, s.ExportMetadata(ctx, &after))
				require.Equal(t, before.String(), after.String())
				require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
				return
			case "earlier retraction":
				r := decideFaceForTest(t, s, n, first.FaceID, "", "retract")
				decideFaceForTest(t, s, n, second.FaceID, "", "retract")
				remaining = first
				undo, err := s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
				require.NoError(t, err)
				require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
				faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.Len(t, assertions, 1)
				for _, face := range faces {
					if face.FaceID == remaining.FaceID {
						require.Equal(t, assertions[0].AssertionID, face.AssertionID)
					} else {
						require.Empty(t, face.AssertionID)
					}
				}
				_, err = s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
				require.NoError(t, err)
				require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
				require.NoError(t, s.ValidateMetadata(ctx))
				return
			}
			faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, assertions, 1)
			for _, face := range faces {
				if face.FaceID == remaining.FaceID {
					require.Equal(t, assertions[0].AssertionID, face.AssertionID)
				} else {
					require.Empty(t, face.AssertionID)
				}
			}
			require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionRetiredCorrections(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, p.PersonID)
	decideFaceForTest(t, s, n, f.FaceID, "", "retract")
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	_, err = s.RetirePerson(ctx, p.PersonID, p.Revision)
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].RejectedPersonIDs)
	// A separate untouched retraction receipt still fences restoration by person state.
	other, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n2 := browsePhotoNode(t, s, "second.jpg", fakeHash("b2"), "image/jpeg")
	f2, _, err := s.CreatePhotoFace(ctx, n2.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, other.PersonID)
	require.NoError(t, err)
	r := decideFaceForTest(t, s, n2, f2.FaceID, "", "retract")
	_, err = s.RetirePerson(ctx, other.PersonID, other.Revision)
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoFaceDecisionKeepsPhotoMark(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	mark, err := s.MarkPersonInPhoto(ctx, n.CurrentVersionID, p.PersonID)
	require.NoError(t, err)
	require.True(t, mark.PhotoMark)
	named := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
	_, err = s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
	retracted := decideFaceForTest(t, s, n, f.FaceID, "", "retract")
	faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces[0].AssertionID)
	require.Len(t, assertions, 1)
	require.True(t, assertions[0].PhotoMark)
	require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &backup))
	_, assertions, err = restored.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, assertions[0].PhotoMark)
	_, err = restored.UndoPhotoFaceDecision(ctx, retracted.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, restored.ValidateMetadata(ctx))
	mark = assertions[0]
	mark.Action = "withdrawn"
	mark.PhotoMark = false
	_, err = s.AssertDocumentPerson(ctx, mark)
	require.NoError(t, err)
	require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
}

func TestPhotoFaceDecisionMergeKeepsPhotoMark(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, a.PersonID)
	other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, b.PersonID)
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, n.CurrentVersionID, b.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, "", "retract")
	decideFaceForTest(t, s, n, other.FaceID, "", "retract")
	_, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.True(t, assertions[0].PhotoMark)
	require.Equal(t, a.PersonID, assertions[0].PersonID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceWithdrawnAssertionsLeaveCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<1000) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) SELECT printf('00000000-0000-4000-8000-%012d',i),'Synthetic person','synthetic person','operator','curated',1,?,? FROM slots`, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) SELECT person_id,?,person_id,'depicted','withdrawn','',?,1 FROM persons WHERE person_id<>?`, n.CurrentVersionID, nowRFC3339(), p.PersonID)
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
	_, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestQueryPersonPhotoLiveSibling(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, nodes := photoTrashFixture(t, s)
	jpeg, _, err := s.ReplaceContent(ctx, nodes[1].ID, nodes[1].Revision, fakeHash("b2"), 20, "image/jpeg")
	require.NoError(t, err)
	nodes[1] = jpeg
	image := fileByRole(asset.Files, PhotoRoleImage)
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &image.ID)
	require.NoError(t, err)
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, nodes[0].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, p.PersonID)
	require.NoError(t, err)
	filter := `{"filters":{"person_ids":["` + p.PersonID + `"]}}`
	require.Len(t, browsePhotoPage(t, s, filter).Items, 1)
	_, _, err = s.Trash(ctx, nodes[0].ID, nodes[0].Revision)
	require.NoError(t, err)
	require.Empty(t, browsePhotoPage(t, s, filter).Items)
	_, _, err = s.CreatePhotoFace(ctx, nodes[1].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, p.PersonID)
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, filter).Items, 1)
}

func TestPhotoFaceDecisionOperationReplay(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	request := PhotoFaceDecisionRequest{OperationID: uuid.NewString(), FaceID: f.FaceID, PersonID: p.PersonID, Action: "name", Revision: 1}
	first, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, request)
	require.NoError(t, err)
	replayed, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, request)
	require.NoError(t, err)
	require.Equal(t, first.ReceiptID, replayed.ReceiptID)
	require.Equal(t, first.RequestSHA256, replayed.RequestSHA256)
	request.Action = "reject"
	_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, request)
	require.ErrorIs(t, err, ErrExists)
	operationID := uuid.NewString()
	undo, err := s.UndoPhotoFaceDecisionWithOperationID(ctx, first.ReceiptID, operationID)
	require.NoError(t, err)
	replayed, err = s.UndoPhotoFaceDecisionWithOperationID(ctx, first.ReceiptID, operationID)
	require.NoError(t, err)
	require.Equal(t, undo.ReceiptID, replayed.ReceiptID)
	require.Equal(t, undo.After.Faces, replayed.After.Faces)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPersonSplitMissingAssertion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	_, err = s.SplitPerson(t.Context(), PersonSplitRequest{PersonID: p.PersonID, OperationID: uuid.NewString(), DisplayName: "Grace", Revision: p.Revision, AssertionIDs: []string{uuid.NewString()}})
	require.ErrorIs(t, err, ErrPersonMergeConflict)
}

func TestPhotoFaceDecisionRejectionBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	require.NoError(t, err)
	c, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "face:" + f.FaceID, DisplayName: "Face", SuggestedPersonID: p.PersonID, Reason: "face_match", Evidence: raw})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<1000) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) SELECT printf('00000000-0000-4000-8000-%012d',i),'Synthetic person','synthetic person','operator','curated',1,?,? FROM slots`, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at,decided_at) SELECT person_id,?,'Face',person_id,'face_match',?,?,1,1,'rejected',?,? FROM persons WHERE person_id<>?`, c.ActorKey, c.Evidence, c.EvidenceSHA256, nowRFC3339(), nowRFC3339(), p.PersonID)
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces[0].RejectedPersonIDs, 1000)
	_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, PersonID: p.PersonID, Action: "reject", Revision: f.Revision})
	require.ErrorIs(t, err, ErrPhotoFaceRejectionLimit)
	faces, _, err = s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, f.Revision, faces[0].Revision)
	require.Len(t, faces[0].RejectedPersonIDs, 1000)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	require.NoError(t, newTestStore(t).ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	now := nowRFC3339()
	extra, err := json.Marshal(metadataPersonMatchCandidate{Type: metadataPersonCandidateType, CandidateID: uuid.NewString(), ActorKey: c.ActorKey, DisplayName: c.DisplayName, SuggestedPersonID: &p.PersonID, Reason: c.Reason, EvidenceJSON: c.Evidence, EvidenceSHA256: c.EvidenceSHA256, OccurrenceCount: 1, Revision: 1, State: "rejected", CreatedAt: now, DecidedAt: &now})
	require.NoError(t, err)
	backup.Write(extra)
	backup.WriteByte('\n')
	require.ErrorIs(t, newTestStore(t).ImportMetadata(ctx, &backup), ErrInvalidPhotoFace)
	var open PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var e error
		open, _, e = s.OpenPersonCandidate(ctx, tx, c)
		return e
	}))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: open.CandidateID, ExpectedRevision: open.Revision, Action: "reject"})
	require.ErrorIs(t, err, ErrPhotoFaceRejectionLimit)
}

func TestPhotoFaceCandidateAcceptanceClearsDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	candidate := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	candidate.Reason = "face_match"
	// A grouped suggestion coexists with the single-face rejection.
	var occurrences []PersonCandidateOccurrence
	require.NoError(t, json.Unmarshal(candidate.Evidence, &occurrences))
	other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	occurrences = append(occurrences, PersonCandidateOccurrence{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID})
	candidate.Evidence, err = json.Marshal(occurrences)
	require.NoError(t, err)
	c := openCandidateForTest(t, s, candidate)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "link", PersonID: p.PersonID})
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	for _, face := range faces {
		require.NotEmpty(t, face.AssertionID)
		require.False(t, face.Dismissed)
		require.Empty(t, face.RejectedPersonIDs)
	}
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionWithdrawalPreservesOpenSuggestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	require.NoError(t, err)
	var c PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var e error
		c, _, e = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + f.FaceID, DisplayName: "Ada", SuggestedPersonID: p.PersonID, Reason: "face_match", Evidence: raw})
		return e
	}))
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	var state string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT state FROM person_match_candidates WHERE candidate_id=?`, c.CandidateID).Scan(&state))
	require.Equal(t, "open", state)
}
func TestPhotoFaceMergeFencesMovedFace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Duplicate", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, b.PersonID)
	_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, Action: "retract", Revision: f.Revision})
	require.ErrorIs(t, err, ErrStaleRevision)
}
func TestPhotoFaceKeyReplacementWithLockedOldPhoto(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	old, f := faceForDecisionTest(t, s, p.PersonID)
	_, err = s.DecidePhotoFace(ctx, old.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, PersonID: p.PersonID, Action: "key", Revision: 1, PersonRevision: 1})
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, old.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	n := browsePhotoNode(t, s, "second.jpg", fakeHash("b2"), "image/jpeg")
	next, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, p.PersonID)
	require.NoError(t, err)
	receipt, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: next.FaceID, PersonID: p.PersonID, Action: "key", Revision: 1, PersonRevision: 2})
	require.NoError(t, err)
	require.Equal(t, next.FaceID, personKeyFaceForTest(t, s, p.PersonID))
	detail, err := s.PersonDetail(ctx, p.PersonID)
	require.NoError(t, err)
	require.Equal(t, next.FaceID, detail.KeyFaceID)
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, f.FaceID, personKeyFaceForTest(t, s, p.PersonID))
	detail, err = s.PersonDetail(ctx, p.PersonID)
	require.NoError(t, err)
	require.Empty(t, detail.KeyFaceID)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, &backup))
	require.Equal(t, f.FaceID, personKeyFaceForTest(t, restored, p.PersonID))
	require.NoError(t, restored.ValidateMetadata(ctx))
}
func TestPersonAssertionSelectorsFilterBeforePaging(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, version := seedPeopleVersion(t, s)
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<1001) INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind) SELECT printf('10000000-0000-4000-8000-%012d',i),node_id,blob_hash,size,mime_type,recorded_at,i+1,printf('30000000-0000-4000-8000-%012d',i),'content_replace' FROM slots JOIN content_versions v ON v.version_id=?`, version)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) SELECT replace(version_id,'10000000-','20000000-'),version_id,?,'depicted','withdrawn','',?,1 FROM content_versions WHERE node_id=? AND version_id<>?`, p.PersonID, nowRFC3339(), node, version)
	require.NoError(t, err)
	first := "f0000000-0000-4000-8000-000000000001"
	second := "f0000000-0000-4000-8000-000000000002"
	for i, id := range []string{first, second} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{AssertionID: id, ContentVersionID: version, PersonID: p.PersonID, Role: []string{"author", "recipient"}[i], Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	page, err := s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Assertions, 1)
	require.Equal(t, first, page.Assertions[0].AssertionID)
	require.Equal(t, first, page.AssertionsNextCursor)
	page, err = s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{After: page.AssertionsNextCursor, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Assertions, 1)
	require.Equal(t, second, page.Assertions[0].AssertionID)
	require.Empty(t, page.AssertionsNextCursor)
}

func TestPhotoFaceCandidateReadsAndDecisionsRequireHiddenAccess(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	require.NoError(t, err)
	var candidate PersonMatchCandidate
	require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var e error
		candidate, _, e = s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + f.FaceID, DisplayName: "Ada", SuggestedPersonID: p.PersonID, Reason: "face_match", Evidence: raw})
		return e
	}))
	asset, err := s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	candidates, total, err := s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Empty(t, candidates)
	require.Zero(t, total)
	count, _, err := s.OpenPersonCandidateCount(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: candidate.Revision, Action: "reject"})
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	ctx = WithPhotoHiddenToken(ctx, token)
	candidates, total, err = s.PersonCandidates(ctx, "open", 10, 0)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.EqualValues(t, 1, total)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: candidate.Revision, Action: "reject"})
	require.NoError(t, err)
}

func seedFaceAssertionSlots(t *testing.T, s *Store, version string, count int) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<?) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) SELECT printf('00000000-0000-4000-9000-%012d',i),'Synthetic person','synthetic person','operator','curated',1,?,? FROM slots`, count, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) SELECT person_id,?,person_id,'depicted','assert','',?,1 FROM persons WHERE person_id LIKE '00000000-0000-4000-9000-%'`, version, nowRFC3339())
	require.NoError(t, err)
}

func TestPhotoFaceDecisionSuppressionSurvivesDetachAndUndo(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "last face", true: "shared assertion"}[shared], func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, p.PersonID)
			if shared {
				_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, p.PersonID)
				require.NoError(t, err)
			}
			rows, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			rows[0].Action = "suppress"
			_, err = s.AssertDocumentPerson(ctx, rows[0])
			require.NoError(t, err)
			receipt := decideFaceForTest(t, s, n, f.FaceID, "", "retract")
			rows, err = s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, "suppress", rows[0].Action)
			undo, err := s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, "suppress", assertions[0].Action)
			for _, face := range faces {
				require.NotEmpty(t, face.AssertionID)
			}
			redone, err := s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
			require.NoError(t, err)
			rows, err = s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			rows[0].Action = "assert"
			_, err = s.AssertDocumentPerson(ctx, rows[0])
			require.NoError(t, err)
			_, err = s.UndoPhotoFaceDecision(ctx, redone.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionUndoRenameAtAssertionCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, a.PersonID)
	seedFaceAssertionSlots(t, s, n.CurrentVersionID, 999)
	receipt := decideFaceForTest(t, s, n, f.FaceID, b.PersonID, "name")
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1000)
	require.Equal(t, f.AssertionID, faces[0].AssertionID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceReferencedWithdrawalCountsAtCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, _ := faceForDecisionTest(t, s, p.PersonID)
	seedFaceAssertionSlots(t, s, n.CurrentVersionID, 999)
	rows, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	var withdrawn PersonDocumentAssertion
	for _, a := range rows {
		if a.PersonID == p.PersonID {
			withdrawn = a
		}
	}
	withdrawn.Action = "withdrawn"
	withdrawn, err = s.AssertDocumentPerson(ctx, withdrawn)
	require.NoError(t, err)
	other, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, n.CurrentVersionID, other.PersonID)
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
	withdrawn.Action = "assert"
	_, err = s.AssertDocumentPerson(ctx, withdrawn)
	require.NoError(t, err)
	_, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1000)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionRetiredGroupedRejection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	candidate := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}, {ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID}})
	candidate.Reason = "face_match"
	c := openCandidateForTest(t, s, candidate)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "reject"})
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, p.PersonID, p.Revision)
	require.NoError(t, err)
	receipt := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	for _, face := range faces {
		if face.FaceID == f.FaceID {
			require.Empty(t, face.RejectedPersonIDs)
		} else {
			require.Equal(t, []string{p.PersonID}, face.RejectedPersonIDs)
		}
	}
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionRejectPreservesDismissal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p, err := s.CreatePerson(t.Context(), "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
	receipt := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	faces, _, err := s.PhotoFaces(t.Context(), n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Equal(t, []string{p.PersonID}, faces[0].RejectedPersonIDs)
	_, err = s.UndoPhotoFaceDecision(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(t.Context(), n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Empty(t, faces[0].RejectedPersonIDs)
}

func TestPhotoFaceDecisionUndoRestoresLinkedCandidate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	candidate := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	candidate.Reason = "face_match"
	c := openCandidateForTest(t, s, candidate)
	linked, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "link", PersonID: p.PersonID})
	require.NoError(t, err)
	receipt := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	rows, total, err := s.PersonCandidates(ctx, "linked", 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, linked.DecidedPersonID, rows[0].DecidedPersonID)
	require.Equal(t, linked.DecidedAt, rows[0].DecidedAt)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionUndoRespectsCandidateCapacity(t *testing.T) {
	t.Parallel()
	for _, action := range []string{faceDecisionName, "dismiss"} {
		t.Run(action, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, "")
			candidate := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
			candidate.Reason = "face_match"
			openCandidateForTest(t, s, candidate)
			personID := p.PersonID
			if action == "dismiss" {
				personID = ""
			}
			receipt := decideFaceForTest(t, s, n, f.FaceID, personID, action)
			raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "author", EvidenceKind: "content_version", EvidenceID: n.CurrentVersionID}})
			require.NoError(t, err)
			queued, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "name_alias:queued", DisplayName: "Queued", Reason: "name_only", Evidence: raw})
			require.NoError(t, err)
			_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<10000) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at) SELECT printf('00000000-0000-4000-8000-%012d',i),printf('name_alias:queued-%d',i),'Queued','name_only',?,?,1,1,'open',? FROM slots`, queued.Evidence, queued.EvidenceSHA256, nowRFC3339())
			require.NoError(t, err)
			_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.ErrorIs(t, err, ErrInvalidPhotoFace)
			count, full, err := s.OpenPersonCandidateCount(ctx)
			require.NoError(t, err)
			require.True(t, full)
			require.EqualValues(t, 10000, count)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionSharedWithdrawalReactivation(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, first := faceForDecisionTest(t, s, p.PersonID)
			personID := ""
			if existing {
				personID = p.PersonID
			}
			other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, personID)
			require.NoError(t, err)
			assertions, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			assertion := assertions[0]
			assertion.Action = "withdrawn"
			_, err = s.AssertDocumentPerson(ctx, assertion)
			require.NoError(t, err)
			receipt := decideFaceForTest(t, s, n, other.FaceID, p.PersonID, "name")
			require.Len(t, receipt.Before.Faces, 2)
			require.Len(t, receipt.After.Faces, 2)
			for i, face := range receipt.Before.Faces {
				require.Greater(t, receipt.After.Faces[i].Revision, face.Revision)
			}
			_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: first.FaceID, Action: "retract", Revision: first.Revision})
			require.ErrorIs(t, err, ErrStaleRevision)
			undo, err := s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			assertions, err = s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Equal(t, "withdrawn", assertions[0].Action)
			faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			for _, face := range faces {
				if face.FaceID == first.FaceID {
					require.Equal(t, first.AssertionID, face.AssertionID)
				}
				if face.FaceID == other.FaceID {
					require.Equal(t, other.AssertionID, face.AssertionID)
				}
			}
			typed := snapshotTestQuery(t, `{}`)
			typed.Filters.PersonIDs = []string{p.PersonID}
			require.Empty(t, personMatchNodeIDs(t, s, typed))
			redo, err := s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
			require.NoError(t, err)
			require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, typed))
			decideFaceForTest(t, s, n, first.FaceID, "", "retract")
			_, err = s.UndoPhotoFaceDecision(ctx, redo.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionUndoRenamePreservesSharedWithdrawal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	grace, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n, first := faceForDecisionTest(t, s, ada.PersonID)
	_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, ada.PersonID)
	require.NoError(t, err)
	assertions, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	assertion := assertions[0]
	assertion.Action = "withdrawn"
	_, err = s.AssertDocumentPerson(ctx, assertion)
	require.NoError(t, err)
	receipt := decideFaceForTest(t, s, n, first.FaceID, grace.PersonID, "name")
	undo, err := s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	assertions, err = s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	for _, assertion := range assertions {
		require.Equal(t, "withdrawn", assertion.Action)
	}
	typed := snapshotTestQuery(t, `{}`)
	typed.Filters.PersonIDs = []string{ada.PersonID}
	require.Empty(t, personMatchNodeIDs(t, s, typed))
	typed.Filters.PersonIDs = []string{grace.PersonID}
	require.Empty(t, personMatchNodeIDs(t, s, typed))
	redo, err := s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, typed))
	_, err = s.UndoPhotoFaceDecision(ctx, redo.ReceiptID)
	require.NoError(t, err)
	require.Empty(t, personMatchNodeIDs(t, s, typed))
	typed.Filters.PersonIDs = []string{ada.PersonID}
	require.Empty(t, personMatchNodeIDs(t, s, typed))
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionRepeatedGroupedDismissal(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"", "linked", "rejected"} {
		t.Run("remainder="+terminal, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, first := faceForDecisionTest(t, s, "")
			other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
			require.NoError(t, err)
			remainder := candidateForTest(t, "face:"+other.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID}})
			remainder.Reason = "face_match"
			if terminal != "" {
				c := openCandidateForTest(t, s, remainder)
				decision := CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "reject"}
				if terminal == "linked" {
					decision.Action = "link"
					decision.PersonID = p.PersonID
				}
				_, err = s.DecidePersonCandidate(ctx, decision)
				require.NoError(t, err)
			}
			group := candidateForTest(t, "face:"+first.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: first.FaceID}, {ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID}})
			group.Reason = "face_match"
			openCandidateForTest(t, s, group)
			if terminal == "" {
				raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "author", EvidenceKind: "content_version", EvidenceID: n.CurrentVersionID}})
				require.NoError(t, err)
				queued, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "name_alias:queued", DisplayName: "Queued", Reason: "name_only", Evidence: raw})
				require.NoError(t, err)
				_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<9999) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at) SELECT printf('00000000-0000-4000-8000-%012d',i),printf('name_alias:queued-%d',i),'Queued','name_only',?,?,1,1,'open',? FROM slots`, queued.Evidence, queued.EvidenceSHA256, nowRFC3339())
				require.NoError(t, err)
			}
			for range 2 {
				receipt := decideFaceForTest(t, s, n, first.FaceID, "", "dismiss")
				var retained PersonMatchCandidate
				require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
					normalized, err := normalizePersonCandidate(remainder)
					if err != nil {
						return err
					}
					retained, err = personCandidateByIdentityTx(ctx, tx, normalized)
					return err
				}))
				expected := terminal
				if expected == "" {
					expected = "open"
				}
				require.Equal(t, expected, retained.State)
				normalized, err := normalizePersonCandidate(remainder)
				require.NoError(t, err)
				require.Equal(t, normalized.Evidence, retained.Evidence)
				require.Equal(t, normalized.EvidenceSHA256, retained.EvidenceSHA256)
				if terminal == "" {
					count, full, err := s.OpenPersonCandidateCount(ctx)
					require.NoError(t, err)
					require.True(t, full)
					require.EqualValues(t, 10000, count)
				}
				_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
				require.NoError(t, err)
			}
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceCandidateRejectAssignedPersonIsAtomic(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, first := faceForDecisionTest(t, s, "")
	assigned, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .4, Width: .2, Height: .2}, p.PersonID)
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .7, Width: .2, Height: .2}, p.PersonID)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: assigned.FaceID, PersonID: p.PersonID, Action: "key", Revision: assigned.Revision, PersonRevision: p.Revision})
	require.NoError(t, err)
	candidate := candidateForTest(t, "face:"+first.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: first.FaceID}, {ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: assigned.FaceID}})
	candidate.Reason = "face_match"
	c := openCandidateForTest(t, s, candidate)
	var before, after bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "reject"})
	require.ErrorIs(t, err, ErrPersonIdentityConflict)
	require.NoError(t, s.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	require.Equal(t, assigned.FaceID, personKeyFaceForTest(t, s, p.PersonID))
	require.Equal(t, []int64{n.ID}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
}
