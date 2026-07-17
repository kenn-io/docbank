package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type auditedCreationBaseline struct {
	scope    auditScopeState
	record   audit.Record
	digest   auditRecordHash
	binding  audit.Record
	nodeID   uint64
	scopeID  audit.Value
	version  *audit.Record
	topology audit.Record
}

func persistAuditedNodeCreation(
	ctx context.Context, tx *sql.Tx, vaultID string,
	authority auditAuthorityState, scopes []auditScopeState,
	priorParent, resultingParent, created Node, version ContentVersion,
	operationID, recordedAt string,
) error {
	if len(scopes) != 1 {
		return unsupportedAuditedNodeMutation(priorParent.ID)
	}
	operationSequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(
		vaultID, authority.lineageID, operationID, recordedAt,
	)
	if err != nil {
		return err
	}
	priorParentTopology, err := auditTopologyForLiveNode(priorParent)
	if err != nil {
		return err
	}
	resultingParentTopology, err := auditTopologyForLiveNode(resultingParent)
	if err != nil {
		return err
	}
	createdTopology, err := auditTopologyForLiveNode(created)
	if err != nil {
		return err
	}
	topologyDelta, err := makeAuditedCreationTopologyDelta(
		values.operationID, priorParentTopology, resultingParentTopology, createdTopology,
	)
	if err != nil {
		return err
	}
	topologyDigest, err := hashAuditRecord(topologyDelta)
	if err != nil {
		return err
	}
	baseline, err := buildAuditedCreationBaseline(
		tx, values, scopes[0], created, version, createdTopology,
	)
	if err != nil {
		return err
	}
	events, err := makeAuditedCreationEvents(values, baseline, created, topologyDigest)
	if err != nil {
		return err
	}
	parentChange, err := makeAuditMemberStateChange(priorParent, resultingParent)
	if err != nil {
		return err
	}
	auditOperationSequence, err := positiveAuditInteger("operation sequence", operationSequence)
	if err != nil {
		return err
	}
	mutation := makeAuditedCreationMutation(
		values, auditOperationSequence, events, parentChange, baseline, topologyDigest,
	)
	mutationDigest, err := hashAuditRecord(mutation)
	if err != nil {
		return err
	}
	if err := persistAuditedCreationRecords(
		ctx, tx, baseline, topologyDelta, events, mutation,
	); err != nil {
		return err
	}
	if err := advanceAuditedCreationScope(
		ctx, tx, values, baseline, mutationDigest,
	); err != nil {
		return err
	}
	nodeSequence, err := auditNodeSequence(ctx, tx)
	if err != nil {
		return err
	}
	allocation, err := makeAuditedCreationAllocationEntry(
		values, operationSequence, nodeSequence, created.ID,
		authority.allocationHead, mutationDigest.value, topologyDigest.value,
	)
	if err != nil {
		return err
	}
	allocationHead, err := hashAuditRecord(allocation)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, allocation); err != nil {
		return err
	}
	allocationCount, err := nextAuditInteger("allocation entry count", authority.allocationCount)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_authority
		SET operation_sequence_high_water=?,allocation_entry_count=?,allocation_head=?
		WHERE singleton=1`, operationSequence, allocationCount, allocationHead.text); err != nil {
		return fmt.Errorf("advancing audit allocation authority: %w", err)
	}
	return nil
}

func auditNodeSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name='nodes'`,
	).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	return sequence, nil
}

func auditTopologyForLiveNode(node Node) (audit.Record, error) {
	if node.ID <= 0 || node.TrashedAt != nil {
		return audit.Record{}, fmt.Errorf("node %d is not live audit topology", node.ID)
	}
	row := auditTopologyRow{
		id: node.ID, name: node.Name, kind: node.Kind,
		createdAt: node.CreatedAt, modifiedAt: node.ModifiedAt,
	}
	if node.ParentID != nil {
		row.parentID = sql.NullInt64{Int64: *node.ParentID, Valid: true}
	}
	return topologyRecord(row)
}

