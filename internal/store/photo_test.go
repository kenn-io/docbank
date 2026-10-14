package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func authoredPair(t *testing.T, s *Store) PhotoAsset {
	t.Helper()
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("a1"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpg, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	owned, err := s.PhotoAssetForNode(ctx, jpg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, owned.ID, owned.Revision, owned.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	return asset
}

func TestClassifyPhotoMediaMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, mime, filename, kind, role string
		qualifies                        bool
	}{
		{name: "jpeg", mime: "image/jpeg", filename: "a.jpg", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "sony raw MIME", mime: "image/x-sony-arw", filename: "capture.ARW", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "fuji raw MIME", mime: "image/x-fuji-raf", filename: "capture.raw", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "adobe raw MIME", mime: "image/x-adobe-dng", filename: "capture.raw", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "canon raw MIME", mime: "image/x-canon-cr2", filename: "capture.raw", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "nikon raw MIME", mime: "image/x-nikon-nef", filename: "capture.raw", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "webp", mime: "image/webp", filename: "capture.WEBP", kind: PhotoKindPhoto, qualifies: true, role: PhotoRoleImage},
		{name: "video", mime: "video/mp4", filename: "a.mp4", kind: PhotoKindVideo, qualifies: true},
		{name: "audio", mime: "audio/mpeg", filename: "a.mp3", qualifies: false},
		{name: "generic raw", mime: "application/octet-stream", filename: "a.cr2", qualifies: false},
		{name: "generic text", mime: "application/octet-stream", filename: "a.txt", qualifies: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, qualifies, kind := classifyPhotoMedia(test.mime, test.filename)
			assert.Equal(t, test.qualifies, qualifies)
			if test.role != "" {
				assert.Equal(t, test.role, inferPhotoRole(photoNodeFacts(Node{MimeType: test.mime, Name: test.filename})))
			}
			if test.kind != "" {
				assert.Equal(t, test.kind, kind)
			}
			if test.kind == PhotoKindVideo {
				facts := photoNodeFacts(Node{Name: test.filename, MimeType: test.mime})
				assert.Equal(t, "audio_video", facts.MediaFamily)
				assert.Equal(t, PhotoKindVideo, facts.AssetKind)
			}
		})
	}
}

func TestPhotoEnrollmentClassifiesCreatedNodes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "image.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	require.Len(t, asset.Files, 1)
	assert.Equal(t, int64(1), asset.Revision)

	audio, err := s.CreateFile(ctx, s.RootID(), "audio.mp3", fakeHash("c3"), 4, "audio/mpeg")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, audio.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoSettingsRecomputeInheritedAssets(t *testing.T) {
	t.Parallel()
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
	assert.Equal(t, PhotoDisplayDefault, group.DisplaySource)
	assert.Equal(t, PhotoRoleRAW, fileByID(group.Files, *group.DisplayFileID).Role)
	preference := "image"
	settings, err := s.SetPhotoSettings(ctx, 1, &preference)
	require.NoError(t, err)
	assert.Equal(t, int64(2), settings.Revision)
	group, err = s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayVault, group.DisplaySource)
	assert.Equal(t, PhotoRoleImage, fileByID(group.Files, *group.DisplayFileID).Role)
	settings, err = s.SetPhotoSettings(ctx, settings.Revision, nil)
	require.NoError(t, err)
	assert.Nil(t, settings.Preference)
	assert.Equal(t, int64(3), settings.Revision)
	group, err = s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, PhotoDisplayDefault, group.DisplaySource)
	assert.Equal(t, PhotoRoleRAW, fileByID(group.Files, *group.DisplayFileID).Role)
	imageFileID := fileByRole(group.Files, PhotoRoleImage).ID
	group, err = s.SetPhotoDisplay(ctx, group.ID, group.Revision, &imageFileID)
	require.NoError(t, err)
	preference = "raw"
	settings, err = s.SetPhotoSettings(ctx, settings.Revision, &preference)
	require.NoError(t, err)
	assert.Equal(t, int64(4), settings.Revision)
	unchangedGroup, err := s.PhotoAssetByID(ctx, group.ID)
	require.NoError(t, err)
	assert.Equal(t, group.Revision, unchangedGroup.Revision)
	assert.Equal(t, imageFileID, *unchangedGroup.DisplayFileID)
	_, err = s.SetPhotoSettings(ctx, settings.Revision-1, &preference)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Contains(t, receiptOperations(photoReceiptRows(t, s, group.ID)), "settings_recompute")
	rawFileID := fileByRole(group.Files, PhotoRoleRAW).ID
	group, err = s.DetachPhotoFile(ctx, group.ID, group.Revision, imageFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	require.NotNil(t, group.DisplayFileID)
	assert.Equal(t, rawFileID, *group.DisplayFileID)
	assert.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAdmissionSurvivesRenameAndMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "generic.jpg", fakeHash("c001"), 1, "application/octet-stream")
	require.NoError(t, err)

	before, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, image.ID, s.RootID(), "generic.bin", image.Revision)
	require.NoError(t, err)
	renamed, err := s.NodeByID(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, image.MimeType, renamed.MimeType)
	after, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredAdmission, err := target.NodeByID(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, renamed.MimeType, restoredAdmission.MimeType)
	restoredMembership, err := target.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	assert.Equal(t, before, restoredMembership)
}

