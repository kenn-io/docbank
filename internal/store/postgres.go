package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/waitlock"
)

// OpenPostgres opens an installed schema through runtime credentials. It owns
// the pool; Close releases it. Setup and logical import use separate owner
// credentials. This internal entry point does not open a public Vault.
func OpenPostgres(ctx context.Context, connection *pgx.ConnConfig, cfg pgschema.Config) (result *Store, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, errors.New("postgres metadata requires a connection configuration")
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
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema, providerEgressMu: waitlock.New(), photoHiddenSessions: make(map[string]time.Time), photoExportSlot: make(chan struct{}, 1)}
	if err := db.QueryRowContext(ctx, `SELECT id,(SELECT vault_uid FROM vault_metadata),(SELECT store_id FROM blob_stores WHERE role='primary') FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID, &s.vaultID, &s.primaryStoreID); err != nil {
		return nil, fmt.Errorf("reading postgres vault roots: %w", err)
	}
	return s, nil
}
