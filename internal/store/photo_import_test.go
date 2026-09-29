package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhotoSourceClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		kind PhotoSourceKind
		role string
		mime string
	}{
		{name: "capture.ARW", kind: PhotoSourceRAW, role: PhotoRoleRAW, mime: "image/x-sony-arw"},
		{name: "capture.dNg", kind: PhotoSourceRAW, role: PhotoRoleRAW, mime: "image/x-adobe-dng"},
		{name: "capture.JPEG", kind: PhotoSourceImage, role: PhotoRoleImage, mime: "image/jpeg"},
		{name: "capture.MP4", kind: PhotoSourceVideo, role: PhotoRoleVideo, mime: "video/mp4"},
		{name: "capture.XMP", kind: PhotoSourceSidecar, role: PhotoRoleSidecar, mime: "application/rdf+xml"},
		{name: "capture.mp3", kind: PhotoSourceUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := ClassifyPhotoSource(test.name)
			assert.Equal(t, test.kind, source.Kind)
			assert.Equal(t, test.role, source.Role)
			assert.Equal(t, test.mime, source.MediaType)
		})
	}
}

func TestPhotoNodeFactsKeepVideoFamily(t *testing.T) {
	t.Parallel()
	video := photoNodeFacts(Node{Name: "capture.MP4", MimeType: "video/mp4"})
	assert.Equal(t, "audio_video", video.MediaFamily)
	assert.Equal(t, PhotoKindVideo, video.AssetKind)
}

func TestPhotoImportCandidatePathUsesIndex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var id, parent, unused int
	var detail string
	err := s.db.QueryRowContext(t.Context(), `EXPLAIN QUERY PLAN
		SELECT identity FROM provenance INDEXED BY provenance_original_path_nocase
		WHERE original_path LIKE ? ESCAPE '!'`, `C:\camera!_!%!\%`).Scan(&id, &parent, &unused, &detail)
	require.NoError(t, err)
	assert.Contains(t, detail, "SEARCH provenance USING INDEX provenance_original_path_nocase")
}

func TestOrdinaryPhotoMediaEnrollment(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, test := range []struct {
		name string
		mime string
		role string
	}{
		{name: "capture.ARW", mime: "image/x-sony-arw", role: PhotoRoleImage},
		{name: "capture.JPG", mime: "image/jpeg", role: PhotoRoleImage},
		{name: "capture.WEBP", mime: "image/webp", role: PhotoRoleImage},
	} {
		file, err := s.CreateFile(ctx, s.RootID(), test.name, fakeHash(test.name), 4, test.mime)
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, file.ID)
		require.NoError(t, err)
		require.Len(t, asset.Files, 1)
		assert.Equal(t, test.role, asset.Files[0].Role)
	}
}

func photoImportTestMember(path, role, hash, mediaType string) PhotoImportMember {
	return PhotoImportMember{Name: filepath.Base(path), Role: role, BlobHash: hash,
		Size: 4, MediaType: mediaType, OriginalPath: path}
}

func photoImportTestGroup(members ...PhotoImportMember) PhotoImportGroup {
	return PhotoImportGroup{Members: members, DestinationID: 1}
}

func TestPhotoImportAtomicGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	group := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0001.ARW"), PhotoRoleRAW, fakeHash("raw"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.JPG"), PhotoRoleImage, fakeHash("jpg"), "image/jpeg"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.XMP"), PhotoRoleSidecar, fakeHash("xmp"), "application/rdf+xml"),
	)
	result, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	assert.True(t, result.Added)
	assert.Len(t, result.Asset.Files, 3)
	assert.Equal(t, PhotoRoleRAW, fileByRole(result.Asset.Files, PhotoRoleRAW).Role)

	var nodes int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&nodes))
	bad := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0002.ARW"), PhotoRoleRAW, fakeHash("raw2"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0002.JPG"), PhotoRoleImage, fakeHash("jpg2"), "image/jpeg"),
		photoImportTestMember(filepath.Join(root, "IMG_0002.XMP"), PhotoRoleSidecar, "", "application/rdf+xml"),
	)
	_, err = s.IngestPhotoGroup(ctx, run, bad)
	require.Error(t, err)
	var after int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&after))
	assert.Equal(t, nodes, after)

	rerun, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	assert.True(t, rerun.Skipped)
	assert.False(t, rerun.Added)
	assert.Equal(t, result.Asset.ID, rerun.Asset.ID)
}

