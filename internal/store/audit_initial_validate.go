package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type initialAuditAuthority struct {
	lineageID       string
	sequence        int64
	genesisDigest   string
	allocationCount int64
	allocationHead  string
}

type initialAuditScope struct {
	scopeID      string
	targetNodeID uint64
	operationID  string
	entryCount   int64
	chainHead    string
}

func validateAuditAuthority(
	ctx context.Context, tx metadataQuerier, vaultID string, nodeSequence int64,
) error {
	counts, err := auditAuthorityCounts(ctx, tx)
	if err != nil {
		return err
	}
	if counts[0] == 0 {
		for index, count := range counts {
			if count != 0 {
				return fmt.Errorf("dormant audit authority has %d row(s) in relation %d", count, index)
			}
		}
		return nil
	}
	if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 || counts[3] == 0 {
		return fmt.Errorf("audit authority must contain one authority, scope, and baseline: counts=%v", counts)
	}
	authority, scope, err := loadInitialAuditProjection(ctx, tx)
	if err != nil {
		return err
	}
	records, err := loadInitialAuditRecords(ctx, tx)
	if err != nil {
		return err
	}
	initial, err := selectInitialAuditRecords(authority, scope, records)
	if err != nil {
		return err
	}
	if err := validateInitialAuditRecords(
		ctx, tx, vaultID, nodeSequence, authority, scope, initial,
	); err != nil {
		return err
	}
	return validateAuditedContentReplacementHistory(
		ctx, tx, vaultID, nodeSequence, authority, scope, records, initial,
	)
}

func auditAuthorityCounts(ctx context.Context, tx metadataQuerier) ([5]int64, error) {
	var counts [5]int64
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM audit_authority),
		(SELECT COUNT(*) FROM audit_scopes),
		(SELECT COUNT(*) FROM audit_baselines),
		(SELECT COUNT(*) FROM audit_memberships),
		(SELECT COUNT(*) FROM audit_records)`).Scan(
		&counts[0], &counts[1], &counts[2], &counts[3], &counts[4])
	if err != nil {
		return counts, fmt.Errorf("counting audit authority: %w", err)
	}
	return counts, nil
}

func loadInitialAuditProjection(
	ctx context.Context, tx metadataQuerier,
) (initialAuditAuthority, initialAuditScope, error) {
	var authority initialAuditAuthority
	err := tx.QueryRowContext(ctx, `SELECT lineage_id,operation_sequence_high_water,
		allocation_genesis_digest,allocation_entry_count,allocation_head
		FROM audit_authority WHERE singleton=1`).Scan(
		&authority.lineageID, &authority.sequence, &authority.genesisDigest,
		&authority.allocationCount, &authority.allocationHead)
	if err != nil {
		return authority, initialAuditScope{}, fmt.Errorf("reading initial audit authority: %w", err)
	}
	var scope initialAuditScope
	err = tx.QueryRowContext(ctx, `SELECT scope_id,target_node_id,enable_operation_id,
		entry_count,chain_head FROM audit_scopes`).Scan(
		&scope.scopeID, &scope.targetNodeID, &scope.operationID,
		&scope.entryCount, &scope.chainHead)
	if err != nil {
		return authority, scope, fmt.Errorf("reading initial audit scope: %w", err)
	}
	return authority, scope, nil
}

func loadInitialAuditRecords(
	ctx context.Context, tx metadataQuerier,
) (map[string][]storedAuditRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT digest,kind,operation_id,operation_sequence,
		scope_id,entry_count,event_id,event_ordinal,record_json FROM audit_records`)
	if err != nil {
		return nil, fmt.Errorf("reading initial audit records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string][]storedAuditRecord)
	for rows.Next() {
		var digest, kind, recordJSON string
		var operationID, scopeID, eventID sql.NullString
		var sequence, entryCount, ordinal sql.NullInt64
		if err := rows.Scan(&digest, &kind, &operationID, &sequence, &scopeID,
			&entryCount, &eventID, &ordinal, &recordJSON); err != nil {
			return nil, fmt.Errorf("scanning initial audit record: %w", err)
		}
		record, index, canonical, err := validateAuditRecord(metadataAuditRecord{
			Type: metadataAuditRecordType, Digest: digest, Record: []byte(recordJSON),
		})
		if err != nil {
			return nil, err
		}
		if kind != record.Kind || recordJSON != string(canonical) ||
			!sameNullString(operationID, index.operationID) ||
			!sameNullInt64(sequence, index.operationSequence) ||
			!sameNullString(scopeID, index.scopeID) ||
			!sameNullInt64(entryCount, index.entryCount) ||
			!sameNullString(eventID, index.eventID) ||
			!sameNullInt64(ordinal, index.eventOrdinal) {
			return nil, fmt.Errorf("audit record %s relational index does not match canonical fields", digest)
		}
		result[kind] = append(result[kind], storedAuditRecord{
			digest: digest, record: record, index: index,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading initial audit records: %w", err)
	}
	return result, nil
}

func sameNullString(value sql.NullString, expected *string) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.String == *expected)
}

func sameNullInt64(value sql.NullInt64, expected *int64) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.Int64 == *expected)
}

