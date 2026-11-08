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
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Blur: 1, Framing: 0.5}, ""))
	var backup bytes.Buffer
	require.NoError(t, exportBackupMetadataSnapshot(ctx, s.db, &backup))
	require.NotContains(t, backup.String(), "photo_quality_signals")
	restored, err := OpenForRestore(t.Context(), filepath.Join(t.TempDir(), "restore.db"), s.driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.Len(t, browsePhotoPage(t, restored, `{"filters":{"unevaluated":true}}`).Items, 2)
	_, err = s.db.ExecContext(t.Context(), `UPDATE photo_quality_signals SET evaluator_fingerprint=?`, fakeHash("af"))
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	require.Empty(t, browsePhotoPage(t, s, `{"filters":{"focus_max":"0"}}`).Items)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Blur: 1, Framing: 0.5}, ""))
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	var fingerprint string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT group_concat(evaluator_fingerprint) FROM photo_quality_signals`).Scan(&fingerprint))
	require.Equal(t, fingerprints.Evaluator, fingerprint)
	old := qualityTarget(ready)
	ready, _, err = s.ReplaceContent(ctx, ready.ID, ready.Revision, browseHash("replacement-quality"), 20, "image/jpeg")
	require.NoError(t, err)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 2)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_quality_signals WHERE content_version_id=?`, old.VersionID).Scan(&count))
	require.Equal(t, 1, count)
	require.Error(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(ready), document.PhotoQualitySignals{Focus: math.NaN()}, ""))
	_, _, err = s.Trash(ctx, ready.ID, ready.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
	require.Zero(t, count)
}

func checkPhotoQualityDisplayAndPublicationFence(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	for _, state := range []string{"exclude", "trash", "replace", "display"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t)
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
			require.NoError(t, s.PublishPhotoQualitySignals(ctx, target, document.PhotoQualitySignals{Focus: 0.7}, ""))
			var count int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_quality_signals`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestPhotoQualityFaceInputsFence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "faces.jpg", browseHash("face-quality"), "image/jpeg")
	recipe, _ := document.BuiltInVisualPreviewRecipe("grid")
	canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("faces-preview"), 20, recipe)
	_, err := s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, &BlobPhysical{Encoding: "raw", StoredBytes: 20})
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), document.PhotoQualitySignals{Focus: 0.7}, ""))
	id, err := newUUIDv4()
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, id, document.FaceBox{Width: 0.5, Height: 0.5}, "")
	require.NoError(t, err)
	targets, err := s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	report := document.PhotoFaceReport{FaceID: face.FaceID, Sharpness: new(0.8), Prominence: 0.25}
	_, inputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	scores := document.PhotoQualitySignals{Focus: 0.7, FaceReports: []document.PhotoFaceReport{report}}
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), scores, inputs))
	read, err := s.photoQualityForVersions(ctx, s.db, []string{node.CurrentVersionID})
	require.NoError(t, err)
	require.Len(t, read.signals[node.CurrentVersionID].FaceReports, 1)
	targets, err = s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
	otherID, err := newUUIDv4()
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, otherID, document.FaceBox{X: 0.5, Width: 0.5, Height: 0.5}, "")
	require.NoError(t, err)
	// A queued publication for the previous face set cannot create a cache row.
	_, err = s.db.ExecContext(t.Context(), `DELETE FROM photo_quality_signals WHERE content_version_id=?`, node.CurrentVersionID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), scores, inputs))
	read, err = s.photoQualityForVersions(ctx, s.db, []string{node.CurrentVersionID})
	require.NoError(t, err)
	require.NotContains(t, read.signals, node.CurrentVersionID)
}

func TestPhotoQualityDismissedFaceExcluded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, face := faceForDecisionTest(t, s, "")
	decideFaceForTest(t, s, node, face.FaceID, "", "dismiss")
	faces, _, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces)
}

func TestPhotoQualityGroupRejectionThenDismissInvalidatesFaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	pair := faceCandidatePair(t, s)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSnapshot(t, s), []PersonMatchCandidate{pair.group}))
	groups, _, err := s.FaceCandidates(ctx, 24, 0)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	_, err = s.DecidePersonCandidate(ctx, CandidateDecision{CandidateID: groups[0].CandidateID, ExpectedRevision: groups[0].Revision, Action: "reject", FaceOnly: true})
	require.NoError(t, err)
	_, before, err := s.PhotoQualityFaces(ctx, pair.node.CurrentVersionID)
	require.NoError(t, err)
	reports := []document.PhotoFaceReport{{FaceID: pair.first.FaceID, Sharpness: new(0.8), Prominence: 0.25}, {FaceID: pair.second.FaceID, Sharpness: new(0.7), Prominence: 0.25}}
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(pair.node), document.PhotoQualitySignals{Focus: 0.7, FaceReports: reports}, before))
	read, err := s.photoQualityForVersions(ctx, s.db, []string{pair.node.CurrentVersionID})
	require.NoError(t, err)
	require.Len(t, read.signals[pair.node.CurrentVersionID].FaceReports, 2)
	decideFaceForTest(t, s, pair.node, pair.first.FaceID, "", "dismiss")
	faces, after, err := s.PhotoQualityFaces(ctx, pair.node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	if before == after {
		t.Error("individual dismissal must change the quality signature after group rejection")
	}
	read, err = s.photoQualityForVersions(ctx, s.db, []string{pair.node.CurrentVersionID})
	require.NoError(t, err)
	for _, report := range read.signals[pair.node.CurrentVersionID].FaceReports {
		if report.FaceID == pair.first.FaceID {
			t.Error("cached quality still reports the dismissed face")
		}
	}
}
