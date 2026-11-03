package store

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			request := SnapshotRequest{Photos: true, PageSize: 250, Query: snapshotTestQuery(t, `{"mode":"hybrid","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "hybrid", ActualMode: "lexical", FallbackReason: "provider_unavailable"}}
			page, err := service.Create(t.Context(), "owner", request)
			require.NoError(t, err)
			require.EqualValues(t, 1000, page.Total)
			require.Equal(t, count > 1000, page.PhotoRanking.Truncated)
		})
	}
}