func selectInitialAuditRecords(
	authority initialAuditAuthority, scope initialAuditScope,
	records map[string][]storedAuditRecord,
) (map[string][]storedAuditRecord, error) {
	if len(records) != len(auditRecordKinds) {
		return nil, fmt.Errorf("audit authority has %d record kinds, want %d", len(records), len(auditRecordKinds))
	}
	for _, kind := range []string{
		"topology_genesis", "attached_metadata_genesis", "allocation_genesis",
		"enrollment_baseline",
	} {
		if len(records[kind]) != 1 {
			return nil, fmt.Errorf("audit authority has %d %s records, want 1", len(records[kind]), kind)
		}
	}
	if authority.sequence < 1 || authority.allocationCount != authority.sequence ||
		int64(len(records["canonical_mutation"])) != authority.sequence ||
		int64(len(records["allocation_entry"])) != authority.allocationCount {
		return nil, errors.New("audit allocation projection does not match its operation records")
	}
	if scope.entryCount < 1 || int64(len(records["scope_chain_entry"])) != scope.entryCount {
		return nil, errors.New("audit scope projection does not match its chain records")
	}
	result := map[string][]storedAuditRecord{
		"topology_genesis":          records["topology_genesis"],
		"attached_metadata_genesis": records["attached_metadata_genesis"],
		"allocation_genesis":        records["allocation_genesis"],
		"enrollment_baseline":       records["enrollment_baseline"],
	}
	selectors := []struct {
		kind  string
		match func(storedAuditRecord) bool
	}{
		{"canonical_mutation", func(record storedAuditRecord) bool {
			return record.index.operationSequence != nil && *record.index.operationSequence == 1
		}},
		{"allocation_entry", func(record storedAuditRecord) bool {
			return record.index.operationSequence != nil && *record.index.operationSequence == 1
		}},
		{"scope_chain_entry", func(record storedAuditRecord) bool {
			return record.index.scopeID != nil && *record.index.scopeID == scope.scopeID &&
				record.index.entryCount != nil && *record.index.entryCount == 1
		}},
		{"event", func(record storedAuditRecord) bool {
			return record.index.operationID != nil && *record.index.operationID == scope.operationID &&
				record.index.eventOrdinal != nil && *record.index.eventOrdinal == 0
		}},
	}
	for _, selector := range selectors {
		for _, record := range records[selector.kind] {
			if selector.match(record) {
				result[selector.kind] = append(result[selector.kind], record)
			}
		}
		if len(result[selector.kind]) != 1 {
			return nil, fmt.Errorf("audit authority has %d initial %s records, want 1",
				len(result[selector.kind]), selector.kind)
		}
	}
	return result, nil
}

