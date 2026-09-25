package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// enrollPhotoCandidatesTx creates a singleton asset for each qualifying,
// unowned, live file. It never writes a human decision receipt and it is a
// no-op once audit authority is active.
func (s *Store) enrollPhotoCandidatesTx(ctx context.Context, tx *sql.Tx, nodeIDs ...int64) error {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		_, err := s.photoAssetCreateTx(ctx, tx, nodeID, "", "")
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrPhotoNodeNotEligible) ||
			errors.Is(err, ErrPhotoNodeOwned) {
			continue
		}
		return fmt.Errorf("enrolling photo node %d: %w", nodeID, err)
	}
	return nil
}
