package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/waitlock"
)

// OpenPostgres opens an installed schema through runtime credentials. It owns
// the pool; Close releases it. vaultDir locates local worker controls. Setup and
// logical import use separate owner credentials.
func OpenPostgres(ctx context.Context, connection *pgx.ConnConfig, cfg pgschema.Config, vaultDir string) (result *Store, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, errors.New("postgres metadata requires a connection configuration")
	}
	if vaultDir == "" {
		return nil, errors.New("postgres metadata requires a local vault directory")
	}
	vaultDir, err = filepath.Abs(vaultDir)
	if err != nil {
		return nil, fmt.Errorf("resolving postgres vault directory: %w", err)
	}
	config := connection.Copy()
	config.RuntimeParams["search_path"] = pgx.Identifier{cfg.Schema}.Sanitize() + ",pg_catalog"
	db := stdlib.OpenDB(*config)
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	var effective string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&effective); err != nil {
		return nil, fmt.Errorf("checking postgres schema binding: %w", err)
	}
	if effective != cfg.Schema {
		return nil, errors.New("postgres metadata connection does not use the configured schema")
	}
	if _, err := pgschema.Validate(ctx, db, cfg); err != nil {
		return nil, fmt.Errorf("validating postgres metadata: %w", err)
	}
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema, postgresConfig: config.Copy(), vaultDir: vaultDir, providerEgressMu: waitlock.New(), photoHiddenSessions: make(map[string]time.Time), photoExportSlot: make(chan struct{}, 1)}
	if err := db.QueryRowContext(ctx, `SELECT n.id,v.vault_uid,b.store_id,v.primary_storage_bound
		FROM nodes n CROSS JOIN vault_metadata v CROSS JOIN blob_stores b
		WHERE n.parent_id IS NULL AND v.singleton=1 AND b.role='primary'`).
		Scan(&s.rootID, &s.vaultID, &s.primaryStoreID, &s.postgresPrimaryBound); err != nil {
		return nil, fmt.Errorf("reading postgres vault roots: %w", err)
	}
	return s, nil
}

// PostgresPrimaryBound reports whether initialization or restore published
// physical storage for this PostgreSQL catalog.
func (s *Store) PostgresPrimaryBound() bool { return s.postgresPrimaryBound }

func selectPostgresStorageSchema(ctx context.Context, tx *sql.Tx, schema string) error {
	// PostgreSQL implicitly searches temporary relations first. List pg_temp
	// last so a reused owner session gives the vault catalog priority.
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path=`+pgx.Identifier{schema}.Sanitize()+`,pg_catalog,pg_temp`); err != nil {
		return fmt.Errorf("selecting postgres storage schema: %w", err)
	}
	return nil
}
