package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// buildFaceMatchCandidateForTest inserts an open face_match candidate linked
// to face via the person_candidate_faces trigger, padded with extra occurrence
// entries so its evidence (and so the receipt that touches it) can be grown to
// a controlled size.
func buildFaceMatchCandidateForTest(t *testing.T, s *Store, versionID, faceID string, slot, padOccurrences int) {
	t.Helper()
	occ := []PersonCandidateOccurrence{{ContentVersionID: versionID, Role: "depicted", EvidenceKind: "face", EvidenceID: faceID}}
	for j := range padOccurrences {
		occ = append(occ, PersonCandidateOccurrence{ContentVersionID: versionID, Role: "depicted", EvidenceKind: "face", EvidenceID: fmt.Sprintf("00000000-0000-4000-9001-%03d%09d", slot, j)})
	}
	raw, err := json.Marshal(occ)
	require.NoError(t, err)
	normalized, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "face:" + faceID, DisplayName: "Face", Reason: personCandidateReasonFaceMatch, Evidence: raw})
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at,decided_at
	) VALUES(printf('00000000-0000-4000-9000-%012d', ?), ?, ?, NULL, 'face_match', ?, ?, ?, 1, 'open', ?, NULL)`,
		slot, normalized.ActorKey, normalized.DisplayName, normalized.Evidence, normalized.EvidenceSHA256, normalized.OccurrenceCount, nowRFC3339())
	require.NoError(t, err)
}

// TestPhotoFaceDecisionDismissReservesUndoBudget covers a dismiss whose own
// receipt fits the 1 MiB limit by a handful of bytes, but whose undo -- which
// adds undo_of and advances every touched revision -- would not have. Before
// reserving that headroom, the dismiss above would have been accepted and
// then stuck: no undo could ever be written for it.
func TestPhotoFaceDecisionDismissReservesUndoBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, face := faceForDecisionTest(t, s, "")
	for i := range 12 {
		buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, i, 100)
	}
	buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, 12, 25)
	_, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: face.Revision})
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	require.NoError(t, s.ValidateMetadata(ctx))
}

// TestPhotoFaceDecisionDismissNearLimitUndoes covers the same shape of
// dismiss sized so its reserved budget (and so its eventual undo) still
// fits, confirming the reservation in TestPhotoFaceDecisionDismissReservesUndoBudget
// does not block a large dismiss that is genuinely undoable.
func TestPhotoFaceDecisionDismissNearLimitUndoes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, face := faceForDecisionTest(t, s, "")
	for i := range 12 {
		buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, i, 100)
	}
	buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, 12, 15)
	receipt, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: face.Revision})
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

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
	decideFaceForTest(t, s, node, face.FaceID, "", "dismiss")
	receipt := decideFaceForTest(t, s, node, face.FaceID, ada.PersonID, "reject")
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Equal(t, []string{ada.PersonID}, faces[0].RejectedPersonIDs)
	_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	require.Empty(t, faces[0].RejectedPersonIDs)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceDecisionKeyFaceAndSplit(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"split", "locked old key"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, p.PersonID)
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, PersonID: p.PersonID, Action: "key", Revision: face.Revision, PersonRevision: p.Revision})
			require.NoError(t, err)
			if mode == "locked old key" {
				asset, err := s.PhotoAssetForNode(ctx, node.ID)
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
				detail, err := s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{})
				require.NoError(t, err)
				require.Equal(t, next.FaceID, detail.KeyFaceID)
				_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
				require.NoError(t, err)
				require.Equal(t, face.FaceID, personKeyFaceForTest(t, s, p.PersonID))
				detail, err = s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{})
				require.NoError(t, err)
				require.Empty(t, detail.KeyFaceID)
				var backup bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &backup))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(ctx, &backup))
				require.Equal(t, face.FaceID, personKeyFaceForTest(t, restored, p.PersonID))
				require.NoError(t, restored.ValidateMetadata(ctx))
				return
			}
			detail, err := s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{})
			require.NoError(t, err)
			require.Equal(t, face.FaceID, detail.KeyFaceID)
			require.Len(t, detail.Assertions, 1)
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", PersonRevision: 1})
			require.ErrorIs(t, err, ErrStaleRevision)
			cleared, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", PersonRevision: 2})
			require.NoError(t, err)
			require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
			restored, err := s.UndoPhotoFaceDecision(ctx, cleared.ReceiptID)
			require.NoError(t, err)
			detail, err = s.PersonDetailPage(ctx, p.PersonID, PersonAssertionPage{})
			require.NoError(t, err)
			split, err := s.SplitPerson(ctx, PersonSplitRequest{PersonID: p.PersonID, OperationID: uuid.NewString(), DisplayName: "Grace", Revision: detail.Revision, AssertionIDs: []string{detail.Assertions[0].AssertionID}})
			require.NoError(t, err)
			require.Equal(t, []string{face.AssertionID}, split.MovedAssertionIDs)
			require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
			require.Equal(t, face.FaceID, personKeyFaceForTest(t, s, split.NewPersonID))
			moved, err := s.PersonDetailPage(ctx, split.NewPersonID, PersonAssertionPage{})
			require.NoError(t, err)
			require.Len(t, moved.Assertions, 1)
			require.Equal(t, split.NewPersonID, moved.Assertions[0].PersonID)
			_, err = s.UndoPhotoFaceDecision(ctx, restored.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionUndoFencesChangedRecords(t *testing.T) {
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
	for _, test := range []struct {
		name                                                               string
		reverse, hidden, excluded, trashed, retained, bounded, reactivated bool
		visible                                                            int
		hiddenConflict, inaccessible, more                                 bool
	}{
		{name: "forward", visible: 1},
		{name: "reverse", reverse: true, visible: 1},
		{name: "hidden-only", hidden: true, hiddenConflict: true},
		{name: "excluded", excluded: true, inaccessible: true},
		{name: "trashed", trashed: true, inaccessible: true},
		{name: "retained", retained: true, visible: 1},
		{name: "bounded", bounded: true, visible: 20, hiddenConflict: true, more: true},
		{name: "reactivated", reactivated: true, visible: 1},
		{name: "reactivated reverse", reactivated: true, reverse: true, visible: 1},
		{name: "reactivated hidden", reactivated: true, hidden: true, hiddenConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			b, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, a.PersonID)
			decideFaceForTest(t, s, n, f.FaceID, b.PersonID, "reject")
			rejectedPerson := b.PersonID
			if test.reactivated {
				assertions, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				assertion := assertions[0]
				assertion.Action = "withdrawn"
				_, err = s.AssertDocumentPerson(ctx, assertion)
				require.NoError(t, err)
				_, err = s.MarkPersonInPhoto(ctx, n.CurrentVersionID, b.PersonID)
				require.NoError(t, err)
			}
			var hiddenFaceID string
			if test.bounded {
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
			switch {
			case test.reverse:
				a, b = b, a
			case test.hidden:
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				hiddenFaceID = f.FaceID
			case test.excluded:
				asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			case test.trashed:
				asset, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
			case test.retained:
				_, _, err = s.ReplaceContent(ctx, n.ID, n.Revision, fakeHash("b2"), 20, "image/jpeg")
			}
			require.NoError(t, err)
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
			var conflict *PersonFaceMergeConflictError
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, test.hiddenConflict, conflict.HiddenConflict)
			if hiddenFaceID != "" {
				require.NotContains(t, conflict.FaceIDs, hiddenFaceID)
				require.NotContains(t, err.Error(), hiddenFaceID)
			}
			require.Len(t, conflict.FaceIDs, test.visible)
			require.Equal(t, test.more, conflict.More)
			require.Equal(t, test.inaccessible, conflict.InaccessibleConflict)
			if test.visible == 1 {
				require.Equal(t, []string{f.FaceID}, conflict.FaceIDs)
			}
			if test.inaccessible {
				require.NotContains(t, err.Error(), f.FaceID)
				if test.excluded {
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
			}

			if test.reactivated {
				if test.hidden {
					token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
					require.NoError(t, err)
					ctx = WithPhotoHiddenToken(ctx, token)
				}
				faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, PersonID: rejectedPerson, Action: "withdraw_rejection", Revision: faces[0].Revision})
				require.NoError(t, err)
				_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
				require.NoError(t, err)
				faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.Len(t, assertions, 1)
				require.Equal(t, "assert", assertions[0].Action)
				require.Equal(t, assertions[0].AssertionID, faces[0].AssertionID)
				require.Empty(t, faces[0].RejectedPersonIDs)
				require.NoError(t, s.ValidateMetadata(ctx))
			}
			if test.excluded || test.trashed || test.retained {
				if test.retained {
					detail, err := s.PersonDetailPage(ctx, a.PersonID, PersonAssertionPage{})
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
				if test.retained {
					_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
					require.ErrorIs(t, err, ErrStaleRevision)
				}
			}
		})
	}
}

func TestPhotoFaceDecisionGroupedRejection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, action string
		retired      bool
	}{
		{name: "name", action: "name"}, {name: "withdraw retired rejection", action: "withdraw_rejection", retired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, "")
			other, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
			require.NoError(t, err)
			c := candidateForTest(t, "face:"+face.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}, {ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: other.FaceID}})
			c.Reason = personCandidateReasonFaceMatch
			c = openCandidateForTest(t, s, c)
			_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: c.CandidateID, ExpectedRevision: c.Revision, Action: "reject"})
			require.NoError(t, err)
			if test.retired {
				_, err = s.RetirePerson(ctx, p.PersonID, p.Revision)
				require.NoError(t, err)
			}
			receipt := decideFaceForTest(t, s, node, face.FaceID, p.PersonID, test.action)
			if !test.retired {
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
			_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			for _, f := range faces {
				require.Equal(t, []string{p.PersonID}, f.RejectedPersonIDs)
			}
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoFaceDecisionUndoRejectsReusedPreviousKeyFace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	previousNode, previousFace := faceForDecisionTest(t, s, person.PersonID)
	_, err = s.DecidePhotoFace(ctx, previousNode.CurrentVersionID, PhotoFaceDecisionRequest{
		FaceID: previousFace.FaceID, PersonID: person.PersonID, Action: "key",
		Revision: previousFace.Revision, PersonRevision: person.Revision,
	})
	require.NoError(t, err)

	currentNode := browsePhotoNode(t, s, "second.jpg", fakeHash("c1"), "image/jpeg")
	currentFace, _, err := s.CreatePhotoFace(ctx, currentNode.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, person.PersonID)
	require.NoError(t, err)
	personDetail, err := s.PersonDetailPage(ctx, person.PersonID, PersonAssertionPage{})
	require.NoError(t, err)
	changed, err := s.DecidePhotoFace(ctx, currentNode.CurrentVersionID, PhotoFaceDecisionRequest{
		FaceID: currentFace.FaceID, PersonID: person.PersonID, Action: "key",
		Revision: currentFace.Revision, PersonRevision: personDetail.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, previousNode.CurrentVersionID, changed.Before.People[0].KeyFaceContentVersionID)
	require.Equal(t, currentNode.CurrentVersionID, changed.After.People[0].KeyFaceContentVersionID)

	updated, replacementVersion, err := s.ReplaceContent(ctx, previousNode.ID, previousNode.Revision, fakeHash("b2"), 20, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, previousNode.ID, updated.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, replacementVersion.ID, previousFace.FaceID, previousFace.Box, person.PersonID)
	require.NoError(t, err)

	_, err = s.UndoPhotoFaceDecision(ctx, changed.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	require.Equal(t, currentFace.FaceID, personKeyFaceForTest(t, s, person.PersonID))
	require.NoError(t, s.ValidateMetadata(ctx))
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

func TestPhotoFaceDecisionWithdrawalMerge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Duplicate", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	r := decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "name")
	_, err = s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
	require.NoError(t, err)
	assertions, err := s.DocumentPersonAssertions(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "withdrawn", assertions[0].Action)
	_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, b.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "name")
}

func TestPhotoFaceDecisionUndoSharedAssertion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                             string
		reverse, key, undoBoth, explicitWithdrawal, roundTrip, moveAsset bool
	}{
		{name: "first with key", key: true},
		{name: "second with key", reverse: true, key: true},
		{name: "first then second", undoBoth: true},
		{name: "second then first", reverse: true, undoBoth: true},
		{name: "earlier retraction", roundTrip: true},
		{name: "earlier retraction after asset move", roundTrip: true, moveAsset: true},
		{name: "explicit withdrawal", explicitWithdrawal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			if test.reverse {
				remaining, a, b = first, b, a
			}
			switch {
			case test.key:
				_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: remaining.FaceID, PersonID: p.PersonID, Action: "key", Revision: 2, PersonRevision: p.Revision})
				require.NoError(t, err)
				_, err = s.UndoPhotoFaceDecision(ctx, a.ReceiptID)
				require.NoError(t, err)
				require.Equal(t, remaining.FaceID, personKeyFaceForTest(t, s, p.PersonID))
			case test.undoBoth:
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
				require.NoError(t, s.ValidateMetadata(ctx))
				_, err = s.UndoPhotoFaceDecision(ctx, lastUndo.ReceiptID)
				require.NoError(t, err)
			case test.explicitWithdrawal:
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
				return
			case test.roundTrip:
				const note = "Ada \"quoted\"\n雪"
				_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET note=? WHERE assertion_id=?`, note, a.After.Faces[0].AssertionID)
				require.NoError(t, err)
				r := decideFaceForTest(t, s, n, first.FaceID, "", "retract")
				decideFaceForTest(t, s, n, second.FaceID, "", "retract")
				var backup bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &backup))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(ctx, &backup))
				s = restored
				var assetID string
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT asset_id FROM photo_change_receipts WHERE receipt_id=?`, r.ReceiptID).Scan(&assetID))
				asset, err := s.PhotoAssetForNode(ctx, n.ID)
				require.NoError(t, err)
				require.Equal(t, asset.ID, assetID)
				if test.moveAsset {
					targetNode := browsePhotoNode(t, s, "other-portrait.jpg", fakeHash("a2"), "image/jpeg")
					targetAsset, err := s.PhotoAssetForNode(ctx, targetNode.ID)
					require.NoError(t, err)
					_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
					require.NoError(t, err)
					_, err = s.AttachPhotoFile(ctx, targetAsset.ID, targetAsset.Revision, n.ID, PhotoRoleImage, nil)
					require.NoError(t, err)
				}
				remaining = first
				undo, err := s.UndoPhotoFaceDecision(ctx, r.ReceiptID)
				require.NoError(t, err)
				faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
				require.NoError(t, err)
				require.Len(t, assertions, 1)
				require.Equal(t, note, assertions[0].Note)
				for _, face := range faces {
					if face.FaceID == remaining.FaceID {
						require.Equal(t, assertions[0].AssertionID, face.AssertionID)
					} else {
						require.Empty(t, face.AssertionID)
					}
				}
				_, err = s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
				require.NoError(t, err)
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
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
	checkSuppression := func(shared bool) func(*testing.T, *Store, Person) {
		return func(t *testing.T, s *Store, p Person) {
			t.Helper()
			ctx := t.Context()
			n, f := faceForDecisionTest(t, s, p.PersonID)
			if shared {
				_, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, p.PersonID)
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
		}
	}

	checkReactivation := func(existing bool) func(*testing.T, *Store, Person) {
		return func(t *testing.T, s *Store, p Person) {
			t.Helper()
			ctx := t.Context()
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
			redo, err := s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
			require.NoError(t, err)
			decideFaceForTest(t, s, n, first.FaceID, "", "retract")
			_, err = s.UndoPhotoFaceDecision(ctx, redo.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ValidateMetadata(ctx))
		}
	}

	for _, test := range []struct {
		name string
		run  func(*testing.T, *Store, Person)
	}{
		{"suppression/false", checkSuppression(false)},
		{"suppression/true", checkSuppression(true)},
		{"reactivation/false", checkReactivation(false)},
		{"reactivation/true", checkReactivation(true)},
		{"rename withdrawn", func(t *testing.T, s *Store, p Person) {
			t.Helper()
			ctx := t.Context()
			grace, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			n, first := faceForDecisionTest(t, s, p.PersonID)
			_, _, err = s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, p.PersonID)
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
			redo, err := s.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
			require.NoError(t, err)
			_, active, err := s.PhotoFaces(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, active, 2)
			for _, assertion := range active {
				if assertion.PersonID == grace.PersonID {
					require.Equal(t, "assert", assertion.Action)
				} else {
					require.Equal(t, "withdrawn", assertion.Action)
				}
			}
			_, err = s.UndoPhotoFaceDecision(ctx, redo.ReceiptID)
			require.NoError(t, err)
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
		{"repeated naming with key", func(t *testing.T, s *Store, p Person) {
			t.Helper()
			ctx := t.Context()
			n, first := faceForDecisionTest(t, s, "")
			named := decideFaceForTest(t, s, n, first.FaceID, p.PersonID, "name")
			_, err := s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
			require.NoError(t, err)
			repeated := decideFaceForTest(t, s, n, first.FaceID, p.PersonID, "name")
			other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
			require.NoError(t, err)
			otherNaming := decideFaceForTest(t, s, n, other.FaceID, p.PersonID, "name")
			_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: other.FaceID, PersonID: p.PersonID, Action: "key", Revision: otherNaming.After.Faces[0].Revision, PersonRevision: p.Revision})
			require.NoError(t, err)
			var before, after bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &before))
			_, err = s.UndoPhotoFaceDecision(ctx, repeated.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ExportMetadata(ctx, &after))
			require.Equal(t, before.String(), after.String())
			require.Equal(t, other.FaceID, personKeyFaceForTest(t, s, p.PersonID))
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
		{"retraction after split", func(t *testing.T, s *Store, p Person) {
			t.Helper()
			ctx := t.Context()
			n, first := faceForDecisionTest(t, s, p.PersonID)
			other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, p.PersonID)
			require.NoError(t, err)
			retracted := decideFaceForTest(t, s, n, first.FaceID, "", "retract")
			split, err := s.SplitPerson(ctx, PersonSplitRequest{PersonID: p.PersonID, OperationID: uuid.NewString(), DisplayName: "Grace", Revision: p.Revision, AssertionIDs: []string{other.AssertionID}})
			require.NoError(t, err)
			var before, after bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &before))
			_, err = s.UndoPhotoFaceDecision(ctx, retracted.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.NoError(t, s.ExportMetadata(ctx, &after))
			require.Equal(t, before.String(), after.String())
			detail, err := s.PersonDetailPage(ctx, split.NewPersonID, PersonAssertionPage{})
			require.NoError(t, err)
			require.Len(t, detail.Assertions, 1)
			require.Equal(t, split.NewPersonID, detail.Assertions[0].PersonID)
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			p, err := s.CreatePerson(t.Context(), "Ada", "operator")
			require.NoError(t, err)
			test.run(t, s, p)
		})
	}
}

func TestPhotoFaceDecisionUndoAfterRetirement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
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
	for _, mode := range []string{"name", "merge"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			personID := ""
			if mode == "merge" {
				personID = p.PersonID
			}
			n, f := faceForDecisionTest(t, s, personID)
			if mode == "merge" {
				b, err := s.CreatePerson(ctx, "Grace", "operator")
				require.NoError(t, err)
				other, _, err := s.CreatePhotoFace(ctx, n.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, b.PersonID)
				require.NoError(t, err)
				_, err = s.MarkPersonInPhoto(ctx, n.CurrentVersionID, b.PersonID)
				require.NoError(t, err)
				_, err = s.MergePersons(ctx, p.PersonID, b.PersonID, uuid.NewString(), 1, 1)
				require.NoError(t, err)
				decideFaceForTest(t, s, n, other.FaceID, "", "retract")
			} else {
				mark, err := s.MarkPersonInPhoto(ctx, n.CurrentVersionID, p.PersonID)
				require.NoError(t, err)
				require.True(t, mark.PhotoMark)
				named := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
				_, err = s.UndoPhotoFaceDecision(ctx, named.ReceiptID)
				require.NoError(t, err)
				decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
			}
			retracted := decideFaceForTest(t, s, n, f.FaceID, "", "retract")
			faces, assertions, err := s.PhotoFaces(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			for _, face := range faces {
				require.Empty(t, face.AssertionID)
			}
			require.Len(t, assertions, 1)
			require.True(t, assertions[0].PhotoMark)
			require.Equal(t, p.PersonID, assertions[0].PersonID)
			if mode == "name" {
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
			} else {
				require.NoError(t, s.ValidateMetadata(ctx))
			}
		})
	}
}

func TestPhotoFaceDecisionOperationReplay(t *testing.T) {
	t.Parallel()
	for _, newName := range []bool{false, true} {
		s := newTestStore(t)
		ctx := t.Context()
		p, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		n, f := faceForDecisionTest(t, s, "")
		request := PhotoFaceDecisionRequest{OperationID: uuid.NewString(), FaceID: f.FaceID, PersonID: p.PersonID, Action: "name", Revision: 1}
		var epoch, afterEpoch int64
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&epoch))
		if newName {
			for _, invalid := range []PhotoFaceDecisionRequest{
				{Action: "name", FaceID: f.FaceID, PersonID: p.PersonID, PersonName: "Grace", Revision: 1},
				{Action: "name", FaceID: f.FaceID, PersonName: " ", Revision: 1},
				{Action: "retract", FaceID: f.FaceID, PersonName: "Grace", Revision: 1},
			} {
				_, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, invalid)
				require.ErrorIs(t, err, ErrInvalidPhotoFace)
			}
			request.PersonID = ""
			request.PersonName = "Grace"
		}
		first, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, request)
		require.NoError(t, err)
		if !newName {
			legacyDigest, err := requestDigest([]any{n.CurrentVersionID, map[string]any{
				"operation_id": request.OperationID, "face_id": f.FaceID, "person_id": p.PersonID, "action": "name", "revision": 1,
			}})
			require.NoError(t, err)
			require.Equal(t, legacyDigest, first.RequestSHA256)
		}
		replayed, err := s.DecidePhotoFace(ctx, n.CurrentVersionID, request)
		require.NoError(t, err)
		require.Equal(t, first.ReceiptID, replayed.ReceiptID)
		require.Equal(t, first.RequestSHA256, replayed.RequestSHA256)
		require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&afterEpoch))
		if newName {
			require.Equal(t, epoch+1, afterEpoch)
			require.Equal(t, first.Before.People, first.After.People)
			stale := request
			stale.OperationID = uuid.NewString()
			_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, stale)
			require.ErrorIs(t, err, ErrStaleRevision)
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM persons WHERE display_name='Grace'`).Scan(&count))
			require.Equal(t, 1, count)
		}
		request.PersonName = ""
		request.PersonID = p.PersonID
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
		if newName {
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM persons WHERE display_name='Grace'`).Scan(&count))
			require.Equal(t, 1, count)
			require.Empty(t, undo.After.Faces[0].AssertionID)
		}
		require.NoError(t, s.ValidateMetadata(ctx))
	}
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
	updated, current, err := s.ReplaceContent(ctx, n.ID, n.Revision, fakeHash("b2"), 20, "image/jpeg")
	require.NoError(t, err)
	surviving, _, err := s.CreatePhotoFace(ctx, current.ID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	grouped := candidateForTest(t, c.ActorKey, "Face", p.PersonID, []PersonCandidateOccurrence{
		{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID},
		{ContentVersionID: current.ID, Role: "depicted", EvidenceKind: "face", EvidenceID: surviving.FaceID},
	})
	grouped.Reason = personCandidateReasonFaceMatch
	grouped = openCandidateForTest(t, s, grouped)
	_, err = s.PruneContentVersions(ctx, n.ID, updated.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	decided, err := s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: grouped.CandidateID, ExpectedRevision: grouped.Revision, Action: "reject"})
	require.NoError(t, err)
	require.Equal(t, personCandidateStateRejected, decided.State)
	faces, _, err = s.PhotoFaces(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, []string{p.PersonID}, faces[0].RejectedPersonIDs)
	backup.Reset()
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	require.NoError(t, newTestStore(t).ImportMetadata(ctx, &backup))
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
	c := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	c.Reason = personCandidateReasonFaceMatch
	c = openCandidateForTest(t, s, c)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	var state string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT state FROM person_match_candidates WHERE candidate_id=?`, c.CandidateID).Scan(&state))
	require.Equal(t, "open", state)
}

