package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (s *Store) deleteAuditedTagTx(
	ctx context.Context, tx *sql.Tx, current Tag,
) (Tag, error) {
	assignmentNodeIDs, err := tagAssignmentNodeIDsTx(ctx, tx, current.ID)
	if err != nil {
		return Tag{}, err
	}
	priorNodes, scope, err := auditedTaggedNodesTx(ctx, tx, current.ID)
	if err != nil {
		return Tag{}, err
	}
	authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return Tag{}, err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return Tag{}, fmt.Errorf("allocating audited tag-delete operation: %w", err)
	}
	recordedAt := nowRFC3339()
	if err := touchAuditedTagDefinitionNodesTx(
		tx, current.ID, priorNodes, recordedAt,
	); err != nil {
		return Tag{}, err
	}
	if err := deleteTagDefinitionTx(tx, current.ID); err != nil {
		return Tag{}, err
	}
	resultingNodes := make([]Node, len(priorNodes))
	for index, prior := range priorNodes {
		resultingNodes[index], err = nodeByIDTx(tx, prior.ID)
		if err != nil {
			return Tag{}, err
		}
	}
	if err := persistAuditedTagDelete(
		ctx, tx, s.vaultID, operationID, recordedAt, nodeSequence,
		authority, scope, current, assignmentNodeIDs, priorNodes, resultingNodes,
	); err != nil {
		return Tag{}, err
	}
	return current, nil
}

func tagAssignmentNodeIDsTx(
	ctx context.Context, tx *sql.Tx, tagID string,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id FROM node_tags WHERE tag_id=? ORDER BY node_id`, tagID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing assignments for tag %s: %w", tagID, err)
	}
	defer func() { _ = rows.Close() }()
	var nodeIDs []int64
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, fmt.Errorf("scanning tag %s assignment: %w", tagID, err)
		}
		nodeIDs = append(nodeIDs, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing assignments for tag %s: %w", tagID, err)
	}
	return nodeIDs, nil
}

func persistAuditedTagDelete(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scope *auditScopeState,
	deletedTag Tag, assignmentNodeIDs []int64, priorNodes, resultingNodes []Node,
) error {
	if len(priorNodes) != len(resultingNodes) || (len(priorNodes) != 0) != (scope != nil) {
		return errors.New("audited tag delete has inconsistent affected-node authority")
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(vaultID, authority.lineageID, operationID, recordedAt)
	if err != nil {
		return err
	}
	definition, err := tagDefinitionAuditRecord(deletedTag)
	if err != nil {
		return err
	}
	definitionChange, err := makeAttachedMetadataPresenceChange(definition, false)
	if err != nil {
		return err
	}
	changes := make([]audit.Record, 0, len(assignmentNodeIDs)+1)
	assignments := make(map[int64]audit.Record, len(assignmentNodeIDs))
	for _, nodeID := range assignmentNodeIDs {
		assignment, err := auditTagAssignmentRecord(deletedTag.ID, nodeID)
		if err != nil {
			return err
		}
		change, err := makeAttachedMetadataPresenceChange(assignment, false)
		if err != nil {
			return err
		}
		assignments[nodeID] = assignment
		changes = append(changes, change)
	}
	changes = append(changes, definitionChange)
	delta, deltaDigest, err := makeAttachedMetadataDelta(values.operationID, changes)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	changeCount := uint64(1)
	for range assignmentNodeIDs {
		changeCount++
	}
	mutationHash := audit.Absent()
	if len(priorNodes) != 0 {
		events := make([]audit.Record, 0, len(priorNodes)*2)
		stateChanges := make([]audit.Record, len(priorNodes))
		ordinal := uint64(0)
		for index := range priorNodes {
			definitionEvent, err := makeAuditedTagDeleteEvent(
				values, scope.scopeID, ordinal, priorNodes[index], resultingNodes[index], definition,
			)
			if err != nil {
				return err
			}
			events = append(events, definitionEvent)
			ordinal++
			assignment := assignments[priorNodes[index].ID]
			unassignEvent, err := makeAuditedTagAssignmentEvent(
				values, scope.scopeID, ordinal, priorNodes[index], resultingNodes[index],
				assignment, false,
			)
			if err != nil {
				return err
			}
			events = append(events, unassignEvent)
			ordinal++
			stateChanges[index], err = makeAuditMemberStateChange(
				priorNodes[index], resultingNodes[index],
			)
			if err != nil {
				return err
			}
		}
		mutation, err := makeAuditedMemberStatesMutation(values, sequence, events, stateChanges)
		if err != nil {
			return err
		}
		mutation, err = replaceAuditRecordField(
			mutation, auditAttachedMetadataChangeCountField, audit.Unsigned(changeCount),
		)
		if err != nil {
			return err
		}
		mutation, err = replaceAuditRecordField(
			mutation, "attached_metadata_change_digest", deltaDigest.value,
		)
		if err != nil {
			return err
		}
		mutationDigest, err := hashAuditRecord(mutation)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := insertAuditRecord(ctx, tx, audit.Record{Kind: auditEventField, Fields: []audit.Field{
				{Name: auditEventField, Value: audit.Nested(event)},
			}}); err != nil {
				return err
			}
		}
		if err := insertAuditRecord(ctx, tx, mutation); err != nil {
			return err
		}
		if err := advanceAuditedMutationScopes(
			ctx, tx, values, []auditScopeState{*scope}, mutationDigest.value,
		); err != nil {
			return err
		}
		mutationHash = mutationDigest.value
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, mutationHash,
	)
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(
		allocation, changeCount, deltaDigest.value,
	)
	if err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func makeAuditedTagDeleteEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	priorNode, resultingNode Node, definition audit.Record,
) (audit.Record, error) {
	if priorNode.ID != resultingNode.ID {
		return audit.Record{}, errors.New("audited tag delete changes node identity")
	}
	nodeID, err := positiveAuditNodeID(priorNode.ID)
	if err != nil {
		return audit.Record{}, err
	}
	priorRevision, err := positiveAuditRevision(priorNode.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	resultingRevision, err := positiveAuditRevision(resultingNode.Revision)
	if err != nil || resultingRevision != priorRevision+1 {
		return audit.Record{}, errors.New("audited tag delete has an invalid revision transition")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := attachedAuditIdentity(definition)
	if err != nil {
		return audit.Record{}, err
	}
	eventIdentity, err := hashAuditRecord(audit.Record{Kind: auditEventIdentityKind, Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	kind, err := audit.Text("tag_delete")
	if err != nil {
		return audit.Record{}, err
	}
	attachmentKind, err := audit.Text(auditTagDefinitionKind)
	if err != nil {
		return audit.Record{}, err
	}
	priorVersion, err := auditNodeCurrentVersion(priorNode)
	if err != nil {
		return audit.Record{}, err
	}
	resultingVersion, err := auditNodeCurrentVersion(resultingNode)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: eventIdentity.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "event_kind", Value: kind},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: auditTargetNodeIDField, Value: audit.Absent()},
		{Name: "attachment_kind", Value: attachmentKind},
		{Name: "attachment_identity", Value: audit.Nested(identity)},
		{Name: auditSourceVersionIDField, Value: audit.Absent()},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
		{Name: auditRecordedAtField, Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersion},
		{Name: "resulting_current_version_id", Value: resultingVersion},
		{Name: auditOriginField, Value: values.origin},
		{Name: auditAgentLabelField, Value: audit.Absent()},
		{Name: auditPreField, Value: audit.Nested(definition)},
		{Name: auditPostField, Value: audit.Absent()},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: auditBaselineDigestField, Value: audit.Absent()},
	}}, nil
}
