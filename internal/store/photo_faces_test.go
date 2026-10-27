package store

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPersonFacePreviews(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"dated", "tied", "projection"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			var nodes []Node
			var assets []PhotoAsset
			var faces []PhotoFace
			for i := range 6 {
				node := browsePhotoNode(t, s, fmt.Sprintf("portrait-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", i+1)), "image/jpeg")
				stamp := fmt.Sprintf("2026-01-%02dT12:00:00Z", i+1)
				if mode == "tied" && i < 4 {
					stamp = "2026-02-01T12:00:00Z"
				}
				browsePhotoMetadata(t, s, node, fmt.Sprintf("preview-%d", i), exifCapture(stamp))
				var face PhotoFace
				if mode == "projection" {
					_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
				} else {
					face, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: 0.2, Height: 0.2}, person.PersonID)
				}
				require.NoError(t, err)
				asset, err := s.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				nodes, assets, faces = append(nodes, node), append(assets, asset), append(faces, face)
			}
			if mode == "projection" {
				box := document.FaceBox{X: .1, Y: .2, Width: .4, Height: .4}
				_, _, err = s.CreatePhotoFace(ctx, nodes[5].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .1, Height: .1}, person.PersonID)
				require.NoError(t, err)
				_, _, err = s.CreatePhotoFace(ctx, nodes[5].CurrentVersionID, uuid.NewString(), box, person.PersonID)
				require.NoError(t, err)
				recipe := visualPreviewRecipe()
				recipe.MaxEdgePixels = 512
				generation, err := s.PublishVisualPreviewGeneration(ctx, nodes[5].CurrentVersionID, readyVisualPreviewWithRecipe(t, nodes[5].BlobHash, fakeHash("b1"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
				require.NoError(t, err)
				_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{assets[5].ID}}), Patch: PhotoAuthoredPatch{Rotation: new(90)}})
				require.NoError(t, err)
				rows, err := s.PersonFacePreviews(ctx, person.PersonID, generation.RecipeFingerprint, false)
				require.NoError(t, err)
				require.Equal(t, []PersonFacePreview{
					{AssetID: assets[5].ID, ContentVersionID: nodes[5].CurrentVersionID, GenerationID: generation.GenerationID, Rotation: 90, Box: &box},
					{AssetID: assets[4].ID, ContentVersionID: nodes[4].CurrentVersionID},
					{AssetID: assets[3].ID, ContentVersionID: nodes[3].CurrentVersionID},
				}, rows.Items)
				return
			}
			read := func() []PersonFacePreview {
				t.Helper()
				rows, err := s.PersonFacePreviews(ctx, person.PersonID, "grid-recipe", false)
				require.NoError(t, err)
				return rows.Items
			}
			rows := read()
			require.Len(t, rows, 3)
			if mode == "tied" {
				ids := []string{assets[0].ID, assets[1].ID, assets[2].ID, assets[3].ID}
				slices.Sort(ids)
				require.Equal(t, []string{ids[0], ids[1], ids[2]}, []string{rows[0].AssetID, rows[1].AssetID, rows[2].AssetID})
				return
			}
			require.Equal(t, []string{assets[5].ID, assets[4].ID, assets[3].ID}, []string{rows[0].AssetID, rows[1].AssetID, rows[2].AssetID})
			_, err = s.DecidePhotoFace(ctx, nodes[3].CurrentVersionID, PhotoFaceDecisionRequest{Action: "dismiss", FaceID: faces[3].FaceID, Revision: 1})
			require.NoError(t, err)
			rows = read()
			require.Equal(t, []string{assets[5].ID, assets[4].ID, assets[2].ID}, []string{rows[0].AssetID, rows[1].AssetID, rows[2].AssetID})
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			_, err = s.SetPhotoAssetHidden(ctx, assets[2].ID, assets[2].Revision, true)
			require.NoError(t, err)
			rows = read()
			require.Equal(t, []string{assets[5].ID, assets[4].ID, assets[1].ID}, []string{rows[0].AssetID, rows[1].AssetID, rows[2].AssetID})
			_, err = s.PersonFacePreviews(ctx, person.PersonID, "grid-recipe", true)
			require.ErrorIs(t, err, ErrHiddenLocked)
			token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			page, err := s.PersonFacePreviews(WithPhotoHiddenToken(ctx, token), person.PersonID, "grid-recipe", true)
			require.NoError(t, err)
			require.Equal(t, []PersonFacePreview{{AssetID: assets[2].ID, ContentVersionID: nodes[2].CurrentVersionID, Box: &faces[2].Box}}, page.Items)
		})
	}
	t.Run("older boxed asset", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		person, err := s.CreatePerson(ctx, "Ada", "operator")
		require.NoError(t, err)
		box := document.FaceBox{X: .1, Y: .2, Width: .3, Height: .4}
		var previews []PersonFacePreview
		for i := range 4 {
			node := browsePhotoNode(t, s, fmt.Sprintf("portrait-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", i+1)), "image/jpeg")
			browsePhotoMetadata(t, s, node, fmt.Sprintf("preview-%d", i), exifCapture(fmt.Sprintf("2026-01-%02dT12:00:00Z", i+1)))
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			preview := PersonFacePreview{AssetID: asset.ID, ContentVersionID: node.CurrentVersionID}
			if i == 0 {
				_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), box, person.PersonID)
				preview.Box = &box
			} else {
				_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
			}
			require.NoError(t, err)
			previews = append(previews, preview)
		}
		page, err := s.PersonFacePreviews(ctx, person.PersonID, "grid-recipe", false)
		require.NoError(t, err)
		require.Equal(t, []PersonFacePreview{previews[3], previews[2], previews[0]}, page.Items)
	})
}

func TestPersonFacePreviewsPreferAssignedSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, members := photoTrashFixture(t, s)
	jpeg, _, err := s.ReplaceContent(ctx, members[1].ID, members[1].Revision, fakeHash("b2"), 20, "image/jpeg")
	require.NoError(t, err)
	image := fileByRole(asset.Files, PhotoRoleImage)
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &image.ID)
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, jpeg.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	page, err := s.PersonFacePreviews(ctx, person.PersonID, "grid-recipe", false)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Nil(t, page.Items[0].Box)
	source := members[0]
	box := document.FaceBox{Width: .2, Height: .2}
	_, _, err = s.CreatePhotoFace(ctx, source.CurrentVersionID, uuid.NewString(), box, person.PersonID)
	require.NoError(t, err)
	page, err = s.PersonFacePreviews(ctx, person.PersonID, "grid-recipe", false)
	require.NoError(t, err)
	require.Equal(t, []PersonFacePreview{{AssetID: asset.ID, ContentVersionID: source.CurrentVersionID, Box: &box}}, page.Items)
}

func TestPersonPhotoPreviewSourceFollowsAssignment(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, members := photoTrashFixture(t, s)
	source := members[0]
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, source.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, person.PersonID)
	require.NoError(t, err)
	require.NoError(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, person.PersonID))
	other, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, source.CurrentVersionID, PhotoFaceDecisionRequest{Action: "name", FaceID: face.FaceID, PersonID: other.PersonID, Revision: face.Revision})
	require.NoError(t, err)
	require.ErrorIs(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, person.PersonID), ErrNotFound)
	require.NoError(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, other.PersonID))
	_, err = s.DecidePhotoFace(ctx, source.CurrentVersionID, PhotoFaceDecisionRequest{Action: "retract", FaceID: face.FaceID, Revision: face.Revision + 1})
	require.NoError(t, err)
	require.ErrorIs(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, other.PersonID), ErrNotFound)
	_, err = s.DecidePhotoFace(ctx, source.CurrentVersionID, PhotoFaceDecisionRequest{Action: "name", FaceID: face.FaceID, PersonID: person.PersonID, Revision: face.Revision + 2})
	require.NoError(t, err)
	require.NoError(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, person.PersonID))
	_, _, err = s.ReplaceContent(ctx, source.ID, source.Revision, fakeHash("stale-preview"), 4, "image/jpeg")
	require.NoError(t, err)
	require.ErrorIs(t, s.PersonPhotoPreviewSource(ctx, asset.ID, source.CurrentVersionID, person.PersonID), ErrNotFound)
}

