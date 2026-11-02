package app

import (
	"context"
	"database/sql"

	"dep"
)

func Count(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	return dep.Count(rows)
}