func makeAuditedCreationTopologyDelta(
	operationID audit.Value, priorParent, resultingParent, created audit.Record,
) (audit.Record, error) {
	parentID, err := auditUnsignedField(priorParent, metadataNodeIDField)
	if err != nil {
		return audit.Record{}, err
	}
	createdID, err := auditUnsignedField(created, metadataNodeIDField)
	if err != nil {
		return audit.Record{}, err
	}
	changes := []audit.Record{
		{Kind: "topology_change", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(parentID)},
			{Name: "pre", Value: audit.Nested(priorParent)},
			{Name: "post", Value: audit.Nested(resultingParent)},
		}},
		{Kind: "topology_change", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(createdID)},
			{Name: "pre", Value: audit.Absent()},
			{Name: "post", Value: audit.Nested(created)},
		}},
	}
	if err := sortAuditTopologyRecords(changes); err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: auditTopologyDeltaField, Fields: []audit.Field{
		{Name: auditOperationIDField, Value: operationID},
		{Name: "changes", Value: audit.List(auditNestedValues(changes)...)},
	}}, nil
}

func sortAuditTopologyRecords(records []audit.Record) error {
	type keyedRecord struct {
		id     uint64
		record audit.Record
	}
	keyed := make([]keyedRecord, len(records))
	for index, record := range records {
		id, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil {
			return err
		}
		keyed[index] = keyedRecord{id: id, record: record}
	}
	slices.SortFunc(keyed, func(left, right keyedRecord) int { return cmp.Compare(left.id, right.id) })
	for index := range keyed {
		records[index] = keyed[index].record
	}
	return nil
}

func buildAuditedCreationBaseline(
	tx *sql.Tx, values auditedMutationValues,
	scope auditScopeState, created Node, version ContentVersion,
	createdTopology audit.Record,
) (auditedCreationBaseline, error) {
	nodeID, err := positiveAuditNodeID(created.ID)
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	state, err := auditMemberStateForNode(created)
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	var versions []audit.Record
	var versionRecord *audit.Record
	if !created.IsDir() {
		record, err := auditRecordForContentVersion(version)
		if err != nil {
			return auditedCreationBaseline{}, err
		}
		versions = []audit.Record{record}
		versionRecord = &versions[0]
	}
	nodes, witnesses, err := auditCreationBaselineTopology(tx, created.ID, values.operationID)
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	scopeID, err := audit.UUID(scope.scopeID)
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	cause, err := audit.Text("node_create")
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	eventKind, err := audit.Text("audit_inherit")
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	initialValues := initialAuditValues{
		vaultID: values.vaultID, scopeID: scopeID, operationID: values.operationID,
		recordedAt: values.recordedAt, origin: values.origin, agentLabel: audit.Absent(),
		cause: cause, eventKind: eventKind,
	}
	record := makeInitialAuditBaseline(
		initialValues, nodeID, []uint64{nodeID}, []audit.Record{state},
		versions, nil, nodes, witnesses,
	)
	digest, err := hashAuditRecord(record)
	if err != nil {
		return auditedCreationBaseline{}, err
	}
	binding := audit.Record{Kind: "baseline_binding", Fields: []audit.Field{
		{Name: auditScopeIDField, Value: scopeID},
		{Name: "target_node_id", Value: audit.Unsigned(nodeID)},
		{Name: "baseline_digest", Value: digest.value},
	}}
	return auditedCreationBaseline{
		scope: scope, record: record, digest: digest, binding: binding,
		nodeID: nodeID, scopeID: scopeID,
		version: versionRecord, topology: createdTopology,
	}, nil
}

func auditMemberStateForNode(node Node) (audit.Record, error) {
	nodeID, err := positiveAuditNodeID(node.ID)
	if err != nil {
		return audit.Record{}, err
	}
	revision, err := positiveAuditRevision(node.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	current, err := auditNodeCurrentVersion(node)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "member_state", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "node_revision", Value: audit.Unsigned(revision)},
		{Name: "current_version_id", Value: current},
	}}, nil
}

