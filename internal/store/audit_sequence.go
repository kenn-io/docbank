package store

import (
	"context"
	"fmt"
)

// auditNodeSequence reads allocation authority, including deleted node IDs.
// PostgreSQL sequence advancement survives rollback and is not an MVCC snapshot.
func (s *Store) auditNodeSequence(ctx context.Context, tx rowQuerier) (int64, error) {
	query := `SELECT seq FROM sqlite_sequence WHERE name='nodes'`
	if s.postgresSchema != "" {
		// Resolve the identity's sequence as Kata does; neither its generated
		// name nor the maximum retained node ID is allocation authority.
		query = `SELECT s.last_value FROM pg_sequences s
			WHERE s.schemaname=current_schema()
			AND to_regclass(format('%I.%I',s.schemaname,s.sequencename))=
				to_regclass(pg_get_serial_sequence(format('%I.%I',current_schema(),'nodes'),'id'))`
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, query).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	return sequence, nil
}
