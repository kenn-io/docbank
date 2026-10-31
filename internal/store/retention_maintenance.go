package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
)

// beforeContentVersionDeletionTx accounts for the rows that cascades will remove.
// Released SQLite triggers own these epochs; PostgreSQL advances them in the
// same writer transaction before assertions and event heads disappear.
func (s *Store) beforeContentVersionDeletionTx(ctx context.Context, tx *sql.Tx, versionIDs []string) error {
	if s.postgresSchema == "" || len(versionIDs) == 0 {
		return nil
	}
	encoded, err := json.Marshal(versionIDs)
	if err != nil {
		return err
	}
	var bindings, heads int64
	if err := tx.QueryRowContext(ctx, `WITH versions(id) AS (
  SELECT value FROM jsonb_array_elements_text(CAST($1 AS jsonb))
 ) SELECT
  (SELECT count(*) FROM versions v WHERE EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.content_version_id=v.id)),
  (SELECT count(*) FROM document_event_heads h JOIN versions v ON h.content_version_id=v.id)`, string(encoded)).Scan(&bindings, &heads); err != nil {
		return fmt.Errorf("reading deleted version projections: %w", err)
	}
	now := nowRFC3339()
	if bindings > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+$1,updated_at=$2 WHERE singleton=1`, bindings, now); err != nil {
			return fmt.Errorf("invalidating deleted person bindings: %w", err)
		}
	}
	if heads > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE document_event_state SET publication_epoch=publication_epoch+$1,updated_at=$2 WHERE singleton=1`, heads, now); err != nil {
			return fmt.Errorf("invalidating deleted event heads: %w", err)
		}
	}
	return nil
}
