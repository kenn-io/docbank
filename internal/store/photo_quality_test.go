package store

import (
	"bytes"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func qualityTarget(node Node) PhotoVisualPreviewTarget {
	return PhotoVisualPreviewTarget{VersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, Size: node.Size, MediaType: node.MimeType}
}

func TestPhotoQualityRetention(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ready := browsePhotoNode(t, s, "ready.jpg", browseHash("ready-quality"), "image/jpeg")
	browsePhotoNode(t, s, "pending.jpg", browseHash("pending-quality"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Blur: 1, Framing: 0.5}))
	var backup bytes.Buffer
	require.NoError(t, exportBackupMetadataSnapshot(ctx, s.db, &backup))
	require.NotContains(t, backup.String(), "photo_quality_signals")
	restored, err := OpenForRestore(filepath.Join(t.TempDir(), "restore.db"), s.driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.Len(t, browsePhotoPage(t, restored, `{"filters":{"unevaluated":true}}`).Items, 2)
	_, err = s.db.Exec(`UPDATE photo_quality_signals SET evaluator_fingerprint=?`, fakeHash("af"))
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	require.Empty(t, browsePhotoPage(t, s, `{"filters":{"focus_max":"0"}}`).Items)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Blur: 1, Framing: 0.5}))
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	var fingerprint string
	require.NoError(t, s.db.QueryRow(`SELECT group_concat(evaluator_fingerprint) FROM photo_quality_signals`).Scan(&fingerprint))
	require.Equal(t, fingerprints.Evaluator, fingerprint)
	old := qualityTarget(ready)
	ready, _, err = s.ReplaceContent(ctx, ready.ID, ready.Revision, browseHash("replacement-quality"), 20, "image/jpeg")
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals WHERE content_version_id=?`, old.VersionID).Scan(&count))
	require.Equal(t, 1, count)
	require.Error(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Focus: math.NaN()}))
	_, _, err = s.Trash(ctx, ready.ID, ready.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
	require.Zero(t, count)
}

func TestPhotoQualityDisplayAndPublicationFence(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"exclude", "trash", "replace", "display"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node := browsePhotoNode(t, s, "fenced.jpg", browseHash("fenced-"+state), "image/jpeg")
			target := qualityTarget(node)
			targets, err := s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
			require.NoError(t, err)
			require.Empty(t, targets)
			recipe, _ := document.BuiltInVisualPreviewRecipe("grid")
			canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("preview-"+state), 20, recipe)
			_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, &BlobPhysical{Encoding: "raw", StoredBytes: 20})
			require.NoError(t, err)
			targets, err = s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
			require.NoError(t, err)
			require.Equal(t, []PhotoVisualPreviewTarget{target}, targets)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			switch state {
			case "exclude":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			case "trash":
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			case "replace":
				_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("fenced-new"), 20, "image/jpeg")
			case "display":
				other := browsePhotoNode(t, s, "second.jpg", browseHash("second-display"), "image/jpeg")
				second, e := s.PhotoAssetForNode(ctx, other.ID)
				require.NoError(t, e)
				_, e = s.DetachPhotoFile(ctx, second.ID, second.Revision, second.Files[0].ID, PhotoDetachOptions{})
				require.NoError(t, e)
				asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, other.ID, PhotoRoleImage, nil)
				require.NoError(t, err)
				for _, file := range asset.Files {
					if file.NodeID == other.ID {
						_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, new(file.ID))
						break
					}
				}
			}
			require.NoError(t, err)
			require.NoError(t, s.PublishPhotoQualitySignals(ctx, target, document.PhotoQualitySignals{Focus: 0.7}))
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

