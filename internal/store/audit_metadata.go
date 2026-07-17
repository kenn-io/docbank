package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

var auditRecordKinds = map[string]bool{
	"enrollment_baseline":       true,
	"topology_genesis":          true,
	"attached_metadata_genesis": true,
	"event":                     true,
	"canonical_mutation":        true,
	"scope_chain_entry":         true,
	"allocation_genesis":        true,
	"allocation_entry":          true,
	auditTopologyDeltaField:     true,
}

type auditRecordIndex struct {
	operationID       *string
	operationSequence *int64
	scopeID           *string
	entryCount        *int64
	eventID           *string
	eventOrdinal      *int64
}

type storedAuditRecord struct {
	digest string
	record audit.Record
	index  auditRecordIndex
}

func exportAuditMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	var authority metadataAuditAuthority
	err := tx.QueryRowContext(ctx, `SELECT lineage_id,operation_sequence_high_water,
		allocation_genesis_digest,allocation_entry_count,allocation_head
		FROM audit_authority WHERE singleton=1`).Scan(
		&authority.LineageID, &authority.OperationSequenceHighWater,
		&authority.AllocationGenesisDigest, &authority.AllocationEntryCount,
		&authority.AllocationHead)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("exporting audit authority: %w", err)
	}
	authority.Type = metadataAuditAuthorityType
	if err := write(authority); err != nil {
		return err
	}
	if err := exportAuditScopes(ctx, tx, write); err != nil {
		return err
	}
	if err := exportAuditMemberships(ctx, tx, write); err != nil {
		return err
	}
	return exportAuditRecords(ctx, tx, write)
}

func exportAuditScopes(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT scope_id,target_node_id,enable_operation_id,
		entry_count,chain_head FROM audit_scopes ORDER BY scope_id`)
	if err != nil {
		return fmt.Errorf("exporting audit scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataAuditScope{Type: metadataAuditScopeType}
		if err := rows.Scan(&value.ScopeID, &value.TargetNodeID, &value.EnableOperationID,
			&value.EntryCount, &value.ChainHead); err != nil {
			return fmt.Errorf("scanning audit scope: %w", err)
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("audit scopes", rows)
}

func exportAuditMemberships(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT scope_id,node_id,baseline_digest
		FROM audit_memberships ORDER BY scope_id,node_id`)
	if err != nil {
		return fmt.Errorf("exporting audit memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataAuditMembership{Type: metadataAuditMembershipType}
		if err := rows.Scan(&value.ScopeID, &value.NodeID, &value.BaselineDigest); err != nil {
			return fmt.Errorf("scanning audit membership: %w", err)
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("audit memberships", rows)
}

func exportAuditRecords(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT digest,record_json FROM audit_records
		ORDER BY CASE kind
		  WHEN 'topology_genesis' THEN 1
		  WHEN 'attached_metadata_genesis' THEN 2
		  WHEN 'allocation_genesis' THEN 3
		  WHEN 'enrollment_baseline' THEN 4
		  WHEN 'event' THEN 5
		  WHEN 'canonical_mutation' THEN 6
		  WHEN 'scope_chain_entry' THEN 7
		  WHEN 'allocation_entry' THEN 8
		  ELSE 99 END,
		COALESCE(operation_sequence,0),COALESCE(scope_id,''),
		COALESCE(entry_count,0),COALESCE(event_ordinal,0),digest`)
	if err != nil {
		return fmt.Errorf("exporting audit records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataAuditRecord{Type: metadataAuditRecordType}
		var recordJSON string
		if err := rows.Scan(&value.Digest, &recordJSON); err != nil {
			return fmt.Errorf("scanning audit record: %w", err)
		}
		value.Record = json.RawMessage(recordJSON)
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("audit records", rows)
}

func importAuditAuthority(ctx context.Context, tx *sql.Tx, value metadataAuditAuthority) error {
	if err := validateAuditAuthorityRecord(value); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_authority(
		singleton,lineage_id,operation_sequence_high_water,allocation_genesis_digest,
		allocation_entry_count,allocation_head) VALUES(1,?,?,?,?,?)`, value.LineageID,
		value.OperationSequenceHighWater, value.AllocationGenesisDigest,
		value.AllocationEntryCount, value.AllocationHead)
	return err
}

func importAuditScope(ctx context.Context, tx *sql.Tx, value metadataAuditScope) error {
	if err := validateAuditScopeRecord(value); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_scopes(
		scope_id,target_node_id,enable_operation_id,entry_count,chain_head)
		VALUES(?,?,?,?,?)`, value.ScopeID, value.TargetNodeID, value.EnableOperationID,
		value.EntryCount, value.ChainHead)
	return err
}

func importAuditMembership(ctx context.Context, tx *sql.Tx, value metadataAuditMembership) error {
	if err := validateAuditMembershipRecord(value); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_memberships(
		scope_id,node_id,baseline_digest) VALUES(?,?,?)`,
		value.ScopeID, value.NodeID, value.BaselineDigest)
	return err
}