func TestPersonAssertionSelectorsFilterBeforePaging(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	doc := browsePhotoNode(t, s, "selectors.txt", fakeHash("ac"), "text/plain")
	version := doc.CurrentVersionID
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	for group := range 3 {
		photo := browsePhotoNode(t, s, "selector-"+strconv.Itoa(group)+".jpg", fmt.Sprintf("%064x", group+10000), "image/jpeg")
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{AssertionID: fmt.Sprintf("20000000-0000-4000-8000-%012d", group+1), ContentVersionID: photo.CurrentVersionID, PersonID: p.PersonID, Role: "depicted", Action: "assert", Revision: 1})
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, photo.ID)
		require.NoError(t, err)
		switch group {
		case 0:
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
		case 1:
			_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
		case 2:
			_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
		}
		require.NoError(t, err)
	}
	require.NoError(t, s.ValidateMetadata(ctx))
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
	candidate := candidateForTest(t, "face:"+f.FaceID, "Ada", p.PersonID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	candidate.Reason = personCandidateReasonFaceMatch
	candidate = openCandidateForTest(t, s, candidate)
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

func TestPhotoFaceDecisionCandidateCapacity(t *testing.T) {
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
			fillFaceCandidateQueue(t, s, n.CurrentVersionID, 10000)
			_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.ErrorIs(t, err, ErrInvalidPhotoFace)
			count, full, err := s.OpenPersonCandidateCount(ctx)
			require.NoError(t, err)
			require.True(t, full)
			require.EqualValues(t, 10000, count)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
	t.Run("terminal decisions", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		p, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		n, f := faceForDecisionTest(t, s, "")
		fillFaceCandidateQueue(t, s, n.CurrentVersionID, 10000)
		decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
		decideFaceForTest(t, s, n, f.FaceID, "", "dismiss")
		decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
		faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
		require.NoError(t, err)
		require.True(t, faces[0].Dismissed)
		require.Empty(t, faces[0].RejectedPersonIDs)
	})
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
				fillFaceCandidateQueue(t, s, n.CurrentVersionID, 9999)
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
}

