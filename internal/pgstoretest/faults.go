package pgstoretest

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RevokeColumnUpdates injects a permission failure at a specific write.
// PostgreSQL checks a table-level UPDATE grant before any column grant.
func RevokeColumnUpdates(ctx context.Context, setup *sql.DB, quotedRole, table string, denied ...string) error {
	tx, err := setup.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var allowed string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(string_agg(quote_ident(a.attname), ', ' ORDER BY a.attnum), '')
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped
		  AND NOT (a.attname::text=ANY($3))`, templateSchema, table, denied).Scan(&allowed); err != nil {
		return err
	}
	if allowed == "" {
		return fmt.Errorf("no remaining update columns on fixture table %q", table)
	}
	relation := pgx.Identifier{templateSchema, table}.Sanitize()
	if _, err := tx.ExecContext(ctx, "REVOKE UPDATE ON "+relation+" FROM "+quotedRole+
		"; GRANT UPDATE ("+allowed+") ON "+relation+" TO "+quotedRole); err != nil {
		return err
	}
	return tx.Commit()
}
