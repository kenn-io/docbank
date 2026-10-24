package store

import (
	"context"
	"fmt"
)

// RequireBackupClaim prevents captures of a restored vault until the operator
// claims a backup repository.
func (s *Store) RequireBackupClaim(ctx context.Context) error {
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE vault_metadata SET backup_claim_required = 1 WHERE singleton = 1`); err != nil {
		return fmt.Errorf("requiring backup claim: %w", err)
	}
	return nil
}

// BackupClaimRequired reports whether this restored vault still needs an
// operator to claim a backup repository before capturing.
func (s *Store) BackupClaimRequired(ctx context.Context) (bool, error) {
	var required bool
	if err := s.db.QueryRowContext(ctx, `SELECT backup_claim_required FROM vault_metadata WHERE singleton = 1`).Scan(&required); err != nil {
		return false, fmt.Errorf("reading backup claim requirement: %w", err)
	}
	return required, nil
}

// ConfirmBackupClaim permits captures after the operator claims a repository.
func (s *Store) ConfirmBackupClaim(ctx context.Context) error {
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE vault_metadata SET backup_claim_required = 0 WHERE singleton = 1`); err != nil {
		return fmt.Errorf("confirming backup claim: %w", err)
	}
	return nil
}
