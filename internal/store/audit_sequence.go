package store

import (
	"context"
	"fmt"
)

// auditNodeSequence reads transactional allocation authority, including deleted IDs.
func (s *Store) auditNodeSequence(ctx context.Context, tx rowQuerier) (int64, error) {
	query := `SELECT seq FROM sqlite_sequence WHERE name='nodes'`
	if s.postgresSchema != "" {
		query = `SELECT high_water FROM node_allocation WHERE singleton=1`
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, query).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	return sequence, nil
}
