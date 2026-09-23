package store

import (
	"bytes"
	"database/sql"
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
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID)
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, PhotoRoleRAW)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage)
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, rawFile.ID)
	require.NoError(t, err)
	assert.Len(t, asset.Files, 3)
	assert.NotNil(t, asset.DisplayFileID)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, rawFile.ID)
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
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID)
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, PhotoRoleRAW)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayDefault, asset.DisplaySource)
	assert.Equal(t, PhotoRoleRAW, fileByID(asset.Files, *asset.DisplayFileID).Role)
	settings, err := s.SetPhotoSettings(ctx, 1, "image")
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayVault, asset.DisplaySource)
	assert.Equal(t, PhotoRoleImage, fileByID(asset.Files, *asset.DisplayFileID).Role)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, asset.Files[0].ID)
	require.NoError(t, err)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.DisplayFileID)
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
	settings, err := s.SetPhotoSettings(ctx, 1, "image")
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
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
}

func TestPhotoMetadataRoundTripAndInvalidReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	var first bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &first))
	require.NoError(t, s.ValidateMetadata(ctx))
	assert.Contains(t, first.String(), `"type":"photo_asset"`)
	assert.Contains(t, first.String(), image.CurrentVersionID)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	restored, err := target.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, restored.ID)
	assert.Equal(t, asset.DisplayFileID, restored.DisplayFileID)
}

func TestPhotoMutationsRequireRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision+1, true)
	require.ErrorIs(t, err, ErrPhotoAssetRevision)
}

func TestPhotoSidecarTargetsAndNoDisplayableMember(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "a.cr2", fakeHash("a1"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, PhotoRoleRAW)
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "a.xmp", fakeHash("b2"), 1, "application/octet-stream")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, fileByRole(asset.Files, PhotoRoleRAW).ID)
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, rawFile.ID)
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
	receipts, _, err := s.PhotoChangeReceipts(ctx, asset.ID, 20, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, receipts)
	promoted, err := s.PromotePhotoNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeID, promoted.ID)
	assert.Nil(t, promoted.ExcludedAt)
	again, err := s.PromotePhotoNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, promoted.Revision, again.Revision)
}

func TestPhotoVersionTransitionsKeepIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	updated, _, err := s.ReplaceContent(ctx, image.ID, image.Revision, fakeHash("b2"), 1, "image/jpeg")
	require.NoError(t, err)
	assetAfter, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, assetAfter.ID)
	assert.Equal(t, asset.Revision, assetAfter.Revision)
	assert.Equal(t, updated.ID, image.ID)
}

func TestPhotoPolicy(t *testing.T) {
	require.ErrorIs(t, validatePhotoAssetPointers(PhotoAsset{ID: "x", Kind: PhotoKindPhoto, Revision: 1, DisplayFileID: new("missing")}, nil), ErrInvalidPhotoAsset)
	assert.False(t, photoRoleValid("primary"))
	assert.True(t, photoPreferenceValid(new("image")))
	assert.False(t, photoPreferenceValid(new("video")))
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
	promoted, err := s.PromotePhotoNode(ctx, child.NodeID)
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