func importAuditRecord(ctx context.Context, tx *sql.Tx, value metadataAuditRecord) error {
	record, index, canonical, err := validateAuditRecord(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_records(
		digest,kind,operation_id,operation_sequence,scope_id,entry_count,
		event_id,event_ordinal,record_json) VALUES(?,?,?,?,?,?,?,?,?)`,
		value.Digest, record.Kind, index.operationID, index.operationSequence,
		index.scopeID, index.entryCount, index.eventID, index.eventOrdinal, string(canonical))
	if err != nil || record.Kind != "enrollment_baseline" {
		return err
	}
	scopeID, err := auditUUIDField(record, auditScopeIDField)
	if err != nil {
		return err
	}
	targetNodeID, err := auditUnsignedField(record, "target_node_id")
	if err != nil {
		return err
	}
	operationID, err := auditUUIDField(record, auditOperationIDField)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_baselines(
		digest,scope_id,target_node_id,operation_id) VALUES(?,?,?,?)`,
		value.Digest, scopeID, targetNodeID, operationID)
	return err
}

func validateAuditAuthorityRecord(value metadataAuditAuthority) error {
	if value.Type != metadataAuditAuthorityType || value.OperationSequenceHighWater == 0 ||
		value.OperationSequenceHighWater > math.MaxInt64 || value.AllocationEntryCount == 0 ||
		value.AllocationEntryCount > math.MaxInt64 {
		return errors.New("invalid audit authority record")
	}
	if err := validateUUIDv4(value.LineageID); err != nil {
		return fmt.Errorf("invalid audit lineage ID: %w", err)
	}
	if err := validateAuditDigest("allocation genesis", value.AllocationGenesisDigest); err != nil {
		return err
	}
	return validateAuditDigest("allocation head", value.AllocationHead)
}

func validateAuditScopeRecord(value metadataAuditScope) error {
	if value.Type != metadataAuditScopeType || value.TargetNodeID == 0 ||
		value.TargetNodeID > math.MaxInt64 || value.EntryCount == 0 || value.EntryCount > math.MaxInt64 {
		return errors.New("invalid audit scope record")
	}
	if err := validateUUIDv4(value.ScopeID); err != nil {
		return fmt.Errorf("invalid audit scope ID: %w", err)
	}
	if err := validateUUIDv4(value.EnableOperationID); err != nil {
		return fmt.Errorf("invalid audit enable operation ID: %w", err)
	}
	return validateAuditDigest("scope chain head", value.ChainHead)
}

func validateAuditMembershipRecord(value metadataAuditMembership) error {
	if value.Type != metadataAuditMembershipType || value.NodeID == 0 || value.NodeID > math.MaxInt64 {
		return errors.New("invalid audit membership record")
	}
	if err := validateUUIDv4(value.ScopeID); err != nil {
		return fmt.Errorf("invalid audit membership scope ID: %w", err)
	}
	return validateAuditDigest("membership baseline", value.BaselineDigest)
}