func validateInitialAuditRecords(
	ctx context.Context, tx metadataQuerier, vaultID string, nodeSequence int64,
	authority initialAuditAuthority, scope initialAuditScope,
	records map[string][]storedAuditRecord,
) error {
	topology := records["topology_genesis"][0]
	attachments := records["attached_metadata_genesis"][0]
	allocationGenesis := records["allocation_genesis"][0]
	baseline := records["enrollment_baseline"][0]
	event := records["event"][0]
	mutation := records["canonical_mutation"][0]
	scopeEntry := records["scope_chain_entry"][0]
	allocationEntry := records["allocation_entry"][0]
	if err := validateInitialGenesis(vaultID, nodeSequence, authority,
		topology, attachments, allocationGenesis); err != nil {
		return err
	}
	if err := validateInitialBaseline(
		ctx, tx, vaultID, scope, baseline, topology, attachments,
	); err != nil {
		return err
	}
	if err := validateInitialMutation(vaultID, scope, baseline, event, mutation); err != nil {
		return err
	}
	if err := validateInitialScopeChain(vaultID, scope, mutation, scopeEntry); err != nil {
		return err
	}
	return validateInitialAllocation(vaultID, nodeSequence, authority, scope,
		allocationGenesis, mutation, allocationEntry)
}

func validateInitialGenesis(
	vaultID string, nodeSequence int64,
	authority initialAuditAuthority, topology, attachments, allocation storedAuditRecord,
) error {
	nodeHighWater, err := positiveAuditNodeID(nodeSequence)
	if err != nil {
		return err
	}
	storedTopology, err := auditRecordListField(topology.record, "nodes")
	if err != nil {
		return err
	}
	storedAttachments, err := auditRecordListField(attachments.record, "records")
	if err != nil {
		return err
	}
	for _, record := range []audit.Record{topology.record, attachments.record, allocation.record} {
		if err := requireAuditUUID(record, auditVaultIDField, vaultID); err != nil {
			return err
		}
		if err := requireAuditUUID(record, "lineage_id", authority.lineageID); err != nil {
			return err
		}
	}
	if allocation.digest != authority.genesisDigest {
		return errors.New("audit allocation genesis pointer does not match its record")
	}
	if err := requireAuditAbsent(allocation.record, "previous_head"); err != nil {
		return err
	}
	if err := requireAuditUnsigned(allocation.record, "node_id_high_water", nodeHighWater); err != nil {
		return err
	}
	if err := requireAuditUnsigned(allocation.record, "operation_sequence_high_water", 0); err != nil {
		return err
	}
	if err := requireAuditUnsigned(allocation.record, "topology_count", uint64(len(storedTopology))); err != nil {
		return err
	}
	if err := requireAuditDigest(allocation.record, "topology_digest", topology.digest); err != nil {
		return err
	}
	if err := requireAuditUnsigned(allocation.record, "attached_metadata_count", uint64(len(storedAttachments))); err != nil {
		return err
	}
	return requireAuditDigest(allocation.record, "attached_metadata_digest", attachments.digest)
}

