package sqlitefts_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/sqlitefts"
	"go.kenn.io/kit/search/sqlquery"
	_ "modernc.org/sqlite"
)

func TestBuildAndCompose(t *testing.T) {
	h := newHelper(t, "docs_fts", "source_id", "docs", "id")
	q, err := h.Build(sqlitefts.Request{
		Match:           "alpha",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant_id = ?", Args: []any{"tenant"}},
		ExtraSourceCols: []sqlquery.Column{{Name: "title", As: "title"}}, CandidateLimit: 4,
	})
	require.NoError(t, err)
	require.Contains(t, q.SQL, `-bm25("docs_fts")`)

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id TEXT PRIMARY KEY, tenant_id TEXT, title TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(source_id, body);
INSERT INTO docs VALUES ('a','tenant','Alpha one'), ('b','other','Alpha two'), ('c','tenant','Alpha three');
INSERT INTO docs_fts VALUES ('a','alpha one'), ('b','alpha two'), ('c','alpha three');`)
	require.NoError(t, err)

	// The generated SELECT can be embedded as a candidate relation.
	rows, err := db.QueryContext(t.Context(), "SELECT doc_key, title, score FROM ("+q.SQL+") candidates ORDER BY score DESC, doc_key", q.Args...)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, title string
		var score float64
		require.NoError(t, rows.Scan(&id, &title, &score))
		require.Greater(t, score, 0.0)
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"a", "c"}, ids)
}

func TestRowIDMappingFiltersBeforeLimitAndScansTransaction(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id INTEGER PRIMARY KEY, tenant TEXT, title TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(body);
INSERT INTO docs VALUES (1,'other','excluded'), (2,'tenant','kept');
INSERT INTO docs_fts(rowid,body) VALUES (1,'alpha alpha'), (2,'alpha');`)
	require.NoError(t, err)
	q, err := newHelper(t, "docs_fts", "rowid", "docs", "id").Build(sqlitefts.Request{
		Match: "alpha", SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"tenant"}}, CandidateLimit: 1,
	})
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	values, err := q.All(t.Context(), tx, func(rows *sql.Rows) (int, error) {
		var id int
		var score float64
		err := rows.Scan(&id, &score)
		return id, err
	})
	require.NoError(t, tx.Rollback())
	require.NoError(t, err)
	require.Equal(t, []int{2}, values)
}

func TestBuildRejectsCaseInsensitiveFixedAliasCollision(t *testing.T) {
	_, err := newHelper(t, "fts", "rowid", "docs", "id").Build(sqlitefts.Request{
		Match: "x", ExtraSourceCols: []sqlquery.Column{{Name: "title", As: "DOC_KEY"}}, CandidateLimit: 1,
	})
	require.Error(t, err)
}

func TestBuildRejectsPredicateArgsWithoutSQL(t *testing.T) {
	_, err := newHelper(t, "fts", "source_id", "docs", "id").Build(sqlitefts.Request{
		Match: "x", SourcePredicate: sqlquery.Predicate{Args: []any{"tenant"}}, CandidateLimit: 1,
	})
	require.Error(t, err)
}

func TestRankFunctionDefaultsToBM25AndCanBeReplaced(t *testing.T) {
	custom := newHelper(t, "docs_fts", "rowid", "docs", "id", sqlitefts.WithRankFunction("rank"))
	q, err := custom.Build(sqlitefts.Request{Match: "alpha", CandidateLimit: 1})
	require.NoError(t, err)
	require.Contains(t, q.SQL, `-rank("docs_fts")`)
	require.NotContains(t, q.SQL, "bm25")

	_, err = sqlitefts.New(sqlitefts.WithRankFunction("bm25("))
	require.Error(t, err)
	_, err = sqlitefts.New(sqlitefts.WithIndexTable("docs_fts"), sqlitefts.WithIndexKey("rowid"))
	require.Error(t, err)
}

func newHelper(t *testing.T, indexTable, indexKey, sourceTable, sourceKey string, extra ...sqlitefts.Option) sqlitefts.Helper {
	t.Helper()
	opts := []sqlitefts.Option{
		sqlitefts.WithIndexTable(indexTable),
		sqlitefts.WithIndexKey(indexKey),
		sqlitefts.WithSourceTable(sourceTable),
		sqlitefts.WithSourceKey(sourceKey),
	}
	opts = append(opts, extra...)
	h, err := sqlitefts.New(opts...)
	require.NoError(t, err)
	return h
}

func TestBuildRejectsEmptyMatchText(t *testing.T) {
	h := newHelper(t, "docs_fts", "source_id", "docs", "id")
	for _, match := range []string{"", "   "} {
		_, err := h.Build(sqlitefts.Request{Match: match, CandidateLimit: 1})
		require.ErrorContains(t, err, "match text is empty")
	}
}
