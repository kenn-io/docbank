package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

type initialAuditEnrollmentInput struct {
	targetNodeID int64
	scopeID      string
	operationID  string
	lineageID    string
	recordedAt   string
	origin       string
	agentLabel   *string
}

type initialAuditEnrollmentResult struct {
	scopeID        string
	operationID    string
	baselineDigest string
	memberCount    int
}

type initialAuditEnrollmentSet struct {
	input                   initialAuditEnrollmentInput
	nodeSequence            int64
	members                 []uint64
	records                 []audit.Record
	allocationGenesisDigest string
	baselineDigest          string
	scopeHead               string
	allocationHead          string
}

type initialAuditValues struct {
	vaultID, scopeID, operationID, lineageID audit.Value
	recordedAt, origin, agentLabel           audit.Value
	cause, eventKind                         audit.Value
}

// initializeAuditAuthority creates the first audit authority. It remains
// internal until ordinary logical mutations either extend that authority or
// fail through the shared Go mutation boundary with useful operator guidance.
func (s *Store) initializeAuditAuthority(
	ctx context.Context, targetNodeID int64, origin string, agentLabel *string,
) (initialAuditEnrollmentResult, error) {
	scopeID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentResult{}, err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentResult{}, err
	}
	lineageID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentResult{}, err
	}
	return s.initializeAuditAuthorityWithInput(ctx, initialAuditEnrollmentInput{
		targetNodeID: targetNodeID,
		scopeID:      scopeID,
		operationID:  operationID,
		lineageID:    lineageID,
		recordedAt:   nowRFC3339(),
		origin:       origin,
		agentLabel:   agentLabel,
	})
}

func (s *Store) initializeAuditAuthorityWithInput(
	ctx context.Context, input initialAuditEnrollmentInput,
) (initialAuditEnrollmentResult, error) {
	var result initialAuditEnrollmentResult
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		counts, err := auditAuthorityCounts(ctx, tx)
		if err != nil {
			return err
		}
		for _, count := range counts {
			if count != 0 {
				return errors.New("audit authority is already initialized or incomplete")
			}
		}
		set, err := buildInitialAuditEnrollment(ctx, tx, s.vaultID, input)
		if err != nil {
			return err
		}
		if err := persistInitialAuditEnrollment(ctx, tx, set); err != nil {
			return err
		}
		if err := validateMetadataState(ctx, tx, set.nodeSequence); err != nil {
			return fmt.Errorf("validating created audit authority: %w", err)
		}
		result = initialAuditEnrollmentResult{
			scopeID:        input.scopeID,
			operationID:    input.operationID,
			baselineDigest: set.baselineDigest,
			memberCount:    len(set.members),
		}
		return nil
	})
	return result, err
}

