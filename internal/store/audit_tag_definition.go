package store

import (
	"context"
	"database/sql"

	"go.kenn.io/docbank/internal/audit"
)

func (s *Store) persistAuditedTagCreation(
	ctx context.Context, tx *sql.Tx, tag Tag,
) error {
	definition, err := tagDefinitionAuditRecord(tag)
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataPresenceChange(definition, true)
	if err != nil {
		return err
	}
	return s.persistAuditedGlobalMetadataChanges(ctx, tx, []audit.Record{change})
}
