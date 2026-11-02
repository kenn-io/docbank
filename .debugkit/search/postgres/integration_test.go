package postgres_test

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/lexical"
	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
)

func TestPostgreSQLLexicalAndVector(t *testing.T) {
	dsn := os.Getenv("KIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KIT_POSTGRES_TEST_DSN is not set")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `CREATE TEMP TABLE docs (
	id text PRIMARY KEY,
	tenant text NOT NULL,
	title text NOT NULL,
	body text NOT NULL,
	revision int NOT NULL,
	embedding vector(2) NOT NULL
) ON COMMIT DROP`)
	require.NoError(t, err)

	phrase := lexical.CharacterPhrase()
	kana, err := phrase.IndexText("かなを探します。")
	require.NoError(t, err)
	reverse, err := phrase.IndexText("なかを探します。")
	require.NoError(t, err)
	queryText, err := phrase.IndexText("かな")
	require.NoError(t, err)

	_, err = tx.ExecContext(t.Context(), `INSERT INTO docs (id, tenant, title, body, revision, embedding) VALUES
		('near', 'other', 'alpha', 'other', 1, '[1,0]'),
		('title', 't1', 'alpha contract', 'notes', 7, '[0,1]'),
		('body', 't1', 'notes', 'alpha contract details', 8, '[0.2,0.8]'),
		('kana', 't1', 'kana', $1, 3, '[0,1]'),
		('reverse', 't1', 'reverse', $2, 4, '[0,1]')`, kana, reverse)
	require.NoError(t, err)

	vectorExpr := "setweight(to_tsvector('simple', d.title), 'A') || setweight(to_tsvector('simple', d.body), 'B')"
	lexicalQuery, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping:         postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: vectorExpr},
		Query:           postgres.QueryPlain,
		Text:            "alpha contract",
		RevisionColumn:  "revision",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		ExtraSourceCols: []sqlquery.Column{{Name: "title", As: "title"}},
		CandidateLimit:  5,
	})
	require.NoError(t, err)
	hits := scanLexical(t, tx, lexicalQuery)
	require.Len(t, hits, 2)
	assert.Equal(t, "title", hits[0].id)
	assert.Greater(t, hits[0].score, hits[1].score)
	assert.Equal(t, 7, hits[0].revision)
	assert.Equal(t, "alpha contract", hits[0].title)

	phraseQuery, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping:        postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "to_tsvector('simple', d.body)"},
		Query:          postgres.QueryPhrase,
		Text:           queryText,
		RevisionColumn: "revision",
		CandidateLimit: 5,
	})
	require.NoError(t, err)
	// Lexical and vector rows share one column order, so the vector scanner
	// reads a lexical query.
	phraseHits := scanVector(t, tx, phraseQuery)
	require.Len(t, phraseHits, 1)
	assert.Equal(t, "kana", phraseHits[0].id)
	assert.Equal(t, 3, phraseHits[0].revision)

	nearest, err := postgres.BuildVector(postgres.VectorRequest{
		Mapping:        postgres.VectorMapping{SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding"},
		Distance:       postgres.DistanceCosine,
		Query:          "[1,0]",
		RevisionColumn: "revision",
		CandidateLimit: 1,
	})
	require.NoError(t, err)
	vectorHits := scanVector(t, tx, nearest)
	require.Len(t, vectorHits, 1)
	assert.Equal(t, "near", vectorHits[0].id)
	assert.Equal(t, 1, vectorHits[0].revision)

	filtered, err := postgres.BuildVector(postgres.VectorRequest{
		Mapping:         postgres.VectorMapping{SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding"},
		Distance:        postgres.DistanceCosine,
		Query:           "[1,0]",
		RevisionColumn:  "revision",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		CandidateLimit:  1,
	})
	require.NoError(t, err)
	filteredHits := scanVector(t, tx, filtered)
	require.Len(t, filteredHits, 1)
	assert.NotEqual(t, "near", filteredHits[0].id)

	_, err = tx.ExecContext(t.Context(), `UPDATE docs SET revision = 11 WHERE id = $1`, filteredHits[0].id)
	require.NoError(t, err)
	after := scanVector(t, tx, filtered)
	require.Len(t, after, 1)
	assert.Equal(t, filteredHits[0].id, after[0].id)
	assert.Equal(t, 11, after[0].revision)

	var plan string
	require.NoError(t, tx.QueryRowContext(t.Context(), "EXPLAIN "+filtered.SQL, filtered.Args...).Scan(&plan))
	assert.NotEmpty(t, plan)
}

type lexicalRow struct {
	id, title string
	score     float64
	revision  int
}

func scanLexical(t *testing.T, db sqlquery.Queryer, q sqlquery.Query) []lexicalRow {
	t.Helper()
	rows, err := q.All(t.Context(), db, func(rows *sql.Rows) (lexicalRow, error) {
		var row lexicalRow
		err := rows.Scan(&row.id, &row.revision, &row.score, &row.title)
		return row, err
	})
	require.NoError(t, err)
	return rows
}

type vectorRow struct {
	id       string
	revision int
	score    float64
}

func scanVector(t *testing.T, db sqlquery.Queryer, q sqlquery.Query) []vectorRow {
	t.Helper()
	rows, err := q.All(t.Context(), db, func(rows *sql.Rows) (vectorRow, error) {
		var row vectorRow
		err := rows.Scan(&row.id, &row.revision, &row.score)
		return row, err
	})
	require.NoError(t, err)
	return rows
}
