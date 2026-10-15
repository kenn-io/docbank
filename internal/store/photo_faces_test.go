package store

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoFaceManualAndInvalidZeroAreaBox(t *testing.T) {
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

func TestPhotoFaceMergeRetargetsDuplicateAssertions(t *testing.T) {
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
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: 0.5, Width: 0.2, Height: 0.2}, duplicate.PersonID)
	require.NoError(t, err)
	require.NotEqual(t, first.AssertionID, second.AssertionID)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, ada.PersonID, duplicate.PersonID, operation, 1, 1)
	require.NoError(t, err)
	faces, assertions, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	for _, face := range faces {
		require.Equal(t, first.AssertionID, face.AssertionID)
	}
	require.Len(t, assertions, 1)
	require.Equal(t, ada.PersonID, assertions[0].PersonID)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoFaceReplaysNamedFaceAfterPersonRetires(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	faceID := uuid.New().String()
	face, assertion, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, box, person.PersonID)
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, person.PersonID, person.Revision)
	require.NoError(t, err)
	replayed, saved, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, box, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, face, replayed)
	require.Equal(t, assertion, saved)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
	require.ErrorIs(t, err, ErrNotFound)
	unnamed, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, "")
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, unnamed.FaceID, box, person.PersonID)
	require.ErrorIs(t, err, ErrExists)
}

func TestPhotoFaceReplaysMergedPersonAfterSurvivorRetires(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	absorbed, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	middle, err := s.CreatePerson(ctx, "Ada middle", "operator")
	require.NoError(t, err)
	survivor, err := s.CreatePerson(ctx, "Ada survivor", "operator")
	require.NoError(t, err)
	stranger, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	faceID := uuid.New().String()
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, box, absorbed.PersonID)
	require.NoError(t, err)
	for _, merge := range []struct {
		survivorID, absorbedID string
		absorbedRevision       int64
	}{{middle.PersonID, absorbed.PersonID, 1}, {survivor.PersonID, middle.PersonID, 2}} {
		operation, err := newUUIDv4()
		require.NoError(t, err)
		_, err = s.MergePersons(ctx, merge.survivorID, merge.absorbedID, operation, 1, merge.absorbedRevision)
		require.NoError(t, err)
	}
	current, _, err := s.PersonByID(ctx, survivor.PersonID)
	require.NoError(t, err)
	_, err = s.RetirePerson(ctx, survivor.PersonID, current.Revision)
	require.NoError(t, err)
	replayed, saved, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, box, absorbed.PersonID)
	require.NoError(t, err)
	require.Equal(t, face.FaceID, replayed.FaceID)
	require.Equal(t, survivor.PersonID, saved.PersonID)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, box, stranger.PersonID)
	require.ErrorIs(t, err, ErrExists)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, absorbed.PersonID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoFaceHiddenReadsAndWrites(t *testing.T) {
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
	_, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, face.FaceID, face.Box, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	faces, _, err := s.PhotoFaces(unlocked, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, faces)
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
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NoError(t, s.ValidateMetadata(ctx))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	faces, _, err := target.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, faces)
	var roundtrip bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &roundtrip))
	require.Equal(t, exported.String(), roundtrip.String())
	for _, field := range []string{"content_version_id", "assertion_id"} {
		t.Run(field, func(t *testing.T) {
			lines := strings.Split(exported.String(), "\n")
			for i, line := range lines {
				if strings.Contains(line, `"type":"photo_face"`) {
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
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_faces`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM person_document_assertions`).Scan(&count))
	require.Zero(t, count)
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
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&epoch))
	repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	require.NoError(t, s.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	var unchanged int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&unchanged))
	require.Equal(t, epoch, unchanged)
	require.NoError(t, s.ValidateMetadata(ctx))
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
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
	id, err := newUUIDv4()
	require.NoError(t, err)
	row["face_id"] = id
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	bounded.Write(raw)
	bounded.WriteByte('\n')
	require.ErrorIs(t, newTestStore(t).ImportMetadata(ctx, strings.NewReader(bounded.String())), ErrInvalidPhotoFace)
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
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{
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
	face.Revision++
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
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, overflow.PersonID)
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
	overflowAssertion := PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: overflow.PersonID, Role: "depicted", Action: "assert", Revision: 1}
	_, err = s.AssertDocumentPerson(ctx, overflowAssertion)
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
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
