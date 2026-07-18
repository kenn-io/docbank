package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (s *Store) persistAuditedTagCreation(
	ctx context.Context, tx *sql.Tx, tag Tag,
) error {
	authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("allocating audited tag-creation operation: %w", err)
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, authority.lineageID, operationID, nowRFC3339(),
	)
	if err != nil {
		return err
	}
	definition, err := tagDefinitionAuditRecord(tag)
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataPresenceChange(definition, true)
	if err != nil {
		return err
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(
		values.operationID, []audit.Record{change},
	)
	if err != nil {
		return err
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, audit.Absent(),
	)
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(
		allocation, 1, deltaDigest.value,
	)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}
