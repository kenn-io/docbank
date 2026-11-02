package sqlquery_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"go.kenn.io/kit/search/sqlquery"
)

func TestAll(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), "CREATE TABLE docs (id INTEGER, title TEXT); INSERT INTO docs VALUES (1, 'first'), (2, 'second')")
	require.NoError(t, err)

	type document struct {
		ID    int
		Title string
	}
	query := sqlquery.Query{SQL: "SELECT id, title FROM docs WHERE id >= ? ORDER BY id DESC", Args: []any{1}}
	docs, err := query.All(t.Context(), tx, func(rows *sql.Rows) (document, error) {
		var doc document
		err := rows.Scan(&doc.ID, &doc.Title)
		return doc, err
	})
	require.NoError(t, err)
	assert.Equal(t, []document{{2, "second"}, {1, "first"}}, docs)
	// Execution must leave the caller's transaction usable and uncommitted.
	require.NoError(t, tx.Rollback())
}

func TestAllErrorsReleaseRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	scanError := errors.New("caller cannot decode row")
	for _, tc := range []struct {
		name     string
		query    string
		failScan bool
	}{
		{"scan", "SELECT 1 UNION ALL SELECT 2", true},
		{"iteration", "SELECT 1 UNION ALL SELECT abs(-9223372036854775808)", false},
		{"query", "SELECT id FROM missing_table", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			values, err := (sqlquery.Query{SQL: tc.query}).All(t.Context(), db, func(rows *sql.Rows) (int, error) {
				calls++
				if tc.failScan && calls == 2 {
					return 0, scanError
				}
				var value int
				err := rows.Scan(&value)
				return value, err
			})
			require.Error(t, err)
			assert.Nil(t, values, "errors must not return partial results")
			if tc.failScan {
				require.ErrorIs(t, err, scanError)
			}
			// With only one connection, this query can run only if All released it.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var one int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
		})
	}
}