func auditCreationBaselineTopology(
	tx *sql.Tx, nodeID int64, operationID audit.Value,
) ([]audit.Record, []audit.Record, error) {
	var nodes []audit.Record
	current := nodeID
	for {
		node, err := nodeByIDTx(tx, current)
		if err != nil {
			return nil, nil, err
		}
		record, err := auditTopologyForLiveNode(node)
		if err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, record)
		if node.ParentID == nil {
			break
		}
		current = *node.ParentID
	}
	if err := sortAuditTopologyRecords(nodes); err != nil {
		return nil, nil, err
	}
	witnesses := make([]audit.Record, 0, len(nodes)-1)
	createdID, err := positiveAuditNodeID(nodeID)
	if err != nil {
		return nil, nil, err
	}
	for _, node := range nodes {
		id, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return nil, nil, err
		}
		if id == createdID {
			continue
		}
		stateDigest, err := audit.Hash(audit.Record{Kind: "witnessed_state", Fields: []audit.Field{
			{Name: "node", Value: audit.Nested(node)},
		}})
		if err != nil {
			return nil, nil, err
		}
		witnesses = append(witnesses, audit.Record{Kind: "witness", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(id)},
			{Name: "generation_operation_id", Value: operationID},
			{Name: "state_digest", Value: audit.Digest(stateDigest)},
		}})
	}
	return nodes, witnesses, nil
}

func makeAuditedCreationEvents(
	values auditedMutationValues, baseline auditedCreationBaseline,
	created Node, topologyDigest auditRecordHash,
) ([]audit.Record, error) {
	eventCount := 2
	if !created.IsDir() {
		eventCount++
	}
	events := make([]audit.Record, 0, eventCount)
	for _, kind := range []string{"audit_inherit", "content_create", "node_create"} {
		if kind == "content_create" && created.IsDir() {
			continue
		}
		event, err := makeAuditedCreationEvent(
			values, baseline, kind, uint64(len(events)), created, topologyDigest,
		)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func makeAuditedCreationEvent(
	values auditedMutationValues, baseline auditedCreationBaseline, kind string,
	ordinal uint64, created Node, topologyDigest auditRecordHash,
) (audit.Record, error) {
	identity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "event_ordinal", Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	kindValue, err := audit.Text(kind)
	if err != nil {
		return audit.Record{}, err
	}
	current, err := auditNodeCurrentVersion(created)
	if err != nil {
		return audit.Record{}, err
	}
	target, pre, post := audit.Absent(), audit.Absent(), audit.Absent()
	topology, baselineDigest := audit.Absent(), audit.Absent()
	switch kind {
	case "audit_inherit":
		target, baselineDigest = audit.Unsigned(baseline.nodeID), baseline.digest.value
	case "content_create":
		if baseline.version == nil {
			return audit.Record{}, errors.New("content-create audit event lacks a version")
		}
		post = audit.Nested(*baseline.version)
	case "node_create":
		post, topology = audit.Nested(baseline.topology), topologyDigest.value
	default:
		return audit.Record{}, fmt.Errorf("unsupported creation event kind %q", kind)
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: identity.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(baseline.nodeID)},
		{Name: "event_kind", Value: kindValue},
		{Name: auditScopeIDField, Value: baseline.scopeID},
		{Name: "target_node_id", Value: target},
		{Name: "attachment_kind", Value: audit.Absent()},
		{Name: "attachment_identity", Value: audit.Absent()},
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: "event_ordinal", Value: audit.Unsigned(ordinal)},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(0)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(1)},
		{Name: "prior_current_version_id", Value: audit.Absent()},
		{Name: "resulting_current_version_id", Value: current},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "pre", Value: pre},
		{Name: "post", Value: post},
		{Name: auditTopologyDeltaField, Value: topology},
		{Name: "baseline_digest", Value: baselineDigest},
	}}, nil
}

