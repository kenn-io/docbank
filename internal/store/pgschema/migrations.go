package pgschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationName = regexp.MustCompile(`^[0-9]{6}_[a-z0-9_]+\.up\.sql$`)
var migrations, migrationLoadErr = loadMigrations(migrationFiles)

type migration struct {
	name, ddl, fingerprint string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading postgres migration directory: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("postgres migration chain has no baseline")
	}
	history := sha256.New()
	chain := make([]migration, 0, len(entries))
	for i, entry := range entries {
		name := entry.Name()
		if !migrationName.MatchString(name) || !strings.HasPrefix(name, fmt.Sprintf("%06d_", i+1)) {
			return nil, fmt.Errorf("postgres migration %q must be forward migration %06d", name, i+1)
		}
		ddl, err := fs.ReadFile(files, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("reading postgres migration %s: %w", name, err)
		}
		if len(strings.TrimSpace(string(ddl))) == 0 {
			return nil, fmt.Errorf("postgres migration %s is empty", name)
		}
		// The fingerprint commits to names and exact bytes of every applied
		// migration so a later binary can verify any supported prefix.
		_, _ = fmt.Fprintf(history, "%s\x00%d\x00", name, len(ddl))
		_, _ = history.Write(ddl)
		chain = append(chain, migration{name: name, ddl: string(ddl), fingerprint: hex.EncodeToString(history.Sum(nil))})
	}
	return chain, nil
}

func checkMigrationState(installed schemaIdentity, chain []migration, requireCurrent bool) error {
	version := installed.version
	if version < 1 || version > len(chain) {
		return fmt.Errorf("postgres metadata schema version %d is outside this build's supported range 1–%d", version, len(chain))
	}
	if installed.fingerprint != chain[version-1].fingerprint {
		return fmt.Errorf("postgres metadata migration history differs at version %d; use a matching build or restore a compatible backup", version)
	}
	if requireCurrent && version != len(chain) {
		return fmt.Errorf("postgres metadata schema is at version %d; run setup as the schema owner to upgrade to version %d", version, len(chain))
	}
	return nil
}

func applyMigrations(ctx context.Context, tx *sql.Tx, schema string, installed schemaIdentity, chain []migration) error {
	for i := installed.version; i < len(chain); i++ {
		next := chain[i]
		if _, err := tx.ExecContext(ctx, strings.ReplaceAll(next.ddl, "{{schema}}", pgx.Identifier{schema}.Sanitize())); err != nil {
			return fmt.Errorf("applying postgres migration %s: %w", next.name, err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE `+pgx.Identifier{schema, "vault_metadata"}.Sanitize()+
			` SET schema_version=$1, schema_fingerprint=$2 WHERE singleton=1 AND schema_version=$3 AND vault_uid=$4`,
			i+1, next.fingerprint, i, installed.id)
		if err != nil {
			return fmt.Errorf("recording postgres migration %s: %w", next.name, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking postgres migration %s stamp: %w", next.name, err)
		}
		if changed != 1 {
			return fmt.Errorf("postgres migration %s changed the installed vault identity or version", next.name)
		}
	}
	return nil
}
