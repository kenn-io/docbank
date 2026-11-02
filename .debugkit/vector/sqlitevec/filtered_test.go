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

func TestFilteredQueryFiltersAndGroupsBeforeLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 1;
        ALTER TABLE messages ADD COLUMN eligible INTEGER NOT NULL DEFAULT 0;
        WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n < 4096)
        INSERT INTO messages (id, body, last_modified) SELECT n, 'cat', 1 FROM numbers;
        INSERT INTO messages (id, body, last_modified, parent_id, eligible) VALUES
        (4097, 'cat', 1, 2, 1), (4098, 'cat', 1, 3, 1);`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		query sqlitevec.FilteredQuery
		want  []int64
	}{
		{"filter first", sqlitevec.FilteredQuery{ResultLimit: 2, SourcePredicate: sqlquery.Predicate{SQL: "d.eligible = ?", Args: []any{1}}}, []int64{4097, 4098}},
		{"group first", sqlitevec.FilteredQuery{ResultLimit: 2, GroupColumn: "parent_id"}, []int64{1, 4097}},
		{"filter and group", sqlitevec.FilteredQuery{ResultLimit: 2, GroupColumn: "parent_id", SourcePredicate: sqlquery.Predicate{SQL: "d.eligible = ?", Args: []any{1}}}, []int64{4097, 4098}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := store.BuildFilteredQuery(ctx, db, 1, vector.Vector{1, 0, 0}, tc.query)
			require.NoError(t, err)
			q.SQL = "SELECT doc_key FROM (" + q.SQL + ") ORDER BY distance, doc_key"
			ids, err := q.All(ctx, db, func(rows *sql.Rows) (int64, error) {
				var id int64
				err := rows.Scan(&id)
				return id, err
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, ids)
		})
	}
}

func TestFilteredQueryUsesCurrentSnapshotAndRetainsEvidence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, store := setupWithRevision(t)
	_, err := db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 1;
        INSERT INTO messages (id, body, last_modified, parent_id) VALUES
        (1, 'dog', 1, 1), (2, 'dogcat', 1, 1), (3, 'cat', 1, 2), (4, 'cat', 1, 3);`)
	require.NoError(t, err)
	require.NoError(t, store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder(), vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 3}))
	require.NoError(t, err)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 3; DELETE FROM messages WHERE id = 4`)
	require.NoError(t, err)
	q, err := store.BuildFilteredQuery(ctx, tx, 1, vector.Vector{1, 0, 0}, sqlitevec.FilteredQuery{ResultLimit: 2, GroupColumn: "parent_id"})
	require.NoError(t, err)
	type evidence struct {
		doc      int64
		chunk    int
		revision int64
		distance float64
	}
	hits, err := q.All(ctx, tx, func(rows *sql.Rows) (evidence, error) {
		var hit evidence
		err := rows.Scan(&hit.doc, &hit.chunk, &hit.revision, &hit.distance)
		return hit, err
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, evidence{doc: 2, chunk: 1, revision: 1}, hits[0])
}