func TestPhotoImportLateSibling(t *testing.T) {
	t.Parallel()
	for _, first := range []struct {
		name string
		role string
		hash string
		mime string
	}{
		{name: "IMG_0001.ARW", role: PhotoRoleRAW, hash: fakeHash("raw"), mime: "image/x-sony-arw"},
		{name: "IMG_0001.JPG", role: PhotoRoleImage, hash: fakeHash("jpg"), mime: "image/jpeg"},
	} {
		t.Run(first.role+" first", func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
			require.NoError(t, err)
			root := filepath.Join(t.TempDir(), "camera_%!")
			firstPath := filepath.Join(root, first.name)
			_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
				photoImportTestMember(firstPath, first.role, first.hash, first.mime)))
			require.NoError(t, err)
			secondName, secondRole, secondHash, secondMime := "IMG_0001.JPG", PhotoRoleImage, fakeHash("jpg"), "image/jpeg"
			if first.role == PhotoRoleImage {
				secondName, secondRole, secondHash, secondMime = "IMG_0001.ARW", PhotoRoleRAW, fakeHash("raw"), "image/x-sony-arw"
			}
			secondPath := filepath.Join(root, secondName)
			result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
				photoImportTestMember(secondPath, secondRole, secondHash, secondMime)))
			require.NoError(t, err)
			assert.NotEmpty(t, result.Asset.ID)
			asset, err := s.PhotoAssetForNode(ctx, result.Nodes[0].ID)
			require.NoError(t, err)
			assert.Len(t, asset.Files, 2)
		})
	}
}

func TestPhotoImportSeparatePhotosAreReported(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	raw := photoImportTestMember(filepath.Join(root, "IMG.ARW"), PhotoRoleRAW, fakeHash("separate-raw"), "image/x-sony-arw")
	image := photoImportTestMember(filepath.Join(root, "IMG.JPG"), PhotoRoleImage, fakeHash("separate-image"), "image/jpeg")
	rawResult, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw))
	require.NoError(t, err)
	imageGroup := photoImportTestGroup(image)
	imageGroup.Isolated = true
	imageResult, err := s.IngestPhotoGroup(ctx, run, imageGroup)
	require.NoError(t, err)
	require.NotEqual(t, rawResult.Asset.ID, imageResult.Asset.ID)

	rerun, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, image))
	require.NoError(t, err)
	assert.True(t, rerun.Skipped)
	require.NotNil(t, rerun.Ambiguity)
	assert.Equal(t, PhotoImportSeparatePhotos, rerun.Ambiguity.Reason)
	assert.ElementsMatch(t, []string{rawResult.Asset.ID, imageResult.Asset.ID},
		[]string{rerun.Ambiguity.Files[0].AssetID, rerun.Ambiguity.Files[1].AssetID})
	for _, id := range []string{rawResult.Asset.ID, imageResult.Asset.ID} {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		assert.Len(t, asset.Files, 1)
		assert.Equal(t, int64(1), asset.Revision)
	}
}

