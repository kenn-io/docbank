// Package pgschema owns PostgreSQL schema installation and runtime validation.
// It is an internal foundation; the document store still uses SQLite.
package pgschema

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Version identifies the current PostgreSQL layout, independently of SQLite.
// The first supported backend release establishes the migration floor. Until
// then, there is no released PostgreSQL layout to upgrade.
const Version = 1

//go:embed schema.sql
var schemaSQL string

// Config names an application's dedicated schema and its setup role. The
// operator provisions roles and grants; this package never creates roles or
// shares ownership with a runtime credential.
type Config struct {
	Schema      string
	SchemaOwner string
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Validate rejects ambient schemas and invalid names before connecting.
func (c Config) Validate() error {
	if !identifier.MatchString(c.Schema) || c.Schema == "public" || c.Schema == "information_schema" || strings.HasPrefix(c.Schema, "pg_") {
		return errors.New("postgres metadata requires a dedicated lowercase schema name")
	}
	if c.SchemaOwner == "" || len(c.SchemaOwner) > 63 || strings.ContainsRune(c.SchemaOwner, 0) {
		return errors.New("postgres metadata requires an explicit schema owner name of at most 63 bytes")
	}
	return nil
}

// Setup installs a fresh schema atomically or validates an existing one without
// changing its identity. The pool must authenticate as Config.SchemaOwner. Callers
// own the pool and deadline; no credentials are parsed, cached or logged here.
// Existing schemas without a valid identity are refused, including empty ones.
func Setup(ctx context.Context, db *sql.DB, c Config) (uuid.UUID, error) {
	if err := c.Validate(); err != nil {
		return uuid.Nil(), err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return uuid.Nil(), err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path = pg_catalog`); err != nil {
		return uuid.Nil(), err
	}
	var user string
	if err := tx.QueryRowContext(ctx, `SELECT current_user`).Scan(&user); err != nil {
		return uuid.Nil(), err
	}
	if user != c.SchemaOwner {
		return uuid.Nil(), errors.New("postgres metadata setup requires the schema owner role")
	}
	// Locks are database-local. Include the schema so unrelated vault setups do
	// not block one another. Read state in a NEW statement after acquiring it.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('docbank:metadata:' || $1, 0))`, c.Schema); err != nil {
		if ctx.Err() != nil {
			return uuid.Nil(), ctx.Err()
		}
		return uuid.Nil(), fmt.Errorf("lock postgres metadata setup: %w", err)
	}
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1`, c.Schema).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Use pgx's identifier quoting for the restricted schema and provisioned role.
		if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+pgx.Identifier{c.Schema}.Sanitize()+` AUTHORIZATION `+pgx.Identifier{c.SchemaOwner}.Sanitize()); err != nil {
			return uuid.Nil(), fmt.Errorf("create postgres metadata schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, strings.ReplaceAll(schemaSQL, "{{schema}}", pgx.Identifier{c.Schema}.Sanitize())); err != nil {
			return uuid.Nil(), fmt.Errorf("install postgres metadata schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.vault_metadata(singleton, vault_uid, schema_version) VALUES (1,$1,$2)`, uuid.New(), Version); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres vault identity: %w", err)
		}
	case err != nil:
		return uuid.Nil(), err
	case owner != c.SchemaOwner:
		return uuid.Nil(), errors.New("postgres metadata schema has a different owner")
	}
	id, err := validateIdentity(ctx, tx, c)
	if err != nil {
		return uuid.Nil(), err
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

// Validate reads an installed schema through a runtime role. It performs no
// DDL, grants or repairs. The runtime may read the identity, but must neither
// own the schema, assume its owner role, create objects nor change the identity.
func Validate(ctx context.Context, db *sql.DB, c Config) (uuid.UUID, error) {
	if err := c.Validate(); err != nil {
		return uuid.Nil(), err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return uuid.Nil(), err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path = pg_catalog`); err != nil {
		return uuid.Nil(), err
	}
	var owner string
	var canCreate, canAssumeOwner bool
	// SET ROLE follows session_user, even if the pool changed current_user.
	// PostgreSQL resolves indirect SET grants and inherited object privileges.
	if err := tx.QueryRowContext(ctx, `SELECT pg_get_userbyid(nspowner),
		EXISTS(SELECT 1 FROM pg_roles r
			WHERE (r.rolname=current_user OR pg_has_role(session_user, r.oid, 'SET'))
			AND has_schema_privilege(r.oid, n.oid, 'CREATE')),
		pg_has_role(current_user, nspowner, 'MEMBER')
		FROM pg_namespace n WHERE nspname=$1`, c.Schema).Scan(&owner, &canCreate, &canAssumeOwner); err != nil {
		return uuid.Nil(), fmt.Errorf("read postgres metadata schema: %w", err)
	}
	if owner != c.SchemaOwner || canCreate || canAssumeOwner {
		return uuid.Nil(), errors.New("postgres metadata requires a separate runtime role without schema ownership or CREATE")
	}
	id, err := validateIdentity(ctx, tx, c)
	if err != nil {
		return uuid.Nil(), err
	}
	var canWrite bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles r
		WHERE (r.rolname=current_user OR pg_has_role(session_user, r.oid, 'SET'))
		AND (has_table_privilege(r.oid, c.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
			OR has_any_column_privilege(r.oid, c.oid, 'INSERT,UPDATE')))
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname='vault_metadata'`, c.Schema).Scan(&canWrite); err != nil {
		return uuid.Nil(), err
	}
	if canWrite {
		return uuid.Nil(), errors.New("postgres runtime role must not change vault identity or schema version")
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

func validateIdentity(ctx context.Context, tx *sql.Tx, c Config) (uuid.UUID, error) {
	// Never execute a view or adopt a foreign table as the identity authority.
	var owner, kind, persistence string
	var relation int64
	err := tx.QueryRowContext(ctx, `SELECT c.oid, pg_get_userbyid(c.relowner), c.relkind, c.relpersistence
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname='vault_metadata'`, c.Schema).Scan(&relation, &owner, &kind, &persistence)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("read postgres vault identity table: %w", err)
	}
	if owner != c.SchemaOwner || kind != "r" || persistence != "p" {
		return uuid.Nil(), errors.New("postgres vault identity must be a permanent table owned by the setup role")
	}
	// Check the structural keys as well as row contents. A missing key cannot
	// be repaired by runtime startup or hidden by a currently well-formed row.
	var columns, keys int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid=$1 AND attnum>0 AND NOT attisdropped
		AND attnotnull AND ((attname='singleton' AND atttypid='int2'::regtype)
		OR (attname='vault_uid' AND atttypid='uuid'::regtype)
		OR (attname='schema_version' AND atttypid='int4'::regtype))`, relation).Scan(&columns)
	if err != nil {
		return uuid.Nil(), err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid=c.conrelid AND c.conkey=ARRAY[a.attnum]
		WHERE c.conrelid=$1 AND c.convalidated AND NOT c.condeferrable
		AND ((c.contype='p' AND a.attname='singleton') OR (c.contype='u' AND a.attname='vault_uid'))`, relation).Scan(&keys)
	if err != nil {
		return uuid.Nil(), err
	}
	if columns != 3 || keys != 2 {
		return uuid.Nil(), errors.New("postgres vault identity has an unsupported table layout")
	}
	rows, err := tx.QueryContext(ctx, `SELECT singleton, vault_uid, schema_version FROM `+pgx.Identifier{c.Schema}.Sanitize()+`.vault_metadata`)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("read postgres vault identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var id uuid.UUID
	var singleton, version, count int
	for rows.Next() {
		count++
		if count > 1 {
			return uuid.Nil(), errors.New("postgres metadata has multiple vault identities")
		}
		if err := rows.Scan(&singleton, &id, &version); err != nil {
			return uuid.Nil(), err
		}
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil(), err
	}
	if count != 1 || singleton != 1 || id == uuid.Nil() || id[6]>>4 != 4 || id[8]>>6 != 2 {
		return uuid.Nil(), errors.New("postgres metadata has no valid vault identity")
	}
	if version != Version {
		return uuid.Nil(), fmt.Errorf("postgres metadata schema version %d is not supported by this binary (requires %d)", version, Version)
	}
	return id, nil
}