func TestWholePhotoMarkCreatesPersonAtomically(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	personID := uuid.NewString()
	mark, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, personID, "Grace")
	require.NoError(t, err)
	repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, personID, "Grace")
	require.NoError(t, err)
	require.Equal(t, mark, repeated)
	var namedPeople int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons WHERE display_name='Grace'`).Scan(&namedPeople))
	require.Equal(t, 1, namedPeople)
	faces, assertions, people, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces)
	require.Equal(t, []PersonDocumentAssertion{mark}, assertions)
	require.True(t, mark.PhotoMark)
	require.Equal(t, "assert", mark.Action)
	require.Equal(t, []PhotoFacePerson{{PersonID: mark.PersonID, DisplayName: "Grace", State: "curated"}}, people)

	for _, invalid := range []struct {
		personID, name string
		want           error
	}{
		{personID, "Ada", ErrInvalidPhotoFace},
		{"", "", ErrInvalidPhotoFace},
		{uuid.NewString(), " ", ErrInvalidPhotoFace},
		{uuid.NewString(), strings.Repeat("a", 201), ErrInvalidPhotoFace},
		{uuid.NewString(), strings.Repeat("雪", 67), ErrInvalidPhotoFace},
		{"", "Grace", ErrInvalidPhotoFace},
		{uuid.NewString(), "", ErrNotFound},
	} {
		_, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, invalid.personID, invalid.name)
		require.ErrorIs(t, err, invalid.want)
	}

	seedFaceAssertionSlots(t, s, node.CurrentVersionID, document.MaxPhotoRecordsPerVersion-1)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, uuid.NewString(), "Refused name")
	require.ErrorIs(t, err, ErrPhotoFaceLimit)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons WHERE display_name='Refused name'`).Scan(&count))
	require.Zero(t, count)
	_, assertions, _, err = s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, document.MaxPhotoRecordsPerVersion)
}

func TestPhotoFaceManualNamesAndBoxValidation(t *testing.T) {
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
	faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, named, faces[0])
	assertions, err := s.DocumentPersonAssertions(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, assertions, 1)
	require.Equal(t, "depicted", assertions[0].Role)
	marked, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	require.Equal(t, named.AssertionID, marked.AssertionID)
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, named.AssertionID, second.AssertionID)
	faces, _, _, err = s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 2)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{}, person.PersonID)
	require.ErrorIs(t, err, ErrInvalidPhotoFace)
	t.Run("returns names for curated and retired people", func(t *testing.T) {
		_, _, people, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
		require.NoError(t, err)
		require.Equal(t, []PhotoFacePerson{{PersonID: person.PersonID, DisplayName: "Ada", State: "curated"}}, people)
		_, err = s.RetirePerson(ctx, person.PersonID, person.Revision)
		require.NoError(t, err)
		_, _, people, err = s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
		require.NoError(t, err)
		require.Equal(t, []PhotoFacePerson{{PersonID: person.PersonID, DisplayName: "Ada", State: "retired"}}, people)
	})
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
	faces, assertions, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
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
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, absorbed.PersonID, "")
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
	faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Empty(t, faces)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: 0.2, Height: 0.2}, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, face.FaceID, face.Box, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	faces, _, _, err = s.PhotoFacePersonNames(unlocked, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []PhotoFace{face}, faces)
	require.NoError(t, s.LockPhotoHidden(ctx))
	faces, _, _, err = s.PhotoFacePersonNames(unlocked, node.CurrentVersionID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Empty(t, faces)
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
	faces, _, _, err := target.PhotoFacePersonNames(ctx, node.CurrentVersionID)
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
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
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
	first, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &before))
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state`).Scan(&epoch))
	repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
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
	wrongVersion, err := s.MarkPersonInPhoto(ctx, other.CurrentVersionID, person.PersonID, "")
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
		_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, id, box, otherPerson.PersonID)
		require.ErrorIs(t, err, ErrExists)
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

	faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
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
			unavailable, _, _, err := s.PhotoFacePersonNames(ctx, target.CurrentVersionID)
			require.ErrorIs(t, err, ErrNotFound)
			require.Empty(t, unavailable)
			_, _, err = s.CreatePhotoFace(ctx, target.CurrentVersionID, uuid.New().String(), box, person.PersonID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.MarkPersonInPhoto(ctx, target.CurrentVersionID, person.PersonID, "")
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
	faces, assertions, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	face.Revision++
	require.Equal(t, []PhotoFace{face}, faces)
	require.Equal(t, []PersonDocumentAssertion{suppressed}, assertions)
}

func seedFaceAssertionSlots(t *testing.T, s *Store, version string, count int) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `WITH RECURSIVE slots(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM slots WHERE i<?) INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) SELECT printf('00000000-0000-4000-9000-%012d',i),'Synthetic person','synthetic person','operator','curated',1,?,? FROM slots`, count, nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) SELECT person_id,?,person_id,'depicted','assert','',?,1 FROM persons WHERE person_id LIKE '00000000-0000-4000-9000-%'`, version, nowRFC3339())
	require.NoError(t, err)
}

