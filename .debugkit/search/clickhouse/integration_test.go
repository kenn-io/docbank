package clickhouse_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	clickhousego "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/clickhouse"
	"go.kenn.io/kit/search/sqlquery"
)

func TestClickHouseTextAndVector(t *testing.T) {
	addr := os.Getenv("KIT_CLICKHOUSE_TEST_ADDR")
	if addr == "" {
		t.Skip("KIT_CLICKHOUSE_TEST_ADDR is not set")
	}
	db := clickhousego.OpenDB(&clickhousego.Options{Addr: []string{addr}, Protocol: clickhousego.Native})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))

	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT version()").Scan(&version))
	require.True(t, strings.HasPrefix(version, "26."), "unexpected ClickHouse version %s", version)

	tokenRows, err := clickhouse.TokenizersQuery().All(ctx, db, func(rows *sql.Rows) (string, error) {
		var name string
		err := rows.Scan(&name)
		return name, err
	})
	require.NoError(t, err)
	assert.Contains(t, tokenRows, clickhouse.TokenizerNgrams)
	assert.Contains(t, tokenRows, clickhouse.TokenizerSplitByNonAlpha)
	assert.NotContains(t, tokenRows, "asciiCJK")

	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS docs`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
CREATE TABLE docs (
	id String,
	tenant String,
	body String,
	chars String,
	revision UInt32,
	embedding Array(Float32),
	INDEX body_text body TYPE text(tokenizer = splitByNonAlpha),
	INDEX chars_text chars TYPE text(tokenizer = ngrams(1)),
	INDEX embedding_ann embedding TYPE vector_similarity('hnsw', 'cosineDistance', 2)
) ENGINE = MergeTree ORDER BY id`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO docs VALUES
		('kana', 't1', 'かなを探します。', 'かなを探します。', 3, [0, 1]),
		('reverse', 't1', 'なかを探します。', 'なかを探します。', 4, [0, 1]),
		('hangul', 't1', '검색합니다.', '검색합니다.', 5, [0.2, 0.8]),
		('near', 'other', 'alpha beta', 'alpha beta', 1, [1, 0]),
		('literal', 't1', 'SQLite error-401', 'SQLite error-401', 9, [0, 1])`)
	require.NoError(t, err)

	whole, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchToken, Text: "かなを探します。",
		RevisionColumn: "revision", CandidateLimit: 10,
	})
	require.NoError(t, err)
	wholeHits := scanText(t, ctx, db, whole)
	require.Len(t, wholeHits, 1)
	assert.Equal(t, "kana", wholeHits[0].id)
	assert.Equal(t, uint32(3), wholeHits[0].revision)

	substring, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Text: "かな", CandidateLimit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"kana"}, scanIDs(t, ctx, db, substring))
	reversed, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Text: "なか", CandidateLimit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"reverse"}, scanIDs(t, ctx, db, reversed))

	literal, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchAll, Text: "error-401",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		CandidateLimit:  10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"literal"}, scanIDs(t, ctx, db, literal))

	unordered, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "chars",
		Match: clickhouse.MatchAll, Tokens: []string{"か", "な"}, CandidateLimit: 10,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kana", "reverse"}, scanIDs(t, ctx, db, unordered))

	vectorQuery, err := clickhouse.BuildVector(clickhouse.VectorRequest{
		SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding",
		Distance: clickhouse.DistanceCosine, Query: []float32{1, 0},
		RevisionColumn: "revision", CandidateLimit: 1,
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
	})
	require.NoError(t, err)
	// Limit 1 with a predicate can be empty: the HNSW candidate is the other
	// tenant, and the predicate removes it. That is not exhaustion.
	assert.Empty(t, scanVector(t, ctx, db, vectorQuery))
	wider, err := clickhouse.BuildVector(clickhouse.VectorRequest{
		SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding",
		Distance: clickhouse.DistanceCosine, Query: []float32{1, 0},
		RevisionColumn: "revision", CandidateLimit: 5,
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
	})
	require.NoError(t, err)
	widerHits := scanVector(t, ctx, db, wider)
	require.NotEmpty(t, widerHits)
	for _, hit := range widerHits {
		assert.NotEqual(t, "near", hit.id)
	}

	explained := explain(t, ctx, db, "EXPLAIN indexes = 1 "+unordered.SQL, unordered.Args)
	assert.Contains(t, explained, "chars_text")
	nearest, err := clickhouse.BuildVector(clickhouse.VectorRequest{
		SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding",
		Distance: clickhouse.DistanceCosine, Query: []float32{1, 0},
		RevisionColumn: "revision", CandidateLimit: 1,
	})
	require.NoError(t, err)
	nearestHits := scanVector(t, ctx, db, nearest)
	require.Len(t, nearestHits, 1)
	assert.Equal(t, "near", nearestHits[0].id)
	vectorPlan := explain(t, ctx, db, "EXPLAIN indexes = 1 "+nearest.SQL, nearest.Args)
	assert.Contains(t, vectorPlan, "embedding_ann")
}

type textHit struct {
	id       string
	revision uint32
}

func scanText(t *testing.T, ctx context.Context, db *sql.DB, q sqlquery.Query) []textHit {
	t.Helper()
	rows, err := q.All(ctx, db, func(rows *sql.Rows) (textHit, error) {
		var hit textHit
		err := rows.Scan(&hit.id, &hit.revision)
		return hit, err
	})
	require.NoError(t, err)
	return rows
}

func scanIDs(t *testing.T, ctx context.Context, db *sql.DB, q sqlquery.Query) []string {
	t.Helper()
	rows, err := q.All(ctx, db, func(rows *sql.Rows) (string, error) {
		var id string
		err := rows.Scan(&id)
		return id, err
	})
	require.NoError(t, err)
	return rows
}

type vectorHit struct {
	id       string
	revision uint32
	score    float64
}

func scanVector(t *testing.T, ctx context.Context, db *sql.DB, q sqlquery.Query) []vectorHit {
	t.Helper()
	rows, err := q.All(ctx, db, func(rows *sql.Rows) (vectorHit, error) {
		var hit vectorHit
		err := rows.Scan(&hit.id, &hit.revision, &hit.score)
		return hit, err
	})
	require.NoError(t, err)
	return rows
}

func explain(t *testing.T, ctx context.Context, db *sql.DB, query string, args []any) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var b strings.Builder
	for rows.Next() {
		cols, err := rows.Columns()
		require.NoError(t, err)
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		for _, value := range values {
			fmt.Fprintf(&b, "%v ", value)
		}
		b.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return b.String()
}
