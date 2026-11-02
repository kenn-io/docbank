// Package sqlquery executes parameterized search relations through a caller's
// database handle. Backend packages construct the SQL and define its columns.
package sqlquery

import (
	"context"
	"database/sql"
	"fmt"
)

// Query is a composable SELECT and its bound arguments. The producing backend
// defines the SQL dialect, placeholders and output columns. When composing it,
// callers must preserve argument order and rebase numbered placeholders where
// required by that dialect. A composed outer query must specify its own order.
type Query struct {
	SQL  string
	Args []any
	// RawWindow, when set, reports whether the backend filled its raw
	// candidate window before source filters removed rows. A caller that
	// needs to know whether a short result is complete should use it instead
	// of comparing the returned row count with the candidate limit.
	//
	// RawWindow is a separate statement. It describes the same window as SQL
	// only when both run on one snapshot, such as one transaction. It probes
	// the backend's own window, so wrapping SQL in an outer query or adding
	// an outer filter does not change what it reports.
	RawWindow func(context.Context, Queryer) (bool, error)
}

// Column projects a source column under an explicit result alias.
// Backends quote Name and As as identifiers.
type Column struct {
	Name string
	As   string
}

// Predicate is a trusted SQL filter over the source row, which every backend
// aliases as d. Refer to columns as d.<column> and bind values with anonymous
// ? placeholders. Args are bound in textual order. User values belong in
// Args, never in SQL. Each backend documents how it rewrites placeholders.
type Predicate struct {
	SQL  string
	Args []any
}

// Queryer is the query capability shared by *sql.DB, *sql.Tx and *sql.Conn.
// The caller retains ownership of the handle and any transaction.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// All executes a search relation and maps its rows to caller-defined values.
// scan reads the current row only; All owns iteration and closing the rows.
// On any query, scan or iteration error, All returns no partial result.
func (q Query) All[T any](ctx context.Context, db Queryer, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search rows: %w", err)
	}
	return result, nil
}
