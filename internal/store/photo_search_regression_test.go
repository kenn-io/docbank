package store

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoSnapshotSuggestionBadgesKeepRankOrder(t *testing.T) {
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
		for _, mode := range []string{"semantic", "hybrid"} {
			t.Run(fmt.Sprintf("%s/%d", mode, count), func(t *testing.T) {
				request := SnapshotRequest{Photos: true, PageSize: 250, Query: snapshotTestQuery(t, fmt.Sprintf(`{"mode":%q,"text":"sunset","sort":{"field":"relevance","direction":"desc"}}`, mode)), PhotoRanking: &PhotoRanking{RequestedMode: mode, ActualMode: "lexical", FallbackReason: "provider_unavailable"}}
				page, err := service.Create(t.Context(), "owner", request)
				require.NoError(t, err)
				require.EqualValues(t, 1000, page.Total)
				require.Equal(t, count > 1000, page.PhotoRanking.Truncated)
				var ids []string
				for {
					for _, row := range page.Rows {
						ids = append(ids, row.AssetID)
					}
					if page.NextCursor == "" {
						break
					}
					page, err = service.PhotoPage(t.Context(), "owner", page.SnapshotID, page.NextCursor, request)
					require.NoError(t, err)
					require.Equal(t, count > 1000, page.PhotoRanking.Truncated)
				}
				require.Len(t, ids, 1000)
				lexical := request.Query
				lexical.Mode = "lexical"
				compiled, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), lexical, queryResolver{q: s.db})
				require.NoError(t, err)
				ranked, err := rankedPhotoRows(t.Context(), s.db, compiled, "", CoverageSelection{}, 1001)
				require.NoError(t, err)
				expected := make([]string, 1000)
				for i := range expected {
					expected[i] = ranked[i].AssetID
				}
				require.Equal(t, expected, ids)
				album, err := s.CreatePhotoSet(t.Context(), fmt.Sprintf("Captured %s %d", mode, count))
				require.NoError(t, err)
				album, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: ids})
				require.NoError(t, err)
				var count int
				require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_set_members WHERE set_id=?`, album.ID).Scan(&count))
				require.Equal(t, 1000, count)
			})
		}
	}
}

func TestPhotoSuggestionMarksBeyondSQLiteBindLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	machine := albumAsset(t, s, "machine.jpg")
	plain := albumAsset(t, s, "harbor.jpg")
	putPhotoSuggestion(t, s, machine, document.PhotoDescription{Caption: "harbor"})
	compiled, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), snapshotTestQuery(t, `{"text":"harbor"}`), queryResolver{q: s.db})
	require.NoError(t, err)
	items := make([]PhotoBrowseRow, 33000)
	for i := range items {
		items[i].AssetID = plain.ID
	}
	items[0].AssetID = machine.ID
	for _, hybrid := range []bool{false, true} {
		require.NoError(t, markSuggestionOnlyMatches(t.Context(), s.db, compiled, "", CoverageSelection{}, false, items, hybrid))
		require.True(t, items[0].MatchedAISuggestion)
		require.False(t, items[1].MatchedAISuggestion)
	}
}
