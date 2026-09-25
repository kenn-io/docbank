package store

import (
	"bytes"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyPhotoMediaMatrix(t *testing.T) {
	tests := []struct {
		name, mime, filename, kind string
		qualifies                  bool
	}{
		{name: "jpeg", mime: "image/jpeg", filename: "a.jpg", kind: PhotoKindPhoto, qualifies: true},
		{name: "video", mime: "video/mp4", filename: "a.mp4", kind: PhotoKindVideo, qualifies: true},
		{name: "audio", mime: "audio/mpeg", filename: "a.mp3", qualifies: false},
		{name: "generic raw", mime: "application/octet-stream", filename: "a.cr2", qualifies: false},
		{name: "generic text", mime: "application/octet-stream", filename: "a.txt", qualifies: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, qualifies, kind := classifyPhotoMedia(test.mime, test.filename)
			assert.Equal(t, test.qualifies, qualifies)
			if test.kind != "" {
				assert.Equal(t, test.kind, kind)
			}
		})
	}
}

func TestPhotoEnrollmentClassifiesCreatedNodes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "image.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), asset.Revision)
	assert.Equal(t, PhotoRoleImage, asset.Files[0].Role)

	video, err := s.CreateFile(ctx, s.RootID(), "video.mp4", fakeHash("b2"), 4, "video/mp4")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, video.ID)
	require.NoError(t, err)
	audio, err := s.CreateFile(ctx, s.RootID(), "audio.mp3", fakeHash("c3"), 4, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, audio.ID)
	require.ErrorIs(t, err, ErrNotFound)
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("d4"), 4, "application/octet-stream")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, raw.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoAssetGroupsRawJPEGSidecar(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("a1"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpeg, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawFile.ID)
	require.NoError(t, err)
	assert.Len(t, asset.Files, 3)
	assert.NotNil(t, asset.DisplayFileID)
	var sidecarReceipt photoReceiptRow
	for _, receipt := range photoReceiptRows(t, s, asset.ID) {
		if receipt.Operation == "attach" && strings.Contains(receipt.AfterJSON, `"node_id":`+strconv.FormatInt(sidecar.ID, 10)) {
			sidecarReceipt = receipt
			break
		}
	}
	assert.NotEmpty(t, sidecarReceipt.AfterJSON)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &rawFile.ID)
	require.NoError(t, err)
	assert.Equal(t, rawFile.ID, *asset.DisplayFileID)
}

func TestPhotoDisplayPrecedenceAndDetachFallback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("a1"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpeg, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayDefault, asset.DisplaySource)
	assert.Equal(t, PhotoRoleRAW, fileByID(asset.Files, *asset.DisplayFileID).Role)
	preference := "image"
	settings, err := s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayVault, asset.DisplaySource)
	assert.Equal(t, PhotoRoleImage, fileByID(asset.Files, *asset.DisplayFileID).Role)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &asset.Files[0].ID)
	require.NoError(t, err)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, *asset.DisplayFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	assert.NotNil(t, asset.DisplayFileID)
}

