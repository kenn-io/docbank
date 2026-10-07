package store

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
)

func TestPhotoQualityScalarBounds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "measured.jpg", browseHash("measured-bounds"), "image/jpeg")
	scores := document.PhotoQualitySignals{Focus: 0.704, Blur: 0.296, Brightness: 0.5, ColorRed: 0.4, ColorGreen: 0.5, ColorBlue: 0.6, Framing: 0.7, Aesthetics: 0.8}
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), scores))
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{Focus: 1}))
	browsePhotoNode(t, s, "pending.jpg", browseHash("pending-bounds"), "image/jpeg")
	page := browsePhotoPage(t, s, `{"filters":{"focus_max":"0.704"}}`)
	require.Len(t, page.Items, 1)
	require.Equal(t, node.ID, page.Items[0].NodeID)
	require.NotNil(t, page.Items[0].Quality)
	require.Equal(t, scores.Focus, page.Items[0].Quality.Focus)
	for _, text := range []string{"focus_min:0.5*", "unevaluated:yes", "blur_max:NaN"} {
		_, err := s.CompileQuery(t.Context(), snapshotTestQuery(t, `{"syntax":"advanced","text":"`+text+`"}`))
		require.Error(t, err)
	}
	for _, tc := range []struct {
		name  string
		value float64
	}{{"focus", scores.Focus}, {"blur", scores.Blur}, {"brightness", scores.Brightness}, {"color_red", scores.ColorRed}, {"color_green", scores.ColorGreen}, {"color_blue", scores.ColorBlue}, {"framing", scores.Framing}, {"aesthetics", scores.Aesthetics}} {
		for _, op := range []string{"min", "max"} {
			field := tc.name + "_" + op
			for _, typed := range []bool{true, false} {
				raw := fmt.Sprintf(`{"syntax":"advanced","text":"%s:%g"}`, field, tc.value)
				if typed {
					raw = fmt.Sprintf(`{"filters":{"%s":"%g"}}`, field, tc.value)
				}
				require.Len(t, browsePhotoPage(t, s, raw).Items, 1, raw)
			}
			excluded := tc.value + 0.001
			if op == "max" {
				excluded = tc.value - 0.001
			}
			raw := fmt.Sprintf(`{"filters":{"%s":"%g"}}`, field, excluded)
			require.Empty(t, browsePhotoPage(t, s, raw).Items, raw)
		}
	}
}

func TestPhotoQualityPendingDisplayOwnership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw := browsePhotoNode(t, s, "pending-pair.raw", browseHash("pending-pair-raw"), "image/x-raw")
	jpg := browsePhotoNode(t, s, "pending-pair.jpg", browseHash("pending-pair-jpg"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, jpg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.PhotoAssetForNode(ctx, raw.ID)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	var rawFileID, jpgFileID string
	for _, file := range asset.Files {
		if file.NodeID == raw.ID {
			rawFileID = file.ID
		}
		if file.NodeID == jpg.ID {
			jpgFileID = file.ID
		}
	}
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpgFileID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(jpg), document.PhotoQualitySignals{Focus: 0.1}))
	browsePhotoNode(t, s, "ordinary.txt", browseHash("quality-document"), "text/plain")
	browsePhotoNode(t, s, "video.mp4", browseHash("quality-video"), "video/mp4")
	excluded := browsePhotoNode(t, s, "excluded-quality.jpg", browseHash("excluded-quality"), "image/jpeg")
	asset, err = s.PhotoAssetForNode(ctx, excluded.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	missing := browsePhotoNode(t, s, "missing-preview.jpg", browseHash("quality-missing-preview"), "image/jpeg")
	unsupported := browsePhotoNode(t, s, "unsupported-preview.jpg", browseHash("quality-unsupported-preview"), "image/jpeg")
	missingMIME := browsePhotoNode(t, s, "missing-mime.jpg", browseHash("quality-missing-mime"), "")
	genericMIME := browsePhotoNode(t, s, "generic-mime.jpg", browseHash("quality-generic-mime"), "application/octet-stream")
	recipe, err := document.BuiltInVisualPreviewRecipe("grid")
	require.NoError(t, err)
	canonical := terminalVisualPreviewWithRecipe(t, unsupported.BlobHash, recipe, document.VisualPreviewUnsupported, "unsupported_format")
	_, err = s.PublishVisualPreview(ctx, unsupported.CurrentVersionID, canonical, nil)
	require.NoError(t, err)
	check := func(pending, ready []int64) {
		t.Helper()
		for _, tc := range []struct {
			query string
			want  []int64
		}{
			{`{"filters":{"unevaluated":true}}`, pending},
			{`{"syntax":"advanced","text":"unevaluated:false"}`, ready},
		} {
			value := snapshotTestQuery(t, tc.query)
			snapshot, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
			require.NoError(t, err)
			var documentIDs []int64
			for _, row := range snapshot.Rows {
				documentIDs = append(documentIDs, row.NodeID)
			}
			require.ElementsMatch(t, tc.want, documentIDs, tc.query)
			page := browsePhotoPage(t, s, tc.query)
			var photoIDs []int64
			for _, row := range page.Items {
				photoIDs = append(photoIDs, row.NodeID)
			}
			require.ElementsMatch(t, tc.want, photoIDs, tc.query)
		}
	}
	check([]int64{missing.ID, unsupported.ID, missingMIME.ID, genericMIME.ID}, []int64{jpg.ID})
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(missingMIME), document.PhotoQualitySignals{}))
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(genericMIME), document.PhotoQualitySignals{}))
	check([]int64{missing.ID, unsupported.ID}, []int64{jpg.ID, missingMIME.ID, genericMIME.ID})

	asset, err = s.PhotoAssetForNode(ctx, raw.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &rawFileID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(raw), document.PhotoQualitySignals{Focus: 0.8}))
	_, err = s.CreateSavedQuery(ctx, "Selected focus", "", SavedQueryKindQuery, []byte(`{"filters":{"focus_min":"0.7"}}`))
	require.NoError(t, err)
	q := `{"syntax":"advanced","text":"saved:\"Selected focus\""}`
	require.Len(t, browsePhotoPage(t, s, q).Items, 1)
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpgFileID)
	require.NoError(t, err)
	require.Empty(t, browsePhotoPage(t, s, q).Items)
	require.Len(t, browsePhotoPage(t, s, `{"syntax":"advanced","text":"extension:raw AND focus_max:0.2"}`).Items, 1)
	snapshot, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, q)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	require.Equal(t, raw.ID, snapshot.Rows[0].NodeID)
}