func TestPhotoFaceAssertionCapacityAndRestore(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"capacity and restore", func(t *testing.T) {
			t.Helper()
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
			seedFaceAssertionSlots(t, s, node.CurrentVersionID, document.MaxPhotoRecordsPerVersion-1)
			person, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			marked, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
			require.NoError(t, err)
			repeated, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
			require.NoError(t, err)
			require.Equal(t, marked, repeated)
			box := document.FaceBox{Width: 0.2, Height: 0.2}
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, person.PersonID)
			require.NoError(t, err)
			overflow, err := s.CreatePerson(ctx, "Grace", "operator")
			require.NoError(t, err)
			_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, overflow.PersonID, "")
			require.ErrorIs(t, err, ErrPhotoFaceLimit)
			_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), box, overflow.PersonID)
			require.ErrorIs(t, err, ErrPhotoFaceLimit)
			overflowAssertion := PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: overflow.PersonID, Role: "depicted", Action: "assert", Revision: 1}
			_, err = s.AssertDocumentPerson(ctx, overflowAssertion)
			require.ErrorIs(t, err, ErrPhotoFaceLimit)
			overflowAssertion.Role = "author"
			_, err = s.AssertDocumentPerson(ctx, overflowAssertion)
			require.NoError(t, err)
			_, assertions, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
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
		}},
		{"unreferenced withdrawal", func(t *testing.T) {
			t.Helper()
			s := newTestStore(t)
			ctx := t.Context()
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			n, f := faceForDecisionTest(t, s, "")
			seedFaceAssertionSlots(t, s, n.CurrentVersionID, 1000)
			_, err = s.db.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn' WHERE content_version_id=?`, n.CurrentVersionID)
			require.NoError(t, err)
			decideFaceForTest(t, s, n, f.FaceID, p.PersonID, "name")
			_, assertions, _, err := s.PhotoFacePersonNames(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, assertions, 1)
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
		{"referenced withdrawal", func(t *testing.T) {
			t.Helper()
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
			_, err = s.MarkPersonInPhoto(ctx, n.CurrentVersionID, other.PersonID, "")
			require.ErrorIs(t, err, ErrPhotoFaceLimit)
			withdrawn.Action = "assert"
			_, err = s.AssertDocumentPerson(ctx, withdrawn)
			require.NoError(t, err)
			_, assertions, _, err := s.PhotoFacePersonNames(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, assertions, 1000)
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
		{"undo rename", func(t *testing.T) {
			t.Helper()
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
			faces, assertions, _, err := s.PhotoFacePersonNames(ctx, n.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, assertions, 1000)
			require.Equal(t, f.AssertionID, faces[0].AssertionID)
			require.NoError(t, s.ValidateMetadata(ctx))
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestPhotoFaceGenericDepictedAuthorization(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	assertion, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
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

func TestPeoplePhotoSummary(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = s.CreatePerson(ctx, "A Contact", "operator")
	require.NoError(t, err)
	hiddenOnly, err := s.CreatePerson(ctx, "Hidden only", "operator")
	require.NoError(t, err)
	var nodes []Node
	var assets []PhotoAsset
	var faces []PhotoFace
	for i := range 3 {
		node := browsePhotoNode(t, s, fmt.Sprintf("summary-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", i+30)), "image/jpeg")
		browsePhotoMetadata(t, s, node, fmt.Sprintf("summary-%d", i), exifCapture(fmt.Sprintf("2026-01-%02dT12:00:00Z", i+1)))
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .3, Height: .3}, person.PersonID)
		require.NoError(t, err)
		nodes = append(nodes, node)
		assets = append(assets, asset)
		faces = append(faces, face)
	}
	// Repeated faces and whole-photo evidence count the asset once.
	_, _, err = s.CreatePhotoFace(ctx, nodes[0].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .1, Height: .1}, person.PersonID)
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, nodes[0].CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	p, _, err := s.PersonByID(ctx, person.PersonID)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, nodes[0].CurrentVersionID, PhotoFaceDecisionRequest{Action: "key", Revision: 1, PersonID: p.PersonID, PersonRevision: p.Revision, FaceID: faces[0].FaceID})
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, assets[2].ID, assets[2].Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	_, err = s.MarkPersonInPhoto(unlocked, nodes[2].CurrentVersionID, hiddenOnly.PersonID, "")
	require.NoError(t, err)
	options := PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"}
	// Filter contacts before applying the page limit.
	page, _, err := s.PeopleByDisplayName(ctx, "", "", "", 1, true, options)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, person.PersonID, page[0].PersonID)
	rows, _, err := s.PeopleByDisplayName(unlocked, "", "", "", 100, true, options)
	require.NoError(t, err)
	// A person without photo evidence gets no card.
	require.Len(t, rows, 2)
	require.Equal(t, int64(2), rows[0].PhotoSummary.LibraryCount)
	require.Equal(t, int64(1), *rows[0].PhotoSummary.HiddenCount)
	require.Equal(t, hiddenOnly.PersonID, rows[1].PersonID)
	require.Equal(t, int64(1), *rows[1].PhotoSummary.HiddenCount)
	require.Equal(t, nodes[0].CurrentVersionID, rows[0].PhotoSummary.Face.ContentVersionID)
	rows, _, err = s.PeopleByDisplayName(ctx, "", "", "", 100, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].PhotoSummary.HiddenCount)
	// Hiding the selected face makes the remaining readable face the card source.
	_, err = s.SetPhotoAssetHidden(unlocked, assets[0].ID, assets[0].Revision, true)
	require.NoError(t, err)
	rows, _, err = s.PeopleByDisplayName(ctx, "Ada", "", "", 100, true, options)
	require.NoError(t, err)
	require.Equal(t, nodes[1].CurrentVersionID, rows[0].PhotoSummary.Face.ContentVersionID)
	// Unlocking restores the Hidden key face.
	rows, _, err = s.PeopleByDisplayName(unlocked, "Ada", "", "", 100, true, options)
	require.NoError(t, err)
	require.Equal(t, nodes[0].CurrentVersionID, rows[0].PhotoSummary.Face.ContentVersionID)
	require.True(t, rows[0].PhotoSummary.FaceHidden)
	// A Hidden-only person gets a portrait while unlocked.
	_, _, err = s.CreatePhotoFace(unlocked, nodes[2].CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Y: .5, Width: .2, Height: .2}, hiddenOnly.PersonID)
	require.NoError(t, err)
	rows, _, err = s.PeopleByDisplayName(unlocked, "Hidden only", "", "", 100, true, options)
	require.NoError(t, err)
	require.Equal(t, nodes[2].CurrentVersionID, rows[0].PhotoSummary.Face.ContentVersionID)
	require.True(t, rows[0].PhotoSummary.FaceHidden)
	require.Equal(t, int64(1), *rows[0].PhotoSummary.HiddenCount)
	require.Zero(t, rows[0].PhotoSummary.LibraryCount)
	rows, _, err = s.PeopleByDisplayName(ctx, "Hidden only", "", "", 100, true, options)
	require.NoError(t, err)
	require.Empty(t, rows)
	// Excluded, trashed and stale photos leave the count, and a person left
	// with none gets no card.
	grace, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	lapsed, err := s.CreatePerson(ctx, "Lapsed", "operator")
	require.NoError(t, err)
	unavailable := func(owner Person, name, condition string) {
		node := browsePhotoNode(t, s, name+".jpg", fakeHash(name), "image/jpeg")
		_, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .1, Y: .1, Width: .3, Height: .3}, owner.PersonID)
		require.NoError(t, err)
		switch condition {
		case "excluded":
			asset, assetErr := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, assetErr)
			_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
		case "trashed":
			_, _, err = s.Trash(ctx, node.ID, node.Revision)
		case "stale":
			_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("d4"), 4, "image/jpeg")
		}
		require.NoError(t, err)
	}
	unavailable(grace, "grace-available", "")
	unavailable(lapsed, "lapsed-trashed", "trashed")
	for _, condition := range []string{"excluded", "trashed", "stale"} {
		unavailable(grace, "grace-"+condition, condition)
	}
	rows, _, err = s.PeopleByDisplayName(ctx, "Grace", "", "", 100, true, options)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1), rows[0].PhotoSummary.LibraryCount)
	rows, _, err = s.PeopleByDisplayName(ctx, "Lapsed", "", "", 100, true, options)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestPeoplePhotoSummaryExcludesNamesakeWithoutPhotos(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = s.CreatePerson(ctx, "ada", "operator")
	require.NoError(t, err)
	node := browsePhotoNode(t, s, "ada.jpg", fakeHash("ada-photo"), "image/jpeg")
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	for _, recent := range []bool{false, true} {
		for _, prefix := range []string{"", "ada"} {
			rows, _, err := s.PeopleByDisplayName(ctx, prefix, "", "", 24, true, PeopleSearchOptions{PhotoSummary: true, RecentFirst: recent})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, person.PersonID, rows[0].PersonID)
			require.Equal(t, int64(1), rows[0].PhotoSummary.LibraryCount)
		}
	}
}

