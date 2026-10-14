package store

import (
	"context"
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
		{name: "capture.CR3", kind: PhotoSourceRAW, role: PhotoRoleRAW, mime: "image/x-canon-cr3"},
		{name: "capture.JPEG", kind: PhotoSourceImage, role: PhotoRoleImage, mime: "image/jpeg"},
		{name: "capture.MP4", kind: PhotoSourceVideo, role: PhotoRoleVideo, mime: "video/mp4"},
		{name: "capture.XMP", kind: PhotoSourceSidecar, role: PhotoRoleSidecar, mime: "application/rdf+xml"},
		{name: "capture.mp3", kind: PhotoSourceUnsupported},
		{name: "capture.mp2", kind: PhotoSourceUnsupported},
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

func photoImportTestMember(path, role, hash, mediaType string) PhotoImportMember {
	return PhotoImportMember{Name: filepath.Base(path), Role: role, BlobHash: hash,
		Size: 4, MediaType: mediaType, OriginalPath: path}
}

func photoImportTestGroup(members ...PhotoImportMember) PhotoImportGroup {
	return PhotoImportGroup{Members: members, DestinationID: 1}
}

func TestPhotoImportLateSidecarUpdatesLockedHiddenAsset(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "camera")
	run, err := s.BeginIngest(ctx, "photo-import", root)
	require.NoError(t, err)
	raw := photoImportTestMember(filepath.Join(root, "IMG.ARW"), PhotoRoleRAW, fakeHash("hidden-raw"), "image/x-sony-arw")
	first, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw))
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	hidden, err := s.SetPhotoAssetHidden(ctx, first.Asset.ID, first.Asset.Revision, true)
	require.NoError(t, err)
	xmp := photoImportTestMember(filepath.Join(root, "IMG.XMP"), PhotoRoleSidecar, fakeHash("hidden-xmp"), "application/rdf+xml")
	updated, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, xmp))
	require.NoError(t, err)
	require.Equal(t, hidden.ID, updated.Asset.ID)
	require.Equal(t, hidden.HiddenAt, updated.Asset.HiddenAt)
	require.Len(t, updated.Asset.Files, 2)
	require.Equal(t, PhotoRoleSidecar, fileByRole(updated.Asset.Files, PhotoRoleSidecar).Role)
}

func TestPhotoImportRollsBackFailedGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	group := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0001.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.JPG"), PhotoRoleImage, browseHash("jpg"), "image/jpeg"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml"),
	)
	result, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	assert.True(t, result.Added)
	assert.Len(t, result.Asset.Files, 3)
	assert.Equal(t, PhotoRoleRAW, fileByRole(result.Asset.Files, PhotoRoleRAW).Role)

	var nodes int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&nodes))
	bad := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0002.ARW"), PhotoRoleRAW, browseHash("raw2"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0002.JPG"), PhotoRoleImage, browseHash("jpg2"), "image/jpeg"),
		photoImportTestMember(filepath.Join(root, "IMG_0002.XMP"), PhotoRoleSidecar, browseHash("xmp2"), "application/rdf+xml"),
	)
