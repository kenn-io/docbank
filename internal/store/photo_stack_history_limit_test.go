package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewNewestStackBoundsHistoryReads(t *testing.T) {
	t.Parallel()
	f, ids := indexedPhotoRun(t, 1500, 20)
	groups, unavailable, _, incomplete, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.False(t, incomplete)
	require.Contains(t, groups, ids[len(ids)-4:])
	vectors, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	last := ids[len(ids)-1]
	frames, err := f.s.photoStackFrames(t.Context(), f.s.db, []string{last}, false, "")
	require.NoError(t, err)
	pager, err := newPhotoStackPager(t.Context(), f.s.db, photoBrowseScope{match: compiledQueryFragment{sql: "1=1"}}, 2, true, "", vectors)
	require.NoError(t, err)
	defer func() { require.NoError(t, pager.Close()) }()
	hydrated := 0
	vectors.onHydrate = func(count int) { hydrated += count }
	component, err := pager.component(t.Context(), frames[last])
	require.NoError(t, err)
	var got []string
	for _, frame := range component.frames {
		got = append(got, frame.AssetID)
	}
	require.Equal(t, ids[len(ids)-4:], got)
	require.LessOrEqual(t, hydrated, MaxPhotoStackFrames+1, "resolving the newest stack must bound total hydration")
}