func validateAuditRecord(value metadataAuditRecord) (audit.Record, auditRecordIndex, []byte, error) {
	if value.Type != metadataAuditRecordType {
		return audit.Record{}, auditRecordIndex{}, nil, errors.New("invalid audit record wrapper")
	}
	if err := validateAuditDigest("record", value.Digest); err != nil {
		return audit.Record{}, auditRecordIndex{}, nil, err
	}
	record, err := audit.UnmarshalJSONRecord(value.Record)
	if err != nil {
		return audit.Record{}, auditRecordIndex{}, nil,
			fmt.Errorf("decoding canonical audit record: %w", err)
	}
	if !auditRecordKinds[record.Kind] {
		return audit.Record{}, auditRecordIndex{}, nil,
			fmt.Errorf("audit record kind %q is not valid in metadata-v1 audit authority", record.Kind)
	}
	digest, err := audit.Hash(record)
	if err != nil {
		return audit.Record{}, auditRecordIndex{}, nil, err
	}
	if hex.EncodeToString(digest[:]) != value.Digest {
		return audit.Record{}, auditRecordIndex{}, nil,
			errors.New("audit record digest does not match its canonical fields")
	}
	canonical, err := audit.MarshalJSONRecord(record)
	if err != nil {
		return audit.Record{}, auditRecordIndex{}, nil, err
	}
	index, err := indexAuditRecord(record)
	if err != nil {
		return audit.Record{}, auditRecordIndex{}, nil, err
	}
	return record, index, canonical, nil
}

func indexAuditRecord(record audit.Record) (auditRecordIndex, error) {
	var index auditRecordIndex
	switch record.Kind {
	case "enrollment_baseline":
		operationID, err := auditUUIDField(record, auditOperationIDField)
		if err != nil {
			return index, err
		}
		scopeID, err := auditUUIDField(record, auditScopeIDField)
		if err != nil {
			return index, err
		}
		index.operationID, index.scopeID = &operationID, &scopeID
	case "event":
		event, err := auditNestedField(record, "event")
		if err != nil {
			return index, err
		}
		operationID, err := auditUUIDField(event, auditOperationIDField)
		if err != nil {
			return index, err
		}
		scopeID, err := auditUUIDField(event, auditScopeIDField)
		if err != nil {
			return index, err
		}
		eventID, err := auditDigestField(event, "event_id")
		if err != nil {
			return index, err
		}
		ordinal, err := auditInt64UnsignedField(event, "event_ordinal")
		if err != nil {
			return index, err
		}
		index.operationID, index.scopeID = &operationID, &scopeID
		index.eventID, index.eventOrdinal = &eventID, &ordinal
	case "canonical_mutation", "allocation_entry":
		operationID, err := auditUUIDField(record, auditOperationIDField)
		if err != nil {
			return index, err
		}
		sequence, err := auditInt64UnsignedField(record, "operation_sequence")
		if err != nil {
			return index, err
		}
		index.operationID, index.operationSequence = &operationID, &sequence
	case "scope_chain_entry":
		scopeID, err := auditUUIDField(record, auditScopeIDField)
		if err != nil {
			return index, err
		}
		entryCount, err := auditInt64UnsignedField(record, "entry_count")
		if err != nil {
			return index, err
		}
		index.scopeID, index.entryCount = &scopeID, &entryCount
	case "topology_genesis", "attached_metadata_genesis", "allocation_genesis", auditTopologyDeltaField:
	default:
		return index, fmt.Errorf("unsupported initial audit record kind %q", record.Kind)
	}
	return index, nil
}

func validateAuditDigest(subject, value string) error {
	if _, err := audit.DigestHex(value); err != nil {
		return fmt.Errorf("invalid audit %s digest: %w", subject, err)
	}
	return nil
}

func auditField(record audit.Record, name string) (audit.Value, error) {
	value, ok := audit.FieldValue(record, name)
	if !ok {
		return audit.Value{}, fmt.Errorf("audit record %s lacks field %q", record.Kind, name)
	}
	return value, nil
}