func TestPeoplePhotoSummaryPairedSources(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, nodes := photoTrashFixture(t, s)
	imageFile := fileByRole(asset.Files, PhotoRoleImage)
	_, err := s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &imageFile.ID)
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	for _, node := range nodes[:2] {
		_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, person.PersonID)
		require.NoError(t, err)
	}
	options := PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"}
	rows, _, err := s.PeopleByDisplayName(ctx, "Ada", "", "", 100, true, options)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows[0].PhotoSummary.LibraryCount)
	// Once the RAW changes, its old face no longer supplies the crop.
	_, _, err = s.ReplaceContent(ctx, nodes[0].ID, nodes[0].Revision, fakeHash("d4"), 4, "image/x-canon-cr2")
	require.NoError(t, err)
	rows, _, err = s.PeopleByDisplayName(ctx, "Ada", "", "", 100, true, options)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows[0].PhotoSummary.LibraryCount)
	require.Equal(t, nodes[1].CurrentVersionID, rows[0].PhotoSummary.Face.ContentVersionID)
}

func TestPeoplePhotoSummarySharedPhotos(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	bob, err := s.CreatePerson(ctx, "Bob", "operator")
	require.NoError(t, err)
	// Ada's faces are on the left and Bob's on the right.
	left := map[string]float64{ada.PersonID: .1, bob.PersonID: .5}
	for i, people := range [][]Person{{ada, bob}, {ada, bob}, {bob}} {
		node := browsePhotoNode(t, s, fmt.Sprintf("shared-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", i+60)), "image/jpeg")
		for _, person := range people {
			_, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: left[person.PersonID], Y: .1, Width: .3, Height: .3}, person.PersonID)
			require.NoError(t, err)
		}
	}
	options := PeopleSearchOptions{PhotoSummary: true, GridRecipe: "grid"}
	page, _, err := s.PeopleByDisplayName(ctx, "", "", "", 24, true, options)
	require.NoError(t, err)
	require.Len(t, page, 2)
	// Photos shared by people on one page count for each of them.
	require.Equal(t, int64(2), page[0].PhotoSummary.LibraryCount)
	require.Equal(t, int64(3), page[1].PhotoSummary.LibraryCount)
	// The portrait is the person's own face, not another face in the same photo.
	require.InDelta(t, .1, page[0].PhotoSummary.Face.Box.X, 1e-9)
	require.InDelta(t, .5, page[1].PhotoSummary.Face.Box.X, 1e-9)
}

func TestMarkPersonInPhotoNamedRetryAfterMerge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("a1"), "image/jpeg")
	id := uuid.NewString()
	_, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, id, "Ada duplicate")
	require.NoError(t, err)
	survivor, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, survivor.PersonID, id, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, id, "Ada duplicate")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.ValidateMetadata(ctx))
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons WHERE person_id=?`, id).Scan(&count))
	require.Zero(t, count)
}
