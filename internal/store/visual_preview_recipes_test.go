package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func checkVisualPreviewGenerationByRecipe(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", fakeHash("41"), 12, "image/jpeg")
	require.NoError(t, err)
	for _, edge := range []int{512, 2560} {
		recipe := visualPreviewRecipe()
		recipe.MaxEdgePixels = edge
		canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("42"), 9, recipe)
		generation, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		view, err := s.ContentVersionVisualPreviewByRecipe(t.Context(), node.CurrentVersionID, generation.RecipeFingerprint)
		require.NoError(t, err)
		require.Equal(t, generation, view.Generation)
		require.Equal(t, node.CurrentVersionID, view.Version.ID)
		require.Equal(t, generation.CreatedAt, view.PublishedAt)
	}
	_, err = s.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
	require.ErrorIs(t, err, ErrNotFound)
}

func checkVisualPreviewNonActiveRecipesKeepLargeHead(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", fakeHash("51"), 12, "image/jpeg")
	require.NoError(t, err)
	recipe := visualPreviewRecipe()
	large, err := s.PublishVisualPreview(t.Context(), node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("52"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	for _, edge := range []int{512, 2560} {
		recipe.MaxEdgePixels = edge
		_, err = s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("52"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		active, err := s.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		require.Equal(t, large.GenerationID, active.Generation.GenerationID)
	}
}

func TestVisualPreviewRecipeStates(t *testing.T) {
	t.Parallel()
	for _, state := range []document.VisualPreviewState{document.VisualPreviewReady, document.VisualPreviewUnsupported, document.VisualPreviewFailed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			node, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", fakeHash("61"), 12, "image/jpeg")
			require.NoError(t, err)
			recipe := visualPreviewRecipe()
			recipe.MaxEdgePixels = 512
			var canonical []byte
			if state != document.VisualPreviewReady {
				canonical = terminalVisualPreviewWithRecipe(t, node.BlobHash, recipe, state, "decode_failed")
			}
			var physical *BlobPhysical
			if state == document.VisualPreviewReady {
				canonical = readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("62"), 9, recipe)
				physical = &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9}
			}
			generation, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, physical)
			require.NoError(t, err)
			var storedState string
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT state FROM visual_preview_generations WHERE generation_id=?`, generation.GenerationID).Scan(&storedState))
			require.Equal(t, string(state), storedState)
			view, err := s.ContentVersionVisualPreviewByRecipe(t.Context(), node.CurrentVersionID, generation.RecipeFingerprint)
			require.NoError(t, err)
			require.Equal(t, state, view.Generation.Preview.State)
			_, err = s.db.ExecContext(t.Context(), `UPDATE visual_preview_generations SET state='invalid' WHERE generation_id=?`, generation.GenerationID)
			require.ErrorContains(t, err, "immutable")
		})
	}
}

func checkMissingPhotoVisualPreviewTargetsAfter(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	recipe := visualPreviewRecipe()
	recipe.MaxEdgePixels = 512
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	var versions []string
	var readyNode Node
	for index, state := range []string{"missing", "ready", "unsupported", "failed", "excluded", "trashed", "video"} {
		name, mediaType := state+".jpg", "image/jpeg"
		if state == "video" {
			name, mediaType = state+".mp4", "video/mp4"
		}
		node, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash(fmt.Sprintf("%02d", 90+index)), 12, mediaType)
		require.NoError(t, err)
		var assetID, kind string
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT a.asset_id,a.kind FROM photo_assets a JOIN photo_files f ON f.asset_id=a.asset_id WHERE f.node_id=$1`, node.ID).Scan(&assetID, &kind))
		if state == "video" {
			require.Equal(t, string(PhotoKindVideo), kind)
			continue
		}
		if state == "excluded" {
			_, err = s.db.ExecContext(t.Context(), `UPDATE photo_assets SET excluded_at='2026-01-01T00:00:00Z' WHERE asset_id=$1`, assetID)
			require.NoError(t, err)
			continue
		}
		if state == "trashed" {
			_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
			require.NoError(t, err)
			continue
		}
		if state == "missing" {
			versions = append(versions, node.CurrentVersionID)
			continue
		}
		var canonical []byte
		if state != "ready" {
			canonical = terminalVisualPreviewWithRecipe(t, node.BlobHash, recipe, document.VisualPreviewState(state), "decode_failed")
		}
		var physical *BlobPhysical
		if state == "ready" {
			canonical = readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("76"), 9, recipe)
			physical = &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9}
		}
		if state == "ready" {
			readyNode = node
		}
		_, err = s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, physical)
		require.NoError(t, err)
	}
	targets, err := s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, versions[0], targets[0].VersionID)
	eligible, err := s.PhotoVisualPreviewTargetEligible(t.Context(), targets[0], fingerprint)
	require.NoError(t, err)
	require.True(t, eligible)
	stale := targets[0]
	stale.SourceSHA256 = fakeHash("ab")
	eligible, err = s.PhotoVisualPreviewTargetEligible(t.Context(), stale, fingerprint)
	require.NoError(t, err)
	require.False(t, eligible)
	targets, err = s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, versions[0], 100)
	require.NoError(t, err)
	require.Empty(t, targets)
	_, replaced, err := s.ReplaceContent(t.Context(), readyNode.ID, readyNode.Revision, fakeHash("79"), 12, "image/jpeg")
	require.NoError(t, err)
	targets, err = s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	require.Less(t, targets[0].VersionID, targets[1].VersionID)
	require.ElementsMatch(t, []string{versions[0], replaced.ID}, []string{targets[0].VersionID, targets[1].VersionID})
	page, err := s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, targets[0].VersionID, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, targets[1], page[0])
}