func validateInitialBaseline(
	ctx context.Context, tx metadataQuerier, vaultID string, scope initialAuditScope,
	baseline, topology, attachments storedAuditRecord,
) error {
	if err := requireAuditUUID(baseline.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUUID(baseline.record, "scope_id", scope.scopeID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(baseline.record, "target_node_id", scope.targetNodeID); err != nil {
		return err
	}
	if err := requireAuditUUID(baseline.record, auditOperationIDField, scope.operationID); err != nil {
		return err
	}
	if err := requireAuditText(baseline.record, "cause", "explicit"); err != nil {
		return err
	}
	members, err := auditUnsignedListField(baseline.record, "members")
	if err != nil {
		return err
	}
	genesisTopology, err := auditRecordListField(topology.record, "nodes")
	if err != nil {
		return err
	}
	expectedMembers, err := deriveInitialAuditMembersFromRecords(
		genesisTopology, scope.targetNodeID,
	)
	if err != nil {
		return err
	}
	if !slices.Equal(members, expectedMembers) {
		return errors.New("audit enrollment members do not match the protected closure")
	}
	if err := validateInitialMembershipRows(ctx, tx, scope, baseline.digest, members); err != nil {
		return err
	}
	storedStates, err := auditRecordListField(baseline.record, "member_states")
	if err != nil {
		return err
	}
	storedVersions, err := auditRecordListField(baseline.record, "versions")
	if err != nil {
		return err
	}
	if err := validateInitialAuditMemberProjection(
		members, genesisTopology, storedStates, storedVersions,
	); err != nil {
		return err
	}
	allAttachments, err := auditRecordListField(attachments.record, "records")
	if err != nil {
		return err
	}
	expectedAttachments, err := auditRecordsForNodes(allAttachments, auditMemberSet(members))
	if err != nil {
		return err
	}
	storedAttachments, err := auditRecordListField(baseline.record, "attachments")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(storedAttachments, expectedAttachments) {
		return errors.New("audit enrollment attachments do not match current metadata")
	}
	return validateInitialBaselineTopology(
		baseline.record, members, scope.operationID, genesisTopology,
	)
}

func validateInitialBaselineTopology(
	baseline audit.Record, members []uint64, operationID string, topology []audit.Record,
) error {
	expectedNodes, expectedWitnesses, err := initialBaselineTopology(
		topology, members, operationID,
	)
	if err != nil {
		return err
	}
	storedNodes, err := auditRecordListField(baseline, "nodes")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(storedNodes, expectedNodes) {
		return errors.New("audit enrollment topology does not match current dependencies")
	}
	storedWitnesses, err := auditRecordListField(baseline, "witnesses")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(storedWitnesses, expectedWitnesses) {
		return errors.New("audit enrollment witnesses do not match topology dependencies")
	}
	return nil
}

func validateInitialMembershipRows(
	ctx context.Context, tx metadataQuerier, scope initialAuditScope,
	baselineDigest string, members []uint64,
) error {
	var relationScope, relationOperation string
	var targetID uint64
	err := tx.QueryRowContext(ctx, `SELECT scope_id,target_node_id,operation_id
		FROM audit_baselines WHERE digest=?`, baselineDigest).Scan(
		&relationScope, &targetID, &relationOperation)
	if err != nil {
		return fmt.Errorf("reading audit baseline relation: %w", err)
	}
	if relationScope != scope.scopeID || targetID != scope.targetNodeID ||
		relationOperation != scope.operationID {
		return errors.New("audit baseline relation does not match its scope and operation")
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id,baseline_digest FROM audit_memberships
		WHERE scope_id=? ORDER BY node_id`, scope.scopeID)
	if err != nil {
		return fmt.Errorf("reading audit membership projection: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var projected []uint64
	for rows.Next() {
		var nodeID uint64
		var digest string
		if err := rows.Scan(&nodeID, &digest); err != nil {
			return fmt.Errorf("scanning audit membership projection: %w", err)
		}
		if digest != baselineDigest {
			return errors.New("audit membership points to the wrong baseline")
		}
		projected = append(projected, nodeID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading audit membership projection: %w", err)
	}
	if !slices.Equal(projected, members) {
		return errors.New("audit membership projection does not match the baseline")
	}
	return nil
}

func validateInitialMutation(
	vaultID string, scope initialAuditScope, baseline, eventWrapper,
	mutation storedAuditRecord,
) error {
	event, err := auditNestedField(eventWrapper.record, "event")
	if err != nil {
		return err
	}
	if err := validateInitialEvent(scope, baseline, event); err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, "operation_sequence", 1); err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditOperationIDField, scope.operationID); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	events, err := auditRecordListField(mutation.record, "events")
	if err != nil {
		return err
	}
	if len(events) != 1 || !auditRecordEqual(events[0], event) {
		return errors.New("initial audit mutation does not bind its enrollment event")
	}
	changes, err := auditRecordListField(mutation.record, "member_state_changes")
	if err != nil {
		return err
	}
	if len(changes) != 0 {
		return errors.New("initial audit enrollment must not contain member-state changes")
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 1 {
		return errors.New("initial audit mutation must bind one enrollment baseline")
	}
	if err := validateInitialBaselineBinding(bindings[0], scope, baseline.digest); err != nil {
		return err
	}
	if err := requireNoChangeMutationFields(mutation.record); err != nil {
		return err
	}
	return requireMatchingEventEnvelope(mutation.record, event)
}

func validateInitialEvent(scope initialAuditScope, baseline storedAuditRecord, event audit.Record) error {
	identityOperation, err := audit.UUID(scope.operationID)
	if err != nil {
		return err
	}
	identityDigest, err := audit.Hash(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: identityOperation},
		{Name: "event_ordinal", Value: audit.Unsigned(0)},
	}})
	if err != nil {
		return err
	}
	checks := []func() error{
		func() error { return requireAuditDigest(event, "event_id", hex.EncodeToString(identityDigest[:])) },
		func() error { return requireAuditUUID(event, auditOperationIDField, scope.operationID) },
		func() error { return requireAuditUnsigned(event, metadataNodeIDField, scope.targetNodeID) },
		func() error { return requireAuditText(event, "event_kind", "audit_enroll") },
		func() error { return requireAuditUUID(event, "scope_id", scope.scopeID) },
		func() error { return requireAuditUnsigned(event, "target_node_id", scope.targetNodeID) },
		func() error { return requireAuditUnsigned(event, "event_ordinal", 0) },
		func() error { return requireAuditDigest(event, "baseline_digest", baseline.digest) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	states, err := auditRecordListField(baseline.record, "member_states")
	if err != nil {
		return err
	}
	if err := validateInitialEventState(event, scope.targetNodeID, states); err != nil {
		return err
	}
	return requireAuditAbsentFields(event, "attachment_kind", "attachment_identity",
		"source_version_id", "pre", "post", "topology_delta")
}

func validateInitialEventState(event audit.Record, targetNodeID uint64, states []audit.Record) error {
	for _, state := range states {
		nodeID, err := auditUnsignedField(state, metadataNodeIDField)
		if err != nil {
			return err
		}
		if nodeID != targetNodeID {
			continue
		}
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		if err := requireAuditUnsigned(event, "prior_node_revision", revision); err != nil {
			return err
		}
		if err := requireAuditUnsigned(event, "resulting_node_revision", revision); err != nil {
			return err
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		if err := requireAuditOptionalUUID(event, "prior_current_version_id", current); err != nil {
			return err
		}
		return requireAuditOptionalUUID(event, "resulting_current_version_id", current)
	}
	return errors.New("initial audit event target has no baseline member state")
}

func validateInitialBaselineBinding(record audit.Record, scope initialAuditScope, digest string) error {
	if err := requireAuditUUID(record, "scope_id", scope.scopeID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(record, "target_node_id", scope.targetNodeID); err != nil {
		return err
	}
	return requireAuditDigest(record, "baseline_digest", digest)
}

func requireNoChangeMutationFields(record audit.Record) error {
	if err := requireAuditAbsentFields(record, "topology_delta", "path_effect_digest",
		"witness_change_digest", "attached_metadata_change_digest"); err != nil {
		return err
	}
	for _, field := range []string{
		"path_effect_count", "witness_change_count", "attached_metadata_change_count",
	} {
		if err := requireAuditUnsigned(record, field, 0); err != nil {
			return err
		}
	}
	return nil
}

func requireMatchingEventEnvelope(mutation, event audit.Record) error {
	for _, field := range []string{"recorded_at", "origin", "agent_label"} {
		left, err := auditField(mutation, field)
		if err != nil {
			return err
		}
		right, err := auditField(event, field)
		if err != nil {
			return err
		}
		if !equalAuditEnvelopeValue(left, right) {
			return fmt.Errorf("initial audit mutation and event disagree on %s", field)
		}
	}
	return nil
}

func validateInitialScopeChain(
	vaultID string, scope initialAuditScope, mutation, entry storedAuditRecord,
) error {
	if err := requireAuditUUID(entry.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUUID(entry.record, "scope_id", scope.scopeID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(entry.record, "entry_count", 1); err != nil {
		return err
	}
	if err := requireAuditAbsent(entry.record, "previous_head"); err != nil {
		return err
	}
	return requireAuditDigest(entry.record, "mutation_hash", mutation.digest)
}

func validateInitialAllocation(
	vaultID string, nodeSequence int64, authority initialAuditAuthority,
	scope initialAuditScope, genesis, mutation, entry storedAuditRecord,
) error {
	nodeHighWater, err := positiveAuditNodeID(nodeSequence)
	if err != nil {
		return err
	}
	if err := requireAuditUUID(entry.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUUID(entry.record, "lineage_id", authority.lineageID); err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "previous_head", genesis.digest); err != nil {
		return err
	}
	if err := requireAuditUnsigned(entry.record, "operation_sequence", 1); err != nil {
		return err
	}
	if err := requireAuditUUID(entry.record, auditOperationIDField, scope.operationID); err != nil {
		return err
	}
	allocated, err := auditUnsignedListField(entry.record, "allocated_node_ids")
	if err != nil {
		return err
	}
	if len(allocated) != 0 {
		return errors.New("initial audit enrollment must not allocate node IDs")
	}
	for field, want := range map[string]uint64{
		"node_id_high_water":             nodeHighWater,
		"operation_sequence_high_water":  1,
		"witness_change_count":           0,
		"attached_metadata_change_count": 0,
	} {
		if err := requireAuditUnsigned(entry.record, field, want); err != nil {
			return err
		}
	}
	if err := requireAuditBool(entry.record, "has_audited_mutation", true); err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutation.digest); err != nil {
		return err
	}
	for _, field := range []string{
		"has_topology_change", "has_witness_change", "has_attached_metadata_change",
	} {
		if err := requireAuditBool(entry.record, field, false); err != nil {
			return err
		}
	}
	return requireAuditAbsentFields(entry.record, "topology_delta", "witness_change_digest",
		"attached_metadata_change_digest")
}

func auditRecordListField(record audit.Record, name string) ([]audit.Record, error) {
	values, err := auditListField(record, name)
	if err != nil {
		return nil, err
	}
	return auditRecordList(values)
}

func auditUnsignedListField(record audit.Record, name string) ([]uint64, error) {
	values, err := auditListField(record, name)
	if err != nil {
		return nil, err
	}
	return auditUnsignedList(values)
}

func requireAuditUUID(record audit.Record, name, want string) error {
	got, err := auditUUIDField(record, name)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("audit field %s.%s is %q, want %q", record.Kind, name, got, want)
	}
	return nil
}

func requireAuditDigest(record audit.Record, name, want string) error {
	got, err := auditDigestField(record, name)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("audit field %s.%s does not match", record.Kind, name)
	}
	return nil
}

func requireAuditUnsigned(record audit.Record, name string, want uint64) error {
	got, err := auditUnsignedField(record, name)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("audit field %s.%s is %d, want %d", record.Kind, name, got, want)
	}
	return nil
}

func requireAuditText(record audit.Record, name, want string) error {
	got, err := auditTextField(record, name)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("audit field %s.%s is %q, want %q", record.Kind, name, got, want)
	}
	return nil
}

func requireAuditBool(record audit.Record, name string, want bool) error {
	got, err := auditBoolField(record, name)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("audit field %s.%s has the wrong boolean value", record.Kind, name)
	}
	return nil
}

func requireAuditOptionalUUID(record audit.Record, name string, want *string) error {
	got, err := auditOptionalUUIDField(record, name)
	if err != nil {
		return err
	}
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		return fmt.Errorf("audit field %s.%s does not match", record.Kind, name)
	}
	return nil
}

func requireAuditAbsent(record audit.Record, name string) error {
	value, err := auditField(record, name)
	if err != nil {
		return err
	}
	if !value.IsAbsent() {
		return fmt.Errorf("audit field %s.%s must be absent", record.Kind, name)
	}
	return nil
}

func requireAuditAbsentFields(record audit.Record, fields ...string) error {
	for _, field := range fields {
		if err := requireAuditAbsent(record, field); err != nil {
			return err
		}
	}
	return nil
}

// equalAuditEnvelopeValue compares the optional text and timestamp field types
// shared by canonical mutations and their events.
func equalAuditEnvelopeValue(left, right audit.Value) bool {
	if left.IsAbsent() || right.IsAbsent() {
		return left.IsAbsent() && right.IsAbsent()
	}
	if leftText, ok := left.TextValue(); ok {
		rightText, rightOK := right.TextValue()
		return rightOK && leftText == rightText
	}
	if leftTime, ok := left.TimestampValue(); ok {
		rightTime, rightOK := right.TimestampValue()
		return rightOK && leftTime == rightTime
	}
	return false
}
