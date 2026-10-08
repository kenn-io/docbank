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
	require.InDelta(t, scores.Focus, page.Items[0].Quality.Focus, 1e-12)
	for _, text := range []string{"unevaluated:yes", "blur_max:NaN"} {
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
	jpg := browsePhotoNode(t, s, "ready.jpg", browseHash("ready-ownership"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(jpg), document.PhotoQualitySignals{Focus: 0.1}))
	browsePhotoNode(t, s, "ordinary.txt", browseHash("quality-document"), "text/plain")
	browsePhotoNode(t, s, "video.mp4", browseHash("quality-video"), "video/mp4")
	excluded := browsePhotoNode(t, s, "excluded-quality.jpg", browseHash("excluded-quality"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, excluded.ID)
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
}

func TestPhotoQualityUnavailableIsTerminal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "undecodable.jpg", browseHash("undecodable-quality"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualityUnavailable(t.Context(), qualityTarget(node)))
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{Focus: 1}))
	page := browsePhotoPage(t, s, `{}`)
	require.Len(t, page.Items, 1)
	require.Nil(t, page.Items[0].Quality)
	require.True(t, page.Items[0].QualityUnavailable)
	require.Len(t, browsePhotoPage(t, s, `{"filters":{"unevaluated":true}}`).Items, 1)
	require.Empty(t, browsePhotoPage(t, s, `{"filters":{"focus_min":"0"}}`).Items)
	require.Empty(t, browsePhotoPage(t, s, `{"filters":{"focus_max":"1"}}`).Items)
}

func TestPhotoQualityRejectsInvalidStoredRows(t *testing.T) {
	t.Parallel()
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	for name, row := range map[string]string{
		"unknown state":         `'stale',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL`,
		"ready missing a score": `'ready',0,1,0,0,0,0,NULL,0`,
		"unavailable with data": `'unavailable',0,NULL,NULL,NULL,NULL,NULL,NULL,NULL`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			node := browsePhotoNode(t, s, "stored.jpg", browseHash("stored-quality"), "image/jpeg")
			_, err := s.db.Exec(`INSERT INTO photo_quality_signals(content_version_id,evaluator_fingerprint,state,`+
				photoQualityColumns+`) VALUES(?,?,`+row+`)`, node.CurrentVersionID, fingerprints.Evaluator)
			require.NoError(t, err)
			_, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`)}, nil)
			require.ErrorContains(t, err, "invalid photo quality state")
		})
	}
}
