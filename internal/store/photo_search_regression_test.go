package store

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoSnapshotSuggestionBadgesKeepRankOrderAndCountTowardSizeLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	machine := browsePhotoNode(t, s, "machine.jpg", browseHash("machine"), "image/jpeg")
	plain := browsePhotoNode(t, s, "harbor.jpg", browseHash("harbor"), "image/jpeg")
	machineAsset, err := s.PhotoAssetForNode(t.Context(), machine.ID)
	require.NoError(t, err)
	plainAsset, err := s.PhotoAssetForNode(t.Context(), plain.ID)
	require.NoError(t, err)
	putPhotoSuggestion(t, s, machineAsset, document.PhotoDescription{Caption: "harbor"})
	for _, mode := range []string{"lexical", "hybrid", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			ranking := &PhotoRanking{RequestedMode: "hybrid", ActualMode: mode}
			expected := []string{plainAsset.ID, machineAsset.ID}
			if mode != "lexical" {
				ranking.Members = []SnapshotMember{
					{AssetID: machineAsset.ID, NodeID: machine.ID, ContentVersionID: machine.CurrentVersionID},
					{AssetID: plainAsset.ID, NodeID: plain.ID, ContentVersionID: plain.CurrentVersionID},
				}
				expected = []string{machineAsset.ID, plainAsset.ID}
			}
			request := SnapshotRequest{Photos: true, PhotoRanking: ranking, Query: snapshotTestQuery(t, `{"mode":"hybrid","text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)}
			projection, err := s.MaterializeQuerySnapshot(t.Context(), request)
			require.NoError(t, err)
			require.Len(t, projection.Rows, 2)
			for i, row := range projection.Rows {
				require.Equal(t, expected[i], row.AssetID)
				require.Equal(t, mode != "semantic" && row.AssetID == machineAsset.ID, row.SuggestionMatch)
				if row.SuggestionMatch {
					encoded, err := json.Marshal(row)
					require.NoError(t, err)
					_, err = s.materializeQuerySnapshot(t.Context(), request, snapshotMaterializeOptions{MaxRowBytes: int64(len(encoded) - 1)})
					require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
				}
			}
		})
	}
}

func TestPhotoFallbackSnapshotCapsMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 1000 {
		name := fmt.Sprintf("sunset-%04d.jpg", i)
		browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
	}
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	for _, count := range []int{1000, 1001} {
		if count == 1001 {
			browsePhotoNode(t, s, "sunset-1000.jpg", browseHash("sunset-1000.jpg"), "image/jpeg")
		}
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			request := SnapshotRequest{Photos: true, PageSize: 250, Query: snapshotTestQuery(t, `{"mode":"hybrid","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "hybrid", ActualMode: "lexical", FallbackReason: "provider_unavailable"}}
			page, err := service.Create(t.Context(), "owner", request)
			require.NoError(t, err)
			require.EqualValues(t, 1000, page.Total)
			require.Equal(t, count > 1000, page.PhotoRanking.Truncated)
		})
	}
}