func TestPhotoSettingsRecomputeInheritedAssets(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "b.jpg", fakeHash("b2"), 1, "image/jpeg")
	require.NoError(t, err)
	firstAsset, err := s.PhotoAssetForNode(ctx, first.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, firstAsset.ID, firstAsset.Revision, true)
	require.NoError(t, err)
	secondAsset, err := s.PhotoAssetForNode(ctx, second.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoDisplay(ctx, secondAsset.ID, secondAsset.Revision, nil)
	require.NoError(t, err)
	raw, err := s.CreateFile(ctx, s.RootID(), "fanout.capture", fakeHash("f1"), 1, "application/octet-stream")
	require.NoError(t, err)
	group, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	secondAsset, err = s.PhotoAssetForNode(ctx, second.ID)
	require.NoError(t, err)
	secondFileID := secondAsset.Files[0].ID
	_, err = s.DetachPhotoFile(ctx, secondAsset.ID, secondAsset.Revision, secondFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	group, err = s.AttachPhotoFile(ctx, group.ID, group.Revision, second.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	preference := "image"
	settings, err := s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
	group, err = s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoRoleImage, fileByID(group.Files, *group.DisplayFileID).Role)
	rawFileID := fileByRole(group.Files, PhotoRoleRAW).ID
	group, err = s.SetPhotoDisplay(ctx, group.ID, group.Revision, &rawFileID)
	require.NoError(t, err)
	settings, err = s.SetPhotoSettings(ctx, settings.Revision, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), settings.Revision)
	unchangedGroup, err := s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, group.Revision, unchangedGroup.Revision)
	_, err = s.SetPhotoSettings(ctx, settings.Revision-1, &preference)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Contains(t, receiptOperations(photoReceiptRows(t, s, group.ID)), "settings_recompute")
	assert.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoNodeModesAndPurgeRepair(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, image.ID, image.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Empty(t, asset.Files)
	assert.Nil(t, asset.DisplayFileID)
	assert.Equal(t, PhotoDisplayNone, asset.DisplaySource)
	raw, err := s.CreateFile(ctx, s.RootID(), "purge.capture", fakeHash("purge-raw"), 1, "application/octet-stream")
	require.NoError(t, err)
	group, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "purge.xmp", fakeHash("purge-sidecar"), 1, "application/octet-stream")
	require.NoError(t, err)
	rawID := fileByRole(group.Files, PhotoRoleRAW).ID
	group, err = s.AttachPhotoFile(ctx, group.ID, group.Revision, sidecar.ID, PhotoRoleSidecar, &rawID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, raw.ID, raw.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	group, err = s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Empty(t, group.Files)
	assert.Nil(t, group.DisplayFileID)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
}

func TestPhotoMetadataRoundTripAndInvalidReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	secondNode, err := s.CreateFile(ctx, s.RootID(), "b.jpg", fakeHash("b1"), 1, "image/jpeg")
	require.NoError(t, err)
	var first bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &first))
	require.NoError(t, s.ValidateMetadata(ctx))
	assert.Contains(t, first.String(), `"type":"photo_asset"`)
	assert.Contains(t, first.String(), image.CurrentVersionID)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
	var second bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &second))
	assert.Equal(t, first.String(), second.String())
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	secondAsset, err := s.PhotoAssetForNode(ctx, secondNode.ID)
	require.NoError(t, err)
	restored, err := target.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, restored.ID)
	assert.Equal(t, asset.DisplayFileID, restored.DisplayFileID)
	invalidLines := strings.Split(first.String(), "\n")
	for index, line := range invalidLines {
		if strings.Contains(line, `"type":"photo_file"`) {
			invalidLines[index] = strings.Replace(line, `"role":"image"`, `"role":"sidecar"`, 1)
			break
		}
	}
	invalid := newTestStore(t)
	require.Error(t, invalid.ImportMetadata(ctx, strings.NewReader(strings.Join(invalidLines, "\n"))))

	crossAssetLines := strings.Split(first.String(), "\n")
	for index, line := range crossAssetLines {
		if strings.Contains(line, `"type":"photo_file"`) && strings.Contains(line, secondAsset.Files[0].ID) {
			crossAssetLines[index] = strings.Replace(line, `"`+secondAsset.ID+`"`, `"`+asset.ID+`"`, 1)
			break
		}
	}
	crossAsset := newTestStore(t)
	require.Error(t, crossAsset.ImportMetadata(ctx, strings.NewReader(strings.Join(crossAssetLines, "\n"))))
}

func TestPhotoMetadataRejectsUnattachedSidecar(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "restore.capture", fakeHash("bb01"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "restore.xmp", fakeHash("bb02"), 1, "application/octet-stream")
	require.NoError(t, err)
	rawFileID := fileByRole(asset.Files, PhotoRoleRAW).ID
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawFileID)
	require.NoError(t, err)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	lines := strings.Split(exported.String(), "\n")
	found := false
	for index, line := range lines {
		if strings.Contains(line, `"type":"photo_file"`) && strings.Contains(line, `"role":"sidecar"`) {
			lines[index] = strings.Replace(line,
				`"sidecar_of_file_id":"`+rawFileID+`"`,
				`"sidecar_of_file_id":null`, 1)
			found = true
			break
		}
	}
	require.True(t, found)
	invalid := newTestStore(t)
	require.Error(t, invalid.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))))
}

