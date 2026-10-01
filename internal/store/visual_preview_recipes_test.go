package store

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestVisualPreviewGenerationByRecipe(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
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

func TestVisualPreviewNonActiveRecipesKeepLargeHead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
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
			require.NoError(t, s.db.QueryRow(`SELECT state FROM visual_preview_generations WHERE generation_id=?`, generation.GenerationID).Scan(&storedState))
			require.Equal(t, string(state), storedState)
			view, err := s.ContentVersionVisualPreviewByRecipe(t.Context(), node.CurrentVersionID, generation.RecipeFingerprint)
			require.NoError(t, err)
			require.Equal(t, state, view.Generation.Preview.State)
			_, err = s.db.Exec(`UPDATE visual_preview_generations SET state='invalid' WHERE generation_id=?`, generation.GenerationID)
			require.ErrorContains(t, err, "immutable")
		})
	}
}

func TestMissingPhotoVisualPreviewTargetsAfter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
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
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		if state == "video" {
			require.Equal(t, PhotoKindVideo, asset.Kind)
			continue
		}
		if state == "excluded" {
			_, err = s.db.Exec(`UPDATE photo_assets SET excluded_at='2026-01-01T00:00:00Z' WHERE asset_id=?`, asset.ID)
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
	for _, change := range []string{"trash", "exclude"} {
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
			if change == "trash" {
				_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
			} else {
				_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
			}
			require.NoError(t, err)
			eligible, err := s.PhotoVisualPreviewTargetEligible(t.Context(), targets[0], fingerprint)
			require.NoError(t, err)
			require.False(t, eligible)
		})
	}
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