n, err := s.PreparePhotoImportDestination(ctx, "/new/photos")
	require.NoError(t, err)
	beforePlan := plan
	bad.DestinationPlan = &plan
	_, err = s.db.Exec(`CREATE TRIGGER reject_final_photo_member BEFORE INSERT ON nodes
		WHEN NEW.name='IMG_0002.XMP' BEGIN SELECT RAISE(ABORT, 'injected final photo member failure'); END`)
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, run, bad)
	require.ErrorContains(t, err, "injected final photo member failure")
	assert.Equal(t, beforePlan, plan)
	_, err = s.NodeByPath(ctx, "/new")
	require.ErrorIs(t, err, ErrNotFound)
	var after int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&after))
	assert.Equal(t, nodes, after)
	unchanged, err := s.PhotoAssetByID(ctx, result.Asset.ID)
	require.NoError(t, err)
	assert.Equal(t, result.Asset, unchanged)
	var assets int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_assets`).Scan(&assets))
	assert.Equal(t, 1, assets)
}

func TestPhotoImportKeepsSameNameVideoSeparate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	group := photoImportTestGroup(
		photoImportTestMember(filepath.Join(root, "IMG_0001.ARW"), PhotoRoleRAW, browseHash("raw"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.JPG"), PhotoRoleImage, browseHash("jpg"), "image/jpeg"),
		photoImportTestMember(filepath.Join(root, "IMG_0001.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml"),
	)
	result, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	video, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(photoImportTestMember(
		filepath.Join(root, "IMG_0001.MOV"), PhotoRoleVideo, browseHash("mov"), "video/quicktime")))
	require.NoError(t, err)
	assert.NotEqual(t, result.Asset.ID, video.Asset.ID)
	assert.Equal(t, PhotoKindVideo, video.Asset.Kind)
	assert.Len(t, video.Asset.Files, 1)
}

func TestPhotoImportQualifiedSidecarTargets(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ambiguous", "qualified RAW first", "qualified JPEG first", "missing extension claim"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			root := t.TempDir()
			run, err := s.BeginIngest(ctx, "photo-import", root)
			require.NoError(t, err)
			members := []PhotoImportMember{
				photoImportTestMember(filepath.Join(root, "capture.ARW"), PhotoRoleRAW, fakeHash("a1"), "image/x-sony-arw"),
				photoImportTestMember(filepath.Join(root, "capture.JPG"), PhotoRoleImage, fakeHash("b2"), "image/jpeg"),
				photoImportTestMember(filepath.Join(root, "capture.ARW.xmp"), PhotoRoleSidecar, fakeHash("c3"), "application/rdf+xml"),
				photoImportTestMember(filepath.Join(root, "capture.JPG.xmp"), PhotoRoleSidecar, fakeHash("d4"), "application/rdf+xml"),
			}
			if mode == "ambiguous" {
				members[2] = photoImportTestMember(filepath.Join(root, "capture.jpg"), PhotoRoleImage, fakeHash("e5"), "image/jpeg")
			}
			if mode == "missing extension claim" {
				members[3] = photoImportTestMember(filepath.Join(root, "capture.xmp"), PhotoRoleSidecar, fakeHash("d4"), "application/rdf+xml")
			}
			if mode == "qualified JPEG first" {
				members[0], members[3] = members[3], members[0]
			}
			result, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(members...))
			require.NoError(t, err)
			if mode == "ambiguous" {
				require.Len(t, result.Asset.Files, 3)
				_, err = s.PhotoAssetForNode(ctx, result.Nodes[3].ID)
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			jpg := fileByRole(result.Asset.Files, PhotoRoleImage)
			receipts := map[string]PhotoAuthoredReceipt{}
			var rawSidecar, jpgSidecar PhotoFile
			for _, f := range result.Asset.Files {
				if f.Role != PhotoRoleSidecar {
					continue
				}
				node, err := s.NodeByID(ctx, f.NodeID)
				require.NoError(t, err)
				rating, caption, extension := int64(5), "", ""
				if node.Name == "capture.ARW.xmp" {
					rawSidecar = f
				} else {
					jpgSidecar = f
					rating, caption = 3, "JPEG caption"
					if mode == "missing extension claim" {
						extension = "NEF"
					} else {
						require.Equal(t, &jpg.ID, f.SidecarOfID)
					}
				}
				canonical := photoCanonical(t,
					photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean()),
					photoMetadataField("image.xmp.rating", "image.xmp", "Rating", photoInteger(rating)),
					photoMetadataField("image.xmp.caption", "image.xmp", "description", photoString(caption)),
					photoMetadataField("image.xmp.sidecar_for_extension", "image.xmp", "SidecarForExtension", photoString(extension)),
				)
				_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("ee"), canonical)
				require.NoError(t, err)
			}
			for {
				targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				if len(targets) == 0 {
					break
				}
				receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
				require.NoError(t, err)
				if len(receipt.After) > 0 {
					receipts[receipt.After[0].FileID] = receipt
				}
			}
			asset, err := s.PhotoAssetByID(ctx, result.Asset.ID)
			require.NoError(t, err)
			raw, jpg := fileByRole(asset.Files, PhotoRoleRAW), fileByRole(asset.Files, PhotoRoleImage)
			assert.Equal(t, 5, raw.Rating)
			assert.Equal(t, int64(2), raw.Revision)
			require.NotNil(t, receipts[raw.ID].Sidecar)
			assert.Equal(t, rawSidecar.ID, receipts[raw.ID].Sidecar.FileID)
			if mode == "missing extension claim" {
				assert.Zero(t, jpg.Rating)
				assert.Equal(t, int64(1), jpg.Revision)
			} else {
				assert.Equal(t, 3, jpg.Rating)
				assert.Equal(t, "JPEG caption", jpg.Caption)
				assert.Equal(t, int64(2), jpg.Revision)
				require.NotNil(t, receipts[jpg.ID].Sidecar)
				assert.Equal(t, jpgSidecar.ID, receipts[jpg.ID].Sidecar.FileID)
			}
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
	// The same JPEG bytes first seen elsewhere become their own photo.
	elsewhere := image
	elsewhere.OriginalPath = filepath.Join(t.TempDir(), "IMG.JPG")
	imageResult, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(elsewhere))
	require.NoError(t, err)
	require.NotEqual(t, rawResult.Asset.ID, imageResult.Asset.ID)

	duplicate := raw
	duplicate.OriginalPath = filepath.Join(filepath.Dir(elsewhere.OriginalPath), "IMG.ARW")
	for _, candidates := range [][]PhotoImportMember{{raw, image}, {duplicate, elsewhere}} {
		rerun, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(candidates...))
		require.NoError(t, err)
		assert.True(t, rerun.Skipped)
		require.NotNil(t, rerun.Ambiguity)
		assert.Equal(t, PhotoImportSeparatePhotos, rerun.Ambiguity.Reason)
		assert.ElementsMatch(t, []string{rawResult.Asset.ID, imageResult.Asset.ID},
			[]string{rerun.Ambiguity.Files[0].AssetID, rerun.Ambiguity.Files[1].AssetID})
		for _, original := range []PhotoAsset{rawResult.Asset, imageResult.Asset} {
			asset, err := s.PhotoAssetByID(ctx, original.ID)
			require.NoError(t, err)
			assert.Equal(t, original, asset)
		}
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
	var files int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&files))
	rerun, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(arw, dng, jpeg, xmp))
	require.NoError(t, err)
	assert.True(t, rerun.Skipped)
	require.NotNil(t, rerun.Ambiguity)
	var after int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&after))
	assert.Equal(t, files, after)

	// A second RAW arriving later leaves the established pair untouched.
	later := t.TempDir()
	raw := photoImportTestMember(filepath.Join(later, "PAIR.ARW"), PhotoRoleRAW, fakeHash("pair-raw"), "image/x-sony-arw")
	image := photoImportTestMember(filepath.Join(later, "PAIR.JPG"), PhotoRoleImage, fakeHash("pair-jpeg"), "image/jpeg")
	paired, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, image))
	require.NoError(t, err)
	require.Len(t, paired.Asset.Files, 2)
	extra := photoImportTestMember(filepath.Join(later, "PAIR.DNG"), PhotoRoleRAW, fakeHash("pair-dng"), "image/x-adobe-dng")
	reported, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(extra, raw, image))
	require.NoError(t, err)
	require.NotNil(t, reported.Ambiguity)
	assert.Equal(t, PhotoImportMultipleRAW, reported.Ambiguity.Reason)
	unchanged, err := s.PhotoAssetByID(ctx, paired.Asset.ID)
	require.NoError(t, err)
	assert.Len(t, unchanged.Files, 2)
	alone, err := s.PhotoAssetForNode(ctx, reported.Nodes[0].ID)
	require.NoError(t, err)
	assert.Len(t, alone.Files, 1)
	t.Run("operator resolved", func(t *testing.T) {
		target := mustPhotoAsset(t, s, result.Nodes[0].ID)
		other := mustPhotoAsset(t, s, result.Nodes[1].ID)
		_, err := s.DetachPhotoFile(ctx, other.ID, other.Revision, other.Files[0].ID, PhotoDetachOptions{})
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(ctx, target.ID, target.Revision, result.Nodes[1].ID, PhotoRoleRAW, nil)
		require.NoError(t, err)
		rerun, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(arw, dng))
		require.NoError(t, err)
		assert.Nil(t, rerun.Ambiguity)
		assert.True(t, rerun.Skipped)
	})
}

func TestPhotoImportLoneSidecarWaitsForItsImage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "camera")
	xmp := photoImportTestMember(filepath.Join(root, "IMG.XMP"), PhotoRoleSidecar, fakeHash("lone-xmp"), "application/rdf+xml")
	alone, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(xmp))
	require.NoError(t, err)
	assert.True(t, alone.Added)
	require.Len(t, alone.Nodes, 1)
	_, err = s.PhotoAssetForNode(ctx, alone.Nodes[0].ID)
	require.ErrorIs(t, err, ErrNotFound)

	again, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(xmp))
	require.NoError(t, err)
	assert.True(t, again.Skipped)

	jpeg := photoImportTestMember(filepath.Join(root, "IMG.JPG"), PhotoRoleImage, fakeHash("lone-jpeg"), "image/jpeg")
	paired, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(jpeg, xmp))
	require.NoError(t, err)
	require.Len(t, paired.Asset.Files, 2)
	assert.Equal(t, alone.Nodes[0].ID, fileByRole(paired.Asset.Files, PhotoRoleSidecar).NodeID)
}

func TestPhotoImportDedupObservation(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"photo import", "ordinary file"} {
		t.Run(origin, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
			require.NoError(t, err)
			hash := fakeHash("same-raw")
			firstRoot := filepath.Join(t.TempDir(), "first")
			secondRoot := filepath.Join(t.TempDir(), "second")
			var original Node
			if origin == "ordinary file" {
				inbox, err := s.Mkdir(ctx, s.RootID(), "Inbox")
				require.NoError(t, err)
				original, err = s.CreateFile(ctx, inbox.ID, "capture.ARW", hash, 4, "image/x-sony-arw")
				require.NoError(t, err)
			} else {
				first, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
					photoImportTestMember(filepath.Join(firstRoot, "IMG.ARW"), PhotoRoleRAW, hash, "image/x-sony-arw")))
				require.NoError(t, err)
				original = first.Nodes[0]
			}
			duplicate, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
				photoImportTestMember(filepath.Join(secondRoot, "IMG.DNG"), PhotoRoleRAW, hash, "image/x-adobe-dng")))
			require.NoError(t, err)
			assert.True(t, duplicate.Skipped)
			assert.Equal(t, original.ID, duplicate.Nodes[0].ID)
			var nodes int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&nodes))
			assert.Equal(t, 1, nodes)
			group := photoImportTestGroup(
				photoImportTestMember(filepath.Join(secondRoot, "IMG.JPG"), PhotoRoleImage, fakeHash("same-jpg"), "image/jpeg"),
				photoImportTestMember(filepath.Join(secondRoot, "IMG.DNG"), PhotoRoleRAW, hash, "image/x-adobe-dng"),
				photoImportTestMember(filepath.Join(secondRoot, "IMG.XMP"), PhotoRoleSidecar, fakeHash("same-xmp"), "application/rdf+xml"))
			result, err := s.IngestPhotoGroup(ctx, run, group)
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, result.Nodes[0].ID)
			require.NoError(t, err)
			assert.True(t, result.Added)
			assert.Len(t, asset.Files, 3)
			rawAsset, err := s.PhotoAssetForNode(ctx, original.ID)
			require.NoError(t, err)
			assert.Equal(t, rawAsset.ID, asset.ID)
			rerun, err := s.IngestPhotoGroup(ctx, run, group)
			require.NoError(t, err)
			assert.True(t, rerun.Skipped)
			assert.False(t, rerun.Added)
			var observations int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM provenance WHERE original_path=?`, filepath.Join(secondRoot, "IMG.DNG")).Scan(&observations))
			assert.Equal(t, 1, observations)
		})
	}
}

