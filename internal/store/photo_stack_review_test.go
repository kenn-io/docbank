package store

import (
	"context"
	"database/sql"
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

// Only the hydrate query is sent through this reader, so the plan assertion
// follows executed SQL without recognizing a query by its text.
type photoStackPlanQuerier struct {
	*sql.DB

	t *testing.T
}

func (q photoStackPlanQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := q.DB.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(q.t, err)
	defer func() { require.NoError(q.t, rows.Close()) }()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(q.t, rows.Scan(&id, &parent, &unused, &detail))
		require.NotContains(q.t, detail, "AUTOMATIC", "hydrate must use existing indexes")
	}
	require.NoError(q.t, rows.Err())
	require.NoError(q.t, rows.Close())
	return q.DB.QueryContext(ctx, query, args...) //nolint:sqlclosecheck // The hydrate reader owns and closes the returned rows.
}

func TestPhotoStackProjectionReaderPropagatesCancellation(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	record, vector := f.publish(t)
	index := newCursorIndexFixture(f)
	index.add(record, vector)
	index.generation(t, "reader-errors")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	q := photoStackGenerationQuerier{DB: f.s.db, beforeLoad: func() {
		reads++
		cancel()
	}}
	reader, err := f.s.newPhotoStackVectorReader(ctx, q)
	require.Equal(t, 1, reads)
	require.Nil(t, reader)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPhotoStackNewestComponentBoundsHistoryReads(t *testing.T) {
	t.Parallel()
	for _, indexed := range []bool{true, false} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			var f imageEmbeddingCatalogFixture
			var last string
			limit := MaxPhotoStackFrames
			if indexed {
				var ids []string
				f, ids = indexedPhotoRun(t, 1500, 20)
				last = ids[len(ids)-1]
				limit++
			} else {
				f = newImageEmbeddingCatalogFixture(t)
				record, vector := f.publish(t)
				index := newCursorIndexFixture(f)
				index.add(record, vector)
				index.generation(t, "unrelated-stream-index")
				run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic unindexed camera stream")
				require.NoError(t, err)
				for i := range 2000 {
					stamp := time.Date(2026, 1, 1, 0, 0, i*30, 0, time.UTC).Format(time.RFC3339)
					last = stackPhoto(t, f.s, run, fmt.Sprintf("/unindexed/frame-%04d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
				}
			}
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
			require.LessOrEqual(t, hydrated, limit, "resolving the newest stack must bound total hydration")
		})
	}
}