func TestPhotoImportMultipleRAWsImportAlone(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := t.TempDir()
	arw := photoImportTestMember(filepath.Join(root, "IMG.ARW"), PhotoRoleRAW, fakeHash("arw"), "image/x-sony-arw")
	dng := photoImportTestMember(filepath.Join(root, "IMG.DNG"), PhotoRoleRAW, fakeHash("dng"), "image/x-adobe-dng")
	jpeg := photoImportTestMember(filepath.Join(root, "IMG.JPG"), PhotoRoleImage, fakeHash("jpeg"), "image/jpeg")
	xmp := photoImportTestMember(filepath.Join(root, "IMG.XMP"), PhotoRoleSidecar, fakeHash("xmp"), "application/rdf+xml")
	result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(arw, dng, jpeg, xmp))
	require.NoError(t, err)
	assert.True(t, result.Added)
	require.NotNil(t, result.Ambiguity)
	assert.Equal(t, PhotoImportMultipleRAW, result.Ambiguity.Reason)
	require.Len(t, result.Ambiguity.Files, 4)
	assets := make(map[string]struct{})
	for index, node := range result.Nodes[:3] {
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err, index)
		assert.Len(t, asset.Files, 1)
		assets[asset.ID] = struct{}{}
	}
	assert.Len(t, assets, 3)
	_, err = s.PhotoAssetForNode(ctx, result.Nodes[3].ID)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, result.Ambiguity.Files[3].AssetID)

	// A second RAW arriving later leaves the established pair untouched.
	later := t.TempDir()
	raw := photoImportTestMember(filepath.Join(later, "PAIR.ARW"), PhotoRoleRAW, fakeHash("pair-raw"), "image/x-sony-arw")
	image := photoImportTestMember(filepath.Join(later, "PAIR.JPG"), PhotoRoleImage, fakeHash("pair-jpeg"), "image/jpeg")
	paired, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, image))
	require.NoError(t, err)
	require.Len(t, paired.Asset.Files, 2)
	extra := photoImportTestMember(filepath.Join(later, "PAIR.DNG"), PhotoRoleRAW, fakeHash("pair-dng"), "image/x-adobe-dng")
	reported, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(extra))
	require.NoError(t, err)
	require.NotNil(t, reported.Ambiguity)
	assert.Equal(t, PhotoImportMultipleRAW, reported.Ambiguity.Reason)
	unchanged, err := s.PhotoAssetByID(ctx, paired.Asset.ID)
	require.NoError(t, err)
	assert.Len(t, unchanged.Files, 2)
	alone, err := s.PhotoAssetForNode(ctx, reported.Nodes[0].ID)
	require.NoError(t, err)
	assert.Len(t, alone.Files, 1)
}