func TestPhotoMetadataRoundTripValidatesPointersReceiptsAndSourceProvenance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ingestRun, err := s.BeginIngest(ctx, "photo-import", t.TempDir())
	require.NoError(t, err)
	camera := filepath.Join(t.TempDir(), "camera")
	jpeg, err := s.IngestPhotoGroup(ctx, ingestRun, photoImportTestGroup(
		photoImportTestMember(filepath.Join(camera, "IMG.JPG"), PhotoRoleImage, fakeHash("a1"), "image/jpeg")))
	require.NoError(t, err)
	paired, err := s.IngestPhotoGroup(ctx, ingestRun, photoImportTestGroup(
		photoImportTestMember(filepath.Join(camera, "IMG.ARW"), PhotoRoleRAW, fakeHash("b2"), "image/x-sony-arw"),
		photoImportTestMember(filepath.Join(camera, "IMG.JPG"), PhotoRoleImage, fakeHash("a1"), "image/jpeg"),
		photoImportTestMember(filepath.Join(camera, "IMG.ARW.XMP"), PhotoRoleSidecar, browseHash("xmp"), "application/rdf+xml")))
	require.NoError(t, err)
	assert.Equal(t, jpeg.Asset.ID, paired.Asset.ID)
	laterRun, err := s.BeginIngest(ctx, "photo-import", "renamed duplicate")
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, laterRun, photoImportTestGroup(
		photoImportTestMember(filepath.Join(camera, "AAA.ARW"), PhotoRoleRAW, fakeHash("b2"), "image/x-sony-arw")))
	require.NoError(t, err)
	raw := fileByRole(paired.Asset.Files, PhotoRoleRAW)
	asset := paired.Asset
	imageFileID := fileByRole(asset.Files, PhotoRoleImage).ID
	sidecar, err := s.IngestFileExact(ctx, ingestRun, s.RootID(), "two.xmp", browseHash("two.xmp"), 1, "application/rdf+xml", filepath.Join(camera, "two.xmp"), "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &imageFileID)
	require.NoError(t, err)
	otherSidecar, err := s.IngestFileExact(ctx, ingestRun, s.RootID(), "three.xmp", browseHash("three.xmp"), 1, "application/rdf+xml", filepath.Join(camera, "three.xmp"), "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, otherSidecar.ID, PhotoRoleSidecar, &imageFileID)
	require.NoError(t, err)
	secondNode, err := s.CreateFile(ctx, s.RootID(), "b.jpg", fakeHash("b1"), 1, "image/jpeg")
	require.NoError(t, err)
	var first bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &first))
	t.Run("round trip", func(t *testing.T) {
		require.NoError(t, s.ValidateMetadata(ctx))
		assert.Contains(t, first.String(), `"type":"photo_asset"`)
		assert.Contains(t, first.String(), `"operation":"import"`)
		assert.Contains(t, first.String(), jpeg.Nodes[0].CurrentVersionID)
		target := newTestStore(t)
		require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(first.Bytes())))
		var second bytes.Buffer
		require.NoError(t, target.ExportMetadata(ctx, &second))
		assert.Equal(t, first.String(), second.String())
		restored, err := target.PhotoAssetByID(ctx, asset.ID)
		require.NoError(t, err)
		assert.Equal(t, asset.Files, restored.Files)
		assert.Equal(t, asset.ID, restored.ID)
		assert.Equal(t, asset.DisplayFileID, restored.DisplayFileID)
	})
	t.Run("null sidecar pointer", func(t *testing.T) {
		invalidLines := strings.Split(first.String(), "\n")
		found := false
		for index, line := range invalidLines {
			if strings.Contains(line, `"type":"photo_file"`) && strings.Contains(line, fileByRole(paired.Asset.Files, PhotoRoleSidecar).ID) {
				require.Contains(t, line, `"sidecar_of_file_id":"`+raw.ID+`"`)
				invalidLines[index] = strings.Replace(line, `"sidecar_of_file_id":"`+raw.ID+`"`, `"sidecar_of_file_id":null`, 1)
				require.NotEqual(t, line, invalidLines[index])
				found = true
				break
			}
		}
		require.True(t, found)
		invalid := newTestStore(t)
		require.ErrorContains(t, invalid.ImportMetadata(ctx, strings.NewReader(strings.Join(invalidLines, "\n"))), "invalid photo sidecar pointer")
	})
	t.Run("cross-asset sidecar pointer", func(t *testing.T) {
		secondAsset, err := s.PhotoAssetForNode(ctx, secondNode.ID)
		require.NoError(t, err)
		var sidecarFileID string
		for _, file := range asset.Files {
			if file.NodeID == sidecar.ID {
				sidecarFileID = file.ID
				break
			}
		}
		require.NotEmpty(t, sidecarFileID)
		crossAssetLines := strings.Split(first.String(), "\n")
		found := false
		for index, line := range crossAssetLines {
			if strings.Contains(line, `"type":"photo_file"`) && strings.Contains(line, sidecarFileID) {
				require.Contains(t, line, `"sidecar_of_file_id":"`+imageFileID+`"`)
				crossAssetLines[index] = strings.Replace(line, `"sidecar_of_file_id":"`+imageFileID+`"`, `"sidecar_of_file_id":"`+secondAsset.Files[0].ID+`"`, 1)
				require.NotEqual(t, line, crossAssetLines[index])
				found = true
				break
			}
		}
		require.True(t, found)
		crossAsset := newTestStore(t)
		require.ErrorContains(t, crossAsset.ImportMetadata(ctx, strings.NewReader(strings.Join(crossAssetLines, "\n"))), "must point to a same-asset RAW or image")
	})
	for _, lineType := range []string{"photo_asset", "photo_library_settings"} {
		t.Run("receipt without "+lineType, func(t *testing.T) {
			backup := first.String()
			expectedError := "reference missing assets"
			if lineType == "photo_library_settings" {
				preference := "raw"
				_, err := s.SetPhotoSettings(ctx, 1, &preference)
				require.NoError(t, err)
				var exported bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &exported))
				backup = exported.String()
				expectedError = "reference missing assets or settings"
			}
			lines := strings.Split(backup, "\n")
			for index, line := range lines {
				if strings.Contains(line, `"type":"`+lineType+`"`) || (lineType == "photo_asset" && strings.Contains(line, `"type":"photo_file"`)) {
					lines[index] = ""
				}
			}
			target := newTestStore(t)
			require.ErrorContains(t, target.ImportMetadata(ctx, strings.NewReader(strings.Join(lines, "\n"))), expectedError)
		})
	}
	t.Run("source provenance", func(t *testing.T) {
		sources := make(map[string]string)
		for _, file := range asset.Files {
			var selectedSource string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, file.ID).Scan(&selectedSource))
			sources[file.ID] = selectedSource
		}
		rawSource := sources[raw.ID]
		var recordedPath string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT original_path FROM provenance WHERE identity=?`, rawSource).Scan(&recordedPath))
		require.Equal(t, filepath.Join(camera, "IMG.ARW"), recordedPath)
		var alternateSource string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT identity FROM provenance WHERE node_id=? AND original_path LIKE '%AAA.ARW'`, raw.NodeID).Scan(&alternateSource))
		require.NotEqual(t, rawSource, alternateSource)
		for _, variant := range []string{"alternate source", "missing source_provenance_identity", "another node"} {
			t.Run(variant, func(t *testing.T) {
				expectedSource := rawSource
				if variant == "alternate source" {
					expectedSource = alternateSource
				}
				lines := bytes.Split(first.Bytes(), []byte{'\n'})
				for i, line := range lines {
					if !bytes.Contains(line, []byte(`"type":"photo_file"`)) {
						continue
					}
					var file metadataPhotoFile
					require.NoError(t, json.Unmarshal(line, &file))
					if variant == "missing source_provenance_identity" {
						file.SourceProvenanceIdentity = nil
					} else if variant == "alternate source" && file.FileID == raw.ID {
						file.SourceProvenanceIdentity = &alternateSource
					} else if variant == "another node" && file.Role == PhotoRoleImage {
						file.SourceProvenanceIdentity = &rawSource
					}
					lines[i], err = json.Marshal(file)
					require.NoError(t, err)
				}
				restored := newTestStore(t)
				err = restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
				if variant == "another node" {
					require.ErrorContains(t, err, "invalid photo source provenance reference")
					return
				}
				require.NoError(t, err)
				if variant == "alternate source" {
					var reexported bytes.Buffer
					require.NoError(t, restored.ExportMetadata(ctx, &reexported))
					var exportedRAW *metadataPhotoFile
					for line := range bytes.SplitSeq(reexported.Bytes(), []byte{'\n'}) {
						if !bytes.Contains(line, []byte(`"type":"photo_file"`)) {
							continue
						}
						var file metadataPhotoFile
						require.NoError(t, json.Unmarshal(line, &file))
						if file.FileID == raw.ID {
							exportedRAW = &file
							break
						}
					}
					require.NotNil(t, exportedRAW)
					require.Equal(t, &alternateSource, exportedRAW.SourceProvenanceIdentity)
				}
				for _, file := range asset.Files {
					if variant != "missing source_provenance_identity" && file.ID != raw.ID {
						continue
					}
					want := expectedSource
					if variant == "missing source_provenance_identity" {
						want = sources[file.ID]
					}
					var storedSource string
					require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE file_id=?`, file.ID).Scan(&storedSource))
					require.Equal(t, want, storedSource, "%s: %s", variant, file.ID)
				}
			})
		}
	})
}

func TestPhotoMetadataRejectsRoleAndAssetKindMismatch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		update string
	}{
		{name: "member role", update: `UPDATE photo_files SET role='video'`},
		{name: "asset kind", update: `UPDATE photo_assets SET kind='video'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			_, err := s.CreateFile(t.Context(), s.RootID(), "mismatch.jpg", fakeHash(test.name), 1, "image/jpeg")
			require.NoError(t, err)
			require.NoError(t, validatePhotoMetadataState(t.Context(), s.db, currentStorageSchemaVersion))
			_, err = s.db.ExecContext(t.Context(), test.update)
			require.NoError(t, err)
			require.ErrorIs(t, validatePhotoMetadataState(t.Context(), s.db, currentStorageSchemaVersion), ErrInvalidPhotoAsset)
		})
	}
}

