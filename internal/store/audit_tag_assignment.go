package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (s *Store) changeAuditedTagAssignmentTx(
	ctx context.Context, tx *sql.Tx, tagID string, prior Node, ifRev int64, assign bool,
) (TagAssignmentChange, error) {
	operationID, err := newUUIDv4()
	if err != nil {
		return TagAssignmentChange{}, fmt.Errorf("allocating audited tag operation: %w", err)
	}
	recordedAt := nowRFC3339()
	result, err := changeTagAssignmentTx(
		ctx, tx, tagID, prior, ifRev, assign, recordedAt,
	)
	if err != nil || !result.Changed {
		return result, err
	}
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, prior.ID)
	if err != nil {
		return TagAssignmentChange{}, err
	}
	if err := persistAuditedTagAssignment(
		ctx, tx, s.vaultID, operationID, recordedAt, nodeSequence,
		authority, scopes, prior, result.Node, tagID, assign,
	); err != nil {
		return TagAssignmentChange{}, err
	}
	return result, nil
}

func persistAuditedTagAssignment(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scopes []auditScopeState,
	priorNode, resultingNode Node, tagID string, assign bool,
) error {
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(
		vaultID, authority.lineageID, operationID, recordedAt,
	)
	if err != nil {
		return err
	}
	assignment, err := auditTagAssignmentRecord(tagID, priorNode.ID)
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataPresenceChange(assignment, assign)
	if err != nil {
		return err
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(
		values.operationID, []audit.Record{change},
	)
	if err != nil {
		return err
	}
	events := make([]audit.Record, len(scopes))
	for index, scope := range scopes {
		events[index], err = makeAuditedTagAssignmentEvent(
			values, scope.scopeID, uint64(index), priorNode, resultingNode,
			assignment, assign,
		)
		if err != nil {
			return err
		}
	}
	stateChange, err := makeAuditMemberStateChange(priorNode, resultingNode)
	if err != nil {
		return err
	}
	mutation, err := makeAuditedMemberStateMutation(
		values, sequence, events, stateChange,
	)
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
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	for _, event := range events {
		if err := insertAuditRecord(ctx, tx, audit.Record{
			Kind:   auditEventField,
			Fields: []audit.Field{{Name: auditEventField, Value: audit.Nested(event)}},
		}); err != nil {
			return err
		}
	}
	if err := insertAuditRecord(ctx, tx, mutation); err != nil {
		return err
	}
	if err := advanceAuditedMutationScopes(
		ctx, tx, values, scopes, mutationDigest.value,
	); err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, mutationDigest.value,
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

func auditTagAssignmentRecord(tagID string, nodeID int64) (audit.Record, error) {
	tagValue, err := audit.UUID(tagID)
	if err != nil {
		return audit.Record{}, err
	}
	auditNodeID, err := positiveAuditNodeID(nodeID)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: auditTagAssignmentKind, Fields: []audit.Field{
		{Name: "tag_id", Value: tagValue},
		{Name: metadataNodeIDField, Value: audit.Unsigned(auditNodeID)},
	}}, nil
}

func makeAuditedTagAssignmentEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	priorNode, resultingNode Node, assignment audit.Record, assign bool,
) (audit.Record, error) {
	if priorNode.ID != resultingNode.ID {
		return audit.Record{}, errors.New("audited tag assignment changes node identity")
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
		return audit.Record{}, errors.New("audited tag assignment has an invalid revision transition")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := attachedAuditIdentity(assignment)
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
	eventKind := "tag_assign"
	pre, post := audit.Absent(), audit.Nested(assignment)
	if !assign {
		eventKind, pre, post = "tag_unassign", post, pre
	}
	eventKindValue, err := audit.Text(eventKind)
	if err != nil {
		return audit.Record{}, err
	}
	attachmentKind, err := audit.Text(assignment.Kind)
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
		{Name: "event_kind", Value: eventKindValue},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: "target_node_id", Value: audit.Absent()},
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
		{Name: auditPreField, Value: pre},
		{Name: auditPostField, Value: post},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "baseline_digest", Value: audit.Absent()},
	}}, nil
}