func buildInitialAuditEnrollment(
	ctx context.Context, tx metadataQuerier, vaultID string, input initialAuditEnrollmentInput,
) (initialAuditEnrollmentSet, error) {
	values, err := makeInitialAuditValues(vaultID, input)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	if input.targetNodeID <= 0 {
		return initialAuditEnrollmentSet{}, fmt.Errorf(
			"audit enrollment target %d must be positive", input.targetNodeID,
		)
	}
	var nodeSequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name='nodes'`,
	).Scan(&nodeSequence); err != nil {
		return initialAuditEnrollmentSet{}, fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	if nodeSequence <= 0 {
		return initialAuditEnrollmentSet{}, fmt.Errorf("invalid node ID high-water mark %d", nodeSequence)
	}
	// The positivity checks above make these conversions lossless.
	auditTargetNodeID := uint64(input.targetNodeID)
	auditNodeSequence := uint64(nodeSequence)
	topology, err := currentAuditTopology(ctx, tx)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	attachments, err := currentAuditAttachments(ctx, tx)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	topologyGenesis := audit.Record{Kind: "topology_genesis", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "nodes", Value: audit.List(auditNestedValues(topology)...)},
	}}
	attachmentGenesis := audit.Record{Kind: "attached_metadata_genesis", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "records", Value: audit.List(auditNestedValues(attachments)...)},
	}}
	topologyDigest, err := hashAuditRecord(topologyGenesis)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	attachmentDigest, err := hashAuditRecord(attachmentGenesis)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	allocationGenesis := makeInitialAllocationGenesis(
		values, auditNodeSequence, topology, topologyDigest, attachments, attachmentDigest,
	)
	allocationGenesisDigest, err := hashAuditRecord(allocationGenesis)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}

	members, err := deriveInitialAuditMembers(ctx, tx, auditTargetNodeID)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	states, err := currentAuditMemberStates(ctx, tx, members)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	versions, err := currentAuditVersions(ctx, tx, members)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	baselineAttachments, err := auditRecordsForNodes(attachments, auditMemberSet(members))
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	baselineNodes, witnesses, err := initialBaselineTopology(topology, members, input.operationID)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	baseline := makeInitialAuditBaseline(values, auditTargetNodeID, members,
		states, versions, baselineAttachments, baselineNodes, witnesses)
	baselineDigest, err := hashAuditRecord(baseline)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	event, err := makeInitialAuditEnrollmentEvent(values, auditTargetNodeID, baselineDigest, states)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	eventWrapper := audit.Record{Kind: auditEventField, Fields: []audit.Field{
		{Name: auditEventField, Value: audit.Nested(event)},
	}}
	mutation := makeInitialAuditMutation(values, auditTargetNodeID, baselineDigest, event)
	mutationDigest, err := hashAuditRecord(mutation)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	scopeEntry := audit.Record{Kind: "scope_chain_entry", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: auditScopeIDField, Value: values.scopeID},
		{Name: "entry_count", Value: audit.Unsigned(1)},
		{Name: "previous_head", Value: audit.Absent()},
		{Name: "mutation_hash", Value: mutationDigest.value},
	}}
	scopeHead, err := hashAuditRecord(scopeEntry)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	allocationEntry := makeInitialAllocationEntry(
		values, auditNodeSequence, allocationGenesisDigest.value, mutationDigest.value,
	)
	allocationHead, err := hashAuditRecord(allocationEntry)
	if err != nil {
		return initialAuditEnrollmentSet{}, err
	}
	return initialAuditEnrollmentSet{
		input:        input,
		nodeSequence: nodeSequence,
		members:      members,
		records: []audit.Record{
			topologyGenesis, attachmentGenesis, allocationGenesis, baseline,
			eventWrapper, mutation, scopeEntry, allocationEntry,
		},
		allocationGenesisDigest: allocationGenesisDigest.text,
		baselineDigest:          baselineDigest.text,
		scopeHead:               scopeHead.text,
		allocationHead:          allocationHead.text,
	}, nil
}

type auditRecordHash struct {
	text  string
	value audit.Value
}

func hashAuditRecord(record audit.Record) (auditRecordHash, error) {
	digest, err := audit.Hash(record)
	if err != nil {
		return auditRecordHash{}, fmt.Errorf("hashing %s audit record: %w", record.Kind, err)
	}
	return auditRecordHash{text: hex.EncodeToString(digest[:]), value: audit.Digest(digest)}, nil
}

func makeInitialAuditValues(vaultID string, input initialAuditEnrollmentInput) (initialAuditValues, error) {
	values := initialAuditValues{}
	constructors := []struct {
		name  string
		value string
		make  func(string) (audit.Value, error)
		out   *audit.Value
	}{
		{"vault ID", vaultID, audit.UUID, &values.vaultID},
		{"scope ID", input.scopeID, audit.UUID, &values.scopeID},
		{"operation ID", input.operationID, audit.UUID, &values.operationID},
		{"lineage ID", input.lineageID, audit.UUID, &values.lineageID},
		{"recorded time", input.recordedAt, audit.Timestamp, &values.recordedAt},
		{auditOriginField, input.origin, audit.Text, &values.origin},
		{"enrollment cause", "explicit", audit.Text, &values.cause},
		{"enrollment event kind", "audit_enroll", audit.Text, &values.eventKind},
	}
	for _, item := range constructors {
		value, err := item.make(item.value)
		if err != nil {
			return initialAuditValues{}, fmt.Errorf("encoding audit enrollment %s: %w", item.name, err)
		}
		*item.out = value
	}
	values.agentLabel = audit.Absent()
	if input.agentLabel != nil {
		label, err := audit.Text(*input.agentLabel)
		if err != nil {
			return initialAuditValues{}, fmt.Errorf("encoding audit enrollment agent label: %w", err)
		}
		values.agentLabel = label
	}
	return values, nil
}

func makeInitialAllocationGenesis(
	values initialAuditValues, nodeSequence uint64,
	topology []audit.Record, topologyDigest auditRecordHash,
	attachments []audit.Record, attachmentDigest auditRecordHash,
) audit.Record {
	return audit.Record{Kind: "allocation_genesis", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "previous_head", Value: audit.Absent()},
		{Name: "node_id_high_water", Value: audit.Unsigned(nodeSequence)},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(0)},
		{Name: "topology_count", Value: audit.Unsigned(uint64(len(topology)))},
		{Name: "topology_digest", Value: topologyDigest.value},
		{Name: "attached_metadata_count", Value: audit.Unsigned(uint64(len(attachments)))},
		{Name: "attached_metadata_digest", Value: attachmentDigest.value},
	}}
}

func makeInitialAuditBaseline(
	values initialAuditValues, targetNodeID uint64, members []uint64,
	states, versions, attachments, nodes, witnesses []audit.Record,
) audit.Record {
	memberValues := make([]audit.Value, len(members))
	for index, member := range members {
		memberValues[index] = audit.Unsigned(member)
	}
	return audit.Record{Kind: "enrollment_baseline", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: auditScopeIDField, Value: values.scopeID},
		{Name: "target_node_id", Value: audit.Unsigned(targetNodeID)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "cause", Value: values.cause},
		{Name: "members", Value: audit.List(memberValues...)},
		{Name: "member_states", Value: audit.List(auditNestedValues(states)...)},
		{Name: "nodes", Value: audit.List(auditNestedValues(nodes)...)},
		{Name: "versions", Value: audit.List(auditNestedValues(versions)...)},
		{Name: "attachments", Value: audit.List(auditNestedValues(attachments)...)},
		{Name: "witnesses", Value: audit.List(auditNestedValues(witnesses)...)},
	}}
}

func makeInitialAuditEnrollmentEvent(
	values initialAuditValues, targetNodeID uint64, baselineDigest auditRecordHash,
	states []audit.Record,
) (audit.Record, error) {
	identityDigest, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "event_ordinal", Value: audit.Unsigned(0)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	var targetState audit.Record
	for _, state := range states {
		id, err := auditUnsignedField(state, metadataNodeIDField)
		if err != nil {
			return audit.Record{}, err
		}
		if id == targetNodeID {
			targetState = state
			break
		}
	}
	if targetState.Kind == "" {
		return audit.Record{}, fmt.Errorf("audit enrollment target %d lacks member state", targetNodeID)
	}
	revision, err := auditUnsignedField(targetState, "node_revision")
	if err != nil {
		return audit.Record{}, err
	}
	current, err := auditField(targetState, "current_version_id")
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: identityDigest.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(targetNodeID)},
		{Name: "event_kind", Value: values.eventKind},
		{Name: auditScopeIDField, Value: values.scopeID},
		{Name: "target_node_id", Value: audit.Unsigned(targetNodeID)},
		{Name: "attachment_kind", Value: audit.Absent()},
		{Name: "attachment_identity", Value: audit.Absent()},
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: "event_ordinal", Value: audit.Unsigned(0)},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(revision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(revision)},
		{Name: "prior_current_version_id", Value: current},
		{Name: "resulting_current_version_id", Value: current},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: values.agentLabel},
		{Name: "pre", Value: audit.Absent()},
		{Name: "post", Value: audit.Absent()},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "baseline_digest", Value: baselineDigest.value},
	}}, nil
}

func makeInitialAuditMutation(
	values initialAuditValues, targetNodeID uint64, baselineDigest auditRecordHash,
	event audit.Record,
) audit.Record {
	binding := audit.Record{Kind: "baseline_binding", Fields: []audit.Field{
		{Name: auditScopeIDField, Value: values.scopeID},
		{Name: "target_node_id", Value: audit.Unsigned(targetNodeID)},
		{Name: "baseline_digest", Value: baselineDigest.value},
	}}
	return audit.Record{Kind: "canonical_mutation", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "operation_sequence", Value: audit.Unsigned(1)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "grouping_id", Value: audit.Absent()},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: values.agentLabel},
		{Name: "events", Value: audit.List(audit.Nested(event))},
		{Name: "member_state_changes", Value: audit.List()},
		{Name: "baselines", Value: audit.List(audit.Nested(binding))},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "path_effect_count", Value: audit.Unsigned(0)},
		{Name: "path_effect_digest", Value: audit.Absent()},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}
}

func makeInitialAllocationEntry(
	values initialAuditValues, nodeSequence uint64,
	genesisDigest, mutationDigest audit.Value,
) audit.Record {
	return audit.Record{Kind: "allocation_entry", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "previous_head", Value: genesisDigest},
		{Name: "operation_sequence", Value: audit.Unsigned(1)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "allocated_node_ids", Value: audit.List()},
		{Name: "node_id_high_water", Value: audit.Unsigned(nodeSequence)},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(1)},
		{Name: "has_audited_mutation", Value: audit.Bool(true)},
		{Name: "mutation_hash", Value: mutationDigest},
		{Name: "has_topology_change", Value: audit.Bool(false)},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "has_witness_change", Value: audit.Bool(false)},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: "has_attached_metadata_change", Value: audit.Bool(false)},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}
}

func persistInitialAuditEnrollment(
	ctx context.Context, tx *sql.Tx, set initialAuditEnrollmentSet,
) error {
	for _, record := range set.records {
		if err := insertAuditRecord(ctx, tx, record); err != nil {
			return err
		}
	}
	input := set.input
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_authority(
		singleton,lineage_id,operation_sequence_high_water,allocation_genesis_digest,
		allocation_entry_count,allocation_head) VALUES(1,?,1,?,1,?)`,
		input.lineageID, set.allocationGenesisDigest, set.allocationHead); err != nil {
		return fmt.Errorf("creating audit authority: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_scopes(
		scope_id,target_node_id,enable_operation_id,entry_count,chain_head)
		VALUES(?,?,?,1,?)`, input.scopeID, input.targetNodeID,
		input.operationID, set.scopeHead); err != nil {
		return fmt.Errorf("creating audit scope: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_baselines(
		digest,scope_id,target_node_id,operation_id) VALUES(?,?,?,?)`,
		set.baselineDigest, input.scopeID, input.targetNodeID, input.operationID); err != nil {
		return fmt.Errorf("creating audit baseline projection: %w", err)
	}
	for _, member := range set.members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_memberships(
			scope_id,node_id,baseline_digest) VALUES(?,?,?)`,
			input.scopeID, member, set.baselineDigest); err != nil {
			return fmt.Errorf("creating audit membership for node %d: %w", member, err)
		}
	}
	return nil
}

func insertAuditRecord(ctx context.Context, tx *sql.Tx, record audit.Record) error {
	digest, err := hashAuditRecord(record)
	if err != nil {
		return err
	}
	recordJSON, err := audit.MarshalJSONRecord(record)
	if err != nil {
		return fmt.Errorf("encoding %s audit record: %w", record.Kind, err)
	}
	index, err := indexAuditRecord(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_records(
		digest,kind,operation_id,operation_sequence,scope_id,entry_count,
		event_id,event_ordinal,record_json) VALUES(?,?,?,?,?,?,?,?,?)`,
		digest.text, record.Kind, index.operationID, index.operationSequence,
		index.scopeID, index.entryCount, index.eventID, index.eventOrdinal, string(recordJSON))
	if err != nil {
		return fmt.Errorf("storing %s audit record: %w", record.Kind, err)
	}
	return nil
}