func TestPhotoVisualPreviewTargetEligibilityRechecksListedTargets(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"trash", "exclude", "replace", "publish"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			node, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", fakeHash("89"), 12, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			recipe := visualPreviewRecipe()
			recipe.MaxEdgePixels = 512
			_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
			require.NoError(t, err)
			targets, err := s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			switch change {
			case "trash":
				_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
			case "exclude":
				_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
			case "replace":
				_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("8c"), 12, "image/jpeg")
			case "publish":
				canonical := terminalVisualPreviewWithRecipe(t, node.BlobHash, recipe,
					document.VisualPreviewUnsupported, "unsupported_media_type")
				_, err = s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, nil)
			}
			require.NoError(t, err)
			eligible, err := s.PhotoVisualPreviewTargetEligible(t.Context(), targets[0], fingerprint)
			require.NoError(t, err)
			require.False(t, eligible)
		})
	}
}

func TestPhotoVisualPreviewTargetWithoutMediaType(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	untagged, err := s.CreateFile(t.Context(), s.RootID(), "untagged.jpg", fakeHash("8a"), 12, "")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(t.Context(), untagged.ID)
	require.NoError(t, err)
	tagged, err := s.CreateFile(t.Context(), s.RootID(), "tagged.jpg", fakeHash("8b"), 12, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(t.Context(), tagged.ID)
	require.NoError(t, err)
	var stored sql.NullString
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT mime_type FROM content_versions WHERE version_id=?`, untagged.CurrentVersionID).Scan(&stored))
	require.False(t, stored.Valid)
	recipe := visualPreviewRecipe()
	recipe.MaxEdgePixels = 512
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	targets, err := s.MissingPhotoVisualPreviewTargetsAfter(t.Context(), fingerprint, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	byVersion := map[string]PhotoVisualPreviewTarget{}
	for _, target := range targets {
		byVersion[target.VersionID] = target
	}
	require.Empty(t, byVersion[untagged.CurrentVersionID].MediaType)
	require.Equal(t, "image/jpeg", byVersion[tagged.CurrentVersionID].MediaType)
	for _, target := range targets {
		eligible, err := s.PhotoVisualPreviewTargetEligible(t.Context(), target, fingerprint)
		require.NoError(t, err)
		require.True(t, eligible)
	}
}

func TestPhotoVisualPreviewTargetsFollowDisplaySelection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "photo.cr2", fakeHash("8d"), 12, "application/octet-stream")
	require.NoError(t, err)
	jpeg, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("8e"), 12, "image/jpeg")
	require.NoError(t, err)
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	jpegFile := fileByRole(asset.Files, PhotoRoleImage)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpegFile.ID)
	require.NoError(t, err)
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(visualPreviewRecipe())
	require.NoError(t, err)
	targets, err := s.MissingPhotoVisualPreviewTargetsAfter(ctx, fingerprint, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, jpeg.CurrentVersionID, targets[0].VersionID)

	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &rawFile.ID)
	require.NoError(t, err)
	eligible, err := s.PhotoVisualPreviewTargetEligible(ctx, targets[0], fingerprint)
	require.NoError(t, err)
	require.False(t, eligible)
	targets, err = s.MissingPhotoVisualPreviewTargetsAfter(ctx, fingerprint, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, raw.CurrentVersionID, targets[0].VersionID)
	eligible, err = s.PhotoVisualPreviewTargetEligible(ctx, targets[0], fingerprint)
	require.NoError(t, err)
	require.True(t, eligible)
}

func TestRestoreRetainsVisualPreviewRecipes(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	node, err := source.CreateFile(t.Context(), source.RootID(), "photo.jpg", fakeHash("81"), 12, "image/jpeg")
	require.NoError(t, err)
	var outputs []string
	for index, edge := range []int{512, 2560} {
		recipe := visualPreviewRecipe()
		recipe.MaxEdgePixels = edge
		output := fakeHash(fmt.Sprintf("%02d", 82+index))
		outputs = append(outputs, output)
		_, err = source.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, output, 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
	}
	var exported bytes.Buffer
	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	for _, edge := range []int{512, 2560} {
		recipe := visualPreviewRecipe()
		recipe.MaxEdgePixels = edge
		_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
		require.NoError(t, err)
		_, err = target.ContentVersionVisualPreviewByRecipe(t.Context(), node.CurrentVersionID, fingerprint)
		require.NoError(t, err)
	}
	unreachable, err := target.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	for _, candidate := range unreachable {
		require.NotContains(t, outputs, candidate.Hash)
	}
}

// Paired face state is retained directly: this contract qualifies preview
// discovery and reads, rather than claiming the face-write workflow is ported.
func checkFaceMemberGridPreviews(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "pair.dng", fakeHash("d1"), 12, "image/x-adobe-dng")
	require.NoError(t, err)
	image, err := s.CreateFile(ctx, s.RootID(), "pair.jpg", fakeHash("d2"), 12, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	singleton, err := s.PhotoAssetForNode(ctx, raw.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, singleton.ID, singleton.Revision, singleton.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, PhotoRoleRAW, nil)
	require.NoError(t, err)
	imageFile := fileByRole(asset.Files, PhotoRoleImage)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &imageFile.ID)
	require.NoError(t, err)
	faceID := "00000000-0000-4000-8000-000000000001"
	_, err = s.db.ExecContext(ctx, `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,revision,created_at) VALUES($1,$2,0,0,0.2,0.2,1,$3)`, faceID, raw.CurrentVersionID, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	grid, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	targets, err := s.MissingPhotoVisualPreviewTargetsAfter(ctx, grid.GridRecipe, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	var source PhotoVisualPreviewTarget
	for _, target := range targets {
		if target.VersionID == raw.CurrentVersionID {
			source = target
		}
	}
	require.Equal(t, raw.CurrentVersionID, source.VersionID)
	eligible, err := s.PhotoVisualPreviewTargetEligible(ctx, source, grid.GridRecipe)
	require.NoError(t, err)
	require.True(t, eligible)
	_, fit, err := document.MarshalVisualPreviewRecipeV1(visualPreviewRecipe())
	require.NoError(t, err)
	targets, err = s.MissingPhotoVisualPreviewTargetsAfter(ctx, fit, "", 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, image.CurrentVersionID, targets[0].VersionID)
	eligible, err = s.PhotoVisualPreviewTargetEligible(ctx, source, fit)
	require.NoError(t, err)
	require.False(t, eligible)
	recipe, err := document.BuiltInVisualPreviewRecipe("grid")
	require.NoError(t, err)
	generation, err := s.PublishVisualPreviewGeneration(ctx, raw.CurrentVersionID, readyVisualPreviewWithRecipe(t, raw.BlobHash, fakeHash("d3"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	require.NoError(t, s.FacePhotoPreviewSource(ctx, asset.ID, raw.CurrentVersionID, faceID))
	view, err := s.PhotoVisualPreviewByGeneration(ctx, asset.ID, generation.GenerationID, "", faceID)
	require.NoError(t, err)
	require.Equal(t, raw.CurrentVersionID, view.Version.ID)
	_, _, err = s.ReplaceContent(ctx, raw.ID, raw.Revision, fakeHash("d4"), 12, "image/x-adobe-dng")
	require.NoError(t, err)
	require.ErrorIs(t, s.FacePhotoPreviewSource(ctx, asset.ID, raw.CurrentVersionID, faceID), ErrNotFound)
	_, err = s.PhotoVisualPreviewByGeneration(ctx, asset.ID, generation.GenerationID, "", faceID)
	require.ErrorIs(t, err, ErrNotFound)
}
