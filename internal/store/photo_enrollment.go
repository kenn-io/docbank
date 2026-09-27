package store

import (
	"context"
	"database/sql"
	"fmt"
)

// enrollNewPhotoFileTx creates a singleton asset for a file node created
// earlier in tx when its media qualifies. A new node is live and unowned, so
// only its media decides. It never writes a human decision receipt and it is
// a no-op once audit authority is active.
func (s *Store) enrollNewPhotoFileTx(ctx context.Context, tx *sql.Tx, created Node) error {
	facts := photoNodeFacts(created)
	if !facts.Qualifies {
		return nil
	}
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	if _, err := s.insertPhotoAssetTx(ctx, tx, created.ID, inferPhotoRole(facts), facts.AssetKind); err != nil {
		return fmt.Errorf("enrolling photo node %d: %w", created.ID, err)
	}
	return nil
}