func TestPhotoMetadataAcceptsReceiptIndependentRevisionOneState(t *testing.T) {
	t.Parallel()
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
	require.NoError(t, validatePhotoMetadataState(ctx, s.db, currentStorageSchemaVersion))
}

func TestPhotoExplicitVideoAdmitsGenericVideo(t *testing.T) {
	t.Parallel()
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

func TestPhotoSidecarTargetsAndNoDisplayableMember(t *testing.T) {
	t.Parallel()
	for _, role := range []string{PhotoRoleRAW, PhotoRoleImage} {
		t.Run(role, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			mime := "application/octet-stream"
			if role == PhotoRoleImage {
				mime = "image/jpeg"
			}
			target, err := s.CreateFile(ctx, s.RootID(), "target.capture", fakeHash("a1"), 1, mime)
			require.NoError(t, err)
			if role == PhotoRoleImage {
				auto, err := s.PhotoAssetForNode(ctx, target.ID)
				require.NoError(t, err)
				_, err = s.DetachPhotoFile(ctx, auto.ID, auto.Revision, auto.Files[0].ID, PhotoDetachOptions{})
				require.NoError(t, err)
			}
			asset, err := s.PromotePhotoNode(ctx, target.ID, nil, role, "")
			require.NoError(t, err)
			targetFileID := asset.Files[0].ID
			for _, name := range []string{"one.xmp", "two.xmp"} {
				sidecar, err := s.CreateFile(ctx, s.RootID(), name, browseHash(name), 1, "application/rdf+xml")
				require.NoError(t, err)
				asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &targetFileID)
				require.NoError(t, err)
				var sidecarReceipt photoReceiptRow
				for _, receipt := range photoReceiptRows(t, s, asset.ID) {
					if receipt.Operation == "attach" && strings.Contains(receipt.AfterJSON, `"node_id":`+strconv.FormatInt(sidecar.ID, 10)) {
						sidecarReceipt = receipt
						break
					}
				}
				assert.NotEmpty(t, sidecarReceipt.AfterJSON)
			}
			_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targetFileID, PhotoDetachOptions{})
			require.ErrorIs(t, err, ErrInvalidPhotoAsset)
			asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targetFileID, PhotoDetachOptions{ClearDependentSidecars: true})
			require.NoError(t, err)
			assert.Empty(t, asset.Files)
			assert.Equal(t, PhotoDisplayNone, asset.DisplaySource)
			assert.Nil(t, asset.DisplayFileID)
		})
	}
}

