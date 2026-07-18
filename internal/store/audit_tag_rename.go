package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (s *Store) renameAuditedTagTx(
	ctx context.Context, tx *sql.Tx, current Tag, name string,
) (Tag, error) {
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
		return Tag{}, fmt.Errorf("allocating audited tag-rename operation: %w", err)
	}
	recordedAt := nowRFC3339()
	renamed, err := s.renameTagDefinitionTx(tx, current, name)
	if err != nil {
		return Tag{}, err
	}
	if err := touchAuditedTagDefinitionNodesTx(tx, current.ID, priorNodes, recordedAt); err != nil {
		return Tag{}, err
	}
	resultingNodes := make([]Node, len(priorNodes))
	for index, prior := range priorNodes {
		resultingNodes[index], err = nodeByIDTx(tx, prior.ID)
		if err != nil {
			return Tag{}, err
		}
	}
	if err := persistAuditedTagRename(
		ctx, tx, s.vaultID, operationID, recordedAt, nodeSequence,
		authority, scope, current, renamed, priorNodes, resultingNodes,
	); err != nil {
		return Tag{}, err
	}
	return renamed, nil
}

func touchAuditedTagDefinitionNodesTx(
	tx *sql.Tx, tagID string, audited []Node, recordedAt string,
) error {
	// Revisions cover every assigned node. modified_at is changed only where the
	// scoped mutation commits the operation timestamp; the allocation-only form
	// deliberately has no timestamp from which replay could derive that field.
	if _, err := tx.Exec(`UPDATE nodes SET revision=revision+1
		WHERE id IN (SELECT node_id FROM node_tags WHERE tag_id=?)`, tagID); err != nil {
		return fmt.Errorf("advancing nodes assigned tag %s: %w", tagID, err)
	}
	for _, node := range audited {
		if _, err := tx.Exec(
			`UPDATE nodes SET modified_at=? WHERE id=?`, recordedAt, node.ID,
		); err != nil {
			return fmt.Errorf("recording audited tag rename on node %d: %w", node.ID, err)
		}
	}
	return nil
}

func auditedTaggedNodesTx(
	ctx context.Context, tx *sql.Tx, tagID string,
) ([]Node, *auditScopeState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT membership.node_id,scope.scope_id,
		scope.entry_count,scope.chain_head
		FROM node_tags assignment
		JOIN audit_memberships membership ON membership.node_id=assignment.node_id
		JOIN audit_scopes scope ON scope.scope_id=membership.scope_id
		WHERE assignment.tag_id=? ORDER BY membership.node_id,scope.scope_id`, tagID)
	if err != nil {
		return nil, nil, fmt.Errorf("listing audited nodes assigned tag %s: %w", tagID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		nodeIDs []int64
		scope   *auditScopeState
	)
	for rows.Next() {
		var nodeID int64
		var current auditScopeState
		if err := rows.Scan(
			&nodeID, &current.scopeID, &current.entryCount, &current.chainHead,
		); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scanning audited tag assignment: %w", err)
		}
		if scope != nil && scope.scopeID != current.scopeID {
			_ = rows.Close()
			return nil, nil, errors.New("tag rename across multiple audit scopes is not supported")
		}
		if scope == nil {
			scope = &current
		}
		if len(nodeIDs) == 0 || nodeIDs[len(nodeIDs)-1] != nodeID {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("listing audited nodes assigned tag %s: %w", tagID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("closing audited tag assignment rows: %w", err)
	}
	nodes := make([]Node, len(nodeIDs))
	for index, nodeID := range nodeIDs {
		nodes[index], err = nodeByIDTx(tx, nodeID)
		if err != nil {
			return nil, nil, err
		}
	}
	return nodes, scope, nil
}

func persistAuditedTagRename(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scope *auditScopeState,
	priorTag, resultingTag Tag, priorNodes, resultingNodes []Node,
) error {
	if len(priorNodes) != len(resultingNodes) || (len(priorNodes) != 0) != (scope != nil) {
		return errors.New("audited tag rename has inconsistent affected-node authority")
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(vaultID, authority.lineageID, operationID, recordedAt)
	if err != nil {
		return err
	}
	priorDefinition, err := tagDefinitionAuditRecord(priorTag)
	if err != nil {
		return err
	}
	resultingDefinition, err := tagDefinitionAuditRecord(resultingTag)
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataChange(priorDefinition, resultingDefinition)
	if err != nil {
		return err
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(values.operationID, []audit.Record{change})
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	mutationHash := audit.Absent()
	if len(priorNodes) != 0 {
		events := make([]audit.Record, len(priorNodes))
		stateChanges := make([]audit.Record, len(priorNodes))
		for index := range priorNodes {
			events[index], err = makeAuditedTagRenameEvent(
				values, scope.scopeID, uint64(index), priorNodes[index], resultingNodes[index],
				priorDefinition, resultingDefinition,
			)
			if err != nil {
				return err
			}
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
			mutation, auditAttachedMetadataChangeCountField, audit.Unsigned(1),
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
	allocation, err = addAttachedMetadataToAllocation(allocation, 1, deltaDigest.value)
	if err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func makeAuditedTagRenameEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	priorNode, resultingNode Node, priorDefinition, resultingDefinition audit.Record,
) (audit.Record, error) {
	if priorNode.ID != resultingNode.ID {
		return audit.Record{}, errors.New("audited tag rename changes node identity")
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
		return audit.Record{}, errors.New("audited tag rename has an invalid revision transition")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := attachedAuditIdentity(priorDefinition)
	if err != nil {
		return audit.Record{}, err
	}
	eventIdentity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	kind, err := audit.Text("tag_rename")
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
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
		{Name: auditRecordedAtField, Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersion},
		{Name: "resulting_current_version_id", Value: resultingVersion},
		{Name: auditOriginField, Value: values.origin},
		{Name: auditAgentLabelField, Value: audit.Absent()},
		{Name: auditPreField, Value: audit.Nested(priorDefinition)},
		{Name: auditPostField, Value: audit.Nested(resultingDefinition)},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "baseline_digest", Value: audit.Absent()},
	}}, nil
}
