package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func indexedPhotoRun(t *testing.T, count, seconds int) (imageEmbeddingCatalogFixture, []string) {
	t.Helper()
	f := newImageEmbeddingCatalogFixture(t)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic indexed camera stream")
	require.NoError(t, err)
	index := newCursorIndexFixture(f)
	ids := make([]string, count)
	for i := range ids {
		stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i*seconds) * time.Second).Format(time.RFC3339)
		ids[i] = stackPhoto(t, f.s, run, fmt.Sprintf("/indexed/frame-%04d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
		index.publish(t, ids[i], []float64{1, float64(i%7) * .01, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "indexed-camera-stream")
	return f, ids
}

func TestPhotoStackNewestComponentBoundsHistoryReads(t *testing.T) {
	t.Parallel()
	f, ids := indexedPhotoRun(t, 30, 20)
	last := ids[len(ids)-1]
	vectors, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	frames, err := f.s.photoStackFrames(t.Context(), f.s.db, []string{last}, false, "")
	require.NoError(t, err)
	pager, err := newPhotoStackPager(t.Context(), f.s.db, photoBrowseScope{match: compiledQueryFragment{sql: "1=1"}}, 2, true, "", vectors)
	require.NoError(t, err)
	defer func() { require.NoError(t, pager.Close()) }()
	hydrated := 0
	vectors.onHydrate = func(count int) { hydrated += count }
	component, err := pager.component(t.Context(), frames[last])
	require.NoError(t, err)
	require.Len(t, component.frames, 1)
	require.Equal(t, last, component.frames[0].AssetID)
	require.LessOrEqual(t, hydrated, 7, "resolving the newest stack must hydrate only the 120-second window at 20-second spacing")
}