func TestPhotoExcludePromotePreservesIdentityAndReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision+1, true)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.SetPhotoAssetExcluded(ctx, "00000000-0000-4000-8000-000000000001", 0, true)
	require.ErrorIs(t, err, ErrNotFound)
	beforeID := asset.ID
	updated, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	assert.Equal(t, beforeID, updated.ID)
	assert.NotNil(t, updated.ExcludedAt)
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

func TestPhotoConcurrentRevisionMutationHasOneWinner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "photo.jpg")
	var wait sync.WaitGroup
	results := make(chan error, 2)
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	var winners, staleWrites int
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, ErrStaleRevision)
			staleWrites++
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, staleWrites)
}

func TestPhotoPromoteUnownedNodeRejectsRevisionPrecondition(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, test := range []struct {
		name          string
		pruneOriginal bool
	}{{"JPEG to PDF and revert", false}, {"prune JPEG under PDF", true}} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			image, err := s.CreateFile(ctx, s.RootID(), "a.jpg", fakeHash("a1"), 1, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, image.ID)
			require.NoError(t, err)
			originalVersionID := image.CurrentVersionID
			updated, replacement, err := s.ReplaceContent(ctx, image.ID, image.Revision, fakeHash("b2"), 1, "application/pdf")
			require.NoError(t, err)
			require.NoError(t, validatePhotoMetadataState(ctx, s.db, currentStorageSchemaVersion))
			assetAfter, err := s.PhotoAssetForNode(ctx, image.ID)
			require.NoError(t, err)
			assert.Equal(t, asset.ID, assetAfter.ID)
			assert.Equal(t, asset.Revision, assetAfter.Revision)
			assert.Equal(t, updated.ID, image.ID)
			promoted, err := s.PromotePhotoNode(ctx, image.ID, &assetAfter.Revision, PhotoRoleImage, PhotoKindPhoto)
			require.NoError(t, err)
			assert.Equal(t, assetAfter.Revision, promoted.Revision)
			prunedID := originalVersionID
			if !test.pruneOriginal {
				updated, _, _, err = s.RevertContent(ctx, image.ID, updated.Revision, originalVersionID)
				require.NoError(t, err)
				assert.Equal(t, asset.ID, mustPhotoAsset(t, s, image.ID).ID)
				prunedID = replacement.ID
			}
			_, err = s.PruneContentVersions(ctx, image.ID, updated.Revision, VersionPruneSelector{VersionIDs: []string{prunedID}}, true)
			require.NoError(t, err)
			_, err = s.ContentVersionByID(ctx, prunedID)
			require.ErrorIs(t, err, ErrNotFound)
			require.NoError(t, validatePhotoMetadataState(ctx, s.db, currentStorageSchemaVersion))
			retained, err := s.PhotoAssetForNode(ctx, image.ID)
			require.NoError(t, err)
			assert.Equal(t, asset.ID, retained.ID)
			assert.Equal(t, PhotoRoleImage, retained.Files[0].Role)
		})
	}
}

