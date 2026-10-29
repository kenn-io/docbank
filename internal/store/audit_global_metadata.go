package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

// persistAuditedGlobalMetadataChanges records catalog authority without node events.
func (s *Store) persistAuditedGlobalMetadataChanges(
	ctx context.Context, tx *sql.Tx, changes []audit.Record,
) error {
	authority, nodeSequence, err := s.loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating audited global metadata operation: %w", err)
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, authority.lineageID, operationID, nowRFC3339())
	if err != nil {
		return err
	}
	delta, digest, err := makeAttachedMetadataDelta(values.operationID, changes)
	if err != nil {
		return err
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, audit.Absent())
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(
		allocation, uint64(len(changes)), digest.value)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}