func makeAuditedCreationMutation(
	values auditedMutationValues, sequence uint64, events []audit.Record,
	parentChange audit.Record, baseline auditedCreationBaseline,
	topologyDigest auditRecordHash,
) audit.Record {
	return audit.Record{Kind: "canonical_mutation", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "operation_sequence", Value: audit.Unsigned(sequence)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "grouping_id", Value: audit.Absent()},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "events", Value: audit.List(auditNestedValues(events)...)},
		{Name: "member_state_changes", Value: audit.List(audit.Nested(parentChange))},
		{Name: "baselines", Value: audit.List(audit.Nested(baseline.binding))},
		{Name: auditTopologyDeltaField, Value: topologyDigest.value},
		{Name: "path_effect_count", Value: audit.Unsigned(0)},
		{Name: "path_effect_digest", Value: audit.Absent()},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}
}

func persistAuditedCreationRecords(
	ctx context.Context, tx *sql.Tx, baseline auditedCreationBaseline,
	topologyDelta audit.Record, events []audit.Record, mutation audit.Record,
) error {
	operationID, err := auditUUIDField(mutation, auditOperationIDField)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, baseline.record); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_baselines(
		digest,scope_id,target_node_id,operation_id) VALUES(?,?,?,?)`,
		baseline.digest.text, baseline.scope.scopeID, baseline.nodeID,
		operationID); err != nil {
		return fmt.Errorf("creating inherited audit baseline: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_memberships(
		scope_id,node_id,baseline_digest) VALUES(?,?,?)`,
		baseline.scope.scopeID, baseline.nodeID, baseline.digest.text); err != nil {
		return fmt.Errorf("creating inherited audit membership for node %d: %w", baseline.nodeID, err)
	}
	if err := insertAuditRecord(ctx, tx, topologyDelta); err != nil {
		return err
	}
	for _, event := range events {
		wrapper := audit.Record{Kind: auditEventField, Fields: []audit.Field{
			{Name: auditEventField, Value: audit.Nested(event)},
		}}
		if err := insertAuditRecord(ctx, tx, wrapper); err != nil {
			return err
		}
	}
	return insertAuditRecord(ctx, tx, mutation)
}

func advanceAuditedCreationScope(
	ctx context.Context, tx *sql.Tx, values auditedMutationValues,
	baseline auditedCreationBaseline, mutationDigest auditRecordHash,
) error {
	entryCount, err := nextAuditInteger("scope entry count", baseline.scope.entryCount)
	if err != nil {
		return err
	}
	entry, err := makeAuditScopeChainEntry(
		values, baseline.scope.scopeID, entryCount,
		baseline.scope.chainHead, mutationDigest.value,
	)
	if err != nil {
		return err
	}
	head, err := hashAuditRecord(entry)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, entry); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_scopes
		SET entry_count=?,chain_head=? WHERE scope_id=?`,
		entryCount, head.text, baseline.scope.scopeID); err != nil {
		return fmt.Errorf("advancing audit scope %s: %w", baseline.scope.scopeID, err)
	}
	return nil
}

func makeAuditedCreationAllocationEntry(
	values auditedMutationValues, sequence, nodeSequence, allocatedNodeID int64,
	previousHead string, mutationHash, topologyDigest audit.Value,
) (audit.Record, error) {
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return audit.Record{}, err
	}
	auditNodeSequence, err := positiveAuditInteger("node ID high-water mark", nodeSequence)
	if err != nil {
		return audit.Record{}, err
	}
	auditAllocatedID, err := positiveAuditInteger("allocated node ID", allocatedNodeID)
	if err != nil {
		return audit.Record{}, err
	}
	previousValue, err := audit.DigestHex(previousHead)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "allocation_entry", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "previous_head", Value: previousValue},
		{Name: "operation_sequence", Value: audit.Unsigned(auditSequence)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "allocated_node_ids", Value: audit.List(audit.Unsigned(auditAllocatedID))},
		{Name: "node_id_high_water", Value: audit.Unsigned(auditNodeSequence)},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(auditSequence)},
		{Name: "has_audited_mutation", Value: audit.Bool(true)},
		{Name: "mutation_hash", Value: mutationHash},
		{Name: "has_topology_change", Value: audit.Bool(true)},
		{Name: auditTopologyDeltaField, Value: topologyDigest},
		{Name: "has_witness_change", Value: audit.Bool(false)},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: "has_attached_metadata_change", Value: audit.Bool(false)},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}, nil
}