func TestPhotoPolicy(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, validatePhotoAssetPointers(PhotoAsset{ID: "x", Kind: PhotoKindPhoto, Revision: 1, DisplayFileID: new("missing")}, nil), ErrInvalidPhotoAsset)
	assert.False(t, photoRoleValid("primary"))
	assert.True(t, photoPreferenceValid(new("image")))
	assert.False(t, photoPreferenceValid(new("video")))
}

func TestPhotoExplicitRawAdmissionAndTargetBoundaries(t *testing.T) {
	t.Parallel()
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
	t.Run("camera MIME after automatic asset detach", func(t *testing.T) {
		node, err := s.CreateFile(ctx, s.RootID(), "camera.raw", fakeHash("ca01"), 1, "image/x-sony-arw")
		require.NoError(t, err)
		automatic, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		_, err = s.DetachPhotoFile(ctx, automatic.ID, automatic.Revision, automatic.Files[0].ID, PhotoDetachOptions{})
		require.NoError(t, err)
		explicit, err := s.PromotePhotoNode(ctx, node.ID, nil, PhotoRoleRAW, "")
		require.NoError(t, err)
		assert.Equal(t, PhotoRoleRAW, explicit.Files[0].Role)
	})
}

func TestPhotoAttachRequiresExplicitRawForNonqualifyingNodes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Run("excluded asset on trashed node", func(t *testing.T) {
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
	})
}

func TestPhotoEnrollmentSkipsEmailChildAndKeepsProcessedSource(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
