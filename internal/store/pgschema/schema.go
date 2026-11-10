// Package pgschema owns PostgreSQL schema installation and runtime validation.
// The public vault lifecycle still uses SQLite.
package pgschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/docbank/internal/store/schemaformat"
)

//go:embed schema.sql
var schemaSQL string

// Hash the template before schema-name substitution so vault names do not affect
// compatibility. Retained deployments will start a numbered migration chain.
var schemaFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(schemaSQL)))

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
// changing its identity or root. The pool must authenticate as Config.SchemaOwner. Callers
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
	if err := validateEncoding(ctx, tx); err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.vault_metadata(singleton, vault_uid, schema_fingerprint) VALUES (1,$1,$2)`, uuid.New(), schemaFingerprint); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres vault identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.node_allocation(singleton, high_water) VALUES (1,1)`); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres node allocation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.vault_change_head(singleton,epoch) VALUES(1,$1)`, uuid.New().String()); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres change feed: %w", err)
		}
		// Shared queries compare timestamps as text, using fixed-width UTC values.
		now := time.Now().UTC().Format(schemaformat.TimestampLayout)
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.nodes(id, name, kind, created_at, modified_at) VALUES (1,'','dir',$1,$1)`, now); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres root: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.document_people_state(singleton,binding_epoch,updated_at) VALUES (1,1,$1)`, now); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres people state: %w", err)
		}
		// Consent belongs to this storage incarnation, including an empty vault.
		// Restore keeps a fresh target's pointer rather than reviving old grants.
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.processing_incarnations(incarnation_id,created_at) VALUES ($1::uuid::text,$2)`, uuid.New(), now); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres processing incarnation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.current_processing_incarnation(singleton,incarnation_id) SELECT 1,incarnation_id FROM `+pgx.Identifier{c.Schema}.Sanitize()+`.processing_incarnations`); err != nil {
			return uuid.Nil(), fmt.Errorf("activate postgres processing incarnation: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO `+pgx.Identifier{c.Schema}.Sanitize()+`.blob_stores
			(store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at)
			VALUES ($1::uuid::text,'primary','filesystem','primary','active','builtin-primary',$2::uuid::text,$3)`, uuid.New(), uuid.New(), now); err != nil {
			return uuid.Nil(), fmt.Errorf("initialize postgres primary blob store: %w", err)
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
	if err := validateEncoding(ctx, tx); err != nil {
		return uuid.Nil(), err
	}
	var owner string
	var canCreate, canAssumeOwner bool
	// SET ROLE follows session_user, even if the pool changed current_user.
	// PostgreSQL resolves indirect SET grants and inherited object privileges.
	// Ownership survives revoked object ACLs; reject owner membership for both
	// identities independently. MEMBER includes indirect role memberships.
	if err := tx.QueryRowContext(ctx, `SELECT pg_get_userbyid(nspowner),
		EXISTS(SELECT 1 FROM pg_roles r
			WHERE (r.rolname=current_user OR pg_has_role(session_user, r.oid, 'SET'))
			AND has_schema_privilege(r.oid, n.oid, 'CREATE')),
		pg_has_role(current_user, nspowner, 'MEMBER')
			OR pg_has_role(session_user, nspowner, 'MEMBER')
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
		return uuid.Nil(), errors.New("postgres runtime role must not change vault identity")
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

func validateEncoding(ctx context.Context, tx *sql.Tx) error {
	var encoding string
	if err := tx.QueryRowContext(ctx, `SHOW server_encoding`).Scan(&encoding); err != nil {
		return fmt.Errorf("read postgres database encoding: %w", err)
	}
	// Unicode word boundaries must operate on code points rather than bytes.
	if encoding != "UTF8" {
		return fmt.Errorf("postgres metadata requires UTF8 database encoding; got %s", encoding)
	}
	return nil
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
		OR (attname='schema_fingerprint' AND atttypid='text'::regtype))`, relation).Scan(&columns)
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
		return uuid.Nil(), errors.New("postgres vault identity has an unsupported table layout; recreate this schema with the current build")
	}
	rows, err := tx.QueryContext(ctx, `SELECT singleton, vault_uid, schema_fingerprint FROM `+pgx.Identifier{c.Schema}.Sanitize()+`.vault_metadata`)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("read postgres vault identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var id uuid.UUID
	var singleton, count int
	var fingerprint string
	for rows.Next() {
		count++
		if count > 1 {
			return uuid.Nil(), errors.New("postgres metadata has multiple vault identities")
		}
		if err := rows.Scan(&singleton, &id, &fingerprint); err != nil {
			return uuid.Nil(), err
		}
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil(), err
	}
	if count != 1 || singleton != 1 || id == uuid.Nil() || id[6]>>4 != 4 || id[8]>>6 != 2 {
		return uuid.Nil(), errors.New("postgres metadata has no valid vault identity")
	}
	if fingerprint != schemaFingerprint {
		return uuid.Nil(), errors.New("postgres metadata schema fingerprint does not match this build; recreate this schema with the current build")
	}
	return id, nil
}
