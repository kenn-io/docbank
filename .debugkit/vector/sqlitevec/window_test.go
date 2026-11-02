package sqlitevec_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestQueryGenerationWindowReportsRawProbePastFilteredRows(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a cat slept', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 1; DELETE FROM messages WHERE id = 2`)
	require.NoError(t, err)

	w, err := store.QueryGenerationWindow(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	assert.Empty(t, w.Hits, "stale and deleted candidates are filtered")
	assert.True(t, w.HasProbe, "the raw candidate beyond the boundary remains observable")
}

func TestBuildCandidateQueryComposesAndAppliesFilterBeforeResultLimit(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a dog ran', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)

	// The lookup and the query share the caller's transaction.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = store.BuildCandidateQuery(ctx, tx, 99, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{CandidateLimit: 1})
	require.ErrorIs(t, err, sqlitevec.ErrGenerationNotFound)
	q, err := store.BuildCandidateQuery(ctx, tx, 1, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{
		CandidateLimit:  2,
		ExtraSourceCols: []sqlquery.Column{{Name: "body", As: "text"}},
		ResultLimit:     1,
		SourcePredicate: sqlquery.Predicate{SQL: "d.id = ?", Args: []any{int64(2)}},
	})
	require.NoError(t, err)
	full, err := q.RawWindow(ctx, db)
	require.NoError(t, err)
	assert.True(t, full, "the raw neighbor window was full even though the filter keeps one row")
	// Compose the generated relation while preserving its binding order.
	q.SQL = "SELECT doc_key, chunk_index, revision, distance, text FROM (" + q.SQL + ") AS matches ORDER BY distance, doc_key"
	narrow, err := store.BuildCandidateQuery(ctx, tx, 1, vector.Vector{1, 0, 0}, sqlitevec.CandidateQuery{
		CandidateLimit: 1, ResultLimit: 1,
		SourcePredicate: sqlquery.Predicate{SQL: "d.id = ?", Args: []any{int64(2)}},
	})
	require.NoError(t, err)
	type result struct {
		doc      int64
		chunk    int
		revision int64
		distance float64
		text     string
	}
	results, err := q.All(ctx, tx, func(rows *sql.Rows) (result, error) {
		var r result
		err := rows.Scan(&r.doc, &r.chunk, &r.revision, &r.distance, &r.text)
		if err != nil {
			return r, err
		}
		if _, err := sqlitevec.ScoreFromDistance(r.distance); err != nil {
			return r, err
		}
		return r, nil
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, int64(2), results[0].doc)
	assert.Equal(t, 0, results[0].chunk)
	assert.Equal(t, int64(1), results[0].revision)
	assert.Equal(t, "a dog ran", results[0].text)

	// The smaller raw window contains only the cat, which the predicate
	// excludes. It does not expand to fetch the eligible dog document.
	results, err = narrow.All(ctx, tx, func(rows *sql.Rows) (result, error) {
		var r result
		var distance float64
		err := rows.Scan(&r.doc, &r.chunk, &r.revision, &distance)
		if err != nil {
			return r, err
		}
		if _, err := sqlitevec.ScoreFromDistance(distance); err != nil {
			return r, err
		}
		return r, nil
	})
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestQueryGenerationWindowReturnsRevisionAndExhaustion(t *testing.T) {
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1)`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)

	w, err := store.QueryGenerationWindow(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	require.Len(t, w.Hits, 1)
	require.Equal(t, int64(1), w.Hits[0].Doc)
	require.Equal(t, int64(1), w.Hits[0].Revision)
	require.False(t, w.HasProbe)
	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 1)
	require.NoError(t, err)
	require.Equal(t, w.Hits, hits, "ordinary and window queries share revision-bearing results")
}

func TestQueryGenerationWindowRejectsANonpositiveLimit(t *testing.T) {
	_, store := setupWithRevision(t)
	require.NoError(t, store.EnsureGeneration(t.Context(), 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	for _, limit := range []int{0, -1} {
		_, err := store.QueryGenerationWindow(t.Context(), 1, vector.Vector{1, 0, 0}, limit)
		require.ErrorContains(t, err, "window limit must be positive")
	}
}