func TestPhotoMetadataRejectsRoleMediaAndAssetKindMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		update string
	}{
		{name: "role and media", update: `UPDATE photo_files SET role='video'`},
		{name: "asset kind", update: `UPDATE photo_assets SET kind='video'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			_, err := s.CreateFile(t.Context(), s.RootID(), "mismatch.jpg", fakeHash(test.name), 1, "image/jpeg")
			require.NoError(t, err)
			require.NoError(t, validatePhotoMetadataState(t.Context(), s.db))
			_, err = s.db.ExecContext(t.Context(), test.update)
			require.NoError(t, err)
			require.ErrorIs(t, validatePhotoMetadataState(t.Context(), s.db), ErrInvalidPhotoAsset)
		})
	}
}

func TestPhotoMetadataAcceptsReceiptIndependentRevisionOneState(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "migrated.raw", fakeHash("migrated-raw"), 1, "application/octet-stream")
	require.NoError(t, err)
	image, err := s.CreateFile(ctx, s.RootID(), "migrated.jpg", fakeHash("migrated-image"), 1, "image/jpeg")
	require.NoError(t, err)
	imageAsset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, imageAsset.ID, imageAsset.Revision, imageAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, PhotoKindPhoto)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, image.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	imageFile := fileByRole(asset.Files, PhotoRoleImage)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &imageFile.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM photo_change_receipts`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE photo_assets SET revision=1 WHERE asset_id=?`, asset.ID)
	require.NoError(t, err)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
}

func TestPhotoMetadataRejectsOrphanReceipts(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "orphan.jpg", fakeHash("0a0a"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	lines := strings.Split(exported.String(), "\n")
	for index, line := range lines {
		if strings.Contains(line, `"type":"photo_asset"`) {
			lines[index] = ""
		}
		if strings.Contains(line, `"type":"photo_file"`) {
			lines[index] = ""
		}
	}
	target := newTestStore(t)
	err = target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n")))
	require.ErrorContains(t, err, "reference missing assets")

	preference := "image"
	_, err = s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	exported.Reset()
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	lines = strings.Split(exported.String(), "\n")
	for index, line := range lines {
		if strings.Contains(line, `"type":"photo_library_settings"`) {
			lines[index] = ""
		}
	}
	settingsTarget := newTestStore(t)
	err = settingsTarget.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n")))
	require.ErrorContains(t, err, "reference missing assets or settings")
}

func TestPhotoExplicitVideoAdmitsGenericVideo(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	clip, err := s.CreateFile(ctx, s.RootID(), "clip.mp4", fakeHash("0c11"), 1, "application/octet-stream")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, clip.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.PromotePhotoNode(ctx, clip.ID, nil, "", "")
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
	asset, err := s.PromotePhotoNode(ctx, clip.ID, nil, PhotoRoleVideo, "")
	require.NoError(t, err)
	assert.Equal(t, PhotoKindVideo, asset.Kind)

	genericAudio, err := s.CreateFile(ctx, s.RootID(), "song.mp3", fakeHash("0c12"), 1, "application/octet-stream")
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, genericAudio.ID, nil, PhotoRoleVideo, PhotoKindVideo)
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
	typedAudio, err := s.CreateFile(ctx, s.RootID(), "track.mp4", fakeHash("0c13"), 1, "audio/mp4")
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, typedAudio.ID, nil, PhotoRoleVideo, PhotoKindVideo)
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
}

func TestPhotoMutationsRequireRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision+1, true)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestPhotoSidecarTargetsAndNoDisplayableMember(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "a.cr2", fakeHash("a1"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "a.xmp", fakeHash("b2"), 1, "application/octet-stream")
	require.NoError(t, err)
	rawFileID := fileByRole(asset.Files, PhotoRoleRAW).ID
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawFileID)
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, rawFile.ID, PhotoDetachOptions{})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, rawFile.ID, PhotoDetachOptions{ClearDependentSidecars: true})
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayNone, asset.DisplaySource)
	assert.Nil(t, asset.DisplayFileID)
}

func TestPhotoExcludePromotePreservesIdentityAndReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	beforeID := asset.ID
	updated, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	assert.Equal(t, beforeID, updated.ID)
	_, err = s.PromotePhotoNode(ctx, image.ID, nil, "", "")
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.NotEmpty(t, photoReceiptRows(t, s, asset.ID))
	promoted, err := s.PromotePhotoNode(ctx, image.ID, &updated.Revision, "", "")
	require.NoError(t, err)
	assert.Equal(t, beforeID, promoted.ID)
	assert.Nil(t, promoted.ExcludedAt)
	again, err := s.PromotePhotoNode(ctx, image.ID, &promoted.Revision, "", "")
	require.NoError(t, err)
	assert.Equal(t, promoted.Revision, again.Revision)
	_, err = s.PromotePhotoNode(ctx, image.ID, nil, "", "")
	require.ErrorIs(t, err, ErrStaleRevision)
	stale := promoted.Revision - 1
	_, err = s.PromotePhotoNode(ctx, image.ID, &stale, "", "")
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestPhotoPromoteExistingAssetChecksLiveNode(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "trashed-existing.jpg", fakeHash("cc01"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	excluded, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, image.ID, image.Revision)
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, image.ID, &excluded.Revision, "", "")
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
	unchanged, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, excluded.Revision, unchanged.Revision)
	assert.NotNil(t, unchanged.ExcludedAt)
}

func TestPhotoPromoteUnownedNodeRejectsRevisionPrecondition(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "unowned.cr2", fakeHash("unowned"), 1, "application/octet-stream")
	require.NoError(t, err)

	expectedRevision := int64(1)
	_, err = s.PromotePhotoNode(ctx, raw.ID, &expectedRevision, PhotoRoleRAW, "")
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.PhotoAssetForNode(ctx, raw.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoVersionTransitionsKeepIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	originalVersionID := image.CurrentVersionID
	updated, replacementVersion, err := s.ReplaceContent(ctx, image.ID, image.Revision, fakeHash("b2"), 1, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
	assetAfter, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, assetAfter.ID)
	assert.Equal(t, asset.Revision, assetAfter.Revision)
	assert.Equal(t, updated.ID, image.ID)
	promoted, err := s.PromotePhotoNode(ctx, image.ID, &assetAfter.Revision, PhotoRoleImage, PhotoKindPhoto)
	require.NoError(t, err)
	assert.Equal(t, assetAfter.Revision, promoted.Revision)
	reverted, _, source, err := s.RevertContent(ctx, image.ID, updated.Revision, originalVersionID)
	require.NoError(t, err)
	assert.Equal(t, originalVersionID, source.ID)
	assert.Equal(t, image.ID, reverted.ID)
	assetAfterRevert, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, assetAfterRevert.ID)
	_, err = s.PruneContentVersions(ctx, image.ID, reverted.Revision,
		VersionPruneSelector{VersionIDs: []string{replacementVersion.ID}}, true)
	require.NoError(t, err)
	assetAfterPrune, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, assetAfterPrune.ID)
}

func TestPhotoVersionPrunePreservesChangedMediaMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "pruned.jpg", fakeHash("prune-media-a"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	updated, _, err := s.ReplaceContent(ctx, image.ID, image.Revision, fakeHash("prune-media-b"), 1, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, image.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{image.CurrentVersionID}}, true)
	require.NoError(t, err)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
	retained, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, retained.ID)
	assert.Equal(t, PhotoRoleImage, retained.Files[0].Role)
}

func TestPhotoRenamePreservesAdmissionClassification(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "generic.jpg", fakeHash("c001"), 1, "application/octet-stream")
	require.NoError(t, err)

	_, _, err = s.Move(ctx, image.ID, s.RootID(), "generic.bin", image.Revision)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	require.NoError(t, restored.ExportMetadata(ctx, &bytes.Buffer{}))
}

func TestPhotoPolicy(t *testing.T) {
	require.ErrorIs(t, validatePhotoAssetPointers(PhotoAsset{ID: "x", Kind: PhotoKindPhoto, Revision: 1, DisplayFileID: new("missing")}, nil), ErrInvalidPhotoAsset)
	assert.False(t, photoRoleValid("primary"))
	assert.True(t, photoPreferenceValid(new("image")))
	assert.False(t, photoPreferenceValid(new("video")))
}

func TestPhotoExplicitRawAdmissionAndTargetBoundaries(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "vendor.capture", fakeHash("raw"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err := s.CreatePhotoAsset(ctx, raw.ID, PhotoRoleRAW, "")
	require.NoError(t, err)
	assert.Equal(t, PhotoRoleRAW, asset.Files[0].Role)
	_, err = s.PromotePhotoNode(ctx, raw.ID, &asset.Revision, "", PhotoKindVideo)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)

	image, err := s.CreateFile(ctx, s.RootID(), "target.jpeg", fakeHash("jpeg"), 1, "image/jpeg")
	require.NoError(t, err)
	imageAsset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, image.ID, &imageAsset.Revision, PhotoRoleRAW, "")
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	video, err := s.CreateFile(ctx, s.RootID(), "target.mp4", fakeHash("video"), 1, "video/mp4")
	require.NoError(t, err)
	videoAsset, err := s.PhotoAssetForNode(ctx, video.ID)
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, video.ID, &videoAsset.Revision, PhotoRoleRAW, "")
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)

	sidecar, err := s.CreateFile(ctx, s.RootID(), "target.xmp", fakeHash("sidecar"), 1, "application/octet-stream")
	require.NoError(t, err)
	imageAsset, err = s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &imageAsset.Files[0].ID)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	videoAsset, err = s.PhotoAssetForNode(ctx, video.ID)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &videoAsset.Files[0].ID)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
}

func TestPhotoCameraRawMIMEsAllowExplicitRaw(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	mediaTypes := []string{
		"image/x-sony-arw",
		"image/x-fuji-raf",
		"image/x-adobe-dng",
		"image/x-canon-cr2",
		"image/x-nikon-nef",
	}
	for index, mediaType := range mediaTypes {
		t.Run(mediaType, func(t *testing.T) {
			node, err := s.CreateFile(ctx, s.RootID(), "camera-"+strconv.Itoa(index)+".raw", fakeHash("ca"+strconv.Itoa(index)), 1, mediaType)
			require.NoError(t, err)
			automatic, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			require.Equal(t, PhotoRoleImage, automatic.Files[0].Role)
			_, err = s.DetachPhotoFile(ctx, automatic.ID, automatic.Revision, automatic.Files[0].ID, PhotoDetachOptions{})
			require.NoError(t, err)
			explicit, err := s.PromotePhotoNode(ctx, node.ID, nil, PhotoRoleRAW, "")
			require.NoError(t, err)
			assert.Equal(t, PhotoRoleRAW, explicit.Files[0].Role)
		})
	}
}

func TestPhotoAttachRequiresExplicitRawForNonqualifyingNodes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "attach.capture", fakeHash("aa01"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err := s.CreatePhotoAsset(ctx, raw.ID, PhotoRoleRAW, "")
	require.NoError(t, err)
	textNode, err := s.CreateFile(ctx, s.RootID(), "attach.txt", fakeHash("aa02"), 1, "text/plain")
	require.NoError(t, err)
	audioNode, err := s.CreateFile(ctx, s.RootID(), "attach.mp3", fakeHash("aa03"), 1, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, textNode.ID, "", nil)
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, audioNode.ID, "", nil)
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, textNode.ID, PhotoRoleRAW, nil)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, audioNode.ID, PhotoRoleRAW, nil)
	require.NoError(t, err)
}

func TestPhotoNodeModesRefuseDirectoryAndTrashedPromotion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	directory, _, err := s.MkdirPath(ctx, "/photos")
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, directory.ID, nil, PhotoRoleRAW, "")
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)

	file, err := s.CreateFile(ctx, s.RootID(), "trashed.capture", fakeHash("trash"), 1, "application/octet-stream")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, file.ID, file.Revision)
	require.NoError(t, err)
	_, err = s.PromotePhotoNode(ctx, file.ID, nil, PhotoRoleRAW, "")
	require.ErrorIs(t, err, ErrPhotoNodeNotEligible)
}

func TestPhotoEnrollmentSkipsEmailChildAndKeepsProcessedSource(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	const raw = "Content-Type: multipart/mixed; boundary=photos\r\n\r\n--photos\r\nContent-Type: image/jpeg\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=child.jpg\r\n\r\nAAECAw==\r\n--photos--\r\n"
	f := newEmailSourceFixture(t, s, "source.eml", raw)
	view, err := s.PublishEmailGeneration(ctx, f.publication)
	require.NoError(t, err)
	receipt, err := s.PublishEmailDocuments(ctx, attachmentRequest(t, s, view, "photo-child"))
	require.NoError(t, err)
	require.Len(t, receipt.Relations, 1)
	child := receipt.Relations[0].Child
	require.NotNil(t, child)
	_, err = s.PhotoAssetForNode(ctx, child.NodeID)
	require.ErrorIs(t, err, ErrNotFound)
	promoted, err := s.PromotePhotoNode(ctx, child.NodeID, nil, "", "")
	require.NoError(t, err)
	assert.Equal(t, child.NodeID, promoted.Files[0].NodeID)

	image, err := s.CreateFile(ctx, s.RootID(), "embedded:source.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, image.ID, asset.Files[0].NodeID)
	processed, err := s.CreateFile(ctx, s.RootID(), "ocr-source.jpg", catalogSourceHash, 20, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for hash, contents := range catalogBlobContents {
			if err := s.EnsureBlobTx(tx, hash, int64(len(contents))); err != nil {
				return err
			}
		}
		return nil
	}))
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: processed.CurrentVersionID,
		BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}))
	_, err = s.PhotoAssetForNode(ctx, processed.ID)
	require.NoError(t, err)
	assert.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAuditModePreservesCreationAndGraph(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "before-audit.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())

	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	after, err := s.CreateFile(ctx, s.RootID(), "after-audit.jpg", fakeHash("b2"), 1, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, after.ID)
	require.ErrorIs(t, err, ErrNotFound)
	unchanged, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, unchanged.ID)
	assert.Equal(t, asset.Revision, unchanged.Revision)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func fileByID(files []PhotoFile, id string) PhotoFile {
	for _, file := range files {
		if file.ID == id {
			return file
		}
	}
	return PhotoFile{}
}

func fileByRole(files []PhotoFile, role string) PhotoFile {
	for _, file := range files {
		if file.Role == role {
			return file
		}
	}
	return PhotoFile{}
}

type photoReceiptRow struct {
	Operation string
	AfterJSON string
}

func photoReceiptRows(t *testing.T, s *Store, assetID string) []photoReceiptRow {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT operation, after_json FROM photo_change_receipts WHERE asset_id=? ORDER BY created_at`, assetID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var receipts []photoReceiptRow
	for rows.Next() {
		var receipt photoReceiptRow
		require.NoError(t, rows.Scan(&receipt.Operation, &receipt.AfterJSON))
		receipts = append(receipts, receipt)
	}
	require.NoError(t, rows.Err())
	return receipts
}

func receiptOperations(receipts []photoReceiptRow) []string {
	operations := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		operations = append(operations, receipt.Operation)
	}
	return operations
}
