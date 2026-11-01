package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Audit projection updates share validation with both metadata backends.
// Their callers own the records and the storage transaction that publishes them.
func updateAuditAuthorityHead(ctx context.Context, tx *sql.Tx, sequence, count int64, head string) error {
	if _, err := positiveAuditInteger("operation sequence", sequence); err != nil {
		return err
	}
	if _, err := positiveAuditInteger("allocation entry count", count); err != nil {
		return err
	}
	if sequence != count {
		return errors.New("audit allocation count disagrees with operation sequence")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_authority
		SET operation_sequence_high_water=$1,allocation_entry_count=$2,allocation_head=$3
		WHERE singleton=1`, sequence, count, head); err != nil {
		return fmt.Errorf("advancing audit allocation authority: %w", err)
	}
	return nil
}

func updateAuditScopeHead(ctx context.Context, tx *sql.Tx, scopeID string, count int64, head string) error {
	if _, err := positiveAuditInteger("scope entry count", count); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_scopes
		SET entry_count=$1,chain_head=$2 WHERE scope_id=$3`, count, head, scopeID); err != nil {
		return fmt.Errorf("advancing audit scope %s: %w", scopeID, err)
	}
	return nil
}