func TestPhotoImportEditedSourceBecomesNewVersion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(t.TempDir(), "cam")
	raw := photoImportTestMember(filepath.Join(root, "IMG_0001.ARW"), PhotoRoleRAW, fakeHash("raw"), "image/x-sony-arw")
	jpg := func(hash string) PhotoImportMember {
		return photoImportTestMember(filepath.Join(root, "IMG_0001.JPG"), PhotoRoleImage, fakeHash(hash), "image/jpeg")
	}
	xmp := func(hash string) PhotoImportMember {
		return photoImportTestMember(filepath.Join(root, "IMG_0001.XMP"), PhotoRoleSidecar, fakeHash(hash), "application/rdf+xml")
	}
	first, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, jpg("jpg-v1"), xmp("xmp-v1")))
	require.NoError(t, err)

	edited, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, jpg("jpg-v1"), xmp("xmp-v2")))
	require.NoError(t, err)
	assert.True(t, edited.Added)
	jpegEdited, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(raw, jpg("jpg-v2"), xmp("xmp-v2")))
	require.NoError(t, err)

	asset := mustPhotoAsset(t, s, first.Nodes[0].ID)
	roles := make(map[string]int)
	for _, file := range asset.Files {
		roles[file.Role]++
	}
	assert.Equal(t, map[string]int{PhotoRoleRAW: 1, PhotoRoleImage: 1, PhotoRoleSidecar: 1}, roles)
	var files int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE kind='file'`).Scan(&files))
	assert.Equal(t, 3, files)
	for index, hash := range map[int]string{1: "jpg-v2", 2: "xmp-v2"} {
		current, err := s.NodeByID(ctx, first.Nodes[index].ID)
		require.NoError(t, err)
		assert.Equal(t, first.Nodes[index].ID, jpegEdited.Nodes[index].ID)
		assert.NotEqual(t, first.Nodes[index].CurrentVersionID, current.CurrentVersionID)
		assert.Equal(t, fakeHash(hash), current.BlobHash)
	}
	t.Run("shared source", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		run, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
		require.NoError(t, err)
		first := photoImportTestMember(filepath.Join(t.TempDir(), "a", "IMG.ARW"), PhotoRoleRAW, fakeHash("raw-v1"), "image/x-sony-arw")
		second := photoImportTestMember(filepath.Join(t.TempDir(), "b", "IMG.ARW"), PhotoRoleRAW, fakeHash("raw-v1"), "image/x-sony-arw")
		original, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(first))
		require.NoError(t, err)
		shared, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(second))
		require.NoError(t, err)
		require.Equal(t, original.Nodes[0].ID, shared.Nodes[0].ID)

		first.BlobHash = fakeHash("raw-v2")
		edited, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(first))
		require.NoError(t, err)
		assert.NotEqual(t, original.Nodes[0].ID, edited.Nodes[0].ID)
		kept, err := s.NodeByID(ctx, original.Nodes[0].ID)
		require.NoError(t, err)
		assert.Equal(t, fakeHash("raw-v1"), kept.BlobHash)
		assert.Equal(t, original.Nodes[0].CurrentVersionID, kept.CurrentVersionID)
		assert.Equal(t, first.BlobHash, edited.Nodes[0].BlobHash)
	})
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

func TestFinalizeLocalOperationSettlesCancelRace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	start := func() string {
		operation, err := s.CreateLocalOperation(ctx, StorageOperationKindPhotoImport, `{"source_root":"/camera","destination":"/photos"}`)
		require.NoError(t, err)
		_, err = s.ClaimStorageOperation(ctx, operation.ID)
		require.NoError(t, err)
		return operation.ID
	}
	finalized := start()
	cancelled, err := s.FinalizeLocalOperation(ctx, finalized)
	require.NoError(t, err)
	assert.False(t, cancelled)

	requested := start()
	require.NoError(t, s.RequestStorageOperationCancel(ctx, requested))
	cancelled, err = s.FinalizeLocalOperation(ctx, requested)
	require.NoError(t, err)
	assert.True(t, cancelled)
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
	t.Run("invalid component", func(t *testing.T) {
		before, err := s.NodeByID(ctx, s.RootID())
		require.NoError(t, err)

		_, err = s.EnsurePhotoImportDestination(ctx, "/rollback/.")
		require.ErrorIs(t, err, ErrInvalidName)

		_, err = s.NodeByPath(ctx, "/rollback")
		require.ErrorIs(t, err, ErrNotFound)
		after, err := s.NodeByID(ctx, s.RootID())
		require.NoError(t, err)
		assert.Equal(t, before.Revision, after.Revision)
	})
	t.Run("audited vault", func(t *testing.T) {
		parent, err := s.Mkdir(ctx, s.RootID(), "audited")
		require.NoError(t, err)
		before, err := s.NodeByID(ctx, parent.ID)
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(ctx, parent.ID, "api", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(ctx, plan)
		require.NoError(t, err)

		_, err = s.PreparePhotoImportDestination(ctx, "/audited/new/nested")
		require.ErrorIs(t, err, ErrAuditMutationUnsupported)
		_, err = s.EnsurePhotoImportDestination(ctx, "/audited/new/nested")
		require.ErrorIs(t, err, ErrAuditMutationUnsupported)
		_, err = s.NodeByPath(ctx, "/audited/new")
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.NodeByPath(ctx, "/audited/new/nested")
		require.ErrorIs(t, err, ErrNotFound)
		after, err := s.NodeByID(ctx, parent.ID)
		require.NoError(t, err)
		assert.Equal(t, before.Revision, after.Revision)
	})
}

func mustPhotoAsset(t *testing.T, s *Store, nodeID int64) PhotoAsset {
	t.Helper()
	asset, err := s.PhotoAssetForNode(t.Context(), nodeID)
	require.NoError(t, err)
	return asset
}

func TestPreparePhotoImportDestination(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.PreparePhotoImportDestination(ctx, "/")
	require.NoError(t, err)
	writer, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = s.PreparePhotoImportDestination(readCtx, "/missing/nested")
	require.NoError(t, err)
	require.NoError(t, writer.Rollback())
	_, err = s.NodeByPath(ctx, "/missing")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.EnsurePhotoImportDestination(ctx, "/photos/existing")
	require.NoError(t, err)
	_, err = s.PreparePhotoImportDestination(ctx, "/photos/existing/new")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "file", fakeHash("destination-check"), 4, "text/plain")
	require.NoError(t, err)
	_, err = s.PreparePhotoImportDestination(ctx, "/file/child")
	require.ErrorIs(t, err, ErrNotDir)
	_, err = s.PreparePhotoImportDestination(ctx, "/missing/.")
	require.ErrorIs(t, err, ErrInvalidName)
}