func TestPhotoImportDedupObservation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	hash := fakeHash("same-raw")
	firstRoot := filepath.Join(t.TempDir(), "first")
	secondRoot := filepath.Join(t.TempDir(), "second")
	_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(firstRoot, "IMG.ARW"), PhotoRoleRAW, hash, "image/x-sony-arw")))
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(secondRoot, "IMG.DNG"), PhotoRoleRAW, hash, "image/x-adobe-dng")))
	require.NoError(t, err)
	var nodes int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&nodes))
	assert.Equal(t, 1, nodes)
	result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(secondRoot, "IMG.JPG"), PhotoRoleImage, fakeHash("same-jpg"), "image/jpeg")))
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, result.Nodes[0].ID)
	require.NoError(t, err)
	assert.Len(t, asset.Files, 2)
	var observations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM provenance WHERE original_path=?`, filepath.Join(secondRoot, "IMG.DNG")).Scan(&observations))
	assert.Equal(t, 1, observations)
}

func TestPhotoImportDuplicateRawNextToSeparateJPEGIsReported(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	raw := photoImportTestMember(filepath.Join(first, "IMG.ARW"), PhotoRoleRAW, fakeHash("same-raw"), "image/x-sony-arw")
	jpeg := photoImportTestMember(filepath.Join(second, "IMG.JPG"), PhotoRoleImage, fakeHash("same-jpg"), "image/jpeg")
	_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw))
	require.NoError(t, err)
	jpegResult, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(jpeg))
	require.NoError(t, err)
	duplicate := photoImportTestMember(filepath.Join(second, "IMG.ARW"), PhotoRoleRAW, raw.BlobHash, raw.MediaType)
	reported, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(duplicate))
	require.NoError(t, err)
	assert.True(t, reported.Skipped)
	require.NotNil(t, reported.Ambiguity)
	assert.Equal(t, PhotoImportSeparatePhotos, reported.Ambiguity.Reason)
	jpegAsset, err := s.PhotoAssetByID(ctx, jpegResult.Asset.ID)
	require.NoError(t, err)
	assert.Len(t, jpegAsset.Files, 1)
}

func TestPhotoImportVideoWithSameNameJPEG(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := t.TempDir()
	jpeg := photoImportTestMember(filepath.Join(root, "IMG.JPG"), PhotoRoleImage, fakeHash("jpg"), "image/jpeg")
	image, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(jpeg))
	require.NoError(t, err)
	video := photoImportTestMember(filepath.Join(root, "IMG.MOV"), PhotoRoleVideo, fakeHash("mov"), "video/quicktime")
	clip, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(video))
	require.NoError(t, err)
	assert.NotEqual(t, image.Asset.ID, clip.Asset.ID)
	assert.Equal(t, PhotoKindVideo, clip.Asset.Kind)
	assert.Len(t, clip.Asset.Files, 1)
}

func TestPhotoImportSidecarDedupStaysWithinSourceGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	sidecarHash := fakeHash("shared-sidecar")
	first, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0001.ARW"), PhotoRoleRAW, fakeHash("raw-one"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.XMP"), PhotoRoleSidecar, sidecarHash, "application/rdf+xml"),
	))
	require.NoError(t, err)
	second, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0002.ARW"), PhotoRoleRAW, fakeHash("raw-two"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0002.XMP"), PhotoRoleSidecar, sidecarHash, "application/rdf+xml"),
	))
	require.NoError(t, err)
	assert.NotEqual(t, first.Asset.ID, second.Asset.ID)
	assert.Len(t, first.Asset.Files, 2)
	assert.Len(t, second.Asset.Files, 2)
}

func TestPhotoSidecarImageTarget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "IMG.JPG", fakeHash("image"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.PromotePhotoNode(ctx, image.ID, nil, PhotoRoleImage, PhotoKindPhoto)
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "IMG.XMP", fakeHash("sidecar"), 4, "application/rdf+xml")
	require.NoError(t, err)
	imageFile := fileByRole(asset.Files, PhotoRoleImage)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &imageFile.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, imageFile.ID, PhotoDetachOptions{})
	require.Error(t, err)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, imageFile.ID, PhotoDetachOptions{ClearDependentSidecars: true})
	require.NoError(t, err)
	assert.Empty(t, asset.Files)
}

func TestPhotoSidecarImageLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "lifecycle.JPG", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	imageFile := asset.Files[0]
	sidecarOne, err := s.CreateFile(ctx, s.RootID(), "lifecycle-one.XMP", fakeHash("a2"), 4, "application/rdf+xml")
	require.NoError(t, err)
	sidecarTwo, err := s.CreateFile(ctx, s.RootID(), "lifecycle-two.XMP", fakeHash("a3"), 4, "application/rdf+xml")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecarOne.ID, PhotoRoleSidecar, &imageFile.ID)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecarTwo.ID, PhotoRoleSidecar, &imageFile.ID)
	require.NoError(t, err)
	require.Len(t, asset.Files, 3)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredAsset, err := restored.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, restoredAsset.Files, 3)

	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, imageFile.ID, PhotoDetachOptions{})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, imageFile.ID, PhotoDetachOptions{ClearDependentSidecars: true})
	require.NoError(t, err)
	assert.Empty(t, asset.Files)

	purgeImage, err := s.CreateFile(ctx, s.RootID(), "purge.JPG", fakeHash("b1"), 4, "image/jpeg")
	require.NoError(t, err)
	purgeAsset, err := s.PhotoAssetForNode(ctx, purgeImage.ID)
	require.NoError(t, err)
	purgeFile := purgeAsset.Files[0]
	purgeSidecar, err := s.CreateFile(ctx, s.RootID(), "purge.XMP", fakeHash("b2"), 4, "application/rdf+xml")
	require.NoError(t, err)
	purgeAsset, err = s.AttachPhotoFile(ctx, purgeAsset.ID, purgeAsset.Revision, purgeSidecar.ID, PhotoRoleSidecar, &purgeFile.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, purgeImage.ID, purgeImage.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	purgedAsset, err := s.PhotoAssetByID(ctx, purgeAsset.ID)
	require.NoError(t, err)
	assert.Empty(t, purgedAsset.Files)
	_, err = s.NodeByID(ctx, purgeSidecar.ID)
	require.NoError(t, err)
}

func TestPhotoImportNodeStates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	directory, _, err := s.MkdirPath(ctx, "/state-dir")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, directory.ID, "state.txt", fakeHash("state-file"), 4, "text/plain")
	require.NoError(t, err)
	assert.Equal(t, "dir", directory.Kind)
	assert.Equal(t, "file", file.Kind)
	directory, err = s.NodeByID(ctx, directory.ID)
	require.NoError(t, err)
	trashedDir, _, err := s.Trash(ctx, directory.ID, directory.Revision)
	require.NoError(t, err)
	assert.Equal(t, "dir", trashedDir.Kind)
	trashedFile, err := s.NodeByID(ctx, file.ID)
	require.NoError(t, err)
	assert.Equal(t, "file", trashedFile.Kind)
	require.NotNil(t, trashedFile.TrashedAt)

	group := photoImportTestGroup(photoImportTestMember(
		filepath.Join(t.TempDir(), "state.JPG"), PhotoRoleImage, fakeHash("state-import"), "image/jpeg"))
	group.DestinationID = s.RootID()
	result, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	require.Len(t, result.Nodes, 1)
	assert.NotEqual(t, file.ID, result.Nodes[0].ID)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	_, err = s.NodeByID(ctx, directory.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.NodeByID(ctx, file.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoImportVersionStates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "versions.JPG", fakeHash("version-current"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	original := image.CurrentVersionID
	updated, replacement, err := s.ReplaceContent(ctx, image.ID, image.Revision, fakeHash("version-replacement"), 4, "image/jpeg")
	require.NoError(t, err)
	assert.Equal(t, asset.ID, mustPhotoAsset(t, s, image.ID).ID)
	_, revert, source, err := s.RevertContent(ctx, image.ID, updated.Revision, original)
	require.NoError(t, err)
	assert.Equal(t, original, source.ID)
	versions, total, err := s.ContentVersions(ctx, image.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, versions, 3)
	assert.Equal(t, revert.ID, versions[0].ID)
	assert.Equal(t, replacement.ID, versions[1].ID)
	preview, err := s.PruneContentVersions(ctx, image.ID, updated.Revision+1, VersionPruneSelector{VersionIDs: []string{replacement.ID}}, false)
	require.NoError(t, err)
	assert.False(t, preview.Changed)
	receipt, err := s.PruneContentVersions(ctx, image.ID, updated.Revision+1, VersionPruneSelector{VersionIDs: []string{replacement.ID}}, true)
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	_, err = s.ContentVersionByID(ctx, replacement.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoImportMembershipStates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "membership.JPG", fakeHash("membership-image"), 4, "image/jpeg")
	require.NoError(t, err)
	automatic, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoRoleImage, automatic.Files[0].Role)
	excluded, err := s.SetPhotoAssetExcluded(ctx, automatic.ID, automatic.Revision, true)
	require.NoError(t, err)
	assert.NotNil(t, excluded.ExcludedAt)
	explicit, err := s.PromotePhotoNode(ctx, image.ID, &excluded.Revision, PhotoRoleImage, PhotoKindPhoto)
	require.NoError(t, err)
	assert.Nil(t, explicit.ExcludedAt)

	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	group := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "MEM.ARW"), PhotoRoleRAW, fakeHash("membership-raw"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "MEM.JPG"), PhotoRoleImage, fakeHash("membership-jpg"), "image/jpeg"),
	)
	group.DestinationID = s.RootID()
	groupResult, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	groupAsset, err := s.PhotoAssetByID(ctx, groupResult.Asset.ID)
	require.NoError(t, err)
	imageFile := fileByRole(groupAsset.Files, PhotoRoleImage)
	groupAsset, err = s.SetPhotoDisplay(ctx, groupAsset.ID, groupAsset.Revision, &imageFile.ID)
	require.NoError(t, err)
	preference := "raw"
	_, err = s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	groupAsset, err = s.PhotoAssetByID(ctx, groupAsset.ID)
	require.NoError(t, err)
	assert.Equal(t, imageFile.ID, *groupAsset.DisplayFileID)
	assert.Equal(t, PhotoRoleRAW, fileByRole(groupAsset.Files, PhotoRoleRAW).Role)
}

func TestPhotoImportMetadata(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	ingestRun, err := source.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	camera := filepath.Join(t.TempDir(), "camera")
	jpeg, err := source.IngestPhotoGroup(ctx, ingestRun, photoImportTestGroup(
		photoImportTestMember(filepath.Join(camera, "IMG.JPG"), PhotoRoleImage, fakeHash("a1"), "image/jpeg")))
	require.NoError(t, err)
	paired, err := source.IngestPhotoGroup(ctx, ingestRun, photoImportTestGroup(
		photoImportTestMember(filepath.Join(camera, "IMG.ARW"), PhotoRoleRAW, fakeHash("b2"), "image/x-sony-arw")))
	require.NoError(t, err)
	assert.Equal(t, jpeg.Asset.ID, paired.Asset.ID)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	assert.Contains(t, exported.String(), `"operation":"import"`)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredAsset, err := target.PhotoAssetByID(ctx, paired.Asset.ID)
	require.NoError(t, err)
	assert.Len(t, restoredAsset.Files, 2)
}

func TestPhotoImportOperationLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	operation, err := s.CreateLocalOperation(ctx, StorageOperationKindPhotoImport, `{"source_root":"/camera","destination":"/photos"}`)
	require.NoError(t, err)
	assert.Equal(t, StorageOperationQueued, operation.State)
	require.Error(t, s.SetStorageOperationTotal(ctx, operation.ID, 3))
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetStorageOperationTotal(ctx, operation.ID, 3))
	require.NoError(t, s.AdvanceStorageOperation(ctx, operation.ID, "", 1, 0, 0, `{"added":1}`))
	require.NoError(t, s.RequestStorageOperationCancel(ctx, operation.ID))
	resumable, err := s.ResumableStorageOperations(ctx)
	require.NoError(t, err)
	require.Len(t, resumable, 1)
	current := resumable[0]
	assert.Equal(t, int64(3), current.TotalObjects)
	assert.Equal(t, int64(1), current.CompletedObjects)
	assert.True(t, current.CancelRequested)
	require.NoError(t, s.FinishStorageOperation(ctx, operation.ID, StorageOperationCancelled, current.ReceiptJSON, "", time.Time{}))
	_, err = s.CreateLocalOperation(ctx, "unknown", `{}`)
	require.Error(t, err)
}
func TestPhotoImportRefusesAuditedVault(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	destination, _, err := s.MkdirPath(ctx, "/audited")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, destination.ID)
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	group := photoImportTestGroup(photoImportTestMember(filepath.Join(t.TempDir(), "capture.JPG"), PhotoRoleImage, fakeHash("audited"), "image/jpeg"))
	group.DestinationID = destination.ID
	_, err = s.IngestPhotoGroup(ctx, run, group)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestPhotoImportDestinationCreatesAndReusesPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()

	created, err := s.EnsurePhotoImportDestination(ctx, "/photos/2027")
	require.NoError(t, err)
	assert.True(t, created.IsDir())

	reused, err := s.EnsurePhotoImportDestination(ctx, "/photos/2027/")
	require.NoError(t, err)
	assert.Equal(t, created.ID, reused.ID)

	root, err := s.EnsurePhotoImportDestination(ctx, "/")
	require.NoError(t, err)
	assert.Equal(t, s.RootID(), root.ID)

	_, err = s.CreateFile(ctx, s.RootID(), "file", fakeHash("photo-import-destination-file"), 4, "text/plain")
	require.NoError(t, err)
	_, err = s.EnsurePhotoImportDestination(ctx, "/file/child")
	require.ErrorIs(t, err, ErrNotDir)
}

func TestPhotoImportDestinationRollsBackOnInvalidComponent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	before, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)

	_, err = s.EnsurePhotoImportDestination(ctx, "/rollback/.")
	require.ErrorIs(t, err, ErrInvalidName)

	_, err = s.NodeByPath(ctx, "/rollback")
	require.ErrorIs(t, err, ErrNotFound)
	after, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	assert.Equal(t, before.Revision, after.Revision)
}

func TestPhotoImportDestinationRejectsAuditedVault(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	parent, err := s.Mkdir(ctx, s.RootID(), "audited")
	require.NoError(t, err)
	before, err := s.NodeByID(ctx, parent.ID)
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(ctx, parent.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)

	_, err = s.EnsurePhotoImportDestination(ctx, "/audited/new/nested")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	_, err = s.NodeByPath(ctx, "/audited/new")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.NodeByPath(ctx, "/audited/new/nested")
	require.ErrorIs(t, err, ErrNotFound)
	after, err := s.NodeByID(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Revision, after.Revision)
}

func mustPhotoAsset(t *testing.T, s *Store, nodeID int64) PhotoAsset {
	t.Helper()
	asset, err := s.PhotoAssetForNode(t.Context(), nodeID)
	require.NoError(t, err)
	return asset
}
