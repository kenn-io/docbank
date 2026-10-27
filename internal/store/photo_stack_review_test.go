package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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
		index.publish(t, ids[i], []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "indexed-camera-stream")
	return f, ids
}

func TestPhotoStackLongImageRunLibraryMatchesCull(t *testing.T) {
	t.Parallel()
	f, ids := indexedPhotoRun(t, 502, 20)
	var want [][]string
	for i := 0; i < 500; i += 4 {
		want = append(want, ids[i:i+4])
	}
	want = append(want, ids[500:])
	groups, unavailable, _, incomplete, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.False(t, incomplete)
	require.Equal(t, want, groups)
	for _, direction := range []string{"desc", "asc"} {
		t.Run(direction, func(t *testing.T) {
			request := captureRequest(t, direction)
			request.PageSize = 100
			expected := slices.Clone(want)
			if direction == "desc" {
				slices.Reverse(expected)
			}
			first, err := f.s.ListPhotoAssets(t.Context(), request, nil)
			require.NoError(t, err)
			require.NotEmpty(t, first.Stacks)
			require.Equal(t, expected[0], first.Stacks[0].AssetIDs)
			_, stacks := stackPages(t, f.s, request)
			require.Equal(t, expected, stacks)
		})
	}
}

func TestPhotoStackMalformedProjectionPreservesBurstBrowse(t *testing.T) {
	t.Parallel()
	for _, damage := range []string{"empty", "rows"} {
		t.Run(damage, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			record, vector := f.publish(t)
			index := newCursorIndexFixture(f)
			index.add(record, vector)
			index.generation(t, "malformed-projection")
			update := map[string]string{"empty": "generation_bytes=X'',byte_size=0", "rows": "row_count=1000001"}[damage]
			_, err := f.s.db.Exec(`UPDATE vector_index_generations SET `+update+` WHERE generation_id=?`, browseHash("malformed-projection"))
			require.NoError(t, err)
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

func TestPhotoStackHydrateUsesExistingIndexes(t *testing.T) {
	t.Parallel()
	f, ids := indexedPhotoRun(t, 2, 20)
	reader, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	frames := map[string]photoStackFrame{ids[0]: {AssetID: ids[0]}, ids[1]: {AssetID: ids[1]}}
	require.NoError(t, reader.hydrate(t.Context(), photoStackPlanQuerier{f.s.db, t}, frames))
	require.Len(t, frames[ids[0]].ImageVectors, 1)
}

func TestPhotoStackIndexedRunPageTiming(t *testing.T) { //nolint:paralleltest // Keep informational page timings separate from parallel fixture writes.
	f, _ := indexedPhotoRun(t, 800, 10)
	for _, direction := range []string{"desc", "asc"} {
		request := captureRequest(t, direction)
		request.PageSize = 100
		var after *PhotoBrowsePosition
		for pageNumber := 1; ; pageNumber++ {
			start := time.Now()
			page, err := f.s.ListPhotoAssets(t.Context(), request, after)
			elapsed := time.Since(start)
			require.NoError(t, err)
			t.Logf("%s page %d: %s, %d frames, %d stacks", direction, pageNumber, elapsed, len(page.Items), len(page.Stacks))
			if page.Next == nil {
				break
			}
			after = page.Next
		}
	}
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
