package pgschema

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed runtime_grants.sql
var runtimeGrantsSQL string

// GrantRuntime replaces the schema owner's direct grants to a dedicated runtime login.
func GrantRuntime(ctx context.Context, db *sql.DB, c Config, role string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if role == "" || len(role) > 63 || strings.ContainsRune(role, 0) {
		return errors.New("postgres runtime grants require a login name of at most 63 bytes")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path = pg_catalog, pg_temp`); err != nil {
		return err
	}
	if err := lockSchemaSetup(ctx, tx, c.Schema); err != nil {
		return err
	}
	if err := validateRuntimeGrantTarget(ctx, tx, c, role); err != nil {
		return err
	}
	identity, err := readIdentity(ctx, tx, c)
	if err != nil {
		return err
	}
	if err := checkMigrationState(identity, migrations, true); err != nil {
		return err
	}
	grants := strings.NewReplacer(
		"{{schema}}", pgx.Identifier{c.Schema}.Sanitize(),
		"{{role}}", pgx.Identifier{role}.Sanitize(),
	).Replace(runtimeGrantsSQL)
	if _, err := tx.ExecContext(ctx, grants); err != nil {
		return fmt.Errorf("apply postgres runtime grants: %w", err)
	}
	return tx.Commit()
}

func validateRuntimeGrantTarget(ctx context.Context, tx *sql.Tx, c Config, role string) error {
	var isOwner bool
	if err := tx.QueryRowContext(ctx, `SELECT current_user::text=$1 AND session_user::text=$1`, c.SchemaOwner).Scan(&isOwner); err != nil {
		return err
	}
	if !isOwner {
		return errors.New("postgres runtime grants require a schema owner session")
	}
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1`, c.Schema).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres runtime grants: schema does not exist: %q", c.Schema)
	}
	if err != nil {
		return err
	}
	if owner != c.SchemaOwner {
		return fmt.Errorf("postgres runtime grants: schema %q is owned by %q", c.Schema, owner)
	}

	var canLogIn, hasOwnerAuthority, hasTemp bool
	err = tx.QueryRowContext(ctx, `SELECT rolcanlogin, rolsuper OR pg_has_role(oid, $2::name, 'MEMBER'),
		has_database_privilege(oid, current_database(), 'TEMPORARY')
		FROM pg_roles WHERE rolname::text=$1`, role, owner).Scan(&canLogIn, &hasOwnerAuthority, &hasTemp)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("postgres runtime grants: role %q does not exist", role)
	case err != nil:
		return err
	case !canLogIn:
		return fmt.Errorf("postgres runtime grants: role %q cannot log in", role)
	case hasOwnerAuthority:
		return errors.New("postgres runtime grants require a separate runtime login without schema owner authority")
	case !hasTemp:
		return fmt.Errorf("postgres runtime grants: role %q needs TEMPORARY on the database", role)
	default:
		return nil
	}
}
