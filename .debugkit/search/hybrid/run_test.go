package hybrid_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/hybrid"
	"go.kenn.io/kit/search/sqlitefts"
	"go.kenn.io/kit/search/sqlquery"
	_ "modernc.org/sqlite"
)

func TestRunFusesBackendQueriesAndReportsWindows(t *testing.T) {
	db := openDocs(t)
	fts, err := sqlitefts.New(
		sqlitefts.WithIndexTable("docs_fts"),
		sqlitefts.WithIndexKey("rowid"),
		sqlitefts.WithSourceTable("docs"),
		sqlitefts.WithSourceKey("id"),
	)
	require.NoError(t, err)
	lexical, err := fts.Build(sqlitefts.Request{Match: "alpha", CandidateLimit: 2})
	require.NoError(t, err)
	vector := sqlquery.Query{SQL: `SELECT id AS doc_key, 0.0 AS score FROM docs WHERE id IN (2, 3) ORDER BY id DESC`}

	result, err := hybrid.Run(t.Context(), db, 60, []hybrid.Leg[int]{
		{Name: "lexical", Weight: 1, Query: lexical, CandidateLimit: 2, Scan: scanKey},
		{Name: "vector", Weight: 1, Query: vector, CandidateLimit: 5, Scan: scanKey},
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Hits)
	assert.Equal(t, 2, result.Hits[0].Key)
	assert.Len(t, result.Hits[0].Contributions, 2)
	require.Len(t, result.Legs, 2)
	assert.True(t, result.Legs[0].FullWindow)
	assert.Equal(t, 2, result.Legs[0].Returned)
	assert.False(t, result.Legs[1].FullWindow)
	assert.Equal(t, 2, result.Legs[1].Returned)
}

func TestRunTrustsARawWindowProbe(t *testing.T) {
	db := openDocs(t)
	query := sqlquery.Query{
		SQL: `SELECT id AS doc_key, 0.0 AS score FROM docs WHERE id = 2`,
		RawWindow: func(context.Context, sqlquery.Queryer) (bool, error) {
			return true, nil
		},
	}
	result, err := hybrid.Run(t.Context(), db, 60, []hybrid.Leg[int]{
		{Name: "vector", Weight: 1, Query: query, CandidateLimit: 5, Scan: scanKey},
	})
	require.NoError(t, err)
	require.Len(t, result.Legs, 1)
	assert.Equal(t, 1, result.Legs[0].Returned)
	assert.True(t, result.Legs[0].FullWindow)
}

type countingQueryer struct{ calls int }

func (q *countingQueryer) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.calls++
	return nil, sql.ErrConnDone
}

func TestRunRejectsBadArgumentsBeforeAnyQuery(t *testing.T) {
	db := &countingQueryer{}
	query := sqlquery.Query{SQL: `SELECT 1`}
	_, err := hybrid.Run(t.Context(), db, 60, []hybrid.Leg[int]{
		{Name: "vector", Weight: 1, Query: query, Scan: scanKey},
		{Name: "vector", Weight: 1, Query: query, Scan: scanKey},
	})
	require.ErrorContains(t, err, `hybrid: rrf: duplicate leg "vector"`)
	_, err = hybrid.RunGroups(t.Context(), db, 0, []hybrid.GroupLeg[string, string]{
		{Name: "lexical", Weight: 1, Query: query, Scan: func(*sql.Rows) (string, string, error) { return "", "", nil }},
	})
	require.ErrorContains(t, err, "k must be positive")
	assert.Zero(t, db.calls, "no leg query runs before the arguments are valid")
}

func TestRunGroupsRetainsAlternateMembers(t *testing.T) {
	db := openDocs(t)
	_, err := db.ExecContext(t.Context(), `CREATE TABLE members (grp TEXT, member TEXT, ord INTEGER)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO members VALUES
		('g1', 'fresh', 1), ('g1', 'stale', 2), ('g2', 'other', 3)`)
	require.NoError(t, err)
	query := sqlquery.Query{SQL: `SELECT grp, member FROM members ORDER BY ord`}
	result, err := hybrid.RunGroups(t.Context(), db, 60, []hybrid.GroupLeg[string, string]{
		{Name: "lexical", Weight: 1, Query: query, CandidateLimit: 10, Scan: func(rows *sql.Rows) (string, string, error) {
			var group, member string
			err := rows.Scan(&group, &member)
			return group, member, err
		}},
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 2)
	assert.Equal(t, "g1", result.Hits[0].Group)
	assert.Equal(t, []string{"fresh", "stale"}, []string{result.Hits[0].Alternates[0].Member, result.Hits[0].Alternates[1].Member})
	assert.False(t, result.Legs[0].FullWindow)
}

func TestRunEveryKeepsGroupsWithEvidenceForEveryConcept(t *testing.T) {
	db := openDocs(t)
	_, err := db.ExecContext(t.Context(), `CREATE TABLE hits (concept TEXT, session TEXT, ord INTEGER, score REAL);
INSERT INTO hits VALUES
	('auth', 's1', 3, 0.9), ('auth', 's2', 5, 0.8), ('auth', 's3', 1, 0.4),
	('retry', 's2', 9, 0.7), ('retry', 's1', 8, 0.6), ('retry', 's4', 2, 0.5)`)
	require.NoError(t, err)
	type evidence struct {
		Ordinal int
		Score   float64
	}
	leg := func(concept string, limit int) hybrid.GroupLeg[string, evidence] {
		return hybrid.GroupLeg[string, evidence]{
			Name: concept, Weight: 1, CandidateLimit: limit,
			Query: sqlquery.Query{
				SQL:  `SELECT session, ord, score FROM hits WHERE concept = ? ORDER BY score DESC LIMIT ?`,
				Args: []any{concept, limit},
			},
			Scan: func(rows *sql.Rows) (string, evidence, error) {
				var session string
				var ev evidence
				err := rows.Scan(&session, &ev.Ordinal, &ev.Score)
				return session, ev, err
			},
		}
	}

	result, err := hybrid.RunGroupsEvery(t.Context(), db, 60, []hybrid.GroupLeg[string, evidence]{
		leg("auth", 10), leg("retry", 10),
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 2, "s3 and s4 match one concept each")
	assert.ElementsMatch(t, []string{"s1", "s2"}, []string{result.Hits[0].Group, result.Hits[1].Group})
	for _, hit := range result.Hits {
		require.Len(t, hit.Alternates, 2)
		assert.Equal(t, "auth", hit.Alternates[0].Leg)
		assert.Equal(t, "retry", hit.Alternates[1].Leg)
	}
	assert.False(t, result.AnyFullWindow())

	// A retry window of one row holds only s2, so s1 drops out of the
	// intersection. The full window tells the caller the result may be short.
	result, err = hybrid.RunGroupsEvery(t.Context(), db, 60, []hybrid.GroupLeg[string, evidence]{
		leg("auth", 10), leg("retry", 1),
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, "s2", result.Hits[0].Group)
	assert.True(t, result.AnyFullWindow())
}

func scanKey(rows *sql.Rows) (int, error) {
	var key int
	var score float64
	err := rows.Scan(&key, &score)
	return key, err
}

func openDocs(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE docs (id INTEGER PRIMARY KEY, title TEXT);
CREATE VIRTUAL TABLE docs_fts USING fts5(title);
INSERT INTO docs (id, title) VALUES (1, 'alpha'), (2, 'alpha beta'), (3, 'beta');
INSERT INTO docs_fts (rowid, title) SELECT id, title FROM docs;`)
	require.NoError(t, err)
	return db
}
