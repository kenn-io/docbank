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

func TestPhotoStackMalformedProjectionPreservesBurstBrowse(t *testing.T) {
	t.Parallel()
	for _, damage := range []string{"empty", "rows", "byte flip"} {
		t.Run(damage, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			record, vector := f.publish(t)
			index := newCursorIndexFixture(f)
			index.add(record, vector)
			index.generation(t, "malformed-projection")
			if damage == "byte flip" {
				var data []byte
				require.NoError(t, f.s.db.QueryRow(`SELECT generation_bytes FROM vector_index_generations WHERE generation_id=?`, browseHash("malformed-projection")).Scan(&data))
				data[0] ^= 0xff
				_, err := f.s.db.Exec(`UPDATE vector_index_generations SET generation_bytes=? WHERE generation_id=?`, data, browseHash("malformed-projection"))
				require.NoError(t, err)
			} else {
				update := map[string]string{"empty": "generation_bytes=X'',byte_size=0", "rows": "row_count=1000001"}[damage]
				_, err := f.s.db.Exec(`UPDATE vector_index_generations SET `+update+` WHERE generation_id=?`, browseHash("malformed-projection"))
				require.NoError(t, err)
			}
			ids := burstPhotos(t, f.s, 2)
			for _, direction := range []string{"asc", "desc"} {
				page, err := f.s.ListPhotoAssets(t.Context(), captureRequest(t, direction), nil)
				require.NoError(t, err)
				require.Contains(t, stackAssetIDs(page.Stacks), ids)
			}
		})
	}
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

// Fail the generation read after the real head query has succeeded.
type photoStackLoadFailureQuerier struct {
	*sql.DB

	beforeLoad func()
}

func (q photoStackLoadFailureQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.beforeLoad()
	return q.DB.QueryRowContext(ctx, query, args...)
}

func TestPhotoStackProjectionReaderPropagatesDatabaseAndCancellationErrors(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"database", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			record, vector := f.publish(t)
			index := newCursorIndexFixture(f)
			index.add(record, vector)
			index.generation(t, "reader-errors")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads := 0
			q := photoStackLoadFailureQuerier{DB: f.s.db, beforeLoad: func() {
				reads++
				if failure == "cancellation" {
					cancel()
				} else {
					require.NoError(t, f.s.db.Close())
				}
			}}
			reader, err := f.s.newPhotoStackVectorReader(ctx, q)
			require.Equal(t, 1, reads)
			require.Nil(t, reader)
			require.Error(t, err)
			if failure == "cancellation" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestReviewNewestStackBoundsHistoryReads(t *testing.T) {
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