func auditUUIDField(record audit.Record, name string) (string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return "", err
	}
	result, ok := value.UUIDValue()
	if !ok {
		return "", fmt.Errorf("audit field %s.%s is not a UUID", record.Kind, name)
	}
	return result, nil
}

func auditDigestField(record audit.Record, name string) (string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return "", err
	}
	result, ok := value.DigestValue()
	if !ok {
		return "", fmt.Errorf("audit field %s.%s is not a digest", record.Kind, name)
	}
	return result, nil
}

func auditTextField(record audit.Record, name string) (string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return "", err
	}
	result, ok := value.TextValue()
	if !ok {
		return "", fmt.Errorf("audit field %s.%s is not text", record.Kind, name)
	}
	return result, nil
}

func auditBoolField(record audit.Record, name string) (bool, error) {
	value, err := auditField(record, name)
	if err != nil {
		return false, err
	}
	result, ok := value.BoolValue()
	if !ok {
		return false, fmt.Errorf("audit field %s.%s is not boolean", record.Kind, name)
	}
	return result, nil
}

func auditOptionalUUIDField(record audit.Record, name string) (*string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // A nil pointer is the canonical absent optional UUID.
	}
	result, ok := value.UUIDValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not an optional UUID", record.Kind, name)
	}
	return &result, nil
}

func auditUnsignedField(record audit.Record, name string) (uint64, error) {
	value, err := auditField(record, name)
	if err != nil {
		return 0, err
	}
	result, ok := value.UnsignedValue()
	if !ok {
		return 0, fmt.Errorf("audit field %s.%s is not unsigned", record.Kind, name)
	}
	return result, nil
}

func auditInt64UnsignedField(record audit.Record, name string) (int64, error) {
	value, err := auditUnsignedField(record, name)
	if err != nil {
		return 0, err
	}
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("audit field %s.%s exceeds SQLite integer range", record.Kind, name)
	}
	return int64(value), nil
}

func auditNestedField(record audit.Record, name string) (audit.Record, error) {
	value, err := auditField(record, name)
	if err != nil {
		return audit.Record{}, err
	}
	result, ok := value.RecordValue()
	if !ok {
		return audit.Record{}, fmt.Errorf("audit field %s.%s is not a nested record", record.Kind, name)
	}
	return result, nil
}

func auditListField(record audit.Record, name string) ([]audit.Value, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	result, ok := value.ListValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not a list", record.Kind, name)
	}
	return result, nil
}

func auditRecordList(values []audit.Value) ([]audit.Record, error) {
	result := make([]audit.Record, len(values))
	for index, value := range values {
		record, ok := value.RecordValue()
		if !ok {
			return nil, fmt.Errorf("audit list item %d is not a nested record", index)
		}
		result[index] = record
	}
	return result, nil
}

func auditUnsignedList(values []audit.Value) ([]uint64, error) {
	result := make([]uint64, len(values))
	for index, value := range values {
		item, ok := value.UnsignedValue()
		if !ok {
			return nil, fmt.Errorf("audit list item %d is not unsigned", index)
		}
		result[index] = item
	}
	return result, nil
}

func auditRecordEqual(left, right audit.Record) bool {
	leftBytes, leftErr := audit.Encode(left)
	rightBytes, rightErr := audit.Encode(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func sortAuditRecordsByCanonicalIdentity(
	records []audit.Record, identity func(audit.Record) (audit.Record, error),
) error {
	type keyed struct {
		record audit.Record
		kind   string
		key    []byte
	}
	items := make([]keyed, len(records))
	for index, record := range records {
		identityRecord, err := identity(record)
		if err != nil {
			return err
		}
		key, err := audit.Encode(identityRecord)
		if err != nil {
			return err
		}
		items[index] = keyed{record: record, kind: record.Kind, key: key}
	}
	slices.SortFunc(items, func(left, right keyed) int {
		if left.kind < right.kind {
			return -1
		}
		if left.kind > right.kind {
			return 1
		}
		return bytes.Compare(left.key, right.key)
	})
	for index := range items {
		records[index] = items[index].record
	}
	return nil
}