func TestPersonMergeBumpsAffectedFaceOnce(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		moved bool
	}{
		{name: "assignment and candidate"}, {name: "moved assignment", moved: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			b, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			personID := a.PersonID
			if test.moved {
				personID = b.PersonID
			}
			node, face := faceForDecisionTest(t, s, personID)
			unrelated, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
			require.NoError(t, err)
			if !test.moved {
				candidate := candidateForTest(t, "face:"+face.FaceID, b.DisplayName, b.PersonID, []PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}})
				candidate.Reason = personCandidateReasonFaceMatch
				openCandidateForTest(t, s, candidate)
			}
			_, err = s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), a.Revision, b.Revision)
			require.NoError(t, err)
			var revision int64
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT revision FROM photo_faces WHERE face_id=?`, face.FaceID).Scan(&revision))
			require.Equal(t, face.Revision+1, revision)
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT revision FROM photo_faces WHERE face_id=?`, unrelated.FaceID).Scan(&revision))
			require.Equal(t, unrelated.Revision, revision)
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "retract", Revision: face.Revision})
			require.ErrorIs(t, err, ErrStaleRevision)
		})
	}
}

func fillFaceCandidateQueue(t *testing.T, s *Store, version string, count int) {
	t.Helper()
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: version, Role: "author", EvidenceKind: "content_version", EvidenceID: version}})
	require.NoError(t, err)
	queued, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "name_alias:queued", DisplayName: "Queued", Reason: "name_only", Evidence: raw})
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<?) INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at) SELECT printf('00000000-0000-4000-8000-%012d',i),printf('name_alias:queued-%d',i),'Queued','name_only',?,?,1,1,'open',? FROM slots`, count, queued.Evidence, queued.EvidenceSHA256, nowRFC3339())
	require.NoError(t, err)
}

func TestPhotoFaceMergeUnionsRejections(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	b, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, n, f.FaceID, b.PersonID, "reject")

	decideFaceForTest(t, s, n, f.FaceID, a.PersonID, "reject")
	moved, err := s.MergePersons(ctx, a.PersonID, b.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.Len(t, moved.Moved.FaceCandidates, 1)
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []string{a.PersonID}, faces[0].RejectedPersonIDs)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceCandidateHistoryDoesNotFollowReusedFaceID(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"dismiss", "reject"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			oldPerson, err := s.CreatePerson(ctx, "Old suggestion", "operator")
			require.NoError(t, err)
			newPerson, err := s.CreatePerson(ctx, "New name", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, "")
			personID := ""
			if action == "reject" {
				personID = oldPerson.PersonID
			}
			decision := decideFaceForTest(t, s, node, face.FaceID, personID, action)
			require.Len(t, decision.After.Candidates, 1)
			oldCandidate := decision.After.Candidates[0]

			updated, current, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 20, "image/jpeg")
			require.NoError(t, err)
			_, err = s.PruneContentVersions(ctx, node.ID, updated.Revision, VersionPruneSelector{AllPrior: true}, true)
			require.NoError(t, err)
			_, _, err = s.CreatePhotoFace(ctx, current.ID, face.FaceID, face.Box, "")
			require.NoError(t, err)
			faces, _, err := s.PhotoFaces(ctx, current.ID)
			require.NoError(t, err)
			require.Len(t, faces, 1)
			require.False(t, faces[0].Dismissed)
			require.Empty(t, faces[0].RejectedPersonIDs)

			decideFaceForTest(t, s, updated, face.FaceID, newPerson.PersonID, "name")
			var afterCandidate PersonMatchCandidate
			require.NoError(t, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
				var err error
				afterCandidate, err = personCandidateByIDTx(ctx, tx, oldCandidate.CandidateID)
				return err
			}))
			require.Equal(t, oldCandidate.State, afterCandidate.State)
			require.Equal(t, oldCandidate.Revision, afterCandidate.Revision)
			require.Equal(t, oldCandidate.Evidence, afterCandidate.Evidence)
		})
	}
}

func TestFaceCandidateRetainsMembershipForReusedFaceID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	// This missing UUID sorts first, like a pruned version during candidate normalization.
	const staleVersionID = "00000000-0000-4000-8000-000000000000"
	require.NotEqual(t, staleVersionID, node.CurrentVersionID)

	candidate := candidateForTest(t, "face:"+face.FaceID, "Face", person.PersonID, []PersonCandidateOccurrence{
		{ContentVersionID: staleVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID},
		{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID},
	})
	candidate.Reason = personCandidateReasonFaceMatch
	candidate = openCandidateForTest(t, s, candidate)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: candidate.CandidateID, ExpectedRevision: candidate.Revision, Action: "reject"})
	require.NoError(t, err)

	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []string{person.PersonID}, faces[0].RejectedPersonIDs)
}

func TestPersonMergeIgnoresCandidateFromPrunedFaceVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	node, face := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, node, face.FaceID, absorbed.PersonID, "reject")
	updated, current, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 20, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, node.ID, updated.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, current.ID, face.FaceID, face.Box, survivor.PersonID)
	require.NoError(t, err)
	survivor, _, err = s.PersonByID(ctx, survivor.PersonID)
	require.NoError(t, err)
	absorbed, _, err = s.PersonByID(ctx, absorbed.PersonID)
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.NewString(), survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	faces, _, err := s.PhotoFaces(ctx, current.ID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	require.Empty(t, faces[0].RejectedPersonIDs)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceMergeKeepsSurvivorSuggestion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	absorbed, err := s.CreatePerson(ctx, "Ada duplicate", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	occurrence := []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}}
	var suggestions []PersonMatchCandidate
	for _, p := range []Person{survivor, absorbed} {
		c := candidateForTest(t, "face:"+f.FaceID, "Face", p.PersonID, occurrence)
		c.Reason = personCandidateReasonFaceMatch
		suggestions = append(suggestions, openCandidateForTest(t, s, c))
	}
	_, err = s.MergePersons(ctx, survivor.PersonID, absorbed.PersonID, uuid.NewString(), survivor.Revision, absorbed.Revision)
	require.NoError(t, err)
	open, _, err := s.PersonCandidates(ctx, "open", 250, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, suggestions[0].CandidateID, open[0].CandidateID)
	require.Equal(t, survivor.PersonID, open[0].SuggestedPersonID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceNamingSupersedesOtherSuggestions(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"name", "link"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			named, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			other, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, "")
			suggest := func(personID string) PersonMatchCandidate {
				c := candidateForTest(t, "face:"+f.FaceID, "Face", personID, []PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
				c.Reason = personCandidateReasonFaceMatch
				return openCandidateForTest(t, s, c)
			}
			stale := suggest(other.PersonID)
			var receipt PhotoFaceDecisionReceipt
			if decision == "name" {
				receipt = decideFaceForTest(t, s, n, f.FaceID, named.PersonID, "name")
			} else {
				chosen := suggest(named.PersonID)
				_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: chosen.CandidateID, ExpectedRevision: chosen.Revision, Action: "link", PersonID: named.PersonID})
				require.NoError(t, err)
			}
			open, _, err := s.PersonCandidates(ctx, "open", 250, 0)
			require.NoError(t, err)
			require.Empty(t, open)
			require.NoError(t, s.ValidateMetadata(ctx))
			if decision == "name" {
				_, err = s.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
				require.NoError(t, err)
				open, _, err = s.PersonCandidates(ctx, "open", 250, 0)
				require.NoError(t, err)
				require.Len(t, open, 1)
				require.Equal(t, stale.CandidateID, open[0].CandidateID)
			}
		})
	}
}

func TestPhotoFaceWithdrawalUndoFencedByLaterRejection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	q, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, "")
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: n.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}})
	require.NoError(t, err)
	c, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "face:" + f.FaceID, DisplayName: "Face", SuggestedPersonID: p.PersonID, Reason: "face_match", Evidence: raw})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<999) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) SELECT printf('00000000-0000-4000-8000-%012d',i),'Synthetic person','synthetic person','operator','curated',1,?,? FROM slots`, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,created_at,decided_at) SELECT person_id,?,'Face',person_id,'face_match',?,?,1,1,'rejected',?,? FROM persons WHERE person_id NOT IN (?,?)`, c.ActorKey, c.Evidence, c.EvidenceSHA256, nowRFC3339(), nowRFC3339(), p.PersonID, q.PersonID)
	require.NoError(t, err)
	decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "reject")
	withdrawn := decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "withdraw_rejection")
	decideFaceForTest(t, s, n, f.FaceID, q.PersonID, "reject")
	_, err = s.UndoPhotoFaceDecision(ctx, withdrawn.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	faces, _, err := s.PhotoFaces(ctx, n.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces[0].RejectedPersonIDs, 1000)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceClearedKeyUndoIgnoresRequestPhoto(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	n, f := faceForDecisionTest(t, s, p.PersonID)
	_, err = s.DecidePhotoFace(ctx, n.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: f.FaceID, PersonID: p.PersonID, Action: "key", Revision: f.Revision, PersonRevision: p.Revision})
	require.NoError(t, err)
	unrelated := browsePhotoNode(t, s, "unrelated.jpg", fakeHash("c1"), "image/jpeg")
	cleared, err := s.DecidePhotoFace(ctx, unrelated.CurrentVersionID, PhotoFaceDecisionRequest{PersonID: p.PersonID, Action: "key", PersonRevision: p.Revision + 1})
	require.NoError(t, err)
	require.Empty(t, personKeyFaceForTest(t, s, p.PersonID))
	_, _, err = s.Trash(ctx, unrelated.ID, unrelated.Revision)
	require.NoError(t, err)
	_, err = s.UndoPhotoFaceDecision(ctx, cleared.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, f.FaceID, personKeyFaceForTest(t, s, p.PersonID))
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoWholeMarkClearReceipt(t *testing.T) {
	t.Parallel()
	for _, named := range []bool{false, true} {
		t.Run(strconv.FormatBool(named), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			node, face := faceForDecisionTest(t, s, "")
			if named {
				_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Revision: 1, Action: "name", PersonID: person.PersonID})
				require.NoError(t, err)
			}
			mark, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
			require.NoError(t, err)
			request := PhotoFaceDecisionRequest{AssertionID: mark.AssertionID, Action: "retract", Revision: mark.Revision - 1}
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, request)
			require.ErrorIs(t, err, ErrStaleRevision)
			request.Revision = mark.Revision
			request.OperationID = uuid.NewString()
			receipt, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, request)
			require.NoError(t, err)
			replay, err := s.DecidePhotoFace(ctx, node.CurrentVersionID, request)
			require.NoError(t, err)
			require.Equal(t, receipt.ReceiptID, replay.ReceiptID)
			require.Equal(t, receipt.After.Assertions, replay.After.Assertions)
			request.OperationID = ""
			_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, request)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.False(t, receipt.After.Assertions[0].PhotoMark)
			if named {
				require.Equal(t, "assert", receipt.After.Assertions[0].Action)
			} else {
				require.Equal(t, "withdrawn", receipt.After.Assertions[0].Action)
			}
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, &backup))
			undo, err := restored.UndoPhotoFaceDecision(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			faces, assertions, err := restored.PhotoFaces(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.True(t, assertions[0].PhotoMark)
			if named {
				require.Equal(t, mark.AssertionID, faces[0].AssertionID)
			}
			_, err = restored.UndoPhotoFaceDecision(ctx, undo.ReceiptID)
			require.NoError(t, err)
			require.NoError(t, restored.ValidateMetadata(ctx))
		})
	}
}
